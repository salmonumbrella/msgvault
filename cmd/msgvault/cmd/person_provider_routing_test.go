package cmd

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/peoplesweep"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestPersonProviderFrontendRoutesExactCommandsAndCredential(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		wantArgs []string
	}{
		{name: "status", args: []string{"status", "--json"},
			wantArgs: []string{"person", "provider", "status", "--json"}},
		{name: "consent", args: []string{"consent", "--yes"},
			wantArgs: []string{"person", "provider", "consent", "--yes"}},
		{name: "revoke", args: []string{"revoke"},
			wantArgs: []string{"person", "provider", "revoke"}},
		{name: "list", args: []string{"list", "--json"},
			wantArgs: []string{"person", "provider", "list", "--json"}},
		{name: "history", args: []string{"history", "default", "--json"},
			wantArgs: []string{"person", "provider", "history", "--json", "default"}},
		{name: "semantic consent", args: []string{"consent", "--semantic-embeddings", "--yes"},
			wantArgs: []string{"person", "provider", "consent", "--semantic-embeddings", "--yes"}},
		{name: "check", args: []string{"check", "--json"},
			wantArgs: []string{"person", "provider", "check", "--json"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert := assert.New(t)
			var gotArgs []string
			var gotEnv map[string]string
			lookups := 0
			deps := personProviderCommandDeps{
				config:             personProviderTestConfig,
				isDaemonSubprocess: func() bool { return false },
				lookupEnv: func(name string) (string, bool) {
					lookups++
					assert.Equal("TEST_PROVIDER_KEY", name)
					return "caller-secret-canary", true
				},
				proxy: func(command *cobra.Command, args []string, env map[string]string) error {
					var err error
					gotArgs, err = daemonCLIArgsFromCobra(command, args)
					require.NoError(t, err)
					gotEnv = env
					return nil
				},
			}

			_, err := executePersonProviderCommand(t, deps, test.args...)
			require.NoError(t, err)
			assert.Equal(test.wantArgs, gotArgs)
			assert.Nil(gotEnv)
			assert.Zero(lookups)
		})
	}
}

func TestPersonProviderFrontendNamedCheckSendsOnlyProfileName(t *testing.T) {
	config := personProviderTestConfig()
	secondary := configuredPersonProvider(config)
	secondary.Model = "secondary-model"
	secondary.CredentialEnv = "SECONDARY_PROVIDER_KEY"
	config.Providers["secondary"] = secondary

	var gotArgs []string
	var gotEnv map[string]string
	var lookups []string
	deps := personProviderCommandDeps{
		config:             func() peoplesweep.Config { return config },
		isDaemonSubprocess: func() bool { return false },
		lookupEnv: func(name string) (string, bool) {
			lookups = append(lookups, name)
			return map[string]string{
				"TEST_PROVIDER_KEY":      "active-key-canary",
				"SECONDARY_PROVIDER_KEY": "selected-key-canary",
			}[name], true
		},
		proxy: func(command *cobra.Command, args []string, env map[string]string) error {
			var err error
			gotArgs, err = daemonCLIArgsFromCobra(command, args)
			gotEnv = env
			return err
		},
	}

	_, err := executePersonProviderCommand(t, deps, "check", "secondary", "--json")
	require.NoError(t, err)
	assert.Equal(t, []string{"person", "provider", "check", "--json", "secondary"}, gotArgs)
	assert.Empty(t, lookups)
	assert.Nil(t, gotEnv)
}

func TestPersonProviderLoginAndModelsNeverProxy(t *testing.T) {
	for _, operation := range []string{"login", "models"} {
		t.Run(operation, func(t *testing.T) {
			proxied := false
			deps := personProviderCommandDeps{
				config:             personProviderTestConfig,
				isDaemonSubprocess: func() bool { return false },
				proxy: func(*cobra.Command, []string, map[string]string) error {
					proxied = true
					return nil
				},
				newCodexClient: func(peoplesweep.Config) (personProviderCodexClient, error) {
					return nil, assert.AnError
				},
			}

			_, err := executePersonProviderCommand(t, deps, operation)
			require.Error(t, err)
			assert.False(t, proxied)
		})
	}
}

