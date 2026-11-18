package deletion

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
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
	"go.kenn.io/msgvault/internal/gmail"
	"go.kenn.io/msgvault/internal/imap"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

// This fixture returns OK with no data for the missing UID STORE and UID
// EXPUNGE after archive, as RFC 3501 requires for non-existent UIDs.
type missingUIDOKSession struct {
	imapserver.Session

	stores   *atomic.Int32
	expunges *atomic.Int32
	honest   *atomic.Bool
}

func (s *missingUIDOKSession) Move(w *imapserver.MoveWriter, set imapapi.NumSet, dest string) error {
	mover, ok := s.Session.(imapserver.SessionMove)
	if !ok {
		return errors.New("fixture lacks MOVE")
	}
	if err := mover.Move(w, set, dest); err != nil {
		return fmt.Errorf("fixture MOVE: %w", err)
	}
	return nil
}
func (s *missingUIDOKSession) Store(w *imapserver.FetchWriter, set imapapi.NumSet, flags *imapapi.StoreFlags, opts *imapapi.StoreOptions) error {
	s.stores.Add(1)
	if s.honest.Load() {
		if err := s.Session.Store(w, set, flags, opts); err != nil {
			return fmt.Errorf("fixture STORE: %w", err)
		}
	}
	return nil
}
func (s *missingUIDOKSession) Expunge(w *imapserver.ExpungeWriter, set *imapapi.UIDSet) error {
	s.expunges.Add(1)
	if s.honest.Load() {
		if err := s.Session.Expunge(w, set); err != nil {
			return fmt.Errorf("fixture EXPUNGE: %w", err)
		}
	}
	return nil
}

type deletionProbeJournal struct {
	st     *store.Store
	source *store.Source
	target store.EmailTagTarget
}

func (j deletionProbeJournal) convert(m imap.ArchiveMove) store.IMAPPendingMove {
	target := j.target
	target.Mailbox, target.UIDValidity, target.UID = m.Target.Mailbox, m.Target.UIDValidity, m.Target.UID
	return store.IMAPPendingMove{SourceIdentifier: j.source.Identifier, Target: target, Destination: m.Destination, DestinationUIDValidity: m.DestinationUIDValidity, DestinationUID: m.DestinationUID}
}
func (j deletionProbeJournal) Begin(ctx context.Context, m imap.ArchiveMove) error {
	return j.st.BeginIMAPMoveContext(ctx, j.convert(m))
}
func (j deletionProbeJournal) SaveMapping(ctx context.Context, m imap.ArchiveMove) error {
	return j.st.SaveIMAPMoveMappingContext(ctx, j.convert(m))
}
func (j deletionProbeJournal) Clear(ctx context.Context, m imap.ArchiveMove) error {
	return j.st.ClearIMAPMoveContext(ctx, j.convert(m))
}

