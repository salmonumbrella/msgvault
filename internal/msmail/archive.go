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
	folders, err := c.ListFolders(ctx)
	if err != nil {
		code := "provider_read_failed"
		if errors.Is(err, msgraph.ErrForbidden) {
			code = "insufficient_scope"
		}
		return nil, emailtags.Failure(code, "Cannot read Microsoft folder catalog; sync and retry", nil, err)
	}
	infos, ids, err := c.folderCatalog(ctx, folders)
	if err != nil {
		code := "provider_read_failed"
		if errors.Is(err, msgraph.ErrForbidden) {
			code = "insufficient_scope"
		}
		return nil, emailtags.Failure(code, "Cannot read Microsoft folder roles; sync and retry", nil, err)
	}
	destination, deleted := ids[folder], ids["deleteditems"]
	if destination == "" || deleted == "" {
		return nil, emailtags.Failure("folder_not_found", "Cannot resolve Microsoft mailbox folders; sync and retry", nil, nil)
	}
	parents := make(map[string]string, len(folders))
	for _, f := range folders {
		parents[f.ID] = f.ParentID
	}
	before, err := read(ctx)
	if err != nil {
		return nil, archiveReadError(err)
	}
	parent := before.ParentFolderID
	seen := make(map[string]bool)
	root := ""
	supplemental := false
	hops := 0
	refuse := func() (*emailtags.MessageTagResult, error) {
		return nil, emailtags.Failure("stale_identity", "Message cannot be archived; sync and retry", nil, nil)
	}
	for parent != "" {
		if parent == deleted || seen[parent] {
			return refuse()
		}
		seen[parent] = true
		if next, ok := parents[parent]; ok {
			parent = next
			continue
		}
		if root == "" {
			root, err = c.WellKnownFolderID(ctx, "msgfolderroot")
			supplemental = true
			if err != nil || root == "" {
				return refuse()
			}
		}
		if parent == root {
			break
		}
		hops++
		if hops > 32 {
			return refuse()
		}
		var ancestor struct {
			ID       string `json:"id"`
			ParentID string `json:"parentFolderId"`
		}
		err = c.GetJSON(ctx, "/me/mailFolders/"+url.PathEscape(parent)+"?$select=id,parentFolderId", &ancestor)
		if err != nil || ancestor.ID != parent || ancestor.ParentID == "" {
			return refuse()
		}
		parent = ancestor.ParentID
	}
	if supplemental {
		after, err := read(ctx)
		if err != nil || after.ParentFolderID != before.ParentFolderID {
			return refuse()
		}
		before = after
	}
	changed := before.ParentFolderID != destination
	result := &emailtags.MessageTagResult{Provider: SourceType, DryRun: dryRun}
	if dryRun {
		result.Changed = changed
		return result, nil
	}
	if changed {
		raw, postErr := c.SendNoRetry(ctx, http.MethodPost, "/me/messages/"+url.PathEscape(id)+"/move", map[string]string{"destinationId": destination}, "")
		if errors.Is(postErr, msgraph.ErrForbidden) {
			return result, emailtags.Failure("insufficient_scope", "Microsoft mail moves require Mail.ReadWrite; reauthorize with add-o365 --graph --mail-write", result, postErr)
		}
		if errors.Is(postErr, msgraph.ErrNotFound) {
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
			if postErr != nil {
				return result, emailtags.Failure("remote_unknown", "Microsoft move outcome is uncertain; sync before retrying", result, postErr)
			}
			return result, emailtags.Failure("verification_failed", "Microsoft move readback did not match the destination; sync before retrying", result, nil)
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
