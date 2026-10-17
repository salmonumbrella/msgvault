package cmd

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/daemon"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestScopedConfigExportAndServe(t *testing.T) { //nolint:paralleltest // root startup changes the process logger
	for _, tc := range []struct{ name, content, env, section string }{
		{name: "kata", content: "[integrations.kata]\nendpoint = '://invalid'\n", section: "integrations.kata"},
		{name: "docbank", content: "[integrations.docbank]\nattachment_folder = '../escape'\n", section: "integrations.docbank"},
		{name: "server environment", env: "invalid", section: "server"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			home := t.TempDir()
			require.NoError(os.WriteFile(filepath.Join(home, "config.toml"), []byte(tc.content), 0600))
			if tc.env != "" {
				t.Setenv("MSGVAULT_API_PORT", tc.env)
			}
			st := testutil.NewTestStore(t)
			root := newRootCommand()
			root.AddCommand(newExportMessagesLocalCmd(exportMessagesDeps{openStore: func(context.Context) (*store.Store, func(), error) { return st, func() {}, nil }}))
			var out, stderr bytes.Buffer
			root.SetOut(&out)
			root.SetErr(&stderr)
			root.SetArgs([]string{"--home", home, "export-messages", "--start", "2026-10-01T00:00:00Z", "--end", "2026-10-02T00:00:00Z"})
			require.NoError(root.Execute())
			assert.Contains(out.String(), `"record_type":"complete"`)
			assert.Contains(stderr.String(), "Warning:")
			assert.Contains(stderr.String(), tc.section)
			if inv := invocationFromCommand(root); inv != nil && inv.logResult != nil {
				inv.logResult.Close()
			}

			server := newRootCommand()
			// Root startup must reject the section before the server can listen.
			server.AddCommand(&cobra.Command{Use: "serve", RunE: runServe})
			server.SetErr(&stderr)
			server.SetArgs([]string{"--home", home, "serve"})
			err := server.Execute()
			require.Error(err)
			if tc.env != "" {
				assert.Contains(err.Error(), "MSGVAULT_API_PORT")
			} else {
				assert.Contains(err.Error(), tc.section)
			}
		})
	}
}

func TestScopedConfigParseErrorsRemainGlobal(t *testing.T) { //nolint:paralleltest // root startup changes the process logger
	for _, content := range []string{"[server\n", "[chatwoot]\nunknown = true\n"} {
		for _, name := range []string{"export-messages", "serve"} {
			t.Run(name+content, func(t *testing.T) {
				require := require.New(t)
				assert := assert.New(t)
				home := t.TempDir()
				require.NoError(os.WriteFile(filepath.Join(home, "config.toml"), []byte(content), 0600))
				root := newRootCommand()
				ran := false
				root.AddCommand(&cobra.Command{Use: name, Run: func(*cobra.Command, []string) { ran = true }})
				root.SetErr(&bytes.Buffer{})
				root.SetArgs([]string{"--home", home, name})
				require.Error(root.Execute())
				assert.False(ran)
			})
		}
	}
}

func TestScopedConfigRequiredSections(t *testing.T) { //nolint:paralleltest // root startup changes process logging
	for _, tc := range []struct{ group, leaf, content, section string }{
		{"backup", "create", "[backup]\nzstd_level = 20", "backup"},
		{"", "sync-beeper", "[beeper]\nmax_media_mb = -1", "beeper"},
		{"", "multimodal-probe", "[vector]\nenabled = true", "vector"},
		{"embeddings", "build", "[vector]\nenabled = true", "vector"},
		{"documents", "probe-mistral", "[attachments.documents]\nprovider = 'invalid'", "attachments.documents"},
		{"daemon", "start", "[integrations.kata]\nendpoint = '://invalid'", "integrations.kata"},
	} {
		t.Run(tc.group+tc.leaf, func(t *testing.T) {
			if tc.group == "backup" || tc.group == "embeddings" || tc.leaf == "sync-beeper" {
				t.Setenv(daemonCLISubprocessEnv, strconv.Itoa(os.Getppid()))
			}
			require := require.New(t)
			assert := assert.New(t)
			home := t.TempDir()
			require.NoError(os.WriteFile(filepath.Join(home, "config.toml"), []byte(tc.content), 0600))
			root := newRootCommand()
			leaf := &cobra.Command{Use: tc.leaf, Run: func(*cobra.Command, []string) { assert.Fail("invalid required section reached command") }}
			args := []string{"--home", home}
			if tc.group != "" {
				parent := &cobra.Command{Use: tc.group}
				parent.AddCommand(leaf)
				root.AddCommand(parent)
				args = append(args, tc.group)
			} else {
				root.AddCommand(leaf)
			}
			root.SetErr(&bytes.Buffer{})
			root.SetArgs(append(args, tc.leaf))
			err := root.Execute()
			require.Error(err)
			assert.Contains(err.Error(), tc.section)
		})
	}
}

