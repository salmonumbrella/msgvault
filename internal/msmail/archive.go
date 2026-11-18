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
		return nil, archiveReadError(err)
	}
	infos, ids, err := c.folderCatalog(ctx, folders)
	if err != nil {
		return nil, archiveReadError(err)
	}
	destination, deleted := ids[folder], ids["deleteditems"]
	if _, ok := infos[destination]; destination == "" || deleted == "" || !ok {
		return nil, emailtags.Failure("folder_not_found", "Microsoft mailbox folder is unavailable", nil, nil)
	}
	before, err := read(ctx)
	if err != nil {
		return nil, archiveReadError(err)
	}
	err = c.checkArchiveAncestry(ctx, before.ParentFolderID, deleted)
	if err != nil {
		return nil, err
	}
	after, err := read(ctx)
	if err != nil {
		return nil, archiveReadError(err)
	}
	if after.ParentFolderID != before.ParentFolderID {
		return nil, archiveRefusal()
	}
	before = after
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
	result.MicrosoftFolders = make(map[string]emailtags.MicrosoftFolder, len(infos))
	for folderID, info := range infos {
		result.MicrosoftFolders[folderID] = emailtags.MicrosoftFolder(info)
	}
	return result, nil
}

// archiveRefusal reports a message whose current folder archive actions must not move.
func archiveRefusal() error {
	return emailtags.Failure("stale_identity", "Message cannot be archived from its current folder", nil, nil)
}

// checkArchiveAncestry proves the folder is in the primary mail tree outside Deleted Items.
// Provider read failures keep their own error class; only a positive finding refuses.
func (c *Client) checkArchiveAncestry(ctx context.Context, parent, deleted string) error {
	root, err := c.WellKnownFolderID(ctx, "msgfolderroot")
	if err != nil {
		return archiveReadError(err)
	}
	if root == "" {
		return archiveRefusal()
	}
	for parent != root {
		if parent == deleted {
			return archiveRefusal()
		}
		var ancestor struct {
			ID       string `json:"id"`
			ParentID string `json:"parentFolderId"`
		}
		err = c.GetJSON(ctx, "/me/mailFolders/"+url.PathEscape(parent)+"?$select=id,parentFolderId", &ancestor)
		if err != nil {
			return archiveReadError(err)
		}
		if ancestor.ID != parent || ancestor.ParentID == "" {
			return archiveRefusal()
		}
		parent = ancestor.ParentID
	}
	return nil
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
	return emailtags.Failure(code, "Cannot read the Microsoft mailbox; check account access and sync", nil, err)
}
