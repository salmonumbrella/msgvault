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
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func newFederationHub(t *testing.T) (hub, spoke *store.Store, srv *api.Server, hubUID, spokeUID string) {
	t.Helper()
	hub = testutil.NewTestStore(t)
	spoke = testutil.NewTestStore(t)
	require.NoError(t, hub.ConfigureFederationHubContext(t.Context()))
	var err error
	hubUID, err = hub.ArchiveUIDContext(t.Context())
	require.NoError(t, err)
	spokeUID, err = spoke.ArchiveUIDContext(t.Context())
	require.NoError(t, err)
	cfg := config.NewDefaultConfig()
	cfg.Data.DataDir = t.TempDir()
	cfg.Federation.Role = "hub"
	cfg.Server.APIKey = "owner-fixture"
	srv = api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: hub, FederationStore: hub, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	t.Cleanup(func() { require.NoError(t, srv.Shutdown(context.WithoutCancel(t.Context()))) })
	return
}

func TestJoinHandshakeValidation(t *testing.T) {
	invalidPolicy := config.NewDefaultConfig().Federation.Hub
	invalidPolicy.Retention = "invalid"
	invalidDigest, err := invalidPolicy.PolicyDigest()
	require.NoError(t, err)
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
		{name: "retention", mutate: func(m map[string]any) {
			m["policy"].(map[string]any)["retention"] = "invalid"
			m["policy_digest"] = invalidDigest
		}},
		{name: "digest", mutate: func(m map[string]any) { m["policy_digest"] = "wrong" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hub, spoke, srv, hubUID, spokeUID := newFederationHub(t)
			enrollment, token, err := hub.CreateFederationEnrollmentContext(t.Context(), spokeUID, "alice", "")
			require.NoError(t, err)
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
			assert.Equal(t, enrollment.Member, links[0].Member)
			assert.Equal(t, enrollment.EnrollmentUID, links[0].EnrollmentUID)
			assert.Equal(t, server.URL, links[0].HubOrigin)
			policy := config.NewDefaultConfig().Federation.Hub
			digest, err := policy.PolicyDigest()
			require.NoError(t, err)
			assert.Equal(t, digest, links[0].PolicyDigest)
			var storedPolicy config.FederationHubPolicy
			require.NoError(t, json.Unmarshal([]byte(links[0].PolicyJSON), &storedPolicy))
			assert.Equal(t, policy, storedPolicy)
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
		raw    string
		origin string
	}{
		{"https://example.com", "https://example.com"}, {"http://127.0.0.1:1234/", "http://127.0.0.1:1234"}, {"http://[::1]:1234", "http://[::1]:1234"},
		{"http://example.com", ""}, {"https://user@example.com", ""}, {"https://example.com/a", ""}, {"https://example.com/?x=1", ""}, {"https://example.com/#", ""},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			origin, err := federation.ParseHubURL(tc.raw)
			if tc.origin != "" {
				assert.NoError(t, err)
				assert.Equal(t, tc.origin, origin)
			} else {
				assert.Error(t, err)
			}
		})
	}
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

func TestLeave(t *testing.T) {
	t.Run("hub refuses revoke, then accepts", func(t *testing.T) {
		hub, spoke, srv, hubUID, spokeUID := newFederationHub(t)
		_, token, err := hub.CreateFederationEnrollmentContext(t.Context(), spokeUID, "alice", "")
		require.NoError(t, err)
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
		links, err = spoke.ListFederationLinksContext(t.Context())
		require.NoError(t, err)
		assert.Empty(t, links)
	})
	t.Run("operator revoked first", func(t *testing.T) {
		hub, spoke, srv, hubUID, spokeUID := newFederationHub(t)
		enrollment, token, err := hub.CreateFederationEnrollmentContext(t.Context(), spokeUID, "alice", "")
		require.NoError(t, err)
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
	})
	t.Run("identity changed", func(t *testing.T) {
		hub, spoke, srv, hubUID, spokeUID := newFederationHub(t)
		enrollment, token, err := hub.CreateFederationEnrollmentContext(t.Context(), spokeUID, "alice", "")
		require.NoError(t, err)
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
		path := filepath.Join(tokens, "federation-"+hubUID+".token")
		require.NoError(t, os.Remove(path))
		require.NoError(t, os.Mkdir(path, 0700))
		uid, err := federation.Leave(t.Context(), spoke, tokens, server.URL, true)
		require.NoError(t, err)
		assert.Equal(t, enrollment.EnrollmentUID, uid)
		assert.Equal(t, count, requests)
		_, err = os.Stat(path)
		assert.ErrorIs(t, err, os.ErrNotExist)
		links, err = spoke.ListFederationLinksContext(t.Context())
		require.NoError(t, err)
		assert.Empty(t, links)
	})
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
	hub, spoke, srv, hubUID, spokeUID := newFederationHub(t)
	_, first, err := hub.CreateFederationEnrollmentContext(t.Context(), spokeUID, "alice", "")
	require.NoError(t, err)
	_, second, err := hub.CreateFederationEnrollmentContext(t.Context(), spokeUID, "bob", "")
	require.NoError(t, err)
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
}

