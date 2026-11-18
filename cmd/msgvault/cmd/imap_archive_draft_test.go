package cmd

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	imapapi "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/emailtags"
	"go.kenn.io/msgvault/internal/gmail"
	"go.kenn.io/msgvault/internal/imap"
	"go.kenn.io/msgvault/internal/store"
	msgsync "go.kenn.io/msgvault/internal/sync"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestIMAPArchiveManagedDraftGrantAndReceipt(t *testing.T) {
	addr, user := testutil.StartIMAPMemServerForDrafts(t, testutil.IMAPDraftServerOptions{
		MessagesPerMailbox: map[string]int{"Drafts": 0},
		Caps:               imapapi.CapSet{imapapi.CapIMAP4rev1: {}, imapapi.CapMove: {}, imapapi.CapUIDPlus: {}},
	})
	require.NoError(t, user.Create("Archiv", nil))
	require.NoError(t, user.Create("INBOX", nil))
	fixture := newReviewManagedLifecycleFixtureOnServer(t, addr, func() {
		testutil.AppendIMAPRawMessage(t, user, "Drafts", []byte("From: alice@example.com\r\nTo: bob@example.com\r\nSubject: Original\r\n\r\noriginal\r\n"))
	})
	if os.Getenv("MSGVAULT_TEST_DB") != "" {
		require.True(t, fixture.store.IsPostgreSQL())
	}
	fixture.config.ArchiveMailbox = "Archiv"
	configJSON, err := fixture.config.ToJSON()
	require.NoError(t, err)
	require.NoError(t, fixture.store.UpdateSourceSyncConfig(t.Context(), fixture.source.ID, configJSON))
	_, err = fixture.store.DB().ExecContext(t.Context(), fixture.store.Rebind(`INSERT INTO imap_folder_state(source_id, mailbox, uidvalidity, uidnext) VALUES(?,?,?,?)`), fixture.source.ID, "Drafts", 1, 2)
	require.NoError(t, err)
	cfg := testConfigValue()
	cfg.Data.DataDir = t.TempDir()
	ctx := testInvocationContext(t.Context(), cfg, invocationOptions{})
	fixture.adapter.config = cfg
	fixture.adapter.logger = slog.New(slog.DiscardHandler)
	fixture.adapter.emailTagClientFactory = func(context.Context, *store.Source) (gmail.API, error) {
		return imap.NewClient(fixture.config, testutil.IMAPTestPassword), nil
	}
	fixture.adapter.draftCacheRefresh = func(context.Context, string) error { return nil }
	_, err = fixture.store.ClaimIMAPDraftContext(t.Context(), fixture.draft.DraftID, 1, store.IMAPDraftOperationDelete, nil)
	require.NoError(t, err)
	server := api.NewServerWithOptions(api.ServerOptions{Config: &config.Config{}, Store: fixture.adapter, Logger: slog.New(slog.DiscardHandler)})
	request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/messages/"+strconv.FormatInt(fixture.draft.CurrentMessageID, 10)+"/archive", strings.NewReader(`{"archived":true}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	server.Router().ServeHTTP(response, request)
	require.Equal(t, http.StatusConflict, response.Code, response.Body.String())
	require.Contains(t, response.Body.String(), "pending")
	require.NotContains(t, response.Body.String(), "Drafts")
	require.Equal(t, uint32(1), reviewDraftMailboxCount(t, addr))
	_, err = fixture.store.AbortIMAPDraftContext(t.Context(), fixture.draft.DraftID, 1, "not_attempted")
	require.NoError(t, err)
	archived := true
	_, err = fixture.adapter.ArchiveMessage(ctx, fixture.draft.CurrentMessageID, emailtags.ArchiveChange{Archived: &archived}, nil, nil)
	require.NoError(t, err, "provider cause: %v", errors.Unwrap(err))
	moved, err := fixture.store.GetIMAPDraftContext(t.Context(), fixture.draft.DraftID)
	require.NoError(t, err)
	target, err := fixture.store.EmailTagTargetContext(t.Context(), moved.CurrentMessageID, "")
	require.NoError(t, err)
	require.Equal(t, "Archiv", moved.CurrentReceipt.Mailbox)
	require.Equal(t, target.UIDValidity, moved.CurrentReceipt.UIDValidity)
	require.Equal(t, target.UID, moved.CurrentReceipt.UID)
	for _, args := range [][]string{{api.CLIRunDraftEditCommand, moved.DraftID, "--revision", "1", "--body", "candidate"}, {api.CLIRunDraftDeleteCommand, moved.DraftID, "--revision", "1"}} {
		_, err = runReviewLifecycle(t, fixture.adapter, args...)
		require.ErrorContains(t, err, "invalid_mailbox")
	}
	fixture.adapter.draftPolicy = []config.IMAPDraftSource{{SourceID: fixture.source.ID, Enabled: true, Mailbox: "Archiv"}}
	_, err = runReviewLifecycle(t, fixture.adapter, api.CLIRunDraftEditCommand, moved.DraftID, "--revision", "1", "--body", "candidate")
	require.NoError(t, err)
	_, err = runReviewLifecycle(t, fixture.adapter, api.CLIRunDraftDeleteCommand, moved.DraftID, "--revision", "2")
	require.NoError(t, err)
}