func TestScopedConfigHTTPReads(t *testing.T) { //nolint:paralleltest // root startup changes process logging
	for _, tc := range []struct {
		path             []string
		content, section string
		semantic         bool
	}{
		{[]string{"backup", "create"}, "[backup]\nzstd_level = 20", "backup", false},
		{[]string{"activity", "build"}, "[activity]\ntimezone = 'Local'", "activity", false},
		{[]string{"sync-beeper"}, "[beeper]\nmax_media_mb = -1", "beeper", false},
		{[]string{"repair-dates"}, "[analytics]\nengine = 'invalid'", "analytics", false},
		{[]string{"documents", "search"}, "[attachments.documents]\nprovider = 'invalid'", "attachments.documents", false},
		{[]string{"documents", "status"}, "[attachments.documents]\nprovider = 'invalid'", "attachments.documents", false},
		{[]string{"person", "brief", "status"}, "[people.sweep]\nwork_batch_size = -1", "people.sweep", false},
		{[]string{"person", "scoring", "status"}, "[people.identity_scoring]\nbatch_size = 0", "people.identity_scoring", false},
		{[]string{"person", "provider", "status"}, "[people.sweep]\nwork_batch_size = -1", "people.sweep", true},
	} {
		t.Run(tc.section+tc.path[len(tc.path)-1], func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			home := t.TempDir()
			require.NoError(os.WriteFile(filepath.Join(home, "config.toml"), []byte(tc.content), 0600))
			root := newRootCommand()
			parent := root
			for _, name := range tc.path[:len(tc.path)-1] {
				next := &cobra.Command{Use: name}
				parent.AddCommand(next)
				parent = next
			}
			reached := false
			leaf := &cobra.Command{Use: tc.path[len(tc.path)-1], Run: func(*cobra.Command, []string) { reached = true }}
			leaf.Flags().Bool("semantic-embeddings", false, "")
			parent.AddCommand(leaf)
			args := append([]string{"--home", home}, tc.path...)
			if tc.semantic {
				args = append(args, "--semantic-embeddings")
			}
			var stderr bytes.Buffer
			root.SetErr(&stderr)
			root.SetArgs(args)
			require.NoError(root.Execute())
			assert.True(reached)
			assert.Contains(stderr.String(), tc.section)
			if inv := invocationFromCommand(root); inv != nil && inv.logResult != nil {
				inv.logResult.Close()
			}
		})
	}
}

func TestScopedConfigMCPStatus(t *testing.T) { //nolint:paralleltest // root invocation state
	require := require.New(t)
	assert := assert.New(t)
	home := t.TempDir()
	require.NoError(os.WriteFile(filepath.Join(home, "config.toml"), []byte("[integrations.kata]\nendpoint = '://invalid'"), 0600))
	root := newRootCommand()
	mcp := &cobra.Command{Use: "mcp"}
	mcp.AddCommand(newMCPStatusCommand())
	root.AddCommand(mcp)
	var out, stderr bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&stderr)
	root.SetArgs([]string{"--home", home, "mcp", "status"})
	require.NoError(root.Execute())
	assert.Contains(out.String(), "No HTTP MCP listeners")
	assert.Contains(stderr.String(), "integrations.kata")
}

