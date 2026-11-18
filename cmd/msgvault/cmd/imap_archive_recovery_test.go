package cmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strconv"
	"sync/atomic"
	"testing"

	imapapi "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/emailtags"
	"go.kenn.io/msgvault/internal/gmail"
	"go.kenn.io/msgvault/internal/imap"
	"go.kenn.io/msgvault/internal/store"
	msgsync "go.kenn.io/msgvault/internal/sync"
	"go.kenn.io/msgvault/internal/testutil"
)

type failOnceFetchSession struct {
	imapserver.Session

	fail     *atomic.Bool
	moveFail *atomic.Bool
	conflict *atomic.Bool
	epoch    *atomic.Uint32
	moves    *atomic.Int32
}

func (s *failOnceFetchSession) Move(w *imapserver.MoveWriter, set imapapi.NumSet, dest string) error {
	s.moves.Add(1)
	mover, ok := s.Session.(imapserver.SessionMove)
	if !ok {
		return errors.New("fixture lacks MOVE")
	}
	if s.conflict != nil && s.conflict.Swap(false) {
		if err := w.WriteCopyData(&imapapi.CopyData{UIDValidity: s.epoch.Load(), SourceUIDs: imapapi.UIDSetNum(1), DestUIDs: imapapi.UIDSetNum(1)}); err != nil {
			return fmt.Errorf("fixture receipt: %w", err)
		}
	}
	if err := mover.Move(w, set, dest); err != nil {
		return fmt.Errorf("fixture MOVE: %w", err)
	}
	if s.moveFail.Swap(false) {
		return &imapapi.Error{Type: imapapi.StatusResponseTypeNo, Text: "receipt then rejection"}
	}
	return nil
}
func (s *failOnceFetchSession) Fetch(w *imapserver.FetchWriter, set imapapi.NumSet, opts *imapapi.FetchOptions) error {
	if s.fail.Swap(false) {
		return errors.New("transient verification failure")
	}
	if err := s.Session.Fetch(w, set, opts); err != nil {
		return fmt.Errorf("fixture FETCH: %w", err)
	}
	return nil
}

type disconnectReceiptListener struct {
	net.Listener

	armed *atomic.Bool
}

func (l disconnectReceiptListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, fmt.Errorf("accept fixture connection: %w", err)
	}
	return disconnectReceiptConn{Conn: c, armed: l.armed}, nil
}

type disconnectReceiptConn struct {
	net.Conn

	armed *atomic.Bool
}

func (c disconnectReceiptConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	if err == nil && bytes.Contains(p, []byte("[COPYUID ")) && c.armed.Swap(false) {
		_ = c.Close()
	}
	if err != nil {
		return n, fmt.Errorf("write fixture connection: %w", err)
	}
	return n, nil
}

func archiveRecoverySource(t *testing.T, imapConfig *imap.Config) (*store.Store, *store.Source) {
	t.Helper()
	st := testutil.NewTestStore(t)
	if os.Getenv("MSGVAULT_TEST_DB") != "" {
		require.True(t, st.IsPostgreSQL())
	}
	source, err := st.GetOrCreateSource("imap", imapConfig.Identifier())
	require.NoError(t, err)
	configJSON, err := imapConfig.ToJSON()
	require.NoError(t, err)
	require.NoError(t, st.UpdateSourceSyncConfig(t.Context(), source.ID, configJSON))
	source, err = st.GetSourceByIDContext(t.Context(), source.ID)
	require.NoError(t, err)
	return st, source
}