func TestIMAPArchiveManagedDraftPendingRecoveredBeforeSync(t *testing.T) {
	user := imapmemserver.NewUser(testutil.IMAPTestUsername, testutil.IMAPTestPassword)
	for _, name := range []string{"Drafts", "Archiv", "INBOX"} {
		require.NoError(t, user.Create(name, nil))
	}
	memory := imapmemserver.New()
	memory.AddUser(user)
	var fail, moveFail atomic.Bool
	var moves atomic.Int32
	server := imapserver.New(&imapserver.Options{InsecureAuth: true, Caps: imapapi.CapSet{imapapi.CapIMAP4rev1: {}, imapapi.CapMove: {}, imapapi.CapUIDPlus: {}}, NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
		return &failOnceFetchSession{Session: memory.NewSession(), fail: &fail, moveFail: &moveFail, moves: &moves}, nil, nil
	}})
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })
	fixture := newReviewManagedLifecycleFixtureOnServer(t, listener.Addr().String(), func() {
		testutil.AppendIMAPRawMessage(t, user, "Drafts", []byte("From: alice@example.com\r\nTo: bob@example.com\r\nSubject: Original\r\n\r\noriginal\r\n"))
	})
	if os.Getenv("MSGVAULT_TEST_DB") != "" {
		require.True(t, fixture.store.IsPostgreSQL())
	}
	fixture.config.ArchiveMailbox = "Archiv"
	encoded, err := fixture.config.ToJSON()
	require.NoError(t, err)
	require.NoError(t, fixture.store.UpdateSourceSyncConfig(t.Context(), fixture.source.ID, encoded))
	_, err = fixture.store.DB().ExecContext(t.Context(), fixture.store.Rebind(`INSERT INTO imap_folder_state(source_id, mailbox, uidvalidity, uidnext) VALUES(?,?,?,?)`), fixture.source.ID, "Drafts", 1, 2)
	require.NoError(t, err)
	source, err := fixture.store.GetSourceByIDContext(t.Context(), fixture.source.ID)
	require.NoError(t, err)
	cfg := testConfigValue()
	cfg.Data.DataDir = t.TempDir()
	ctx := testInvocationContext(t.Context(), cfg, invocationOptions{})
	fixture.adapter.config = cfg
	fixture.adapter.logger = slog.New(slog.DiscardHandler)
	fixture.adapter.emailTagClientFactory = func(context.Context, *store.Source) (gmail.API, error) {
		return imap.NewClient(fixture.config, testutil.IMAPTestPassword), nil
	}
	fixture.adapter.draftCacheRefresh = func(context.Context, string) error { return nil }
	moveFail.Store(true)
	archived := true
	_, err = fixture.adapter.ArchiveMessage(ctx, fixture.draft.CurrentMessageID, emailtags.ArchiveChange{Archived: &archived}, nil, nil)
	require.Error(t, err)
	pending, err := fixture.store.IMAPPendingMoveContext(t.Context(), source.ID, fixture.draft.CurrentMessageID)
	require.NoError(t, err)
	require.NotNil(t, pending)
	require.NotZero(t, pending.DestinationUID)
	for _, args := range [][]string{{api.CLIRunDraftEditCommand, fixture.draft.DraftID, "--revision", "1", "--body", "candidate"}, {api.CLIRunDraftDeleteCommand, fixture.draft.DraftID, "--revision", "1"}} {
		_, err = runReviewLifecycle(t, fixture.adapter, args...)
		require.ErrorContains(t, err, "pending_operation")
	}
	require.EqualValues(t, 1, moves.Load())
	client := imap.NewClient(fixture.config, testutil.IMAPTestPassword, imapFolderStateOptions(fixture.store, source, false, cfg, testDiscardLogger())...)
	defer func() { require.NoError(t, client.Close()) }()
	options := msgsync.DefaultOptions()
	options.SourceType = sourceTypeIMAP
	options.NoResume = true
	summary, err := newMessageSyncer(client, fixture.store, options, invocationFromContext(ctx)).WithLogger(slog.New(slog.DiscardHandler)).FullWithFinalizer(ctx, source, func(summary *gmail.SyncSummary) error {
		return saveIMAPFolderStates(ctx, fixture.store, source, client, summary, 0)
	})
	require.NoError(t, err)
	require.Zero(t, summary.Errors)
	require.EqualValues(t, 1, moves.Load())
	recovered, err := fixture.store.GetIMAPDraftContext(t.Context(), fixture.draft.DraftID)
	require.NoError(t, err)
	require.Equal(t, "Archiv", recovered.CurrentReceipt.Mailbox)
	require.Equal(t, pending.DestinationUIDValidity, recovered.CurrentReceipt.UIDValidity)
	require.Equal(t, pending.DestinationUID, recovered.CurrentReceipt.UID)
	pending, err = fixture.store.IMAPPendingMoveContext(t.Context(), source.ID, fixture.draft.CurrentMessageID)
	require.NoError(t, err)
	require.Nil(t, pending)
}
