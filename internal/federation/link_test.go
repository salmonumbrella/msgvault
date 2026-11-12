package federation_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/federation"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestJoinHandshakeValidation(t *testing.T) {
	for _, tc := range []struct {
		name     string
		mutate   func(map[string]any)
		wrongUID bool
		success  bool
	}{
		{name: "valid", success: true},
		{name: "wrong hub", wrongUID: true},
		{name: "inactive", mutate: func(m map[string]any) { m["enrollment_state"] = "revoked" }},
		{name: "protocol", mutate: func(m map[string]any) { m["protocol_versions"] = []int{2} }},
		{name: "retention", mutate: func(m map[string]any) { m["policy"].(map[string]any)["retention"] = "invalid" }},
		{name: "levels", mutate: func(m map[string]any) { m["policy"].(map[string]any)["allowed_levels"] = []string{"invalid"} }},
		{name: "group size", mutate: func(m map[string]any) { m["policy"].(map[string]any)["min_group_size"] = 0 }},
		{name: "digest", mutate: func(m map[string]any) { m["policy_digest"] = "wrong" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hub := testutil.NewTestStore(t)
			spoke := testutil.NewTestStore(t)
			require.NoError(t, hub.ConfigureFederationHubContext(t.Context()))
			spokeUID, err := spoke.ArchiveUIDContext(t.Context())
			require.NoError(t, err)
			hubUID, err := hub.ArchiveUIDContext(t.Context())
			require.NoError(t, err)
			_, token, err := hub.CreateFederationEnrollmentContext(t.Context(), spokeUID, "alice", "")
			require.NoError(t, err)
			cfg := config.NewDefaultConfig()
			cfg.Data.DataDir = t.TempDir()
			cfg.Federation.Role = "hub"
			cfg.Server.APIKey = "owner-fixture"
			srv := api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: hub, FederationStore: hub, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
			t.Cleanup(func() { require.NoError(t, srv.Shutdown(context.WithoutCancel(t.Context()))) })
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.mutate == nil {
					srv.Router().ServeHTTP(w, r)
					return
				}
				rec := httptest.NewRecorder()
				srv.Router().ServeHTTP(rec, r)
				if rec.Code != http.StatusOK {
					w.WriteHeader(rec.Code)
					return
				}
				var body map[string]any
				require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
				tc.mutate(body)
				w.Header().Set("Content-Type", "application/json")
				require.NoError(t, json.NewEncoder(w).Encode(body))
			}))
			t.Cleanup(server.Close)
			tokens := filepath.Join(t.TempDir(), "tokens")
			expected := hubUID
			if tc.wrongUID {
				expected = strings.Repeat("a", 64)
				if expected == hubUID {
					expected = strings.Repeat("b", 64)
				}
			}
			err = federation.Join(t.Context(), spoke, tokens, server.URL, expected, token)
			links, listErr := spoke.ListFederationLinksContext(t.Context())
			require.NoError(t, listErr)
			if !tc.success {
				assert.Error(t, err)
				assert.Empty(t, links)
				_, statErr := os.Stat(tokens)
				assert.ErrorIs(t, statErr, os.ErrNotExist)
				return
			}
			require.NoError(t, err)
			require.Len(t, links, 1)
			assert.Equal(t, hubUID, links[0].HubArchiveUID)
			row, err := json.Marshal(links[0])
			require.NoError(t, err)
			assert.NotContains(t, string(row), token)
			assert.Error(t, federation.Join(t.Context(), spoke, tokens, server.URL, hubUID, token))
			info, err := os.Stat(filepath.Join(tokens, "federation-"+hubUID+".token"))
			require.NoError(t, err)
			if runtime.GOOS != "windows" {
				assert.Equal(t, os.FileMode(0600), info.Mode().Perm())
			}
		})
	}
}

func TestParseHubURL(t *testing.T) {
	for _, tc := range []struct {
		raw   string
		valid bool
	}{
		{"https://example.com", true}, {"http://127.0.0.1:1234/", true}, {"http://[::1]:1234", true},
		{"http://example.com", false}, {"https://user@example.com", false}, {"https://example.com/a", false}, {"https://example.com/?x=1", false}, {"https://example.com/#", false},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			_, err := federation.ParseHubURL(tc.raw)
			if tc.valid {
				assert.NoError(t, err)
			} else {
				assert.Error(t, err)
			}
		})
	}
}

