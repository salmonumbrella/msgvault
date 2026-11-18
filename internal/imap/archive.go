package imap

import (
	"context"
	"errors"
	"fmt"
	"strings"

	imaplib "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"go.kenn.io/msgvault/internal/emailtags"
)

// ArchiveCopyIdentity identifies one exact IMAP mailbox generation and UID.
type ArchiveCopyIdentity struct {
	Mailbox     string
	UIDValidity uint32
	UID         uint32
}

// ArchiveMove is the source and destination mapping needed for recovery.
type ArchiveMove struct {
	Target                 ArchiveCopyIdentity
	Destination            string
	DestinationUIDValidity uint32
	DestinationUID         uint32
}

// ArchiveJournal persists intent before dispatch and COPYUID before readback.
type ArchiveJournal interface {
	Begin(ctx context.Context, move ArchiveMove) error
	SaveMapping(ctx context.Context, move ArchiveMove) error
	Clear(ctx context.Context, move ArchiveMove) error
}

func archiveFailure(code, message string, err error) error {
	return emailtags.Failure(code, message, nil, err)
}

func (c *Client) archiveDestination(ctx context.Context) (string, error) {
	// Archive decisions use a fresh server topology, not an earlier sync cache.
	c.clearMailboxDiscoveryLocked()
	if _, err := c.listMailboxesLocked(ctx); err != nil {
		return "", err
	}
	boxes := c.mailboxDiscovery
	name := c.config.ArchiveMailbox
	var chosen *imaplib.ListData
	for _, box := range boxes {
		if name != "" {
			if box.Mailbox == name {
				chosen = box
				break
			}
			continue
		}
		if hasAttr(box.Attrs, imaplib.MailboxAttrArchive) && !hasAttr(box.Attrs, imaplib.MailboxAttrNoSelect) {
			if chosen != nil {
				return "", errors.New("multiple archive mailboxes")
			}
			chosen = box
		}
	}
	if chosen == nil || strings.EqualFold(chosen.Mailbox, "INBOX") || hasAttr(chosen.Attrs, imaplib.MailboxAttrNoSelect) || hasAttr(chosen.Attrs, imaplib.MailboxAttrTrash) || hasAttr(chosen.Attrs, imaplib.MailboxAttrJunk) || hasAttr(chosen.Attrs, imaplib.MailboxAttrSent) || hasAttr(chosen.Attrs, imaplib.MailboxAttrDrafts) {
		return "", errors.New("missing or unsafe archive mailbox")
	}
	return chosen.Mailbox, nil
}

func (c *Client) archiveSelect(conn *imapclient.Client, mailbox string, epoch uint32, readOnly bool) (*imaplib.SelectData, error) {
	c.selectedMailbox = ""
	c.selectedUIDValidity = 0
	c.selectedNumMessages = 0
	c.clearQresyncCapture()
	selected, err := conn.Select(mailbox, &imaplib.SelectOptions{ReadOnly: readOnly}).Wait()
	if err != nil {
		return nil, fmt.Errorf("select mailbox: %w", err)
	}
	if selected.UIDValidity == 0 || (epoch != 0 && selected.UIDValidity != epoch) {
		return nil, errors.New("UIDVALIDITY changed")
	}
	if !readOnly && (selected.ReadOnly == nil || *selected.ReadOnly) {
		return nil, errors.New("server did not confirm writable selection")
	}
	return selected, nil
}

func archiveUIDExists(conn *imapclient.Client, uid uint32) (bool, error) {
	messages, err := conn.Fetch(imaplib.UIDSetNum(imaplib.UID(uid)), &imaplib.FetchOptions{UID: true}).Collect()
	if err != nil {
		return false, fmt.Errorf("fetch UID: %w", err)
	}
	if len(messages) == 0 {
		return false, nil
	}
	if len(messages) != 1 || uint32(messages[0].UID) != uid {
		return false, errors.New("unexpected UID FETCH response")
	}
	return true, nil
}

func singletonArchiveUID(set imaplib.NumSet) (uint32, bool) {
	uids, ok := set.(imaplib.UIDSet)
	if !ok || len(uids) != 1 || uids[0].Start == 0 || uids[0].Start != uids[0].Stop {
		return 0, false
	}
	return uint32(uids[0].Start), true
}