func TestJoinTokenWriteFailureRemovesLink(t *testing.T) {
	hub, spoke, srv, hubUID, spokeUID := newFederationHub(t)
	_, token, err := hub.CreateFederationEnrollmentContext(t.Context(), spokeUID, "alice", "")
	require.NoError(t, err)
	server := httptest.NewServer(srv.Router())
	t.Cleanup(server.Close)
	blocked := filepath.Join(t.TempDir(), "file")
	require.NoError(t, os.WriteFile(blocked, []byte("fixture"), 0600))
	assert.Error(t, federation.Join(t.Context(), spoke, filepath.Join(blocked, "tokens"), server.URL, hubUID, token))
	links, err := spoke.ListFederationLinksContext(t.Context())
	require.NoError(t, err)
	assert.Empty(t, links)
}

func TestStatus(t *testing.T) {
	for _, state := range []string{"enrolled", "identity_changed", "rejected", "unreachable", "non-success response", "token_missing"} {
		t.Run(state, func(t *testing.T) {
			hub, spoke, srv, hubUID, spokeUID := newFederationHub(t)
			enrollment, token, err := hub.CreateFederationEnrollmentContext(t.Context(), spokeUID, "alice", "")
			require.NoError(t, err)
			changed, nonSuccess, requests := false, false, 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				if nonSuccess {
					http.Redirect(w, r, "http://127.0.0.1:1/", http.StatusFound)
					return
				}
				if changed {
					rec := httptest.NewRecorder()
					srv.Router().ServeHTTP(rec, r)
					var body map[string]any
					require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
					body["hub_instance"] = strings.Repeat("f", 64)
					w.Header().Set("Content-Type", "application/json")
					require.NoError(t, json.NewEncoder(w).Encode(body))
					return
				}
				srv.Router().ServeHTTP(w, r)
			}))
			t.Cleanup(server.Close)
			tokens := filepath.Join(t.TempDir(), "tokens")
			require.NoError(t, federation.Join(t.Context(), spoke, tokens, server.URL, hubUID, token))
			switch state {
			case "identity_changed":
				changed = true
			case "rejected":
				require.NoError(t, hub.RevokeFederationEnrollmentContext(t.Context(), enrollment.EnrollmentUID))
			case "unreachable":
				server.Close()
			case "non-success response":
				nonSuccess = true
			case "token_missing":
				require.NoError(t, os.Remove(filepath.Join(tokens, "federation-"+hubUID+".token")))
			}
			status, err := federation.Status(t.Context(), spoke, tokens)
			require.NoError(t, err)
			require.Len(t, status, 1)
			expected := state
			if state == "non-success response" {
				expected = "unreachable"
			}
			assert.Equal(t, expected, status[0].State)
			links, err := spoke.ListFederationLinksContext(t.Context())
			require.NoError(t, err)
			require.Len(t, links, 1)
			if state == "identity_changed" {
				assert.Equal(t, "identity_changed", links[0].State)
				count := requests
				_, err = federation.Status(t.Context(), spoke, tokens)
				require.NoError(t, err)
				assert.Equal(t, count, requests)
			} else {
				assert.Equal(t, "active", links[0].State)
			}
		})
	}
}