func TestStatusIdentityChangeAndLocalLeave(t *testing.T) {
	hub := testutil.NewTestStore(t)
	spoke := testutil.NewTestStore(t)
	require.NoError(t, hub.ConfigureFederationHubContext(t.Context()))
	spokeUID, err := spoke.ArchiveUIDContext(t.Context())
	require.NoError(t, err)
	hubUID, err := hub.ArchiveUIDContext(t.Context())
	require.NoError(t, err)
	enrollment, token, err := hub.CreateFederationEnrollmentContext(t.Context(), spokeUID, "alice", "")
	require.NoError(t, err)
	cfg := config.NewDefaultConfig()
	cfg.Data.DataDir = t.TempDir()
	cfg.Federation.Role = "hub"
	cfg.Server.APIKey = "owner-fixture"
	srv := api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: hub, FederationStore: hub, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	t.Cleanup(func() { require.NoError(t, srv.Shutdown(context.WithoutCancel(t.Context()))) })
	changed, requests := false, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if !changed {
			srv.Router().ServeHTTP(w, r)
			return
		}
		rec := httptest.NewRecorder()
		srv.Router().ServeHTTP(rec, r)
		var body map[string]any
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
		body["hub_instance"] = strings.Repeat("f", 64)
		w.Header().Set("Content-Type", "application/json")
		require.NoError(t, json.NewEncoder(w).Encode(body))
	}))
	t.Cleanup(server.Close)
	tokens := filepath.Join(t.TempDir(), "tokens")
	require.NoError(t, federation.Join(t.Context(), spoke, tokens, server.URL, hubUID, token))
	status, err := federation.Status(t.Context(), spoke, tokens)
	require.NoError(t, err)
	require.Len(t, status, 1)
	assert.Equal(t, "enrolled", status[0].State)
	changed = true
	status, err = federation.Status(t.Context(), spoke, tokens)
	require.NoError(t, err)
	require.Len(t, status, 1)
	assert.Equal(t, "identity_changed", status[0].State)
	count := requests
	_, err = federation.Leave(t.Context(), spoke, tokens, server.URL, false)
	assert.ErrorContains(t, err, "--local-only")
	_, err = federation.Status(t.Context(), spoke, tokens)
	require.NoError(t, err)
	assert.Equal(t, count, requests)
	path := filepath.Join(tokens, "federation-"+hubUID+".token")
	require.NoError(t, os.Remove(path))
	require.NoError(t, os.Mkdir(path, 0700))
	uid, err := federation.Leave(t.Context(), spoke, tokens, server.URL, true)
	require.NoError(t, err)
	assert.Equal(t, enrollment.EnrollmentUID, uid)
	assert.Equal(t, count, requests)
	_, err = os.Stat(path)
	assert.ErrorIs(t, err, os.ErrNotExist)
	links, err := spoke.ListFederationLinksContext(t.Context())
	require.NoError(t, err)
	assert.Empty(t, links)
}

