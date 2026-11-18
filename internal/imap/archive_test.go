//nolint:wrapcheck,forcetypeassert // Fixture delegates the test server protocol without altering errors.
package imap

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"sync/atomic"
	"testing"

	imaplib "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

type archiveTestSession struct {
	imapserver.Session

	moves     chan<- imaplib.NumSet
	interrupt *atomic.Bool
	failRead  *atomic.Bool
}

func (s *archiveTestSession) Move(w *imapserver.MoveWriter, set imaplib.NumSet, dest string) error {
	s.moves <- set
	if s.interrupt != nil && s.interrupt.Swap(false) {
		s.failRead.Store(true)
	}
	return s.Session.(imapserver.SessionMove).Move(w, set, dest)
}

func (s *archiveTestSession) Fetch(w *imapserver.FetchWriter, set imaplib.NumSet, opts *imaplib.FetchOptions) error {
	if s.failRead != nil && s.failRead.Swap(false) {
		return errors.New("synthetic readback interruption")
	}
	return s.Session.Fetch(w, set, opts)
}

type archiveTestJournal struct {
	st     *store.Store
	source *store.Source
	target store.EmailTagTarget
}

func (j archiveTestJournal) convert(m ArchiveMove) store.IMAPPendingMove {
	target := j.target
	target.Mailbox, target.UIDValidity, target.UID = m.Target.Mailbox, m.Target.UIDValidity, m.Target.UID
	return store.IMAPPendingMove{SourceIdentifier: j.source.Identifier, Target: target, Destination: m.Destination, DestinationUIDValidity: m.DestinationUIDValidity, DestinationUID: m.DestinationUID}
}
func (j archiveTestJournal) Begin(ctx context.Context, m ArchiveMove) error {
	return j.st.BeginIMAPMoveContext(ctx, j.convert(m))
}
func (j archiveTestJournal) SaveMapping(ctx context.Context, m ArchiveMove) error {
	return j.st.SaveIMAPMoveMappingContext(ctx, j.convert(m))
}

func (j archiveTestJournal) Clear(ctx context.Context, m ArchiveMove) error {
	return j.st.ClearIMAPMoveContext(ctx, j.convert(m))
}

