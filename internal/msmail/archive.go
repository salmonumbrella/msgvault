package msmail

import (
	"context"
	"encoding/json/v2"
	"errors"
	"net/http"
	"net/url"
	"time"

	"go.kenn.io/msgvault/internal/emailtags"
	"go.kenn.io/msgvault/internal/msgraph"
)

// ArchiveMessage moves one immutable message within the primary mailbox and verifies it.
func (c *Client) ArchiveMessage(ctx context.Context, id string, archived, dryRun bool) (*emailtags.MessageTagResult, error) {
	folder := "inbox"
	if archived {
		folder = "archive"
	}
	resolve := func(name string) (string, error) {
		value, err := c.WellKnownFolderID(ctx, name)
		if err == nil && value != "" {
			return value, nil
		}
		code := "provider_read_failed"
		if errors.Is(err, msgraph.ErrForbidden) {
			code = "insufficient_scope"
		} else if errors.Is(err, msgraph.ErrNotFound) || (err == nil && value == "") {
			code = "folder_not_found"
		}
		return "", emailtags.Failure(code, "Cannot resolve the Microsoft "+name+" folder in the primary mailbox", nil, err)
	}
	destination, err := resolve(folder)
	if err != nil {
		return nil, err
	}
	deleted, err := resolve("deleteditems")
	if err != nil {
		return nil, err
	}
	read := func(ctx context.Context) (MessageInfo, error) {
		m, e := c.LookupMessage(ctx, id)
		if e != nil {
			return m, e
		}
		if m.ID != id || m.ParentFolderID == "" {
			return m, emailtags.Failure("stale_identity", "Microsoft returned a different or incomplete message; sync and retry", nil, nil)
		}
		return m, nil
	}
	before, err := read(ctx)
	if err != nil {
		return nil, archiveReadError(err)
	}
	folders, err := c.ListFolders(ctx)
	if err != nil {
		return nil, emailtags.Failure("provider_read_failed", "Cannot read Microsoft folder catalog; sync and retry", nil, err)
	}
	infos, _, err := c.folderCatalog(ctx, folders, map[string]string{folder: destination, "deleteditems": deleted})
	if err != nil {
		return nil, emailtags.Failure("provider_read_failed", "Cannot read Microsoft folder roles; sync and retry", nil, err)
	}
	parents := make(map[string]string, len(folders))
	for _, f := range folders {
		parents[f.ID] = f.ParentID
	}
	if _, ok := parents[destination]; !ok {
		return nil, emailtags.Failure("stale_identity", "Microsoft destination folder is not in the catalog; sync and retry", nil, nil)
	}
	if _, ok := parents[deleted]; !ok {
		return nil, emailtags.Failure("stale_identity", "Microsoft Deleted Items is not in the catalog; sync and retry", nil, nil)
	}
	parent := before.ParentFolderID
	for steps := 0; parent != ""; steps++ {
		if steps >= len(parents) {
			return nil, emailtags.Failure("stale_identity", "Microsoft folder ancestry is invalid; sync and retry", nil, nil)
		}
		next, ok := parents[parent]
		if !ok || parent == deleted {
			return nil, emailtags.Failure("stale_identity", "Message folder is unavailable or in Deleted Items; sync and retry", nil, nil)
		}
		parent = next
	}
	changed := before.ParentFolderID != destination
	result := &emailtags.MessageTagResult{Provider: SourceType, DryRun: dryRun}
	if dryRun {
		result.Changed = changed
		return result, nil
	}
	if changed {
		raw, postErr := c.SendNoRetry(ctx, http.MethodPost, "/me/messages/"+url.PathEscape(id)+"/move", map[string]string{"destinationId": destination}, "")
		if errors.Is(postErr, msgraph.ErrForbidden) || errors.Is(postErr, msgraph.ErrNotFound) {
			return result, archiveReadError(postErr)
		}
		var throttled *msgraph.ThrottledError
		if errors.As(postErr, &throttled) || errors.Is(postErr, msgraph.ErrNotSent) {
			return result, emailtags.Failure("provider_write_failed", "Microsoft did not accept the move; retry after the limit clears", result, postErr)
		}
		mismatch := false
		if postErr == nil {
			var moved MessageInfo
			mismatch = json.Unmarshal(raw, &moved) != nil || moved.ID != id
		}
		// Readback must finish even if the caller disconnected after the move.
		readCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
		defer cancel()
		after, readErr := read(readCtx)
		if mismatch {
			return result, emailtags.Failure("stale_identity", "Microsoft returned a different message after the move; sync and retry", result, readErr)
		}
		if readErr != nil {
			return result, emailtags.Failure("remote_unknown", "Microsoft move outcome is uncertain; sync before retrying", result, readErr)
		}
		if after.ParentFolderID != destination {
			return result, emailtags.Failure("verification_failed", "Microsoft move readback did not match the destination; sync before retrying", result, postErr)
		}
		before = after
	}
	result.Changed = changed
	result.Verified = true
	result.MicrosoftFolderID = before.ParentFolderID
	result.MicrosoftCategories = before.Categories
	for folderID, info := range infos {
		result.MicrosoftFolders = append(result.MicrosoftFolders, emailtags.MicrosoftFolder{ID: folderID, Name: info.Name, SystemRole: info.SystemRole, System: info.Type == "system"})
	}
	return result, nil
}

func archiveReadError(err error) error {
	var failure *emailtags.MessageTagError
	if errors.As(err, &failure) {
		return failure
	}
	code := "provider_read_failed"
	if errors.Is(err, msgraph.ErrForbidden) {
		code = "insufficient_scope"
	} else if errors.Is(err, msgraph.ErrNotFound) {
		code = "stale_identity"
	}
	return emailtags.Failure(code, "Cannot read the Microsoft message; check account access and sync", nil, err)
}