func TestLeaveRevokesBeforeRemovingLocalState(t *testing.T) {
	hub := testutil.NewTestStore(t)
	spoke := testutil.NewTestStore(t)
	require.NoError(t, hub.ConfigureFederationHubContext(t.Context()))
	spokeUID, err := spoke.ArchiveUIDContext(t.Context())
	require.NoError(t, err)
	hubUID, err := hub.ArchiveUIDContext(t.Context())
	require.NoError(t, err)
	enrollment, token, err := hub.CreateFederationEnrollmentContext(t.Context(), spokeUID, "alice", "")
	require.NoError(t, err)
	cfg := config.NewDefaultConfig()
	cfg.Data.DataDir = t.TempDir()
	cfg.Federation.Role = "hub"
	cfg.Server.APIKey = "owner-fixture"
	srv := api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: hub, FederationStore: hub, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	t.Cleanup(func() { require.NoError(t, srv.Shutdown(context.WithoutCancel(t.Context()))) })
	refuse := true
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete && refuse {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		srv.Router().ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	tokens := filepath.Join(t.TempDir(), "tokens")
	require.NoError(t, federation.Join(t.Context(), spoke, tokens, server.URL, hubUID, token))
	_, err = federation.Leave(t.Context(), spoke, tokens, server.URL, false)
	assert.ErrorContains(t, err, "--local-only")
	links, err := spoke.ListFederationLinksContext(t.Context())
	require.NoError(t, err)
	require.Len(t, links, 1)
	_, err = os.Stat(filepath.Join(tokens, "federation-"+hubUID+".token"))
	assert.NoError(t, err)
	refuse = false
	_, err = federation.Leave(t.Context(), spoke, tokens, server.URL, false)
	require.NoError(t, err)
	_, err = hub.AuthenticateFederationEnrollmentContext(t.Context(), token, spokeUID)
	assert.Error(t, err)
	entries, err := hub.ListFederationEnrollmentsContext(t.Context())
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Equal(t, enrollment.EnrollmentUID, entries[0].EnrollmentUID)
	assert.NotNil(t, entries[0].RevokedAt)
	links, err = spoke.ListFederationLinksContext(t.Context())
	require.NoError(t, err)
	assert.Empty(t, links)
}

func TestLeaveAfterOperatorRevoke(t *testing.T) {
	hub := testutil.NewTestStore(t)
	spoke := testutil.NewTestStore(t)
	require.NoError(t, hub.ConfigureFederationHubContext(t.Context()))
	spokeUID, err := spoke.ArchiveUIDContext(t.Context())
	require.NoError(t, err)
	hubUID, err := hub.ArchiveUIDContext(t.Context())
	require.NoError(t, err)
	enrollment, token, err := hub.CreateFederationEnrollmentContext(t.Context(), spokeUID, "alice", "")
	require.NoError(t, err)
	cfg := config.NewDefaultConfig()
	cfg.Data.DataDir = t.TempDir()
	cfg.Federation.Role = "hub"
	cfg.Server.APIKey = "owner-fixture"
	srv := api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: hub, FederationStore: hub, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	t.Cleanup(func() { require.NoError(t, srv.Shutdown(context.WithoutCancel(t.Context()))) })
	server := httptest.NewServer(srv.Router())
	t.Cleanup(server.Close)
	tokens := filepath.Join(t.TempDir(), "tokens")
	require.NoError(t, federation.Join(t.Context(), spoke, tokens, server.URL, hubUID, token))
	require.NoError(t, hub.RevokeFederationEnrollmentContext(t.Context(), enrollment.EnrollmentUID))

	_, err = federation.Leave(t.Context(), spoke, tokens, server.URL, false)
	assert.ErrorContains(t, err, "hub rejected enrollment")
	assert.ErrorContains(t, err, "--local-only")
	links, err := spoke.ListFederationLinksContext(t.Context())
	require.NoError(t, err)
	require.Len(t, links, 1)
	assert.Equal(t, "active", links[0].State)
	path := filepath.Join(tokens, "federation-"+hubUID+".token")
	_, err = os.Stat(path)
	assert.NoError(t, err)

	uid, err := federation.Leave(t.Context(), spoke, tokens, server.URL, true)
	require.NoError(t, err)
	assert.Equal(t, enrollment.EnrollmentUID, uid)
	links, err = spoke.ListFederationLinksContext(t.Context())
	require.NoError(t, err)
	assert.Empty(t, links)
	_, err = os.Stat(path)
	assert.ErrorIs(t, err, os.ErrNotExist)
}

func TestJoinRefusesRedirect(t *testing.T) {
	contacted := false
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { contacted = true }))
	t.Cleanup(target.Close)
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, http.StatusFound) }))
	t.Cleanup(redirect.Close)
	spoke := testutil.NewTestStore(t)
	err := federation.Join(t.Context(), spoke, filepath.Join(t.TempDir(), "tokens"), redirect.URL, strings.Repeat("a", 64), "mvf1_fixture")
	assert.Error(t, err)
	assert.False(t, contacted)
}

func TestJoinRejectsReflectedToken(t *testing.T) {
	hub := testutil.NewTestStore(t)
	spoke := testutil.NewTestStore(t)
	require.NoError(t, hub.ConfigureFederationHubContext(t.Context()))
	spokeUID, err := spoke.ArchiveUIDContext(t.Context())
	require.NoError(t, err)
	hubUID, err := hub.ArchiveUIDContext(t.Context())
	require.NoError(t, err)
	_, token, err := hub.CreateFederationEnrollmentContext(t.Context(), spokeUID, "alice", "")
	require.NoError(t, err)
	cfg := config.NewDefaultConfig()
	cfg.Data.DataDir = t.TempDir()
	cfg.Federation.Role = "hub"
	cfg.Server.APIKey = "owner-fixture"
	srv := api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: hub, FederationStore: hub, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	t.Cleanup(func() { require.NoError(t, srv.Shutdown(context.WithoutCancel(t.Context()))) })
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := httptest.NewRecorder()
		srv.Router().ServeHTTP(rec, r)
		var body map[string]any
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
		body["member"] = token
		w.Header().Set("Content-Type", "application/json")
		require.NoError(t, json.NewEncoder(w).Encode(body))
	}))
	t.Cleanup(server.Close)
	tokens := filepath.Join(t.TempDir(), "tokens")
	err = federation.Join(t.Context(), spoke, tokens, server.URL, hubUID, token)
	require.Error(t, err)
	assert.NotContains(t, err.Error(), token)
	links, err := spoke.ListFederationLinksContext(t.Context())
	require.NoError(t, err)
	assert.Empty(t, links)
	_, err = os.Stat(tokens)
	assert.ErrorIs(t, err, os.ErrNotExist)
}

