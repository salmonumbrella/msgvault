package cmd

import (
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/peoplesweep"
)

func TestPersonProviderFrontendRoutesExactCommandsAndCredential(t *testing.T) {
	tests := []struct {
		name        string
		args        []string
		keyValue    string
		wantArgs    []string
		wantEnv     map[string]string
		wantLookups int
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
		{name: "check without key", args: []string{"check", "--json"},
			wantArgs: []string{"person", "provider", "check", "--json"}, wantLookups: 1},
		{name: "check with key", args: []string{"check"}, keyValue: "caller-key",
			wantArgs: []string{"person", "provider", "check"},
			wantEnv:  map[string]string{"TEST_PROVIDER_KEY": "caller-key"}, wantLookups: 1},
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
					return test.keyValue, test.keyValue != ""
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
			assert.Equal(test.wantEnv, gotEnv)
			assert.Equal(test.wantLookups, lookups)
		})
	}
}

func TestPersonProviderFrontendNamedCheckForwardsOnlySelectedExactEnvironment(t *testing.T) {
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
	assert.Equal(t, []string{"SECONDARY_PROVIDER_KEY"}, lookups)
	assert.Equal(t, map[string]string{"SECONDARY_PROVIDER_KEY": "selected-key-canary"}, gotEnv)
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
			return config.ConfigFile{ETag: `"sha256-before"`, Exists: true}, nil
		},
		editConfigTables: func(string, []config.TableEdit) (config.ConfigFile, error) {
			return config.ConfigFile{ETag: `"sha256-after"`, Exists: true}, nil
		},
		restoreConfigFile: func(string, config.ConfigFile) (config.ConfigFile, error) {
			return config.ConfigFile{}, assert.AnError
		},
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