// ArchiveCopy uses only native UID MOVE and durably records its exact mapping
// before readback. The caller holds the source execution lease for writes.
func (c *Client) ArchiveCopy(ctx context.Context, journal ArchiveJournal, target ArchiveCopyIdentity, pending *ArchiveMove, archived, dry bool) (*emailtags.ArchiveResult, *ArchiveMove, error) {
	result := &emailtags.ArchiveResult{Archived: archived, DryRun: dry}
	err := c.withDraftConn(ctx, func(conn *imapclient.Client) error {
		if pending != nil {
			if pending.DestinationUID == 0 {
				return archiveFailure("remote_unknown", "An earlier IMAP move may have run; sync and inspect both folders before retrying", nil)
			}
			if dry {
				return archiveFailure("remote_unknown", "An earlier IMAP move requires reconciliation before preview", nil)
			}
		} else {
			destination := "INBOX"
			if archived {
				var err error
				destination, err = c.archiveDestination(ctx)
				if err != nil {
					return archiveFailure("archive_unsupported", "Set archive_mailbox for this IMAP account with add-imap or add-o365 --archive-mailbox", err)
				}
			}
			selected, err := c.archiveSelect(conn, target.Mailbox, target.UIDValidity, true)
			if err != nil {
				return archiveFailure("stale_identity", "Cannot verify source mailbox and UIDVALIDITY; sync before retrying", err)
			}
			_ = selected
			exists, err := archiveUIDExists(conn, target.UID)
			if err != nil || !exists {
				return archiveFailure("stale_identity", "IMAP message is absent or cannot be read; sync before retrying", err)
			}
			if target.Mailbox == destination || (strings.EqualFold(target.Mailbox, "INBOX") && destination == "INBOX") {
				result.Verified = true
				return nil
			}
			if !conn.Caps().Has(imaplib.CapMove) || !conn.Caps().Has(imaplib.CapUIDPlus) {
				return archiveFailure("archive_unsupported", "IMAP server must support native MOVE and UIDPLUS", nil)
			}
			dest, err := c.archiveSelect(conn, destination, 0, true)
			if err != nil {
				return archiveFailure("provider_read_failed", "Cannot examine destination mailbox", err)
			}
			if dry {
				result.Changed = true
				return nil
			}
			if _, err := c.archiveSelect(conn, target.Mailbox, target.UIDValidity, false); err != nil {
				return archiveFailure("provider_read_failed", "Source mailbox is not explicitly writable", err)
			}
			exists, err = archiveUIDExists(conn, target.UID)
			if err != nil || !exists {
				return archiveFailure("stale_identity", "IMAP source copy changed before MOVE", err)
			}
			m := ArchiveMove{Target: target, Destination: destination, DestinationUIDValidity: dest.UIDValidity}
			if err := journal.Begin(ctx, m); err != nil {
				return archiveFailure("local_failed", "Cannot record IMAP move intent", err)
			}
			if err := ctx.Err(); err != nil {
				// No command has been sent; finish removing the intent after cancellation.
				if clearErr := journal.Clear(context.WithoutCancel(ctx), m); clearErr != nil {
					return archiveFailure("local_failed", "Cannot clear undispatched IMAP move intent", clearErr)
				}
				return err
			}
			data, err := conn.Move(imaplib.UIDSetNum(imaplib.UID(target.UID)), destination).Wait()
			if err != nil || data == nil {
				return archiveFailure("remote_unknown", "IMAP MOVE may have run; sync and inspect both folders before retrying", err)
			}
			old, oldOK := singletonArchiveUID(data.SourceUIDs)
			next, nextOK := singletonArchiveUID(data.DestUIDs)
			if !oldOK || !nextOK || old != target.UID || data.UIDValidity != dest.UIDValidity {
				return archiveFailure("remote_unknown", "IMAP MOVE returned an unusable COPYUID; sync and inspect both folders", nil)
			}
			m.DestinationUID = next
			// Saving the reply must finish even if the caller disconnects.
			if err := journal.SaveMapping(context.WithoutCancel(ctx), m); err != nil {
				return archiveFailure("remote_unknown", "IMAP MOVE mapping could not be saved; sync and inspect both folders", err)
			}
			pending = &m
		}
		old := pending.Target
		if _, err := c.archiveSelect(conn, old.Mailbox, old.UIDValidity, true); err != nil {
			return archiveFailure("remote_unknown", "Cannot verify the original IMAP mailbox; sync and inspect both folders", err)
		}
		exists, err := archiveUIDExists(conn, old.UID)
		if err != nil || exists {
			return archiveFailure("remote_unknown", "Original IMAP UID is not confirmed absent; sync and inspect both folders", err)
		}
		if _, err := c.archiveSelect(conn, pending.Destination, pending.DestinationUIDValidity, true); err != nil {
			return archiveFailure("remote_unknown", "Cannot verify destination IMAP epoch; sync and inspect both folders", err)
		}
		exists, err = archiveUIDExists(conn, pending.DestinationUID)
		if err != nil || !exists {
			return archiveFailure("remote_unknown", "Mapped IMAP destination UID is not confirmed; sync and inspect both folders", err)
		}
		// The daemon reconciles the verified pair under the source lease.
		result.Changed = true
		result.Verified = true
		result.Archived = !strings.EqualFold(pending.Destination, "INBOX")
		return nil
	})
	if err != nil {
		if _, ok := errors.AsType[*emailtags.MessageTagError](err); ok {
			return result, pending, err
		}
		return result, pending, archiveFailure("provider_read_failed", fmt.Sprintf("Cannot access IMAP mailbox %q", target.Mailbox), err)
	}
	return result, pending, nil
}