func TestArchiveCopyNativeRoundTrip(t *testing.T) {
	user := imapmemserver.NewUser(testutil.IMAPTestUsername, testutil.IMAPTestPassword)
	require.NoError(t, user.Create("INBOX", nil))
	require.NoError(t, user.Create("Archiv", nil))
	require.NoError(t, user.Create("archiv", nil))
	testutil.AppendIMAPMessage(t, user, "INBOX")
	testutil.AppendIMAPMessage(t, user, "INBOX")
	mem := imapmemserver.New()
	mem.AddUser(user)
	moves := make(chan imaplib.NumSet, 4)
	var interrupt, failRead atomic.Bool
	srv := imapserver.New(&imapserver.Options{InsecureAuth: true, Caps: imaplib.CapSet{imaplib.CapIMAP4rev1: {}, imaplib.CapMove: {}, imaplib.CapUIDPlus: {}}, NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
		return &archiveTestSession{Session: mem.NewSession(), moves: moves, interrupt: &interrupt, failRead: &failRead}, nil, nil
	}})
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	go func() { _ = srv.Serve(listener) }()
	t.Cleanup(func() { _ = srv.Close() })
	client := newTestClient(t, listener.Addr().String())
	client.config.ArchiveMailbox = "Archiv"
	var epoch uint32
	require.NoError(t, client.withDraftConn(t.Context(), func(conn *imapclient.Client) error {
		if _, err := conn.Select("INBOX", nil).Wait(); err != nil {
			return err
		}
		if _, err := conn.Store(imaplib.UIDSetNum(2), &imaplib.StoreFlags{Op: imaplib.StoreFlagsAdd, Flags: []imaplib.Flag{imaplib.FlagDeleted}}, nil).Collect(); err != nil {
			return err
		}
		data, err := conn.Select("INBOX", &imaplib.SelectOptions{ReadOnly: true}).Wait()
		if err != nil {
			return fmt.Errorf("examine fixture: %w", err)
		}
		epoch = data.UIDValidity
		return nil
	}))
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("imap", "owner@example.test")
	require.NoError(t, err)
	conv, err := st.EnsureConversation(t.Context(), source.ID, "thread", "Fixture")
	require.NoError(t, err)
	id, err := st.UpsertMessage(&store.Message{SourceID: source.ID, SourceMessageID: "INBOX|1", ConversationID: conv, MessageType: "email", RFC822MessageID: sql.NullString{String: "fixture@example.test", Valid: true}})
	require.NoError(t, err)
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`INSERT INTO imap_folder_state(source_id,mailbox,uidvalidity,uidnext) VALUES(?,?,?,?)`), source.ID, "INBOX", epoch, 2)
	require.NoError(t, err)
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`INSERT INTO imap_message_memberships(source_id,mailbox,uidvalidity,uid,message_id) VALUES(?,?,?,?,?)`), source.ID, "INBOX", epoch, 1, id)
	require.NoError(t, err)
	target, err := st.EmailTagTargetContext(t.Context(), id, "")
	require.NoError(t, err)
	journal := archiveTestJournal{st: st, source: source, target: target}
	identity := DraftReceipt{Mailbox: target.Mailbox, UIDValidity: target.UIDValidity, UID: target.UID}
	preview, _, err := client.ArchiveCopy(t.Context(), journal, identity, nil, true, true)
	require.NoError(t, err)
	assert.True(t, preview.Changed)
	assert.Empty(t, moves)
	// A process restart with only intent must refuse a resend.
	intent := ArchiveMove{Target: identity, Destination: "Archiv", DestinationUIDValidity: 1}
	require.NoError(t, journal.Begin(t.Context(), intent))
	_, _, err = client.ArchiveCopy(t.Context(), journal, identity, &intent, true, false)
	require.Error(t, err)
	assert.Empty(t, moves)
	require.NoError(t, st.ClearIMAPMoveContext(t.Context(), journal.convert(intent)))
	interrupt.Store(true)
	_, _, err = client.ArchiveCopy(t.Context(), journal, identity, nil, true, false)
	require.Error(t, err)
	assert.Equal(t, imaplib.UIDSetNum(1), <-moves)
	saved, err := st.IMAPPendingMoveContext(t.Context(), source.ID, id)
	require.NoError(t, err)
	require.NotNil(t, saved)
	require.NotZero(t, saved.DestinationUID)
	require.NoError(t, client.Close())
	client = newTestClient(t, listener.Addr().String())
	client.config.ArchiveMailbox = "Archiv"
	recovery := &ArchiveMove{Target: identity, Destination: saved.Destination, DestinationUIDValidity: saved.DestinationUIDValidity, DestinationUID: saved.DestinationUID}
	result, moved, err := client.ArchiveCopy(t.Context(), journal, identity, recovery, true, false)
	require.NoError(t, err)
	require.NotNil(t, moved)
	assert.True(t, result.Verified)
	assert.Empty(t, moves)
	require.NoError(t, st.ReconcileIMAPMoveContext(t.Context(), journal.convert(*moved)))
	target, err = st.EmailTagTargetContext(t.Context(), id, "")
	require.NoError(t, err)
	journal.target = target
	identity = DraftReceipt{Mailbox: target.Mailbox, UIDValidity: target.UIDValidity, UID: target.UID}
	result, _, err = client.ArchiveCopy(t.Context(), journal, identity, nil, true, false)
	require.NoError(t, err)
	assert.False(t, result.Changed)
	assert.True(t, result.Verified)
	assert.Empty(t, moves)
	// Non-INBOX mailbox names are case-sensitive even when they look alike.
	client.config.ArchiveMailbox = "archiv"
	result, moved, err = client.ArchiveCopy(t.Context(), journal, identity, nil, true, false)
	require.NoError(t, err)
	require.NotNil(t, moved)
	assert.True(t, result.Changed)
	assert.Equal(t, imaplib.UIDSetNum(imaplib.UID(identity.UID)), <-moves)
	require.NoError(t, st.ReconcileIMAPMoveContext(t.Context(), journal.convert(*moved)))
	target, err = st.EmailTagTargetContext(t.Context(), id, "")
	require.NoError(t, err)
	assert.Equal(t, "archiv", target.Mailbox)
	journal.target = target
	identity = DraftReceipt{Mailbox: target.Mailbox, UIDValidity: target.UIDValidity, UID: target.UID}
	result, moved, err = client.ArchiveCopy(t.Context(), journal, identity, nil, false, false)
	require.NoError(t, err)
	require.NotNil(t, moved)
	assert.True(t, result.Verified)
	assert.Equal(t, imaplib.UIDSetNum(imaplib.UID(identity.UID)), <-moves)
	require.NoError(t, st.ReconcileIMAPMoveContext(t.Context(), journal.convert(*moved)))
	target, err = st.EmailTagTargetContext(t.Context(), id, "")
	require.NoError(t, err)
	assert.Equal(t, "INBOX", target.Mailbox)
	assert.Equal(t, id, target.ID)
	require.NoError(t, client.withDraftConn(t.Context(), func(conn *imapclient.Client) error {
		if _, err := client.archiveSelect(conn, "INBOX", target.UIDValidity, true); err != nil {
			return err
		}
		exists, err := archiveUIDExists(conn, 2)
		if err != nil {
			return err
		}
		if !exists {
			return errors.New("unrelated pre-deleted UID disappeared")
		}
		return nil
	}))
}