func TestLeaveDetectsChangedIdentityBeforeRevoke(t *testing.T) {
	hub := testutil.NewTestStore(t)
	spoke := testutil.NewTestStore(t)
	require.NoError(t, hub.ConfigureFederationHubContext(t.Context()))
	spokeUID, err := spoke.ArchiveUIDContext(t.Context())
	require.NoError(t, err)
	hubUID, err := hub.ArchiveUIDContext(t.Context())
	require.NoError(t, err)
	_, token, err := hub.CreateFederationEnrollmentContext(t.Context(), spokeUID, "alice", "")
	require.NoError(t, err)
	cfg := config.NewDefaultConfig()
	cfg.Data.DataDir = t.TempDir()
	cfg.Federation.Role = "hub"
	cfg.Server.APIKey = "owner-fixture"
	srv := api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: hub, FederationStore: hub, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	t.Cleanup(func() { require.NoError(t, srv.Shutdown(context.WithoutCancel(t.Context()))) })
	changed, deletes, requests := false, 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Method == http.MethodDelete {
			deletes++
		}
		if !changed {
			srv.Router().ServeHTTP(w, r)
			return
		}
		rec := httptest.NewRecorder()
		srv.Router().ServeHTTP(rec, r)
		var body map[string]any
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
		body["hub_instance"] = strings.Repeat("e", 64)
		w.Header().Set("Content-Type", "application/json")
		require.NoError(t, json.NewEncoder(w).Encode(body))
	}))
	t.Cleanup(server.Close)
	tokens := filepath.Join(t.TempDir(), "tokens")
	require.NoError(t, federation.Join(t.Context(), spoke, tokens, server.URL, hubUID, token))
	changed = true
	_, err = federation.Leave(t.Context(), spoke, tokens, server.URL, false)
	assert.ErrorContains(t, err, "--local-only")
	assert.Zero(t, deletes)
	count := requests
	_, err = federation.Leave(t.Context(), spoke, tokens, server.URL, false)
	assert.ErrorContains(t, err, "--local-only")
	assert.Equal(t, count, requests)
	links, err := spoke.ListFederationLinksContext(t.Context())
	require.NoError(t, err)
	require.Len(t, links, 1)
	assert.Equal(t, "identity_changed", links[0].State)
}

func TestJoinRejectsInvalidHubUIDBeforeContact(t *testing.T) {
	called := false
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	t.Cleanup(server.Close)
	spoke := testutil.NewTestStore(t)
	tokens := filepath.Join(t.TempDir(), "tokens")
	err := federation.Join(t.Context(), spoke, tokens, server.URL, "../../bad", "mvf1_fixture")
	assert.Error(t, err)
	assert.False(t, called)
	_, err = os.Stat(tokens)
	assert.ErrorIs(t, err, os.ErrNotExist)
}

func TestConcurrentJoinsKeepWinningToken(t *testing.T) {
	hub := testutil.NewTestStore(t)
	spoke := testutil.NewTestStore(t)
	require.NoError(t, hub.ConfigureFederationHubContext(t.Context()))
	spokeUID, err := spoke.ArchiveUIDContext(t.Context())
	require.NoError(t, err)
	hubUID, err := hub.ArchiveUIDContext(t.Context())
	require.NoError(t, err)
	_, first, err := hub.CreateFederationEnrollmentContext(t.Context(), spokeUID, "alice", "")
	require.NoError(t, err)
	_, second, err := hub.CreateFederationEnrollmentContext(t.Context(), spokeUID, "bob", "")
	require.NoError(t, err)
	cfg := config.NewDefaultConfig()
	cfg.Data.DataDir = t.TempDir()
	cfg.Federation.Role = "hub"
	cfg.Server.APIKey = "owner-fixture"
	srv := api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: hub, FederationStore: hub, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	t.Cleanup(func() { require.NoError(t, srv.Shutdown(context.WithoutCancel(t.Context()))) })
	var arrivals atomic.Int32
	both := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if arrivals.Add(1) == 2 {
			close(both)
		}
		<-both
		srv.Router().ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	tokensDir := filepath.Join(t.TempDir(), "tokens")
	type result struct {
		token string
		err   error
	}
	results := make(chan result, 2)
	for _, token := range []string{first, second} {
		go func() {
			results <- result{token, federation.Join(t.Context(), spoke, tokensDir, server.URL, hubUID, token)}
		}()
	}
	a, b := <-results, <-results
	assert.NotEqual(t, a.err == nil, b.err == nil)
	winner := a.token
	if a.err != nil {
		winner = b.token
	}
	links, err := spoke.ListFederationLinksContext(t.Context())
	require.NoError(t, err)
	assert.Len(t, links, 1)
	path := filepath.Join(tokensDir, "federation-"+hubUID+".token")
	contents, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, winner, string(contents))
	info, err := os.Stat(path)
	require.NoError(t, err)
	if runtime.GOOS != "windows" {
		assert.Equal(t, os.FileMode(0600), info.Mode().Perm())
	}
}