func TestIMAPArchivePendingRecoveryBeforeSync(t *testing.T) {
	user := imapmemserver.NewUser(testutil.IMAPTestUsername, testutil.IMAPTestPassword)
	require.NoError(t, user.Create("INBOX", nil))
	require.NoError(t, user.Create("Archiv", nil))
	testutil.AppendIMAPMessage(t, user, "INBOX")
	mem := imapmemserver.New()
	mem.AddUser(user)
	var fail, moveFail, disconnect atomic.Bool
	var moves atomic.Int32
	srv := imapserver.New(&imapserver.Options{InsecureAuth: true, Caps: imapapi.CapSet{imapapi.CapIMAP4rev1: {}, imapapi.CapMove: {}, imapapi.CapUIDPlus: {}}, NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
		return &failOnceFetchSession{Session: mem.NewSession(), fail: &fail, moveFail: &moveFail, moves: &moves}, nil, nil
	}})
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	go func() { _ = srv.Serve(disconnectReceiptListener{Listener: listener, armed: &disconnect}) }()
	t.Cleanup(func() { _ = srv.Close() })
	addr := listener.Addr().String()
	host, portText, err := net.SplitHostPort(addr)
	require.NoError(t, err)
	port, err := strconv.Atoi(portText)
	require.NoError(t, err)
	imapConfig := &imap.Config{Host: host, Port: port, Username: testutil.IMAPTestUsername, ArchiveMailbox: "Archiv"}
	st, source := archiveRecoverySource(t, imapConfig)
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
	// The server sends COPYUID and then NO. A fresh syncer must recover the
	// saved pair after reconnect, without dispatching another MOVE.
	moveFail.Store(true)
	requestedArchive := true
	_, err = adapter.ArchiveMessage(testCtx, id, emailtags.ArchiveChange{Archived: &requestedArchive}, nil, nil)
	var receiptFailure *emailtags.MessageTagError
	require.ErrorAs(t, err, &receiptFailure)
	assert.Equal(t, "remote_unknown", receiptFailure.Code)
	receiptPending, err := st.IMAPPendingMoveContext(t.Context(), source.ID, id)
	require.NoError(t, err)
	require.NotNil(t, receiptPending)
	require.NotZero(t, receiptPending.DestinationUID)
	assert.EqualValues(t, 1, moves.Load())
	runSync(false)
	receiptPending, err = st.IMAPPendingMoveContext(t.Context(), source.ID, id)
	require.NoError(t, err)
	assert.Nil(t, receiptPending)
	assert.EqualValues(t, 1, moves.Load())
	recovered, err := st.EmailTagTargetContext(t.Context(), id, "")
	require.NoError(t, err)
	assert.Equal(t, "Archiv", recovered.Mailbox)
	assert.Equal(t, id, recovered.ID)
	requestedArchive = false
	_, err = adapter.ArchiveMessage(testCtx, id, emailtags.ArchiveChange{Archived: &requestedArchive}, nil, nil)
	require.NoError(t, err)
	runSync(false)
	// A disconnect after COPYUID also leaves durable evidence for a fresh sync.
	disconnect.Store(true)
	requestedArchive = true
	_, err = adapter.ArchiveMessage(testCtx, id, emailtags.ArchiveChange{Archived: &requestedArchive}, nil, nil)
	require.ErrorAs(t, err, &receiptFailure)
	assert.Equal(t, "remote_unknown", receiptFailure.Code)
	receiptPending, err = st.IMAPPendingMoveContext(t.Context(), source.ID, id)
	require.NoError(t, err)
	require.NotNil(t, receiptPending)
	require.NotZero(t, receiptPending.DestinationUID)
	movesBeforeSync := moves.Load()
	runSync(false)
	receiptPending, err = st.IMAPPendingMoveContext(t.Context(), source.ID, id)
	require.NoError(t, err)
	assert.Nil(t, receiptPending)
	assert.Equal(t, movesBeforeSync, moves.Load())
	recovered, err = st.EmailTagTargetContext(t.Context(), id, "")
	require.NoError(t, err)
	assert.Equal(t, id, recovered.ID)
	assert.Equal(t, "Archiv", recovered.Mailbox)
	requestedArchive = false
	_, err = adapter.ArchiveMessage(testCtx, id, emailtags.ArchiveChange{Archived: &requestedArchive}, nil, nil)
	require.NoError(t, err)
	runSync(false)
	// A locally stale destination epoch must refuse before MOVE or intent.
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE imap_folder_state SET uidvalidity=uidvalidity+1 WHERE source_id=? AND mailbox=?`), source.ID, "Archiv")
	require.NoError(t, err)
	desired := true
	_, err = adapter.ArchiveMessage(testCtx, id, emailtags.ArchiveChange{Archived: &desired}, nil, nil)
	require.Error(t, err)
	pendingBefore, err := st.IMAPPendingMoveContext(t.Context(), source.ID, id)
	require.NoError(t, err)
	assert.Nil(t, pendingBefore)
	stillInbox, err := st.EmailTagTargetContext(t.Context(), id, "")
	require.NoError(t, err)
	assert.Equal(t, "INBOX", stillInbox.Mailbox)
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE imap_folder_state SET uidvalidity=uidvalidity-1 WHERE source_id=? AND mailbox=?`), source.ID, "Archiv")
	require.NoError(t, err)
	result, err := adapter.ArchiveMessage(testCtx, id, emailtags.ArchiveChange{Archived: &desired}, nil, nil)
	require.NoError(t, err)
	require.True(t, result.Verified)
	archived, err := st.EmailTagTargetContext(t.Context(), id, "")
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
	fail.Store(true)
	runSync(false)
	assert.False(t, fail.Load(), "recovery must attempt FETCH")
	_, err = adapter.ArchiveMessage(testCtx, id, emailtags.ArchiveChange{Archived: &desired}, nil, nil)
	require.NoError(t, err)
	pending, err = st.IMAPPendingMoveContext(t.Context(), source.ID, id)
	require.NoError(t, err)
	require.Nil(t, pending, "pending move still stuck after exact verified retry")
	final, err := st.EmailTagTargetContext(t.Context(), id, "")
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
	still, err := st.EmailTagTargetContext(t.Context(), id, "")
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
	secondMove, err := external.Move(imapapi.UIDSetNum(mapped[0].Start), "INBOX").Wait()
	require.NoError(t, err)
	runSync(false)
	pending, err = st.IMAPPendingMoveContext(t.Context(), source.ID, id)
	require.NoError(t, err)
	require.NotNil(t, pending)
	assert.Equal(t, intent.DestinationUID, pending.DestinationUID)
	newUIDs, ok := secondMove.DestUIDs.(imapapi.UIDSet)
	require.True(t, ok)
	require.Len(t, newUIDs, 1)
	newUID := uint32(newUIDs[0].Start)
	var key string
	var live, raw, oldCount, mappedCount, newOnOriginal int
	require.NoError(t, st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT source_message_id, CASE WHEN deleted_from_source_at IS NULL THEN 1 ELSE 0 END, (SELECT COUNT(*) FROM message_raw WHERE message_id=messages.id) FROM messages WHERE id=?`), id).Scan(&key, &live, &raw))
	assert.Equal(t, final.SourceMessageID, key)
	assert.Equal(t, 1, live)
	assert.Equal(t, 1, raw)
	require.NoError(t, st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT COUNT(*) FROM imap_message_memberships WHERE source_id=? AND message_id=? AND mailbox=? AND uidvalidity=? AND uid=?`), source.ID, id, final.Mailbox, final.UIDValidity, final.UID).Scan(&oldCount))
	require.NoError(t, st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT COUNT(*) FROM imap_message_memberships WHERE source_id=? AND mailbox=? AND uidvalidity=? AND uid=?`), source.ID, intent.Destination, intent.DestinationUIDValidity, intent.DestinationUID).Scan(&mappedCount))
	require.NoError(t, st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT COUNT(*) FROM imap_message_memberships WHERE source_id=? AND message_id=? AND mailbox=? AND uid=?`), source.ID, id, "INBOX", newUID).Scan(&newOnOriginal))
	assert.Equal(t, 1, oldCount)
	assert.Zero(t, mappedCount)
	assert.Zero(t, newOnOriginal)
	var rows int
	require.NoError(t, st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT COUNT(*) FROM messages WHERE source_id=? AND source_message_id<>? AND deleted_from_source_at IS NULL`), source.ID, "synthetic-conflict").Scan(&rows))
	assert.Equal(t, 2, rows) // An unrecorded UID is an independent message.
	snapshot := func() [4]uint32 {
		inbox, err := external.Select("INBOX", nil).Wait()
		require.NoError(t, err)
		archive, err := external.Select("Archiv", nil).Wait()
		require.NoError(t, err)
		return [4]uint32{inbox.NumMessages, uint32(inbox.UIDNext), archive.NumMessages, uint32(archive.UIDNext)}
	}
	beforeRetry := snapshot()
	for _, requested := range []bool{true, false} {
		_, err := adapter.ArchiveMessage(testCtx, id, emailtags.ArchiveChange{Archived: &requested}, nil, nil)
		require.ErrorAs(t, err, &failure)
		assert.Equal(t, "remote_unknown", failure.Code)
	}
	assert.Equal(t, beforeRetry, snapshot())
	runSync(false)
	assert.Equal(t, beforeRetry, snapshot())
	after, err := st.IMAPPendingMoveContext(t.Context(), source.ID, id)
	require.NoError(t, err)
	assert.Equal(t, pending, after)
	var rowsAfter int
	require.NoError(t, st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT COUNT(*) FROM messages WHERE source_id=? AND source_message_id<>? AND deleted_from_source_at IS NULL`), source.ID, "synthetic-conflict").Scan(&rowsAfter))
	assert.Equal(t, rows, rowsAfter)
}