func TestScopedConfigPolicySnapshots(t *testing.T) { //nolint:paralleltest // snapshot helpers share command setup
	require := require.New(t)
	home := t.TempDir()
	path := filepath.Join(home, "config.toml")
	require.NoError(os.WriteFile(path, []byte("[integrations.kata]\nendpoint = '://invalid'"), 0600))
	snapshot, err := config.ReadConfigFile(path)
	require.NoError(err)
	_, err = personProviderConfigFromSnapshot(personProviderCommandDeps{}, snapshot)
	require.NoError(err)
	_, err = loadSetupConfig(snapshot, home)
	require.NoError(err)
	cfg, err := config.LoadForSections(path, home, config.RuntimeOverrides{})
	require.NoError(err)
	_, err = currentSemanticPersonVectorConfigSource(&invocation{cfg: cfg})()
	require.NoError(err)
	// Preparing a replacement daemon must fail before stopping a healthy one.
	require.ErrorContains(prepareServeConfig(cfg), "integrations.kata")
}

// The HTTP peer implements the daemon wire protocol so these regressions run
// real CLI startup and forwarding without making provider network requests.
func TestScopedConfigForwardingPeer(t *testing.T) { //nolint:paralleltest // command flags and root logging use process state
	oldJSON, oldWatch, oldSource := embeddingsStatusJSON, embeddingsStatusWatch, embeddingsStatusSource
	oldFull, oldYes, oldBackstop := embedFullRebuild, embedYes, embedBackstop
	oldAccounts, oldCollections := embedAccounts, embedCollections
	t.Cleanup(func() {
		embeddingsStatusJSON, embeddingsStatusWatch, embeddingsStatusSource = oldJSON, oldWatch, oldSource
		embedFullRebuild, embedYes, embedBackstop = oldFull, oldYes, oldBackstop
		embedAccounts, embedCollections = oldAccounts, oldCollections
	})
	embeddingsStatusJSON, embeddingsStatusWatch, embeddingsStatusSource = true, false, 0
	for _, tc := range []struct {
		args             []string
		content, section string
	}{
		{[]string{"embeddings", "build", "--yes"}, "[vector]\nenabled = true", "vector"},
		{[]string{"embeddings", "status"}, "[vector]\nenabled = true", "vector"},
		{[]string{"person", "provider", "status"}, "[people.sweep]\nwork_batch_size = -1", "people.sweep"},
		{[]string{"person", "provider", "status", "--semantic-embeddings"}, "[vector]\nenabled = true", "vector"},
		{[]string{"documents", "consent-docling", "--yes"}, "[attachments.documents]\nprovider = 'invalid'", "attachments.documents"},
		{[]string{"documents", "retire", "synthetic-profile", "--yes"}, "[attachments.documents]\nprovider = 'invalid'", "attachments.documents"},
		{[]string{"carddav", "connections"}, "[carddav]\nprovider = 'invalid'", "carddav"},
		{[]string{"sync-carddav"}, "[carddav_connections.default]\nprovider = 'google'", "carddav_connections"},
	} {
		t.Run(fmt.Sprint(tc.args), func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			var requests atomic.Int32
			mux := http.NewServeMux()
			mux.Handle("/api/ping", daemon.NewPingHandler(daemon.PingHandlerOptions{Service: daemonService, Version: Version}))
			mux.HandleFunc("/api/v1/cli/run", daemonCLIRunTestHandler(t, &requests, nil))
			mux.HandleFunc("/api/v1/health", func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = fmt.Fprintf(w, `{"status":"ok","api_schema_version":%q}`, embeddingsStatusMinAPISchemaVersion)
			})
			for path, response := range map[string]string{
				"/api/v1/embeddings/status":   `{"pending":0}`,
				"/api/v1/carddav/connections": `{"connections":[]}`,
				"/api/v1/carddav/sync":        `{"books":0,"created":0,"updated":0,"removed":0}`,
			} {
				mux.HandleFunc(path, func(w http.ResponseWriter, _ *http.Request) {
					requests.Add(1)
					w.Header().Set("Content-Type", "application/json")
					_, _ = fmt.Fprint(w, response)
				})
			}
			server := httptest.NewServer(mux)
			t.Cleanup(server.Close)
			home := t.TempDir()
			content := fmt.Sprintf("[remote]\nurl = %q\nallow_insecure = true\n%s", server.URL, tc.content)
			require.NoError(os.WriteFile(filepath.Join(home, "config.toml"), []byte(content), 0600))
			root := newRootCommand()
			embeddings := &cobra.Command{Use: embeddingsCommandName}
			embeddings.AddCommand(newEmbeddingsBuildCmd("build"), &cobra.Command{Use: statusValue, RunE: embeddingsStatusCmd.RunE})
			person := &cobra.Command{Use: "person"}
			person.AddCommand(newPersonProviderCommand(defaultPersonProviderCommandDeps()))
			root.AddCommand(embeddings, person, newDocumentsCmd(defaultDocumentsCommandDeps()), newCardDAVCmd(), newSyncCardDAVCmd())
			var out, stderr bytes.Buffer
			root.SetOut(&out)
			root.SetErr(&stderr)
			root.SetArgs(append([]string{"--home", home}, tc.args...))
			t.Cleanup(func() {
				if inv := invocationFromCommand(root); inv != nil && inv.logResult != nil {
					inv.logResult.Close()
				}
			})
			require.NoError(root.Execute())
			assert.Equal(int32(1), requests.Load(), "the real command reached the responding daemon")
			assert.Contains(stderr.String(), "Warning: unused config section ["+tc.section+"]")
		})
	}
}

