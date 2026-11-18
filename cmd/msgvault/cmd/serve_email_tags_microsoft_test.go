package cmd

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/emailtags"
	"go.kenn.io/msgvault/internal/microsoft"
	"go.kenn.io/msgvault/internal/msmail"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestDaemonMessageTagsMicrosoftSharedPath(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(sourceTypeMSMail, "owner@example.test")
	require.NoError(err)
	conv, err := st.EnsureConversation(t.Context(), source.ID, "thread-1", "Tags")
	require.NoError(err)
	mid, err := st.UpsertMessage(&store.Message{SourceID: source.ID, ConversationID: conv, SourceMessageID: "immutable-1", MessageType: "email"})
	require.NoError(err)
	folder, err := st.EnsureLabel(t.Context(), source.ID, "folder-1", "Inbox", "system")
	require.NoError(err)
	_, err = st.ReconcileMessageLabels(t.Context(), mid, []int64{folder}, true)
	require.NoError(err)
	cfg := lifecycleTestConfig(t.TempDir())
	tokenPath := microsoft.NewGraphMailManager("", "", "", cfg.TokensDir(), nil).TokenPath(source.Identifier)
	require.NoError(os.MkdirAll(filepath.Dir(tokenPath), 0o700))
	saveScopes := func(scopes []string) {
		data, encodeErr := json.Marshal(map[string]any{"access_token": "synthetic-token", "token_type": "Bearer", "scopes": scopes})
		require.NoError(encodeErr)
		require.NoError(os.WriteFile(tokenPath, data, 0o600))
	}
	writes := new(atomic.Int32)
	tags := []string{"Other"}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal("/me/messages/immutable-1", r.URL.Path)
		if r.Method == http.MethodPatch {
			writes.Add(1)
			var body struct {
				Categories []string `json:"categories"`
			}
			if decodeErr := json.UnmarshalRead(r.Body, &body); decodeErr != nil {
				assert.Fail("decode Graph categories", "%v", decodeErr)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			tags = body.Categories
		}
		assert.NoError(json.MarshalWrite(w, map[string]any{"id": "immutable-1", "categories": tags, "@odata.etag": `W/"v1"`}))
	}))
	t.Cleanup(srv.Close)
	a := &storeAPIAdapter{store: st, config: cfg, logger: slog.New(slog.DiscardHandler), microsoftTagClientFactory: func(context.Context, *store.Source, bool) (*msmail.Client, error) {
		return msmail.NewClient(srv.URL, func(context.Context) (string, error) { return "synthetic-token", nil }, 1000), nil
	}}
	saveScopes(microsoft.GraphMailScopes())
	_, err = a.MessageTags(t.Context(), mid, &emailtags.MessageTagChange{Add: []string{"Next"}, DryRun: true}, "", nil)
	require.NoError(err, "previews need only read scopes")
	assert.Zero(writes.Load())
	_, err = a.MessageTags(t.Context(), mid, &emailtags.MessageTagChange{Add: []string{"Next"}}, "", nil)
	var failure *emailtags.MessageTagError
	require.ErrorAs(err, &failure)
	assert.Equal("insufficient_scope", failure.Code)
	assert.Zero(writes.Load())
	saveScopes(microsoft.GraphMailWriteScopes())
	got, err := a.MessageTags(t.Context(), mid, &emailtags.MessageTagChange{Add: []string{"Next"}}, "", nil)
	require.NoError(err)
	assert.True(got.Verified)
	assert.Equal(mid, got.MessageID)
	msg, err := st.GetMessage(mid)
	require.NoError(err)
	assert.ElementsMatch([]string{"Inbox", "Category: Other", "Category: Next"}, msg.Labels)
	assert.Equal(int32(1), writes.Load())
	require.NoError(st.UpdateSourceOAuthApp(t.Context(), source.ID, sql.NullString{String: "missing", Valid: true}))
	_, err = a.MessageTags(t.Context(), mid, &emailtags.MessageTagChange{Add: []string{"Another"}}, "", nil)
	require.ErrorAs(err, &failure)
	assert.Equal("provider_unavailable", failure.Code)
	assert.Equal(int32(1), writes.Load())
}

