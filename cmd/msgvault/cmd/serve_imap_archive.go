package cmd

import (
	"context"
	"errors"

	"go.kenn.io/msgvault/internal/emailtags"
	"go.kenn.io/msgvault/internal/gmail"
	imaplib "go.kenn.io/msgvault/internal/imap"
	"go.kenn.io/msgvault/internal/oauth"
	"go.kenn.io/msgvault/internal/store"
)

type imapArchiveJournal struct {
	store  *store.Store
	target store.EmailTagTarget
	source *store.Source
}

func (j imapArchiveJournal) move(m imaplib.ArchiveMove) store.IMAPPendingMove {
	t := j.target
	t.Mailbox, t.UIDValidity, t.UID = m.Target.Mailbox, m.Target.UIDValidity, m.Target.UID
	return store.IMAPPendingMove{SourceIdentifier: j.source.Identifier, Target: t, Destination: m.Destination, DestinationUIDValidity: m.DestinationUIDValidity, DestinationUID: m.DestinationUID}
}
func (j imapArchiveJournal) Begin(ctx context.Context, m imaplib.ArchiveMove) error {
	return j.store.BeginIMAPMoveContext(ctx, j.move(m))
}
func (j imapArchiveJournal) SaveMapping(ctx context.Context, m imaplib.ArchiveMove) error {
	return j.store.SaveIMAPMoveMappingContext(ctx, j.move(m))
}

func (j imapArchiveJournal) Clear(ctx context.Context, m imaplib.ArchiveMove) error {
	return j.store.ClearIMAPMoveContext(ctx, j.move(m))
}

func (a *storeAPIAdapter) archiveIMAPCopy(ctx context.Context, id int64, change emailtags.ArchiveChange, acquireWrite func(context.Context) (func(), error), authorize func(context.Context, store.EmailTagTarget) error) (*emailtags.ArchiveResult, error) {
	ctx = a.invocationContext(ctx)
	resolve := func() (store.EmailTagTarget, error) {
		target, err := a.store.EmailTagTargetContext(ctx, id, "")
		if target.SourceID != 0 {
			if authErr := authorize(ctx, target); authErr != nil {
				return store.EmailTagTarget{}, authErr
			}
		}
		if err != nil {
			return target, err
		}
		return a.store.IMAPArchiveTargetContext(ctx, id)
	}
	target, err := resolve()
	if err != nil {
		return nil, err
	}
	if change.Archived == nil {
		return nil, emailtags.Failure("invalid_request", "archived is required", nil, nil)
	}
	write := !change.DryRun
	if write && acquireWrite != nil {
		release, err := acquireWrite(ctx)
		if err != nil {
			return nil, err
		}
		defer release()
	}
	source, err := a.store.GetSourceByIDContext(ctx, target.SourceID)
	if err != nil {
		return nil, err
	}
	var execution *store.SyncExecution
	if write {
		execution, err = a.store.AcquireSyncExecutionContext(ctx, target.SourceID)
		if err != nil {
			code := "local_failed"
			if errors.Is(err, store.ErrSyncAlreadyActive) {
				code = "sync_active"
			}
			return nil, emailtags.Failure(code, "Cannot acquire source ownership for IMAP archive", nil, err)
		}
		defer func() {
			if err := execution.Release(); err != nil && a.logger != nil {
				a.logger.Error("release source after IMAP archive", "source_id", source.ID, "error", err)
			}
		}()
		locked, err := resolve()
		if err != nil {
			return nil, err
		}
		current, sourceErr := a.store.GetSourceByIDContext(ctx, target.SourceID)
		if sourceErr != nil || current.SourceType != source.SourceType || current.Identifier != source.Identifier || current.SyncConfig != source.SyncConfig || current.OAuthApp != source.OAuthApp || !current.CreatedAt.Equal(source.CreatedAt) || current.MergedIntoSourceID != 0 {
			return nil, emailtags.Failure("stale_identity", "IMAP account changed; sync and retry", nil, sourceErr)
		}
		source = current
		if locked != target {
			return nil, emailtags.Failure("stale_identity", "Message identity changed; sync and retry", nil, nil)
		}
	}
	pending, err := a.store.IMAPPendingMoveContext(ctx, target.SourceID, id)
	if err != nil {
		return nil, err
	}
	if pending != nil && (pending.SourceIdentifier != source.Identifier || source.MergedIntoSourceID != 0) {
		return nil, emailtags.Failure("stale_identity", "IMAP account changed while a move was pending; inspect both folders", nil, nil)
	}
	if source.SyncConfig.Valid {
		account, err := imaplib.ConfigFromJSON(source.SyncConfig.String)
		if err != nil || account.Identifier() != source.Identifier {
			return nil, emailtags.Failure("stale_identity", "IMAP account configuration no longer matches the source; inspect both folders", nil, err)
		}
	}
	factory := a.emailTagClientFactory
	if factory == nil {
		factory = func(ctx context.Context, src *store.Source) (gmail.API, error) {
			return buildAPIClient(ctx, src, oauthManagerCache(invocationFromContext(ctx)), oauth.ScopesGmailReadonly)
		}
	}
	client, err := factory(ctx, source)
	if err != nil {
		return nil, emailtags.Failure("provider_unavailable", "Cannot connect to the IMAP mailbox", nil, err)
	}
	defer func() { _ = client.Close() }()
	native, ok := client.(*imaplib.Client)
	if !ok {
		return nil, emailtags.Failure("archive_unsupported", "Source does not support IMAP archiving", nil, nil)
	}
	journal := imapArchiveJournal{store: a.store, target: target, source: source}
	identity := imaplib.ArchiveCopyIdentity{Mailbox: target.Mailbox, UIDValidity: target.UIDValidity, UID: target.UID}
	var recovery *imaplib.ArchiveMove
	if pending != nil {
		identity = imaplib.ArchiveCopyIdentity{Mailbox: pending.Target.Mailbox, UIDValidity: pending.Target.UIDValidity, UID: pending.Target.UID}
		recovery = &imaplib.ArchiveMove{Target: identity, Destination: pending.Destination, DestinationUIDValidity: pending.DestinationUIDValidity, DestinationUID: pending.DestinationUID}
		journal.target = pending.Target
	}
	result, verified, err := native.ArchiveCopy(ctx, journal, identity, recovery, *change.Archived, change.DryRun)
	if result != nil {
		result.MessageID, result.SourceID = id, target.SourceID
	}
	if err != nil {
		return result, err
	}
	if write && result.Verified && result.Changed {
		// Persist verified remote evidence even if the caller disconnects.
		evidenceCtx, cancel := localDraftEvidenceContext(ctx)
		defer cancel()
		if err := a.store.ReconcileIMAPMoveContext(evidenceCtx, journal.move(*verified)); err != nil {
			return result, emailtags.Failure("remote_accepted_local_failed", "IMAP move was verified but could not be saved locally; sync the account", nil, err)
		}
		a.releaseDraftSourceAndRefreshCache(ctx, source, execution)
		if result.Archived != *change.Archived {
			return result, emailtags.Failure("stale_identity", "Earlier IMAP move was reconciled; retry the requested state", nil, nil)
		}
	}
	return result, nil
}