func TestJoinTokenWriteFailureRemovesLink(t *testing.T) {
	hub := testutil.NewTestStore(t)
	spoke := testutil.NewTestStore(t)
	require.NoError(t, hub.ConfigureFederationHubContext(t.Context()))
	spokeUID, err := spoke.ArchiveUIDContext(t.Context())
	require.NoError(t, err)
	hubUID, err := hub.ArchiveUIDContext(t.Context())
	require.NoError(t, err)
	_, token, err := hub.CreateFederationEnrollmentContext(t.Context(), spokeUID, "alice", "")
	require.NoError(t, err)
	cfg := config.NewDefaultConfig()
	cfg.Data.DataDir = t.TempDir()
	cfg.Federation.Role = "hub"
	cfg.Server.APIKey = "owner-fixture"
	srv := api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: hub, FederationStore: hub, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	t.Cleanup(func() { require.NoError(t, srv.Shutdown(context.WithoutCancel(t.Context()))) })
	server := httptest.NewServer(srv.Router())
	t.Cleanup(server.Close)
	blocked := filepath.Join(t.TempDir(), "file")
	require.NoError(t, os.WriteFile(blocked, []byte("fixture"), 0600))
	assert.Error(t, federation.Join(t.Context(), spoke, filepath.Join(blocked, "tokens"), server.URL, hubUID, token))
	links, err := spoke.ListFederationLinksContext(t.Context())
	require.NoError(t, err)
	assert.Empty(t, links)
}

func TestStatusReportsLinkFailure(t *testing.T) {
	for _, state := range []string{"rejected", "unreachable", "redirect", "token_missing"} {
		t.Run(state, func(t *testing.T) {
			hub := testutil.NewTestStore(t)
			spoke := testutil.NewTestStore(t)
			require.NoError(t, hub.ConfigureFederationHubContext(t.Context()))
			spokeUID, err := spoke.ArchiveUIDContext(t.Context())
			require.NoError(t, err)
			hubUID, err := hub.ArchiveUIDContext(t.Context())
			require.NoError(t, err)
			enrollment, token, err := hub.CreateFederationEnrollmentContext(t.Context(), spokeUID, "alice", "")
			require.NoError(t, err)
			cfg := config.NewDefaultConfig()
			cfg.Data.DataDir = t.TempDir()
			cfg.Federation.Role = "hub"
			cfg.Server.APIKey = "owner-fixture"
			srv := api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: hub, FederationStore: hub, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
			t.Cleanup(func() { require.NoError(t, srv.Shutdown(context.WithoutCancel(t.Context()))) })
			redirect := false
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if redirect {
					http.Redirect(w, r, "http://127.0.0.1:1/", http.StatusFound)
					return
				}
				srv.Router().ServeHTTP(w, r)
			}))
			t.Cleanup(server.Close)
			tokens := filepath.Join(t.TempDir(), "tokens")
			require.NoError(t, federation.Join(t.Context(), spoke, tokens, server.URL, hubUID, token))
			switch state {
			case "rejected":
				require.NoError(t, hub.RevokeFederationEnrollmentContext(t.Context(), enrollment.EnrollmentUID))
			case "unreachable":
				server.Close()
			case "redirect":
				redirect = true
			case "token_missing":
				require.NoError(t, os.Remove(filepath.Join(tokens, "federation-"+hubUID+".token")))
			}
			status, err := federation.Status(t.Context(), spoke, tokens)
			require.NoError(t, err)
			require.Len(t, status, 1)
			expected := state
			if state == "redirect" {
				expected = "unreachable"
			}
			assert.Equal(t, expected, status[0].State)
			links, err := spoke.ListFederationLinksContext(t.Context())
			require.NoError(t, err)
			require.Len(t, links, 1)
			assert.Equal(t, "active", links[0].State)
		})
	}
}