func TestProbeDeleteAfterArchiveMustNotFalselyMarkSourceDeleted(t *testing.T) {
	user := imapmemserver.NewUser(testutil.IMAPTestUsername, testutil.IMAPTestPassword)
	require.NoError(t, user.Create("INBOX", nil))
	require.NoError(t, user.Create("Archiv", nil))
	require.NoError(t, user.Create("Trash", nil))
	testutil.AppendIMAPMessage(t, user, "Archiv")
	testutil.AppendIMAPMessage(t, user, "INBOX")
	mem := imapmemserver.New()
	mem.AddUser(user)
	var stores, expunges atomic.Int32
	var honest atomic.Bool
	srv := imapserver.New(&imapserver.Options{InsecureAuth: true, Caps: imapapi.CapSet{imapapi.CapIMAP4rev1: {}, imapapi.CapMove: {}, imapapi.CapUIDPlus: {}}, NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
		return &missingUIDOKSession{Session: mem.NewSession(), stores: &stores, expunges: &expunges, honest: &honest}, nil, nil
	}})
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	go func() { _ = srv.Serve(listener) }()
	t.Cleanup(func() { _ = srv.Close() })
	addr := listener.Addr().String()
	host, portText, err := net.SplitHostPort(addr)
	require.NoError(t, err)
	port, err := strconv.Atoi(portText)
	require.NoError(t, err)
	cfg := &imap.Config{Host: host, Port: port, Username: testutil.IMAPTestUsername, ArchiveMailbox: "Archiv"}
	observer, err := imapclient.DialInsecure(addr, nil)
	require.NoError(t, err)
	defer func() { _ = observer.Close() }()
	require.NoError(t, observer.Login(testutil.IMAPTestUsername, testutil.IMAPTestPassword).Wait())
	selected, err := observer.Select("INBOX", &imapapi.SelectOptions{ReadOnly: true}).Wait()
	require.NoError(t, err)
	st := testutil.NewTestStore(t)
	if os.Getenv("MSGVAULT_TEST_DB") != "" {
		require.True(t, st.IsPostgreSQL(), "fixture must use PostgreSQL")
	}
	source, err := st.GetOrCreateSource("imap", cfg.Identifier())
	require.NoError(t, err)
	conv, err := st.EnsureConversation(t.Context(), source.ID, "thread", "Fixture")
	require.NoError(t, err)
	id, err := st.UpsertMessage(&store.Message{SourceID: source.ID, SourceMessageID: "INBOX|1", ConversationID: conv, MessageType: "email", RFC822MessageID: sql.NullString{String: "fixture@example.test", Valid: true}})
	require.NoError(t, err)
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`INSERT INTO imap_folder_state(source_id,mailbox,uidvalidity,uidnext) VALUES(?,?,?,?)`), source.ID, "INBOX", selected.UIDValidity, 2)
	require.NoError(t, err)
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`INSERT INTO imap_message_memberships(source_id,mailbox,uidvalidity,uid,message_id) VALUES(?,?,?,?,?)`), source.ID, "INBOX", selected.UIDValidity, 1, id)
	require.NoError(t, err)
	target, err := archiveTargetForTest(t.Context(), st, id)
	require.NoError(t, err)
	client := imap.NewClient(cfg, testutil.IMAPTestPassword)
	defer func() { _ = client.Close() }()
	journal := deletionProbeJournal{st: st, source: source, target: target}
	identity := imap.ArchiveCopyIdentity{Mailbox: target.Mailbox, UIDValidity: target.UIDValidity, UID: target.UID}
	result, moved, err := client.ArchiveCopy(t.Context(), journal, identity, nil, true, false)
	require.NoError(t, err)
	require.True(t, result.Verified)
	require.NotNil(t, moved)
	require.NoError(t, st.ReconcileIMAPMoveContext(t.Context(), journal.convert(*moved)))
	// Confirm the old UID is absent before the protocol-correct OK replies.
	_, err = observer.Select("INBOX", &imapapi.SelectOptions{ReadOnly: true}).Wait()
	require.NoError(t, err)
	old, err := observer.Fetch(imapapi.UIDSetNum(imapapi.UID(target.UID)), &imapapi.FetchOptions{UID: true}).Collect()
	require.NoError(t, err)
	require.Empty(t, old)
	archived, err := archiveTargetForTest(t.Context(), st, id)
	require.NoError(t, err)
	exact := imap.ArchiveCopyIdentity{Mailbox: archived.Mailbox, UIDValidity: archived.UIDValidity, UID: archived.UID}
	stale := exact
	stale.UIDValidity++
	require.ErrorContains(t, client.DeleteCopy(t.Context(), stale), "UIDVALIDITY changed")
	require.ErrorContains(t, client.TrashCopy(t.Context(), stale), "UIDVALIDITY changed")
	absent := exact
	absent.UID += 1000
	var notFound *gmail.NotFoundError
	require.ErrorAs(t, client.DeleteCopy(t.Context(), absent), &notFound)
	require.ErrorAs(t, client.TrashCopy(t.Context(), absent), &notFound)
	assert.Zero(t, stores.Load())
	assert.Zero(t, expunges.Load())
	pending := store.IMAPPendingMove{SourceIdentifier: source.Identifier, Target: archived, Destination: "INBOX", DestinationUIDValidity: selected.UIDValidity}
	require.NoError(t, st.BeginIMAPMoveContext(t.Context(), pending))
	executor := NewExecutor(nil, st, client)
	for _, method := range []Method{MethodTrash, MethodDelete} {
		state, err := executor.deleteOne(t.Context(), source.ID, target.SourceMessageID, method)
		assert.Equal(t, resultFailed, state)
		require.ErrorContains(t, err, "pending")
	}
	require.NoError(t, st.ClearIMAPMoveContext(t.Context(), pending))
	// The server lies about successful STORE/EXPUNGE. The executor must keep
	// the local row live when the exact remote UID remains present.

	deletionResult, deleteErr := executor.deleteOne(t.Context(), source.ID, target.SourceMessageID, MethodDelete)
	t.Logf("delete_result=%v delete_error=%v store_calls=%d expunge_calls=%d", deletionResult, deleteErr, stores.Load(), expunges.Load())
	_, err = observer.Select("Archiv", &imapapi.SelectOptions{ReadOnly: true}).Wait()
	require.NoError(t, err)
	found, err := observer.Fetch(imapapi.UIDSetNum(imapapi.UID(moved.DestinationUID)), &imapapi.FetchOptions{UID: true}).Collect()
	require.NoError(t, err)
	var deleted sql.NullTime
	require.NoError(t, st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT deleted_from_source_at FROM messages WHERE id=?`), id).Scan(&deleted))
	t.Logf("remote_archive_copies=%d local_source_deleted=%v", len(found), deleted.Valid)
	assert.EqualValues(t, 1, stores.Load())
	assert.EqualValues(t, 1, expunges.Load())
	assert.False(t, deleted.Valid && len(found) > 0, "must not mark source-deleted while the actual archived copy remains")
	require.Equal(t, resultFailed, deletionResult)
	require.Error(t, deleteErr)
	honest.Store(true)
	trashIdentity := imap.ArchiveCopyIdentity{Mailbox: "Archiv", UIDValidity: archived.UIDValidity, UID: 1}
	require.NoError(t, client.TrashCopy(t.Context(), trashIdentity))
	_, err = observer.Select("Trash", &imapapi.SelectOptions{ReadOnly: true}).Wait()
	require.NoError(t, err)
	trashed, err := observer.Fetch(imapapi.UIDSetNum(1), &imapapi.FetchOptions{UID: true}).Collect()
	require.NoError(t, err)
	assert.Len(t, trashed, 1)
	deletionResult, deleteErr = executor.deleteOne(t.Context(), source.ID, target.SourceMessageID, MethodDelete)
	require.NoError(t, deleteErr)
	assert.Equal(t, resultSuccess, deletionResult)
	_, err = observer.Select("Archiv", &imapapi.SelectOptions{ReadOnly: true}).Wait()
	require.NoError(t, err)
	remaining, err := observer.Fetch(imapapi.UIDSetNum(imapapi.UID(moved.DestinationUID)), &imapapi.FetchOptions{UID: true}).Collect()
	require.NoError(t, err)
	assert.Empty(t, remaining)
	require.NoError(t, st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT deleted_from_source_at FROM messages WHERE id=?`), id).Scan(&deleted))
	assert.True(t, deleted.Valid)

}
