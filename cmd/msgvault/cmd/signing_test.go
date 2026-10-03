package cmd

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/fileutil"
	"go.kenn.io/msgvault/internal/requestsign"
)

func TestSigningInitStateNoArchive(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	dir := t.TempDir()
	requirements.NoError(fileutil.SecureChmod(dir, 0o700))
	home := filepath.Join(dir, "unused-home")
	path := filepath.Join(dir, "replay.json")
	err := executeSigningTestRoot(t, "--home", home, "signing", "init-state", "--file", path)
	requirements.NoError(err)
	_, err = os.Stat(home)
	assertions.True(os.IsNotExist(err))
	guard, err := requestsign.OpenReplayGuard(path, time.Now(), 10)
	requirements.NoError(err)
	requirements.NoError(guard.Close())
	requirements.Error(executeSigningTestRoot(t, "signing", "init-state", "--file", path))
}

func TestSignedHealthPollingDoesNotFallBackUnsigned(t *testing.T) {
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		_, _ = io.WriteString(w, `{"operation":{"label":"fixture maintenance"}}`)
	}))
	defer server.Close()
	cfg := &config.Config{Remote: config.RemoteConfig{URL: server.URL, APIKey: "fixture-client-key", SigningKeyID: "reader-1"}}
	fetch := configuredDaemonOperationFetcher(HTTPStoreInfo{Kind: HTTPStoreConfiguredRemote, URL: server.URL}, cfg)
	assert.Nil(t, fetch(t.Context()), "invalid signing configuration must fail closed")
	assert.Zero(t, requests.Load(), "auxiliary polling must not send unsigned credentials")
}

func TestSignedTokenExportFailsBeforeProviderOrNetworkAccess(t *testing.T) {
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { requests.Add(1); w.WriteHeader(http.StatusCreated) }))
	defer server.Close()
	home := filepath.Join(t.TempDir(), "unused-home")
	cfg := &config.Config{HomeDir: home, Remote: config.RemoteConfig{URL: server.URL, APIKey: "fixture-client-key", SigningKeyID: "reader-1"}}
	command := &cobra.Command{}
	command.SetContext(withInvocation(context.Background(), &invocation{cfg: cfg}))
	require.ErrorContains(t, runExportToken(command, []string{"source@example.test"}), "unavailable with signed remote ingress")
	assert.Zero(t, requests.Load())
	_, err := os.Stat(home)
	assert.True(t, os.IsNotExist(err), "blocked provider setup must not create client state")
}

func executeSigningTestRoot(t *testing.T, args ...string) error {
	t.Helper()
	root := newRootCommand()
	root.AddCommand(newSigningCommand())
	root.SetArgs(args)
	if err := root.ExecuteContext(t.Context()); err != nil {
		return fmt.Errorf("execute signing fixture: %w", err)
	}
	return nil
}
