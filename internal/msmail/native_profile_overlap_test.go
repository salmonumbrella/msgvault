package msmail

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestNativeMicrosoftDelayedProfileDenialCannotRevokeNewerVerification(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	enableNativeMailEvents(t, st)
	graph := newFakeGraph(t)
	oldProfileStarted := make(chan struct{})
	releaseOldProfile := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseOldProfile) }) }
	t.Cleanup(release)
	graph.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/me" {
			if r.Header.Get("Authorization") == "Bearer old-token" {
				close(oldProfileStarted)
				select {
				case <-releaseOldProfile:
				case <-r.Context().Done():
					return
				}
				w.WriteHeader(http.StatusForbidden)
				return
			}
			graph.writeJSON(w, map[string]any{"id": "synthetic-account", "mail": "me@example.com", "userPrincipalName": "me@example.com"})
			return
		}
		graph.serve(w, r)
	})
	client := func(token string) *Client {
		return NewClient(graph.srv.URL, func(context.Context) (string, error) { return token, nil }, 1000,
			WithGrantedScopesReader(func() ([]string, error) { return []string{"Mail.Read"}, nil }))
	}
	opts := Options{Email: "me@example.com"}
	oldDone := make(chan error, 1)
	go func() {
		_, err := Import(t.Context(), st, client("old-token"), opts, slog.Default())
		oldDone <- err
	}()
	select {
	case <-oldProfileStarted:
	case <-t.Context().Done():
		require.FailNow("older profile request did not start")
	}

	_, newerErr := Import(t.Context(), st, client("new-token"), opts, slog.Default())
	if errors.Is(newerErr, store.ErrSyncAlreadyActive) {
		release()
		require.Error(<-oldDone)
		_, newerErr = Import(t.Context(), st, client("new-token"), opts, slog.Default())
	} else {
		release()
		require.Error(<-oldDone)
	}
	require.NoError(newerErr)
	source, err := st.GetSourceByTypeAndIdentifier(SourceType, opts.Email)
	require.NoError(err)
	require.NotNil(source)
	allowed, err := st.NativeSourceMessagePermission(t.Context(), source.ID)
	require.NoError(err)
	assert.True(allowed, "older profile denial must not retire newer verified authority")
}