func TestScopedConfigLocalControlsIgnoreRemote(t *testing.T) { //nolint:paralleltest // runtime environment and root logging
	t.Setenv("MSGVAULT_REMOTE_ALLOW_INSECURE", "invalid")
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		http.Error(w, "local daemon controls must not contact the remote", http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)
	t.Run("remote consumer rejects invalid remote section", func(t *testing.T) {
		require := require.New(t)
		home := t.TempDir()
		content := fmt.Sprintf("[remote]\nurl = %q\nallow_insecure = true\n", server.URL)
		require.NoError(os.WriteFile(filepath.Join(home, "config.toml"), []byte(content), 0600))
		root := newRootCommand()
		root.AddCommand(newSyncCardDAVCmd())
		root.SetErr(&bytes.Buffer{})
		root.SetArgs([]string{"--home", home, "sync-carddav"})
		require.ErrorContains(root.Execute(), "MSGVAULT_REMOTE_ALLOW_INSECURE")
		assert.Zero(t, requests.Load())
	})
	for _, group := range []string{"daemon", "serve"} {
		for _, action := range []string{statusValue, "stop"} {
			t.Run(group+action, func(t *testing.T) {
				require := require.New(t)
				assert := assert.New(t)
				home := t.TempDir()
				content := fmt.Sprintf("[remote]\nurl = %q\nallow_insecure = true\n", server.URL)
				require.NoError(os.WriteFile(filepath.Join(home, "config.toml"), []byte(content), 0600))
				root := newRootCommand()
				if group == "daemon" {
					root.AddCommand(newDaemonCommand())
				} else {
					serve := &cobra.Command{Use: "serve"}
					serve.AddCommand(newLifecycleCommand(statusValue, true), newLifecycleCommand("stop", true))
					root.AddCommand(serve)
				}
				var out, stderr bytes.Buffer
				root.SetOut(&out)
				root.SetErr(&stderr)
				root.SetArgs([]string{"--home", home, group, action})
				t.Cleanup(func() {
					if inv := invocationFromCommand(root); inv != nil && inv.logResult != nil {
						inv.logResult.Close()
					}
				})
				require.NoError(root.Execute())
				assert.Zero(requests.Load())
				assert.Contains(stderr.String(), "Warning: unused config section [remote]")
			})
		}
	}
}
