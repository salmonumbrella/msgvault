package cmd

import (
	"context"
	"errors"

	"go.kenn.io/msgvault/internal/gmail"
	imaplib "go.kenn.io/msgvault/internal/imap"
	"go.kenn.io/msgvault/internal/provideridentity"
	"go.kenn.io/msgvault/internal/store"
	msgsync "go.kenn.io/msgvault/internal/sync"
)

var fastmailIdentityInventoryFactory provideridentity.Factory = provideridentity.NewFastmailInventory

func newMessageSyncer(client gmail.API, st *store.Store, opts *msgsync.Options, state *invocation) *msgsync.Syncer {
	if opts == nil {
		opts = msgsync.DefaultOptions()
	}
	configured := *opts
	if state.cfg != nil {
		configured.RelayedSenders = state.cfg.RelayedSenders
	}
	configured.RemoteImages = configuredRemoteImageFetcher(state.cfg)
	syncer := msgsync.New(client, st, &configured)
	if native, ok := client.(*imaplib.Client); ok {
		syncer.WithBeforeSync(func(ctx context.Context, source *store.Source) error {
			moves, err := st.IMAPPendingMovesContext(ctx, source.ID)
			if err != nil {
				return err
			}
			for _, move := range moves {
				if move.DestinationUID == 0 {
					continue
				}
				if move.SourceIdentifier != source.Identifier || native.AccountIdentifier() != source.Identifier || !source.SyncConfig.Valid {
					return errors.New("pending IMAP move account changed")
				}
				account, err := imaplib.ConfigFromJSON(source.SyncConfig.String)
				if err != nil || account.Identifier() != source.Identifier {
					return errors.New("pending IMAP move account configuration changed")
				}
				identity := imaplib.ArchiveCopyIdentity{Mailbox: move.Target.Mailbox, UIDValidity: move.Target.UIDValidity, UID: move.Target.UID}
				recovery := &imaplib.ArchiveMove{Target: identity, Destination: move.Destination, DestinationUIDValidity: move.DestinationUIDValidity, DestinationUID: move.DestinationUID}
				journal := imapArchiveJournal{store: st, target: move.Target, source: source}
				_, verified, err := native.ArchiveCopy(ctx, journal, identity, recovery, true, false)
				if err == nil {
					err = st.ReconcileIMAPMoveContext(ctx, journal.move(*verified))
				}
				if err != nil {
					if ctx.Err() != nil {
						return ctx.Err()
					}
					continue
				}
			}
			if len(moves) > 0 {
				states, err := loadIMAPFolderStates(st, source.ID)
				if err != nil {
					return err
				}
				native.ResetFolderStates(states)
			}
			return nil
		})
	}
	return withAutomaticProviderIdentityRefresh(syncer, st, state)
}

func withAutomaticProviderIdentityRefresh(syncer *msgsync.Syncer, st *store.Store, state *invocation) *msgsync.Syncer {
	return syncer.WithSuccessfulSyncHook(
		"provider identity refresh",
		func(ctx context.Context, source *store.Source, mailboxChanged bool) error {
			// A run that saw mailbox change always refreshes; a no-op run
			// refreshes only when one is owed, so frequent scheduled no-op
			// syncs do not each cost a provider round trip.
			refresh := provideridentity.AutoRefresh
			if !mailboxChanged {
				refresh = provideridentity.AutoRefreshIfDue
			}
			if state == nil || state.cfg == nil {
				return errors.New("configuration is unavailable")
			}
			_, _, err := refresh(
				ctx,
				state.cfg,
				st,
				source.ID,
				fastmailIdentityInventoryFactory,
			)
			return err
		},
	)
}
