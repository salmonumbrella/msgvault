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

type archiveSnapshot struct {
	ID         string    `json:"id"`
	Parent     string    `json:"parentFolderId"`
	Categories *[]string `json:"categories"`
}

func (c *Client) archiveRead(ctx context.Context, id string) (archiveSnapshot, error) {
	var m archiveSnapshot
	err := c.GetJSON(ctx, "/me/messages/"+url.PathEscape(id)+"?$select=id,parentFolderId,categories", &m)
	if err != nil {
		return m, err
	}
	if m.ID != id || m.Parent == "" {
		return m, emailtags.Failure("stale_identity", "Microsoft returned a different or incomplete message; sync and retry", nil, nil)
	}
	return m, nil
}

// ArchiveMessage moves one immutable message within the primary mailbox and verifies it.
func (c *Client) ArchiveMessage(ctx context.Context, id string, archived, dryRun bool) (*emailtags.MessageTagResult, error) {
	folder := "inbox"
	if archived {
		folder = "archive"
	}
	destination, err := c.WellKnownFolderID(ctx, folder)
	if err != nil || destination == "" {
		code := "provider_read_failed"
		if errors.Is(err, msgraph.ErrForbidden) {
			code = "insufficient_scope"
		}
		if errors.Is(err, msgraph.ErrNotFound) || (err == nil && destination == "") {
			code = "folder_not_found"
		}
		return nil, emailtags.Failure(code, "Cannot resolve the Microsoft "+folder+" folder in the primary mailbox", nil, err)
	}
	before, err := c.archiveRead(ctx, id)
	if err != nil {
		return nil, archiveReadError(err)
	}
	result := &emailtags.MessageTagResult{Provider: SourceType, DryRun: dryRun}
	needsMove := before.Parent != destination
	if dryRun {
		if needsMove {
			result.Before = []string{"INBOX"}
		}
		return result, nil
	}
	if needsMove {
		raw, postErr := c.SendNoRetry(ctx, http.MethodPost, "/me/messages/"+url.PathEscape(id)+"/move", map[string]string{"destinationId": destination}, "")
		if postErr != nil && (errors.Is(postErr, msgraph.ErrForbidden) || errors.Is(postErr, msgraph.ErrNotFound)) {
			return result, archiveReadError(postErr)
		}
		if postErr == nil {
			var moved archiveSnapshot
			if json.Unmarshal(raw, &moved) != nil || moved.ID != id {
				return result, emailtags.Failure("stale_identity", "Microsoft returned a different message after the move; sync and retry", result, nil)
			}
		}
		readCtx := ctx
		cancel := func() {}
		if ctx.Err() != nil {
			// A move may have succeeded after cancellation; bounded readback avoids a blind resend.
			readCtx, cancel = context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
		}
		defer cancel()
		after, readErr := c.archiveRead(readCtx, id)
		if readErr != nil {
			return result, emailtags.Failure("remote_unknown", "Microsoft move outcome is uncertain; sync before retrying", result, readErr)
		}
		if after.Parent != destination {
			return result, emailtags.Failure("verification_failed", "Microsoft move readback did not match the destination; sync before retrying", result, postErr)
		}
		before = after
		result.Before = []string{"INBOX"}
	}
	catalogCtx := ctx
	cancelCatalog := func() {}
	if ctx.Err() != nil {
		// Finish recording verified remote evidence after the caller disconnects.
		catalogCtx, cancelCatalog = context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	}
	defer cancelCatalog()
	folders, err := c.ListFolders(catalogCtx)
	if err != nil {
		return result, emailtags.Failure("remote_accepted_local_failed", "Microsoft folder was verified but its catalog could not be read; sync the account", result, err)
	}
	roles := make(map[string]string)
	for name, role := range systemFolders {
		folderID, roleErr := c.WellKnownFolderID(catalogCtx, name)
		if roleErr != nil && !errors.Is(roleErr, msgraph.ErrNotFound) {
			return result, emailtags.Failure("remote_accepted_local_failed", "Cannot read Microsoft folder roles; sync the account", result, roleErr)
		}
		if folderID != "" {
			roles[folderID] = role
		}
	}
	for _, f := range folders {
		role, system := roles[f.ID]
		result.MicrosoftFolders = append(result.MicrosoftFolders, emailtags.MicrosoftFolder{ID: f.ID, Name: f.Path, SystemRole: role, System: system})
	}
	result.MicrosoftFolderID = before.Parent
	result.MicrosoftCategories = before.Categories
	result.Verified = true
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