func TestPersonProviderFrontendRemoveProxiesOnlyNamedRevoke(t *testing.T) {
	configured := personProviderTestConfig()
	beta := configuredPersonProvider(configured)
	beta.Model = "beta-model"
	configured.Providers["beta"] = beta
	path, _ := retainedPersonProviderTestConfig(t, configured)
	var gotArgs []string
	deps := personProviderCommandDeps{
		config:             func() peoplesweep.Config { return configured },
		isDaemonSubprocess: func() bool { return false },
		proxy: func(command *cobra.Command, args []string, _ map[string]string) error {
			var err error
			gotArgs, err = daemonCLIArgsFromCobra(command, args)
			return err
		},
		openStore: func() (personProviderStore, func(), error) {
			require.FailNow(t, "frontend remove must revoke through the daemon owner")
			return nil, nil, assert.AnError
		},
		readConfigFile: func() (config.ConfigFile, error) {
			return config.ReadConfigFile(path)
		},
		editConfigTables: func(etag string, edits []config.TableEdit) (config.ConfigFile, error) {
			return config.EditConfigTables(path, etag, edits)
		},
		restoreConfigFile: func(etag string, before config.ConfigFile) (config.ConfigFile, error) {
			return config.RestoreConfigFile(path, etag, before)
		},
		configHomeDir: func() string { return filepath.Dir(path) },
	}

	_, err := executePersonProviderCommand(t, deps, "remove", "beta")
	require.NoError(t, err)
	assert.Equal(t, []string{"person", "provider", "revoke", "beta"}, gotArgs)
}

func TestPersonProviderAnonymousCheckForwardsNoCredential(t *testing.T) {
	config := personProviderTestConfig()
	mutateConfiguredPersonProvider(&config, func(provider *peoplesweep.ProviderConfig) {
		provider.Endpoint = "http://127.0.0.1:11434/v1"
		provider.Auth = peoplesweep.AuthNone
		provider.Credential = peoplesweep.CredentialNone
		provider.CredentialEnv = ""
	})
	var gotEnv map[string]string
	deps := personProviderCommandDeps{
		config:             func() peoplesweep.Config { return config },
		isDaemonSubprocess: func() bool { return false },
		lookupEnv: func(string) (string, bool) {
			require.FailNow(t, "anonymous check must not resolve a credential")
			return "", false
		},
		proxy: func(_ *cobra.Command, _ []string, env map[string]string) error {
			gotEnv = env
			return nil
		},
	}

	_, err := executePersonProviderCommand(t, deps, "check")
	require.NoError(t, err)
	assert.Nil(t, gotEnv)
}

// TestPersonProviderFrontendCheckUsesDirectStoreWithoutDaemon catches the
// final add check auto-starting a daemon instead of using the available local
// writer when no daemon owns the database.
func TestPersonProviderFrontendCheckUsesDirectStoreWithoutDaemon(t *testing.T) {
	configured := personProviderTestConfig()
	checker := &fixedPersonProviderChecker{response: peoplesweep.StructuredResponse{
		Output: []byte(`{"ok":true}`), ProviderVersion: peoplesweep.OpenAICompatibleProviderVersion,
		ModelVersion: "direct-model-v1",
	}}
	st := testutil.NewSQLiteTestStore(t)
	deps := localPersonProviderDeps(configured, st, checker)
	deps.isDaemonSubprocess = func() bool { return false }
	deps.providerStoreOwnedByDaemon = func(context.Context) (bool, error) { return false, nil }
	deps.proxy = func(*cobra.Command, []string, map[string]string) error {
		require.FailNow(t, "no-daemon check must not proxy or auto-start a daemon")
		return assert.AnError
	}

	output, err := executePersonProviderCommand(t, deps, "check", "default", "--json")
	require.NoError(t, err)
	assert.Contains(t, output, `"ok":true`)
}

func TestPersonProviderCommandsRejectUnsafeNamesBeforeRoutingOrState(t *testing.T) {
	tests := []struct {
		operation string
		name      string
	}{
		{operation: "add", name: "--json"},
		{operation: "check", name: "bad\nname"},
		{operation: "use", name: strings.Repeat("u", 65)},
		{operation: "remove", name: "--help"},
		{operation: "status", name: "bad\rname"},
		{operation: "consent", name: " leading"},
		{operation: "revoke", name: "trailing "},
		{operation: "history", name: "--json"},
	}
	for _, test := range tests {
		t.Run(test.operation, func(t *testing.T) {
			stateCalls := 0
			proxyCalls := 0
			deps := personProviderCommandDeps{
				config: func() peoplesweep.Config {
					stateCalls++
					return personProviderTestConfig()
				},
				isDaemonSubprocess: func() bool { return false },
				proxy: func(*cobra.Command, []string, map[string]string) error {
					proxyCalls++
					return nil
				},
			}

			_, err := executePersonProviderCommand(t, deps, test.operation, "--", test.name)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "invalid people provider profile name")
			assert.NotContains(t, err.Error(), test.name)
			assert.Zero(t, stateCalls)
			assert.Zero(t, proxyCalls)
		})
	}
}