func TestIMAPArchiveDisputedReceiptSurvivesSync(t *testing.T) {
	user := imapmemserver.NewUser(testutil.IMAPTestUsername, testutil.IMAPTestPassword)
	require.NoError(t, user.Create("INBOX", nil))
	require.NoError(t, user.Create("Archiv", nil))
	testutil.AppendIMAPMessage(t, user, "INBOX")
	testutil.AppendIMAPMessage(t, user, "Archiv")
	mem := imapmemserver.New()
	mem.AddUser(user)
	var fail, moveFail, disconnect, conflict atomic.Bool
	var epoch atomic.Uint32
	var moves atomic.Int32
	srv := imapserver.New(&imapserver.Options{InsecureAuth: true, Caps: imapapi.CapSet{imapapi.CapIMAP4rev1: {}, imapapi.CapMove: {}, imapapi.CapUIDPlus: {}}, NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
		return &failOnceFetchSession{Session: mem.NewSession(), fail: &fail, moveFail: &moveFail, conflict: &conflict, epoch: &epoch, moves: &moves}, nil, nil
	}})
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	go func() { _ = srv.Serve(disconnectReceiptListener{Listener: listener, armed: &disconnect}) }()
	t.Cleanup(func() { _ = srv.Close() })
	addr := listener.Addr().String()
	host, portText, err := net.SplitHostPort(addr)
	require.NoError(t, err)
	port, err := strconv.Atoi(portText)
	require.NoError(t, err)
	imapConfig := &imap.Config{Host: host, Port: port, Username: testutil.IMAPTestUsername, ArchiveMailbox: "Archiv"}
	st, source := archiveRecoverySource(t, imapConfig)
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
	var sourceEpoch uint32
	require.NoError(t, st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT id FROM messages WHERE source_id=? AND source_message_id=?`), source.ID, "INBOX|1").Scan(&id))
	require.NoError(t, st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT uidvalidity FROM imap_folder_state WHERE source_id=? AND mailbox=?`), source.ID, "Archiv").Scan(&sourceEpoch))
	epoch.Store(sourceEpoch)
	adapter := &storeAPIAdapter{store: st, config: cfg, logger: slog.New(slog.DiscardHandler), emailTagClientFactory: func(context.Context, *store.Source) (gmail.API, error) {
		return imap.NewClient(imapConfig, testutil.IMAPTestPassword), nil
	}, draftCacheRefresh: func(context.Context, string) error { return nil }}
	conflict.Store(true)
	desired := true
	_, err = adapter.ArchiveMessage(testCtx, id, emailtags.ArchiveChange{Archived: &desired}, nil, nil)
	var failure *emailtags.MessageTagError
	require.ErrorAs(t, err, &failure)
	assert.Equal(t, "remote_unknown", failure.Code)
	pending, err := st.IMAPPendingMoveContext(t.Context(), source.ID, id)
	require.NoError(t, err)
	require.NotNil(t, pending)
	assert.Zero(t, pending.DestinationUID)
	runSync(false)
	for _, desired = range []bool{true, false} {
		_, err = adapter.ArchiveMessage(testCtx, id, emailtags.ArchiveChange{Archived: &desired}, nil, nil)
		require.ErrorAs(t, err, &failure)
		assert.Equal(t, "remote_unknown", failure.Code)
	}
	assert.EqualValues(t, 1, moves.Load())
	pending, err = st.IMAPPendingMoveContext(t.Context(), source.ID, id)
	require.NoError(t, err)
	require.NotNil(t, pending)
	assert.Zero(t, pending.DestinationUID)
	var unrelated, attached int
	require.NoError(t, st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT COUNT(*) FROM imap_message_memberships WHERE source_id=? AND mailbox='Archiv' AND uid=1 AND message_id<>?`), source.ID, id).Scan(&unrelated))
	require.NoError(t, st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT COUNT(*) FROM imap_message_memberships WHERE source_id=? AND mailbox='Archiv' AND message_id=?`), source.ID, id).Scan(&attached))
	assert.Equal(t, 1, unrelated)
	assert.Zero(t, attached)
	var otherID int64
	require.NoError(t, st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT message_id FROM imap_message_memberships WHERE source_id=? AND mailbox='Archiv' AND uid=1`), source.ID).Scan(&otherID))
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`DELETE FROM imap_message_memberships WHERE source_id=? AND message_id=?`), source.ID, otherID)
	require.NoError(t, err)
	runSync(true)
	desired = true
	_, err = adapter.ArchiveMessage(testCtx, otherID, emailtags.ArchiveChange{Archived: &desired}, nil, nil)
	require.ErrorContains(t, err, "sync-full")
	require.ErrorContains(t, err, "without folder filters")
	assert.EqualValues(t, 1, moves.Load())
}

func TestIMAPArchiveSelectedCopyRoundTrip(t *testing.T) {
	user := imapmemserver.NewUser(testutil.IMAPTestUsername, testutil.IMAPTestPassword)
	require.NoError(t, user.Create("INBOX", nil))
	require.NoError(t, user.Create("Archiv", nil))
	require.NoError(t, user.Create("Other", nil))
	testutil.AppendIMAPMessage(t, user, "INBOX")
	testutil.AppendIMAPMessage(t, user, "INBOX")
	testutil.AppendIMAPMessage(t, user, "Other")
	mem := imapmemserver.New()
	mem.AddUser(user)
	// The shared session and listener dereference these inactive fault switches.
	var fail, moveFail, disconnect atomic.Bool
	var connections atomic.Int32
	var moves atomic.Int32
	srv := imapserver.New(&imapserver.Options{InsecureAuth: true, Caps: imapapi.CapSet{imapapi.CapIMAP4rev1: {}, imapapi.CapMove: {}, imapapi.CapUIDPlus: {}}, NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
		return &failOnceFetchSession{Session: mem.NewSession(), fail: &fail, moveFail: &moveFail, moves: &moves}, nil, nil
	}})
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	go func() { _ = srv.Serve(disconnectReceiptListener{Listener: listener, armed: &disconnect}) }()
	t.Cleanup(func() { _ = srv.Close() })
	addr := listener.Addr().String()
	testutil.SetIMAPFlags(t, addr, "INBOX", 1, imapapi.FlagSeen)
	testutil.SetIMAPFlags(t, addr, "Other", 1, imapapi.FlagFlagged)
	provider, err := imapclient.DialInsecure(addr, nil)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, provider.Close()) })
	require.NoError(t, provider.Login(testutil.IMAPTestUsername, testutil.IMAPTestPassword).Wait())
	checkProvider := func(mailbox string, wantUIDs []imapapi.UID) {
		t.Helper()
		_, err := provider.Select(mailbox, &imapapi.SelectOptions{ReadOnly: true}).Wait()
		require.NoError(t, err)
		var all imapapi.UIDSet
		all.AddRange(1, 0)
		messages, err := provider.Fetch(all, &imapapi.FetchOptions{UID: true, Flags: true}).Collect()
		require.NoError(t, err)
		require.Len(t, messages, len(wantUIDs))
		var got []imapapi.UID
		for _, message := range messages {
			got = append(got, message.UID)
			if message.UID == 1 {
				if mailbox == "INBOX" {
					assert.Contains(t, message.Flags, imapapi.FlagSeen)
				} else {
					assert.Contains(t, message.Flags, imapapi.FlagFlagged)
				}
			}
		}
		assert.ElementsMatch(t, wantUIDs, got)
	}

	host, portText, err := net.SplitHostPort(addr)
	require.NoError(t, err)
	port, err := strconv.Atoi(portText)
	require.NoError(t, err)
	imapConfig := &imap.Config{Host: host, Port: port, Username: testutil.IMAPTestUsername, ArchiveMailbox: "Archiv"}
	st, source := archiveRecoverySource(t, imapConfig)
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
	require.NoError(t, st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT id FROM messages WHERE source_id=? AND source_message_id='INBOX|1'`), source.ID).Scan(&id))
	adapter := &storeAPIAdapter{store: st, config: cfg, logger: slog.New(slog.DiscardHandler), emailTagClientFactory: func(context.Context, *store.Source) (gmail.API, error) {
		connections.Add(1)
		return imap.NewClient(imapConfig, testutil.IMAPTestPassword), nil
	}, draftCacheRefresh: func(context.Context, string) error { return nil }}
	_, err = st.DB().ExecContext(testCtx, st.Rebind(`UPDATE imap_message_memberships SET message_id=? WHERE source_id=?`), id, source.ID)
	require.NoError(t, err)
	for _, oldKey := range []string{"INBOX|2", "Other|1"} {
		require.NoError(t, st.MarkMessageDeletedContext(testCtx, source.ID, oldKey))
	}
	before, err := st.GetMessage(id)
	require.NoError(t, err)
	copies, err := st.IMAPArchiveCopiesContext(testCtx, source.ID, id)
	require.NoError(t, err)
	require.Len(t, copies, 3)
	requested := true
	preview, err := adapter.ArchiveMessage(testCtx, id, emailtags.ArchiveChange{Archived: &requested, DryRun: true}, nil, nil)
	require.NoError(t, err)
	assert.Equal(t, copies, preview.IMAPCandidates)
	assert.Equal(t, []string{"INBOX", "INBOX", "Other"}, []string{copies[0].Mailbox, copies[1].Mailbox, copies[2].Mailbox})
	assert.Less(t, copies[0].UID, copies[1].UID)
	var selected emailtags.IMAPCopy
	for _, c := range copies {
		if c.Mailbox == "INBOX" && c.UID == 2 {
			selected = c
		}
	}
	require.NotZero(t, selected.UID)
	sibling, err := st.EmailTagTargetContext(testCtx, id, "")
	require.NoError(t, err)
	var destEpoch uint32
	require.NoError(t, st.DB().QueryRowContext(testCtx, st.Rebind(`SELECT uidvalidity FROM imap_folder_state WHERE source_id=? AND mailbox=?`), source.ID, "Archiv").Scan(&destEpoch))
	pending := store.IMAPPendingMove{SourceIdentifier: source.Identifier, Target: sibling, Destination: "Archiv", DestinationUIDValidity: destEpoch}
	require.NoError(t, st.BeginIMAPMoveContext(testCtx, pending))
	_, err = adapter.ArchiveMessage(testCtx, id, emailtags.ArchiveChange{Archived: &requested, IMAPCopy: &selected}, nil, nil)
	require.ErrorContains(t, err, "different IMAP copy has a pending move")
	assert.Zero(t, moves.Load())
	require.NoError(t, st.ClearIMAPMoveContext(testCtx, pending))
	result, err := adapter.ArchiveMessage(testCtx, id, emailtags.ArchiveChange{Archived: &requested, IMAPCopy: &selected}, nil, nil)
	require.NoError(t, err)
	require.NotNil(t, result.IMAPCopy)
	assert.True(t, result.Verified)
	assert.Equal(t, "Archiv", result.IMAPCopy.Mailbox)
	checkProvider("INBOX", []imapapi.UID{1})
	checkProvider("Other", []imapapi.UID{1})
	runSync(false)
	beforeStale := connections.Load()
	_, err = adapter.ArchiveMessage(testCtx, id, emailtags.ArchiveChange{Archived: &requested, IMAPCopy: &selected}, nil, nil)
	require.ErrorContains(t, err, "stale_identity")
	assert.Equal(t, beforeStale, connections.Load())
	assert.EqualValues(t, 1, moves.Load())
	restore := false
	result, err = adapter.ArchiveMessage(testCtx, id, emailtags.ArchiveChange{Archived: &restore, IMAPCopy: result.IMAPCopy}, nil, nil)
	require.NoError(t, err)
	require.NotNil(t, result.IMAPCopy)
	assert.Equal(t, "INBOX", result.IMAPCopy.Mailbox)
	checkProvider("INBOX", []imapapi.UID{1, imapapi.UID(result.IMAPCopy.UID)})
	checkProvider("Other", []imapapi.UID{1})
	runSync(false)
	remaining, err := st.IMAPArchiveCopiesContext(testCtx, source.ID, id)
	require.NoError(t, err)
	assert.Len(t, remaining, 3)
	after, err := st.GetMessage(id)
	require.NoError(t, err)
	assert.Equal(t, before.Subject, after.Subject)
	assert.Equal(t, before.Snippet, after.Snippet)
	var flags string
	require.NoError(t, st.DB().QueryRowContext(testCtx, st.Rebind(`SELECT flags FROM imap_message_memberships WHERE source_id=? AND mailbox=? AND uid=?`), source.ID, "INBOX", 1).Scan(&flags))
	assert.Equal(t, `["\\Seen"]`, flags)
	assert.Contains(t, after.Labels, "INBOX")
	assert.Contains(t, after.Labels, "Other")
	for _, c := range copies {
		if c != selected {
			assert.Contains(t, remaining, c)
		}
	}
}