func TestDaemonMessageTagsSourceDeletedRead(t *testing.T) {
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(sourceTypeMSMail, "owner@example.test")
	require.NoError(t, err)
	conv, err := st.EnsureConversation(t.Context(), source.ID, "thread-1", "Tags")
	require.NoError(t, err)
	mid, err := st.UpsertMessage(&store.Message{SourceID: source.ID, ConversationID: conv, SourceMessageID: "immutable-1", MessageType: "email"})
	require.NoError(t, err)
	requests := new(atomic.Int32)
	status := new(atomic.Int32)
	status.Store(http.StatusOK)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		assert.Equal(t, http.MethodGet, r.Method)
		assert.Equal(t, "/me/messages/immutable-1", r.URL.Path)
		w.WriteHeader(int(status.Load()))
		if status.Load() == http.StatusOK {
			assert.NoError(t, json.MarshalWrite(w, map[string]any{"id": "immutable-1", "categories": []string{"Remote"}}))
		}
	}))
	t.Cleanup(srv.Close)
	refreshes := 0
	a := &storeAPIAdapter{
		store: st, config: lifecycleTestConfig(t.TempDir()), logger: slog.New(slog.DiscardHandler),
		microsoftTagClientFactory: func(_ context.Context, _ *store.Source, write bool) (*msmail.Client, error) {
			assert.False(t, write)
			return msmail.NewClient(srv.URL, func(context.Context) (string, error) { return "synthetic-token", nil }, 1000), nil
		},
		draftCacheRefresh: func(context.Context, string) error { refreshes++; return nil },
	}
	acquisitions := 0
	acquire := func(context.Context) (func(), error) { acquisitions++; return func() {}, nil }
	control, err := a.MessageTags(t.Context(), mid, nil, "", acquire)
	require.NoError(t, err)
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE messages SET deleted_from_source_at=CURRENT_TIMESTAMP WHERE id=?`), mid)
	require.NoError(t, err)
	before, err := st.GetMessage(mid)
	require.NoError(t, err)
	require.NotNil(t, before.DeletedAt)
	got, err := a.MessageTags(t.Context(), mid, nil, "", acquire)
	require.NoError(t, err)
	assert.Equal(t, control, got, "the local source marker must not change the provider observation")
	assert.Equal(t, []string{"Remote"}, got.Tags)
	assert.Equal(t, mid, got.MessageID)
	assert.Equal(t, int32(2), requests.Load())
	for _, change := range []*emailtags.MessageTagChange{
		{},
		{Add: []string{"Next"}},
		{Add: []string{"Next"}, DryRun: true},
	} {
		_, err = a.MessageTags(t.Context(), mid, change, "", acquire)
		require.ErrorIs(t, err, store.ErrMessageNotFound)
	}
	assert.Equal(t, int32(2), requests.Load(), "edits and previews must fail before contacting the provider")
	assert.Zero(t, acquisitions)
	assert.Zero(t, refreshes)

	status.Store(http.StatusNotFound)
	got, err = a.MessageTags(t.Context(), mid, nil, "", acquire)
	var failure *emailtags.MessageTagError
	require.ErrorAs(t, err, &failure)
	assert.Equal(t, "stale_identity", failure.Code)
	assert.Nil(t, got, "provider absence must not become an empty successful result")
	after, err := st.GetMessage(mid)
	require.NoError(t, err)
	assert.Equal(t, before, after, "reads must preserve archived state")
}

func TestDaemonMicrosoftArchiveConsentAndPersistence(t *testing.T) {
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(sourceTypeMSMail, "owner@example.test")
	require.NoError(t, err)
	conv, err := st.EnsureConversation(t.Context(), source.ID, "thread-1", "Archive")
	require.NoError(t, err)
	sender, err := st.EnsureParticipant("allowed@example.test", "Allowed", "example.test")
	require.NoError(t, err)
	mid, err := st.PersistMessageContext(t.Context(), &store.MessagePersistData{Message: &store.Message{SourceID: source.ID, ConversationID: conv, SourceMessageID: "immutable-1", MessageType: "email", SenderID: sql.NullInt64{Int64: sender, Valid: true}}, Recipients: []store.RecipientSet{{Type: "from", ParticipantIDs: []int64{sender}, EmailAddresses: []string{"allowed@example.test"}, DisplayNames: []string{"Allowed"}}}})
	require.NoError(t, err)
	cfg := lifecycleTestConfig(t.TempDir())
	tokenPath := microsoft.NewGraphMailManager("", "", "", cfg.TokensDir(), nil).TokenPath(source.Identifier)
	require.NoError(t, os.MkdirAll(filepath.Dir(tokenPath), 0700))
	saveScopes := func(scopes []string) {
		data, e := json.Marshal(map[string]any{"access_token": "synthetic", "token_type": "Bearer", "scopes": scopes})
		require.NoError(t, e)
		require.NoError(t, os.WriteFile(tokenPath, data, 0600))
	}
	var calls atomic.Int32
	var posts atomic.Int32
	parent := "inbox-id"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		switch r.URL.Path {
		case "/me/mailFolders/archive":
			_ = json.MarshalWrite(w, map[string]string{"id": "archive-id"})
		case "/me/mailFolders/inbox":
			_ = json.MarshalWrite(w, map[string]string{"id": "inbox-id"})
		case "/me/mailFolders/deleteditems":
			_ = json.MarshalWrite(w, map[string]string{"id": "deleted-id"})
		case "/me/mailFolders":
			_ = json.MarshalWrite(w, map[string]any{"value": []map[string]any{{"id": "inbox-id", "displayName": "Inbox"}, {"id": "archive-id", "displayName": "Archiv"}, {"id": "deleted-id", "displayName": "Trash", "childFolderCount": 1}}})
		case "/me/mailFolders/deleted-id/childFolders":
			_ = json.MarshalWrite(w, map[string]any{"value": []map[string]any{{"id": "child-id", "displayName": "Child", "childFolderCount": 1}}})
		case "/me/mailFolders/child-id/childFolders":
			_ = json.MarshalWrite(w, map[string]any{"value": []map[string]any{{"id": "grandchild-id", "displayName": "Grandchild"}}})
		case "/me/messages/immutable-1/move":
			posts.Add(1)
			var body map[string]string
			if !assert.NoError(t, json.UnmarshalRead(r.Body, &body)) {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			parent = body["destinationId"]
			_ = json.MarshalWrite(w, map[string]string{"id": "immutable-1"})
		case "/me/messages/immutable-1":
			_ = json.MarshalWrite(w, map[string]any{"id": "immutable-1", "parentFolderId": parent, "categories": []string{"Work"}})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	refreshes := 0
	a := &storeAPIAdapter{store: st, config: cfg, logger: slog.New(slog.DiscardHandler), microsoftTagClientFactory: func(context.Context, *store.Source, bool) (*msmail.Client, error) {
		return msmail.NewClient(srv.URL, func(context.Context) (string, error) { return "synthetic", nil }, 1000), nil
	}, draftCacheRefresh: func(context.Context, string) error { refreshes++; return nil }}
	desired := true
	saveScopes(microsoft.GraphMailScopes())
	result, err := a.ArchiveMessage(t.Context(), mid, emailtags.ArchiveChange{Archived: &desired, DryRun: true}, nil, nil)
	require.NoError(t, err)
	assert.True(t, result.Changed)
	count := calls.Load()
	_, err = a.ArchiveMessage(t.Context(), mid, emailtags.ArchiveChange{Archived: &desired}, nil, nil)
	var failure *emailtags.MessageTagError
	require.ErrorAs(t, err, &failure)
	assert.Equal(t, "insufficient_scope", failure.Code)
	assert.Equal(t, count, calls.Load())
	saveScopes(microsoft.GraphMailWriteScopes())
	result, err = a.ArchiveMessage(t.Context(), mid, emailtags.ArchiveChange{Archived: &desired}, nil, nil)
	require.NoError(t, err)
	assert.True(t, result.Changed)
	assert.True(t, result.Verified)
	msg, err := st.GetMessage(mid)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"Archiv", "Category: Work"}, msg.Labels)
	assert.Equal(t, 1, refreshes)
	result, err = a.ArchiveMessage(t.Context(), mid, emailtags.ArchiveChange{Archived: &desired}, nil, nil)
	require.NoError(t, err)
	assert.False(t, result.Changed)
	assert.True(t, result.Verified)
	// Exercise a sender-restricted grant through the real HTTP route and client.
	cfg.Server.APIKey, cfg.Server.AgentAccess = "owner", true
	routes := api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: a, Engine: query.NewEngine(st.DB(), st.IsPostgreSQL()), Logger: slog.New(slog.DiscardHandler)})
	t.Cleanup(func() { assert.NoError(t, routes.Shutdown(context.WithoutCancel(t.Context()))) })
	httpServer := httptest.NewServer(routes.Router())
	t.Cleanup(httpServer.Close)
	owner, err := daemonclient.New(daemonclient.Config{URL: httpServer.URL, APIKey: "owner", AllowInsecure: true})
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, owner.Close()) })
	token, err := owner.IssueAgentToken(t.Context(), "allowed sender", []string{"message.archive"}, []int64{source.ID}, nil, map[int64][]string{source.ID: {"allowed@example.test"}}, time.Time{})
	require.NoError(t, err)
	client, err := daemonclient.New(daemonclient.Config{URL: httpServer.URL, AgentToken: token.Secret, AllowInsecure: true})
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, client.Close()) })
	parent = "inbox-id"
	for i, archived := range []bool{true, true, false} {
		got, e := client.ArchiveMessage(t.Context(), mid, emailtags.ArchiveChange{Archived: &archived})
		require.NoError(t, e)
		assert.Equal(t, i != 1, got.Changed)
		assert.True(t, got.Verified)
		assert.Equal(t, archived, got.Archived)
		msg, err = st.GetMessage(mid)
		require.NoError(t, err)
		label := "Inbox"
		if archived {
			label = "Archiv"
		}
		assert.ElementsMatch(t, []string{label, "Category: Work"}, msg.Labels)
	}
	// A locally active Inbox row can be stale after Outlook moves the message to trash.
	parent = "grandchild-id"
	beforePosts := posts.Load()
	for _, archived := range []bool{true, false} {
		for _, preview := range []bool{true, false} {
			_, e := client.ArchiveMessage(t.Context(), mid, emailtags.ArchiveChange{Archived: &archived, DryRun: preview})
			require.ErrorAs(t, e, &failure)
			assert.Equal(t, "stale_identity", failure.Code)
		}
	}
	assert.Equal(t, beforePosts, posts.Load())
	msg, err = st.GetMessage(mid)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"Inbox", "Category: Work"}, msg.Labels)
	parent = "inbox-id"
	_, err = st.DB().ExecContext(t.Context(), `CREATE TRIGGER fail_archive_save BEFORE INSERT ON message_labels BEGIN SELECT RAISE(FAIL, 'synthetic save failure'); END`)
	require.NoError(t, err)
	beforePosts = posts.Load()
	failed, err := owner.ArchiveMessage(t.Context(), mid, emailtags.ArchiveChange{Archived: &desired})
	require.ErrorAs(t, err, &failure)
	assert.Equal(t, "remote_accepted_local_failed", failure.Code)
	_ = failed
	if assert.NotNil(t, failure.Result) {
		assert.True(t, failure.Result.Verified)
	}
	assert.Equal(t, beforePosts+1, posts.Load())
	_, err = st.DB().ExecContext(t.Context(), `DROP TRIGGER fail_archive_save`)
	require.NoError(t, err)
	repaired, err := client.ArchiveMessage(t.Context(), mid, emailtags.ArchiveChange{Archived: &desired})
	require.NoError(t, err)
	assert.True(t, repaired.Verified)
	assert.False(t, repaired.Changed)
	assert.Equal(t, beforePosts+1, posts.Load())
	msg, err = st.GetMessage(mid)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"Archiv", "Category: Work"}, msg.Labels)
}