// The stock memory server always confirms writable SELECT. Rewrite just that
// response to exercise a real server downgrade while keeping commands live.
type archiveDowngradeListener struct{ net.Listener }

func (l archiveDowngradeListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return archiveDowngradeConn{Conn: c}, nil
}

type archiveDowngradeConn struct{ net.Conn }

func (c archiveDowngradeConn) Write(p []byte) (int, error) {
	changed := bytes.ReplaceAll(p, []byte("[READ-WRITE]"), []byte("[READ-ONLY]"))
	n, err := c.Conn.Write(changed)
	if err != nil {
		return n, err
	}
	return len(p), nil
}

type archiveRefusalSession struct {
	imapserver.Session

	boxes  []*imaplib.ListData
	writes chan<- string
}

func (s *archiveRefusalSession) List(w *imapserver.ListWriter, _ string, _ []string, _ *imaplib.ListOptions) error {
	for _, box := range s.boxes {
		if err := w.WriteList(box); err != nil {
			return err
		}
	}
	return nil
}
func (s *archiveRefusalSession) Move(w *imapserver.MoveWriter, set imaplib.NumSet, dest string) error {
	s.writes <- "MOVE"
	return s.Session.(imapserver.SessionMove).Move(w, set, dest)
}
func (s *archiveRefusalSession) Copy(set imaplib.NumSet, dest string) (*imaplib.CopyData, error) {
	s.writes <- "COPY"
	return s.Session.Copy(set, dest)
}
func (s *archiveRefusalSession) Store(w *imapserver.FetchWriter, set imaplib.NumSet, flags *imaplib.StoreFlags, opts *imaplib.StoreOptions) error {
	s.writes <- "STORE"
	return s.Session.Store(w, set, flags, opts)
}
func (s *archiveRefusalSession) Expunge(w *imapserver.ExpungeWriter, uids *imaplib.UIDSet) error {
	s.writes <- "EXPUNGE"
	return s.Session.Expunge(w, uids)
}

type refuseJournal struct{ began bool }

func (j *refuseJournal) Begin(context.Context, ArchiveMove) error {
	j.began = true
	return errors.New("unexpected intent")
}
func (*refuseJournal) SaveMapping(context.Context, ArchiveMove) error {
	return errors.New("unexpected mapping")
}

