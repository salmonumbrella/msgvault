package cmd

import (
	"context"
	"log/slog"
	"net"
	"strconv"
	"testing"

	imapapi "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/emailtags"
	"go.kenn.io/msgvault/internal/gmail"
	"go.kenn.io/msgvault/internal/imap"
	"go.kenn.io/msgvault/internal/store"
	msgsync "go.kenn.io/msgvault/internal/sync"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestIMAPArchivePendingRecoveryBeforeSync(t *testing.T) {
	addr, _ := testutil.StartIMAPMemServerForDrafts(t, testutil.IMAPDraftServerOptions{MessagesPerMailbox: map[string]int{"INBOX": 1, "Archiv": 0}, Caps: imapapi.CapSet{imapapi.CapIMAP4rev1: {}, imapapi.CapMove: {}, imapapi.CapUIDPlus: {}}})
	host, portText, err := net.SplitHostPort(addr)
	require.NoError(t, err)
	port, err := strconv.Atoi(portText)
	require.NoError(t, err)
	imapConfig := &imap.Config{Host: host, Port: port, Username: testutil.IMAPTestUsername, ArchiveMailbox: "Archiv"}
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("imap", imapConfig.Identifier())
	require.NoError(t, err)
	configJSON, err := imapConfig.ToJSON()
	require.NoError(t, err)
	require.NoError(t, st.UpdateSourceSyncConfig(t.Context(), source.ID, configJSON))
	source, err = st.GetSourceByIDContext(t.Context(), source.ID)
	require.NoError(t, err)
	cfg := testConfigValue()
	cfg.Data.DataDir = t.TempDir()
	testCtx := testInvocationContext(t.Context(), cfg, invocationOptions{})
	runSync := func(filtered bool) {
		t.Helper()
		options := imapFolderStateOptions(st, source, false, cfg, testDiscardLogger())
		if filtered {
			options = append(options, imap.WithFolderFilter([]string{"INBOX"}, nil))
		}
		client := imap.NewClient(imapConfig, testutil.IMAPTestPassword, options...)
		defer func() { require.NoError(t, client.Close()) }()
		syncOptions := msgsync.DefaultOptions()
		syncOptions.SourceType = sourceTypeIMAP
		syncOptions.NoResume = true
		summary, err := newMessageSyncer(client, st, syncOptions, invocationFromContext(testCtx)).WithLogger(slog.New(slog.DiscardHandler)).FullWithFinalizer(testCtx, source, func(summary *gmail.SyncSummary) error {
			return saveIMAPFolderStates(testCtx, st, source, client, summary, 0)
		})
		require.NoError(t, err)
		require.Zero(t, summary.Errors)
	}
	runSync(false)
	var id int64
	require.NoError(t, st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT id FROM messages WHERE source_id=?`), source.ID).Scan(&id))
	adapter := &storeAPIAdapter{store: st, config: cfg, logger: slog.New(slog.DiscardHandler), emailTagClientFactory: func(context.Context, *store.Source) (gmail.API, error) {
		return imap.NewClient(imapConfig, testutil.IMAPTestPassword), nil
	}, draftCacheRefresh: func(context.Context, string) error { return nil }}
	// A locally stale destination epoch must refuse before MOVE or intent.
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE imap_folder_state SET uidvalidity=uidvalidity+1 WHERE source_id=? AND mailbox=?`), source.ID, "Archiv")
	require.NoError(t, err)
	desired := true
	_, err = adapter.ArchiveMessage(testCtx, id, emailtags.ArchiveChange{Archived: &desired}, nil, nil)
	require.Error(t, err)
	pendingBefore, err := st.IMAPPendingMoveContext(t.Context(), source.ID, id)
	require.NoError(t, err)
	assert.Nil(t, pendingBefore)
	stillInbox, err := st.IMAPArchiveTargetContext(t.Context(), id)
	require.NoError(t, err)
	assert.Equal(t, "INBOX", stillInbox.Mailbox)
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE imap_folder_state SET uidvalidity=uidvalidity-1 WHERE source_id=? AND mailbox=?`), source.ID, "Archiv")
	require.NoError(t, err)
	result, err := adapter.ArchiveMessage(testCtx, id, emailtags.ArchiveChange{Archived: &desired}, nil, nil)
	require.NoError(t, err)
	require.True(t, result.Verified)
	archived, err := st.IMAPArchiveTargetContext(t.Context(), id)
	require.NoError(t, err)
	assert.Equal(t, "Archiv", archived.Mailbox)
	var next, epoch uint32
	require.NoError(t, st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT uidnext,uidvalidity FROM imap_folder_state WHERE source_id=? AND mailbox=?`), source.ID, "INBOX").Scan(&next, &epoch))
	otherConv, err := st.EnsureConversation(t.Context(), source.ID, "conflict-thread", "Conflict")
	require.NoError(t, err)
	other, err := st.UpsertMessage(&store.Message{SourceID: source.ID, ConversationID: otherConv, SourceMessageID: "synthetic-conflict", MessageType: "email"})
	require.NoError(t, err)
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`INSERT INTO imap_message_memberships(source_id,mailbox,uidvalidity,uid,message_id) VALUES(?,?,?,?,?)`), source.ID, "INBOX", epoch, next, other)
	require.NoError(t, err)
	desired = false
	_, err = adapter.ArchiveMessage(testCtx, id, emailtags.ArchiveChange{Archived: &desired}, nil, nil)
	var failure *emailtags.MessageTagError
	require.ErrorAs(t, err, &failure)
	assert.Equal(t, "remote_accepted_local_failed", failure.Code)
	pending, err := st.IMAPPendingMoveContext(t.Context(), source.ID, id)
	require.NoError(t, err)
	require.NotNil(t, pending)
	// operator removes conflict (e.g. conflicting row deleted) and a routine sync runs
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`DELETE FROM imap_message_memberships WHERE message_id=?`), other)
	require.NoError(t, err)
	runSync(false)
	_, err = adapter.ArchiveMessage(testCtx, id, emailtags.ArchiveChange{Archived: &desired}, nil, nil)
	require.NoError(t, err)
	pending, err = st.IMAPPendingMoveContext(t.Context(), source.ID, id)
	require.NoError(t, err)
	require.Nil(t, pending, "pending move still stuck after exact verified retry")
	final, err := st.IMAPArchiveTargetContext(t.Context(), id)
	require.NoError(t, err)
	assert.Equal(t, "INBOX", final.Mailbox)
	assert.Equal(t, id, final.ID)
	// An intent without COPYUID stays uncertain; ordinary sync still proceeds.
	var archiveEpoch uint32
	require.NoError(t, st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT uidvalidity FROM imap_folder_state WHERE source_id=? AND mailbox=?`), source.ID, "Archiv").Scan(&archiveEpoch))
	intent := store.IMAPPendingMove{SourceIdentifier: source.Identifier, Target: final, Destination: "Archiv", DestinationUIDValidity: archiveEpoch}
	require.NoError(t, st.BeginIMAPMoveContext(t.Context(), intent))
	runSync(false)
	desired = true
	_, err = adapter.ArchiveMessage(testCtx, id, emailtags.ArchiveChange{Archived: &desired}, nil, nil)
	require.ErrorAs(t, err, &failure)
	assert.Equal(t, "remote_unknown", failure.Code)
	pending, err = st.IMAPPendingMoveContext(t.Context(), source.ID, id)
	require.NoError(t, err)
	require.NotNil(t, pending)
	assert.Zero(t, pending.DestinationUID)
	still, err := st.IMAPArchiveTargetContext(t.Context(), id)
	require.NoError(t, err)
	assert.Equal(t, final, still)
	// Simulate the saved mapped copy being moved again by another mail client.
	external, err := imapclient.DialInsecure(addr, nil)
	require.NoError(t, err)
	defer func() { _ = external.Close() }()
	require.NoError(t, external.Login(testutil.IMAPTestUsername, testutil.IMAPTestPassword).Wait())
	_, err = external.Select("INBOX", nil).Wait()
	require.NoError(t, err)
	copyData, err := external.Move(imapapi.UIDSetNum(imapapi.UID(final.UID)), "Archiv").Wait()
	require.NoError(t, err)
	require.NotNil(t, copyData)
	mapped, ok := copyData.DestUIDs.(imapapi.UIDSet)
	require.True(t, ok)
	require.Len(t, mapped, 1)
	intent.DestinationUID = uint32(mapped[0].Start)
	require.NoError(t, st.SaveIMAPMoveMappingContext(t.Context(), intent))
	_, err = external.Select("Archiv", nil).Wait()
	require.NoError(t, err)
	_, err = external.Move(imapapi.UIDSetNum(mapped[0].Start), "INBOX").Wait()
	require.NoError(t, err)
	runSync(false)
	pending, err = st.IMAPPendingMoveContext(t.Context(), source.ID, id)
	require.NoError(t, err)
	require.NotNil(t, pending)
	assert.Equal(t, intent.DestinationUID, pending.DestinationUID)
}