func (*refuseJournal) Clear(context.Context, ArchiveMove) error {
	return errors.New("unexpected clear")
}

func TestArchiveCopyRefusalsDoNotWrite(t *testing.T) {
	inbox := &imaplib.ListData{Mailbox: "INBOX", Delim: '/'}
	archive := &imaplib.ListData{Mailbox: "Archiv", Delim: '/', Attrs: []imaplib.MailboxAttr{imaplib.MailboxAttrArchive}}
	trash := &imaplib.ListData{Mailbox: "Trash", Delim: '/', Attrs: []imaplib.MailboxAttr{imaplib.MailboxAttrTrash}}
	for _, tc := range []struct {
		name, override                       string
		boxes                                []*imaplib.ListData
		move, uidplus, wrongEpoch, downgrade bool
	}{
		{name: "missing override", override: "missing", boxes: []*imaplib.ListData{inbox, archive}, move: true, uidplus: true},
		{name: "ambiguous role", boxes: []*imaplib.ListData{inbox, archive, {Mailbox: "Other", Delim: '/', Attrs: []imaplib.MailboxAttr{imaplib.MailboxAttrArchive}}}, move: true, uidplus: true},
		{name: "unsafe override", override: "Trash", boxes: []*imaplib.ListData{inbox, trash}, move: true, uidplus: true},
		{name: "no MOVE", override: "Archiv", boxes: []*imaplib.ListData{inbox, archive}, uidplus: true},
		{name: "no UIDPLUS", override: "Archiv", boxes: []*imaplib.ListData{inbox, archive}, move: true},
		{name: "changed epoch", override: "Archiv", boxes: []*imaplib.ListData{inbox, archive}, move: true, uidplus: true, wrongEpoch: true},
		{name: "server downgrades select", override: "Archiv", boxes: []*imaplib.ListData{inbox, archive}, move: true, uidplus: true, downgrade: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			user := imapmemserver.NewUser(testutil.IMAPTestUsername, testutil.IMAPTestPassword)
			require.NoError(t, user.Create("INBOX", nil))
			require.NoError(t, user.Create("Archiv", nil))
			testutil.AppendIMAPMessage(t, user, "INBOX")
			mem := imapmemserver.New()
			mem.AddUser(user)
			writes := make(chan string, 8)
			caps := imaplib.CapSet{imaplib.CapIMAP4rev1: {}, imaplib.CapSpecialUse: {}}
			if tc.move {
				caps[imaplib.CapMove] = struct{}{}
			}
			if tc.uidplus {
				caps[imaplib.CapUIDPlus] = struct{}{}
			}
			srv := imapserver.New(&imapserver.Options{InsecureAuth: true, Caps: caps, NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
				return &archiveRefusalSession{Session: mem.NewSession(), boxes: tc.boxes, writes: writes}, nil, nil
			}})
			listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
			require.NoError(t, err)
			serving := listener
			if tc.downgrade {
				serving = archiveDowngradeListener{Listener: listener}
			}
			go func() { _ = srv.Serve(serving) }()
			t.Cleanup(func() { _ = srv.Close() })
			client := newTestClient(t, listener.Addr().String())
			client.config.ArchiveMailbox = tc.override
			var epoch uint32
			require.NoError(t, client.withDraftConn(t.Context(), func(conn *imapclient.Client) error {
				d, err := conn.Select("INBOX", &imaplib.SelectOptions{ReadOnly: true}).Wait()
				if err == nil {
					epoch = d.UIDValidity
				}
				return err
			}))
			if tc.wrongEpoch {
				epoch++
			}
			journal := &refuseJournal{}
			_, _, err = client.ArchiveCopy(t.Context(), journal, DraftReceipt{Mailbox: "INBOX", UIDValidity: epoch, UID: 1}, nil, true, false)
			require.Error(t, err)
			assert.False(t, journal.began)
			assert.Empty(t, writes)
		})
	}
}

type badMappingSession struct {
	imapserver.Session

	mapping string
	writes  chan<- string
}

func (s *badMappingSession) Move(w *imapserver.MoveWriter, _ imaplib.NumSet, _ string) error {
	s.writes <- "MOVE"
	if s.mapping == "missing" {
		return nil
	}
	return w.WriteCopyData(&imaplib.CopyData{UIDValidity: 1, SourceUIDs: imaplib.UIDSetNum(999), DestUIDs: imaplib.UIDSetNum(1)})
}

type savedTestIntent struct {
	move  *ArchiveMove
	saves int
}

func (j *savedTestIntent) Begin(_ context.Context, m ArchiveMove) error { j.move = &m; return nil }
func (j *savedTestIntent) SaveMapping(_ context.Context, m ArchiveMove) error {
	j.move = &m
	j.saves++
	return nil
}

func (j *savedTestIntent) Clear(context.Context, ArchiveMove) error { j.move = nil; return nil }

func TestArchiveCopyUnusableMappingNeverResends(t *testing.T) {
	for _, mapping := range []string{"missing", "wrong source"} {
		t.Run(mapping, func(t *testing.T) {
			user := imapmemserver.NewUser(testutil.IMAPTestUsername, testutil.IMAPTestPassword)
			require.NoError(t, user.Create("INBOX", nil))
			require.NoError(t, user.Create("Archiv", nil))
			testutil.AppendIMAPMessage(t, user, "INBOX")
			mem := imapmemserver.New()
			mem.AddUser(user)
			writes := make(chan string, 4)
			srv := imapserver.New(&imapserver.Options{InsecureAuth: true, Caps: imaplib.CapSet{imaplib.CapIMAP4rev1: {}, imaplib.CapMove: {}, imaplib.CapUIDPlus: {}}, NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
				return &badMappingSession{Session: mem.NewSession(), mapping: mapping, writes: writes}, nil, nil
			}})
			listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
			require.NoError(t, err)
			go func() { _ = srv.Serve(listener) }()
			t.Cleanup(func() { _ = srv.Close() })
			client := newTestClient(t, listener.Addr().String())
			client.config.ArchiveMailbox = "Archiv"
			var epoch uint32
			require.NoError(t, client.withDraftConn(t.Context(), func(conn *imapclient.Client) error {
				d, err := conn.Select("INBOX", &imaplib.SelectOptions{ReadOnly: true}).Wait()
				if err == nil {
					epoch = d.UIDValidity
				}
				return err
			}))
			journal := &savedTestIntent{}
			identity := DraftReceipt{Mailbox: "INBOX", UIDValidity: epoch, UID: 1}
			_, _, err = client.ArchiveCopy(t.Context(), journal, identity, nil, true, false)
			require.Error(t, err)
			assert.Equal(t, "MOVE", <-writes)
			require.NotNil(t, journal.move)
			assert.Zero(t, journal.saves)
			_, _, err = client.ArchiveCopy(t.Context(), journal, identity, journal.move, true, false)
			require.Error(t, err)
			assert.Empty(t, writes)
		})
	}
}

type refusalSession struct {
	imapserver.Session

	status  *imaplib.Error
	partial bool
	calls   *atomic.Int32
}

func (s *refusalSession) Move(w *imapserver.MoveWriter, set imaplib.NumSet, dest string) error {
	s.calls.Add(1)
	if s.partial {
		if err := s.Session.(imapserver.SessionMove).Move(w, set, dest); err != nil {
			return err
		}
	}
	return s.status
}

type refusalJournal struct {
	move     *ArchiveMove
	failSave bool
	clears   int
}

func (j *refusalJournal) Begin(_ context.Context, m ArchiveMove) error { j.move = &m; return nil }
func (j *refusalJournal) SaveMapping(_ context.Context, m ArchiveMove) error {
	if j.failSave {
		return errors.New("save failed")
	}
	j.move = &m
	return nil
}
func (j *refusalJournal) Clear(_ context.Context, _ ArchiveMove) error {
	j.clears++
	j.move = nil
	return nil
}

func TestArchiveMoveRefusalKeepsOnlyUncertainIntents(t *testing.T) {
	for _, tc := range []struct {
		name                        string
		status                      imaplib.Error
		partial, retained, failSave bool
	}{
		{"bad", imaplib.Error{Type: imaplib.StatusResponseTypeBad}, false, false, false},
		{"trycreate", imaplib.Error{Type: imaplib.StatusResponseTypeNo, Code: imaplib.ResponseCodeTryCreate}, false, false, false},
		{"no", imaplib.Error{Type: imaplib.StatusResponseTypeNo}, false, true, false},
		{"partial_no", imaplib.Error{Type: imaplib.StatusResponseTypeNo}, true, true, false},
		{"partial_bad", imaplib.Error{Type: imaplib.StatusResponseTypeBad}, true, true, false},
		{"partial_trycreate", imaplib.Error{Type: imaplib.StatusResponseTypeNo, Code: imaplib.ResponseCodeTryCreate}, true, true, false},
		{"save_failure_bad", imaplib.Error{Type: imaplib.StatusResponseTypeBad}, true, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			user := imapmemserver.NewUser(testutil.IMAPTestUsername, testutil.IMAPTestPassword)
			require.NoError(t, user.Create("INBOX", nil))
			require.NoError(t, user.Create("Archiv", nil))
			testutil.AppendIMAPMessage(t, user, "INBOX")
			mem := imapmemserver.New()
			mem.AddUser(user)
			var calls atomic.Int32
			srv := imapserver.New(&imapserver.Options{InsecureAuth: true, Caps: imaplib.CapSet{imaplib.CapIMAP4rev1: {}, imaplib.CapMove: {}, imaplib.CapUIDPlus: {}}, NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
				return &refusalSession{Session: mem.NewSession(), status: &tc.status, partial: tc.partial, calls: &calls}, nil, nil
			}})
			listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
			require.NoError(t, err)
			go func() { _ = srv.Serve(listener) }()
			t.Cleanup(func() { _ = srv.Close() })
			client := newTestClient(t, listener.Addr().String())
			client.config.ArchiveMailbox = "Archiv"
			var epoch uint32
			require.NoError(t, client.withDraftConn(t.Context(), func(conn *imapclient.Client) error {
				selected, err := conn.Select("INBOX", nil).Wait()
				if err == nil {
					epoch = selected.UIDValidity
				}
				return err
			}))
			identity := DraftReceipt{Mailbox: "INBOX", UIDValidity: epoch, UID: 1}
			journal := &refusalJournal{failSave: tc.failSave}
			_, _, err = client.ArchiveCopy(t.Context(), journal, identity, nil, true, false)
			require.Error(t, err)
			assert.Equal(t, tc.retained, journal.move != nil)
			if tc.retained {
				if tc.partial && !tc.failSave {
					require.NotZero(t, journal.move.DestinationUID)
				}
				_, _, err = client.ArchiveCopy(t.Context(), journal, identity, journal.move, true, false)
				if tc.partial && !tc.failSave {
					require.NoError(t, err)
				} else {
					require.Error(t, err)
				}
			}
			assert.EqualValues(t, 1, calls.Load())
			if tc.retained {
				assert.Zero(t, journal.clears)
			}
		})
	}
}

type taggedReceiptListener struct {
	net.Listener
	epoch *atomic.Uint32
	hits  *atomic.Int32
}

func (l taggedReceiptListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return taggedReceiptConn{Conn: conn, epoch: l.epoch, hits: l.hits}, nil
}

type taggedReceiptConn struct {
	net.Conn
	epoch *atomic.Uint32
	hits  *atomic.Int32
}

func (c taggedReceiptConn) Write(p []byte) (int, error) {
	original := p
	for _, marker := range []string{"OK UID MOVE completed", "OK MOVE completed"} {
		if bytes.Contains(p, []byte(marker)) {
			p = bytes.Replace(p, []byte(marker), []byte(fmt.Sprintf("OK [COPYUID %d 1 1] %s", c.epoch.Load(), marker[3:])), 1)
			c.hits.Add(1)
			break
		}
	}
	_, err := c.Conn.Write(p)
	if err != nil {
		return 0, err
	}
	return len(original), nil
}

type repeatedReceiptSession struct {
	imapserver.Session

	epoch     *atomic.Uint32
	different bool
	moves     *atomic.Int32
}

func (s *repeatedReceiptSession) Move(w *imapserver.MoveWriter, set imaplib.NumSet, dest string) error {
	s.moves.Add(1)
	if err := s.Session.(imapserver.SessionMove).Move(w, set, dest); err != nil {
		return err
	}
	if !s.different {
		return nil
	}
	uid := imaplib.UID(2)
	return w.WriteCopyData(&imaplib.CopyData{UIDValidity: s.epoch.Load(), SourceUIDs: imaplib.UIDSetNum(1), DestUIDs: imaplib.UIDSetNum(uid)})
}

func TestArchiveRepeatedCopyUID(t *testing.T) {
	for _, different := range []bool{false, true} {
		t.Run(fmt.Sprintf("different=%v", different), func(t *testing.T) {
			user := imapmemserver.NewUser(testutil.IMAPTestUsername, testutil.IMAPTestPassword)
			require.NoError(t, user.Create("INBOX", nil))
			require.NoError(t, user.Create("Archiv", nil))
			testutil.AppendIMAPMessage(t, user, "INBOX")
			mem := imapmemserver.New()
			mem.AddUser(user)
			var epoch atomic.Uint32
			var moves atomic.Int32
			srv := imapserver.New(&imapserver.Options{InsecureAuth: true, Caps: imaplib.CapSet{imaplib.CapIMAP4rev1: {}, imaplib.CapMove: {}, imaplib.CapUIDPlus: {}}, NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
				return &repeatedReceiptSession{Session: mem.NewSession(), epoch: &epoch, different: different, moves: &moves}, nil, nil
			}})
			listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
			require.NoError(t, err)
			var tagged atomic.Int32
			serving := net.Listener(listener)
			if !different {
				serving = taggedReceiptListener{Listener: listener, epoch: &epoch, hits: &tagged}
			}
			go func() { _ = srv.Serve(serving) }()
			t.Cleanup(func() { _ = srv.Close() })
			client := newTestClient(t, listener.Addr().String())
			client.config.ArchiveMailbox = "Archiv"
			var sourceEpoch uint32
			require.NoError(t, client.withDraftConn(t.Context(), func(conn *imapclient.Client) error {
				src, err := conn.Select("INBOX", nil).Wait()
				if err != nil {
					return err
				}
				sourceEpoch = src.UIDValidity
				dest, err := conn.Select("Archiv", nil).Wait()
				if err != nil {
					return err
				}
				epoch.Store(dest.UIDValidity)
				return nil
			}))
			identity := DraftReceipt{Mailbox: "INBOX", UIDValidity: sourceEpoch, UID: 1}
			journal := &savedTestIntent{}
			result, _, err := client.ArchiveCopy(t.Context(), journal, identity, nil, true, false)
			if different {
				require.Error(t, err)
				assert.False(t, result.Verified)
			} else {
				require.NoError(t, err)
				assert.True(t, result.Verified)
			}
			require.NotNil(t, journal.move)
			assert.EqualValues(t, 1, journal.move.DestinationUID)
			assert.Equal(t, 1, journal.saves)
			assert.EqualValues(t, 1, moves.Load())
			if !different {
				assert.EqualValues(t, 1, tagged.Load())
			}
			if different {
				result, _, err = client.ArchiveCopy(t.Context(), journal, identity, journal.move, true, false)
				require.NoError(t, err)
				assert.True(t, result.Verified)
				assert.EqualValues(t, 1, moves.Load())
			}
		})
	}
}
