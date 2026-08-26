package cmd

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charmbracelet/x/term"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/peoplesweep"
	"go.kenn.io/msgvault/internal/testutil"
)

const providerSetupSecretCanary = "provider-setup-secret-canary"

type providerCredentialChunkReader struct {
	chunks [][]byte
}

type countingProviderReader struct {
	reads int
	data  *bytes.Reader
}

type observedProviderReader struct {
	data           *bytes.Reader
	maxDestination int
}

func (r *countingProviderReader) Read(destination []byte) (int, error) {
	r.reads++
	return r.data.Read(destination) //nolint:wrapcheck // Test reader transparently delegates to bytes.Reader.
}

func (r *observedProviderReader) Read(destination []byte) (int, error) {
	if len(destination) > r.maxDestination {
		r.maxDestination = len(destination)
	}
	return r.data.Read(destination) //nolint:wrapcheck // Test reader transparently delegates to bytes.Reader.
}

func (r *providerCredentialChunkReader) Read(destination []byte) (int, error) {
	if len(r.chunks) == 0 {
		return 0, io.EOF
	}
	chunk := r.chunks[0]
	read := copy(destination, chunk)
	if read == len(chunk) {
		r.chunks = r.chunks[1:]
	} else {
		r.chunks[0] = chunk[read:]
	}
	return read, nil
}

func TestReadProviderCredentialLineRejectsTrailingChunk(t *testing.T) {
	reader := &providerCredentialChunkReader{chunks: [][]byte{
		[]byte(providerSetupSecretCanary + "\n"),
		[]byte("attacker-controlled-trailing-data"),
	}}

	credential, err := readProviderCredentialLine(reader)
	require.Error(t, err)
	assert.Empty(t, credential)
	assert.NotContains(t, err.Error(), providerSetupSecretCanary)
}

func TestReadBoundedMaskedCredentialInputRejectsBeforeGrowingPastLimit(t *testing.T) {
	reader := &observedProviderReader{data: bytes.NewReader([]byte("123456789-discarded\n"))}

	credential, err := readBoundedMaskedCredentialInput(reader, 8)
	require.ErrorContains(t, err, "too large")
	assert.Empty(t, credential)
	assert.Equal(t, 1, reader.maxDestination)
}

func TestReadBoundedMaskedCredentialInputHandlesEditingAndCancel(t *testing.T) {
	credential, err := readBoundedMaskedCredentialInput(bytes.NewBuffer([]byte{'a', 'b', 0x7f, 'c', '\r'}), 8)
	require.NoError(t, err)
	assert.Equal(t, []byte("ac"), credential)

	credential, err = readBoundedMaskedCredentialInput(bytes.NewBuffer([]byte{'a', 0x03}), 8)
	require.ErrorContains(t, err, "canceled")
	assert.Empty(t, credential)
}

func TestReadBoundedMaskedCredentialRestoresTerminalOnEveryReadResult(t *testing.T) {
	for _, test := range []struct {
		name       string
		input      []byte
		restoreErr error
	}{
		{name: "success", input: []byte("safe\n")},
		{name: "cancel", input: []byte{'x', 0x03}},
		{name: "too large", input: []byte("123456789\n")},
		{name: "restore error", input: []byte("safe\n"), restoreErr: errors.New("restore failed")},
	} {
		t.Run(test.name, func(t *testing.T) {
			state := &term.State{}
			makeCalls := 0
			restoreCalls := 0

			credential, err := readBoundedMaskedCredentialWithTerminal(
				bytes.NewReader(test.input), 17, 8,
				func(fd uintptr) (*term.State, error) {
					makeCalls++
					assert.Equal(t, uintptr(17), fd)
					return state, nil
				},
				func(fd uintptr, restored *term.State) error {
					restoreCalls++
					assert.Equal(t, uintptr(17), fd)
					assert.Same(t, state, restored)
					return test.restoreErr
				},
			)
			assert.Equal(t, 1, makeCalls)
			assert.Equal(t, 1, restoreCalls)
			if test.restoreErr != nil {
				require.ErrorContains(t, err, "restore credential terminal")
				assert.Empty(t, credential)
			}
		})
	}
}

func TestPersonProviderAddValidatesPolicyBeforeReadingCredentialOrNegotiating(t *testing.T) {
	path, loaded := providerSetupConfigFile(t)
	deps := providerSetupCommandDeps(t, path, loaded, nil)
	var lookups, negotiations int
	deps.setup.lookupEnv = func(string) (string, bool) {
		lookups++
		return providerSetupSecretCanary, true
	}
	deps.setup.negotiate = func(
		context.Context, peoplesweep.ProviderConfig, peoplesweep.Credential,
	) (peoplesweep.NegotiatedCapabilities, error) {
		negotiations++
		return peoplesweep.NegotiatedCapabilities{}, nil
	}

	output, err := executePersonProviderCommand(t, deps,
		"add", "unsafe-provider", "--custom", "--protocol", "openai_chat",
		"--endpoint", "http://provider.example.test/v1", "--model", "unsafe-model",
		"--auth", "bearer", "--credential-env", "EXACT_UNSAFE_KEY",
		"--retention-posture", "zero_retention", "--training-posture", "no_training",
		"--source", "conversation_text", "--source-since", "2025-01-01", "--yes")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "HTTPS")
	assert.Zero(t, lookups)
	assert.Zero(t, negotiations)
	assert.NotContains(t, output, providerSetupSecretCanary)
}

func TestPersonProviderAddRejectsLocalOptionConflictsBeforeCatalogOrState(t *testing.T) {
	tests := []struct {
		name  string
		flags []string
	}{
		{name: "custom catalog prices", flags: []string{"--custom", "--accept-catalog-prices", "--yes"}},
		{name: "missing confirmation", flags: []string{"--custom"}},
		{name: "mixed credential inputs", flags: []string{"--custom", "--api-key-stdin", "--credential-env", "EXACT_KEY", "--yes"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			deps := personProviderCommandDeps{
				config:         func() peoplesweep.Config { calls++; return personProviderTestConfig() },
				readConfigFile: func() (config.ConfigFile, error) { calls++; return config.ConfigFile{}, assert.AnError },
				editConfigTables: func(string, []config.TableEdit) (config.ConfigFile, error) {
					calls++
					return config.ConfigFile{}, assert.AnError
				},
				restoreConfigFile: func(config.ConfigFile, config.ConfigFile) (config.ConfigFile, error) {
					calls++
					return config.ConfigFile{}, assert.AnError
				},
				setup: personProviderSetupDeps{
					catalog:   func(context.Context) ([]peoplesweep.ProviderSuggestion, error) { calls++; return nil, nil },
					lookupEnv: func(string) (string, bool) { calls++; return providerSetupSecretCanary, true },
					negotiate: func(context.Context, peoplesweep.ProviderConfig, peoplesweep.Credential) (peoplesweep.NegotiatedCapabilities, error) {
						calls++
						return peoplesweep.NegotiatedCapabilities{}, nil
					},
				},
			}
			args := []string{
				"add", "local-options", "--protocol", "openai_chat",
				"--endpoint", "https://options.example.test/v1", "--model", "options-model",
				"--auth", "bearer", "--retention-posture", "zero_retention",
				"--training-posture", "no_training", "--source", "conversation_text",
				"--source-since", "2025-01-01",
			}
			args = append(args, test.flags...)

			_, err := executePersonProviderCommand(t, deps, args...)
			require.Error(t, err)
			assert.Zero(t, calls)
		})
	}
}

func TestPersonProviderAcceptedCatalogPriceMustResolveBeforeSecretOrProviderCall(t *testing.T) {
	path, loaded := providerSetupConfigFile(t)
	deps := providerSetupCommandDeps(t, path, loaded, nil)
	catalogCalls, lookups, negotiations := 0, 0, 0
	deps.setup.catalog = func(context.Context) ([]peoplesweep.ProviderSuggestion, error) {
		catalogCalls++
		return []peoplesweep.ProviderSuggestion{{
			ID: "incomplete", Name: "Incomplete", Endpoint: "https://prices.example.test/v1",
			Models: []peoplesweep.ModelSuggestion{{ID: "prices-model", Name: "Prices Model"}},
		}}, nil
	}
	deps.setup.lookupEnv = func(string) (string, bool) {
		lookups++
		return providerSetupSecretCanary, true
	}
	deps.setup.negotiate = func(
		context.Context, peoplesweep.ProviderConfig, peoplesweep.Credential,
	) (peoplesweep.NegotiatedCapabilities, error) {
		negotiations++
		return peoplesweep.NegotiatedCapabilities{}, nil
	}

	_, err := executePersonProviderCommand(t, deps,
		"add", "prices", "--protocol", "openai_chat",
		"--endpoint", "https://prices.example.test/v1", "--model", "prices-model",
		"--auth", "bearer", "--credential-env", "EXACT_PRICES_KEY",
		"--retention-posture", "zero_retention", "--training-posture", "no_training",
		"--source", "conversation_text", "--source-since", "2025-01-01",
		"--accept-catalog-prices", "--yes")
	require.ErrorContains(t, err, "catalog price")
	assert.Equal(t, 1, catalogCalls)
	assert.Zero(t, lookups)
	assert.Zero(t, negotiations)
}

func TestPersonProviderAcceptedCatalogPricesValidateProposedBudgetBeforeSecretOrProvider(t *testing.T) {
	for _, test := range []struct {
		name      string
		input     int64
		output    int64
		wantError string
	}{
		{name: "zero prices with cost cap", input: 0, output: 0, wantError: "prices are required"},
		{name: "overflowing reservation", input: math.MaxInt64, output: 1, wantError: "overflow"},
	} {
		t.Run(test.name, func(t *testing.T) {
			path, _ := providerSetupConfigFile(t)
			snapshot, err := config.ReadConfigFile(path)
			require.NoError(t, err)
			withCap, err := config.EditConfigFile(path, snapshot.ETag, []config.Edit{{
				Key: "people.sweep.budgets.max_estimated_cost_microusd_per_run", Value: int64(10_000),
			}})
			require.NoError(t, err)
			loaded, err := config.LoadConfigFile(withCap, "")
			require.NoError(t, err)
			checker := &fixedPersonProviderChecker{response: peoplesweep.StructuredResponse{
				Output: []byte(`{"ok":true}`), ProviderVersion: peoplesweep.OpenAICompatibleProviderVersion,
				ModelVersion: "prices-model-v1",
			}}
			deps := providerSetupCommandDeps(t, path, loaded, checker)
			catalogCalls, credentialReads, negotiations, writes := 0, 0, 0, 0
			deps.setup.catalog = func(context.Context) ([]peoplesweep.ProviderSuggestion, error) {
				catalogCalls++
				return []peoplesweep.ProviderSuggestion{{
					ID: "prices", Name: "Prices", Endpoint: "https://prices.example.test/v1",
					Models: []peoplesweep.ModelSuggestion{{
						ID: "prices-model", Name: "Prices Model",
						InputCostMicroUSDPerMillionTokens:  &test.input,
						OutputCostMicroUSDPerMillionTokens: &test.output,
					}},
				}}, nil
			}
			deps.setup.lookupEnv = func(string) (string, bool) {
				credentialReads++
				return providerSetupSecretCanary, true
			}
			deps.setup.negotiate = func(
				context.Context, peoplesweep.ProviderConfig, peoplesweep.Credential,
			) (peoplesweep.NegotiatedCapabilities, error) {
				negotiations++
				return peoplesweep.NegotiatedCapabilities{}, nil
			}
			nativeEdit := deps.editConfigTables
			deps.editConfigTables = func(etag string, edits []config.TableEdit) (config.ConfigFile, error) {
				writes++
				return nativeEdit(etag, edits)
			}

			_, err = executePersonProviderCommand(t, deps,
				"add", "prices", "--protocol", "openai_chat",
				"--endpoint", "https://prices.example.test/v1", "--model", "prices-model",
				"--auth", "bearer", "--credential-env", "EXACT_PRICES_KEY",
				"--retention-posture", "zero_retention", "--training-posture", "no_training",
				"--source", "conversation_text", "--source-since", "2025-01-01",
				"--accept-catalog-prices", "--yes")
			require.ErrorContains(t, err, test.wantError)
			assert.Equal(t, 1, catalogCalls)
			assert.Zero(t, credentialReads)
			assert.Zero(t, negotiations)
			assert.Zero(t, writes)
		})
	}
}

func TestPersonProviderAddRejectsInvalidSnapshotCapsBeforeSecretOrProvider(t *testing.T) {
	path, loaded := providerSetupConfigFile(t)
	deps := providerSetupCommandDeps(t, path, loaded, &fixedPersonProviderChecker{})
	catalogCalls, credentialReads, negotiations, writes := 0, 0, 0, 0
	deps.setup.catalog = func(context.Context) ([]peoplesweep.ProviderSuggestion, error) {
		catalogCalls++
		content, err := os.ReadFile(path)
		require.NoError(t, err)
		content = bytes.Replace(content,
			[]byte("output_cost_microusd_per_million_tokens = 222\n"),
			[]byte("output_cost_microusd_per_million_tokens = 222\nmax_input_tokens_per_run = -1\n"), 1)
		require.NoError(t, os.WriteFile(path, content, 0o640))
		return nil, nil
	}
	deps.setup.lookupEnv = func(string) (string, bool) {
		credentialReads++
		return providerSetupSecretCanary, true
	}
	deps.setup.negotiate = func(
		context.Context, peoplesweep.ProviderConfig, peoplesweep.Credential,
	) (peoplesweep.NegotiatedCapabilities, error) {
		negotiations++
		return peoplesweep.NegotiatedCapabilities{}, nil
	}
	nativeEdit := deps.editConfigTables
	deps.editConfigTables = func(etag string, edits []config.TableEdit) (config.ConfigFile, error) {
		writes++
		return nativeEdit(etag, edits)
	}

	_, err := executePersonProviderCommand(t, deps,
		"add", "invalid-caps", "--protocol", "openai_chat",
		"--endpoint", "https://prices.example.test/v1", "--model", "prices-model",
		"--auth", "bearer", "--credential-env", "EXACT_PRICES_KEY",
		"--retention-posture", "zero_retention", "--training-posture", "no_training",
		"--source", "conversation_text", "--source-since", "2025-01-01", "--yes")
	require.ErrorContains(t, err, "max_input_tokens_per_run")
	assert.Equal(t, 1, catalogCalls)
	assert.Zero(t, credentialReads)
	assert.Zero(t, negotiations)
	assert.Zero(t, writes)
}

func providerSetupConfigFile(t *testing.T) (string, *config.Config) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	require.NoError(t, os.WriteFile(path, []byte(`# retained operator comment
[data]
data_dir = "`+filepath.ToSlash(filepath.Join(dir, "data"))+`"

[people.sweep]
enabled = false
provider = "default" # selector formatting must survive rollback

[people.sweep.providers.default]
protocol = "openai_chat"
endpoint = "https://default.example.test/v1"
model = "default-model"
auth = "bearer"
credential = "env"
credential_env = "DEFAULT_KEY"
output_mode = "native_json_schema"
token_limit_parameter = "max_completion_tokens"
retention_posture = "zero_retention"
training_posture = "no_training"
allowed_sources = ["conversation_text"]
source_since = "2025-01-01"

[people.sweep.budgets]
input_cost_microusd_per_million_tokens = 111 # operator price
output_cost_microusd_per_million_tokens = 222

[future.operator_extension]
answer = 42
`), 0o640))
	loaded, err := config.Load(path, "")
	require.NoError(t, err)
	return path, loaded
}

func providerSetupCommandDeps(
	t *testing.T,
	path string,
	loaded *config.Config,
	checker personProviderChecker,
) personProviderCommandDeps {
	t.Helper()
	st := testutil.NewSQLiteTestStore(t)
	deps := localPersonProviderDeps(loaded.People.Sweep, st, checker)
	deps.config = func() peoplesweep.Config {
		current, err := config.Load(path, "")
		require.NoError(t, err)
		return current.People.Sweep
	}
	deps.readConfigFile = func() (config.ConfigFile, error) { return config.ReadConfigFile(path) }
	deps.editConfigTables = func(etag string, edits []config.TableEdit) (config.ConfigFile, error) {
		return config.EditConfigTables(path, etag, edits)
	}
	deps.restoreConfigFile = func(published, before config.ConfigFile) (config.ConfigFile, error) {
		return config.RestoreConfigFile(path, published, before)
	}
	deps.setup = personProviderSetupDeps{
		catalog: func(context.Context) ([]peoplesweep.ProviderSuggestion, error) {
			return nil, errors.New("catalog unavailable")
		},
		negotiate: func(_ context.Context, candidate peoplesweep.ProviderConfig, credential peoplesweep.Credential) (peoplesweep.NegotiatedCapabilities, error) {
			if credential.Scheme != peoplesweep.AuthNone {
				assert.Equal(t, providerSetupSecretCanary, credential.Value())
			}
			return peoplesweep.NegotiatedCapabilities{
				OutputMode: peoplesweep.OutputModeJSONObject, TokenLimitParameter: "max_tokens",
				DriverVersion: peoplesweep.OpenAICompatibleProviderVersion,
			}, nil
		},
		credentials: peoplesweep.NewFileCredentialStore(loaded.TokensDir()),
		lookupEnv:   os.LookupEnv,
	}
	return deps
}

// TestPersonProviderDefaultDependenciesResolveCredentialsAfterConfigLoad
// reproduces the real command lifecycle: the provider command and its default
// dependencies are constructed during init, before PersistentPreRunE loads the
// selected config. Stored credentials must therefore resolve from the live
// config at execution time rather than from the init-time nil cfg.
func TestPersonProviderDefaultDependenciesResolveCredentialsAfterConfigLoad(t *testing.T) {
	previousConfig := cfg
	previousConfigFile := cfgFile
	previousHomeDir := homeDir
	previousLogger := logger
	previousLogResult := logResult
	t.Cleanup(func() {
		if logResult != nil && logResult != previousLogResult {
			logResult.Close()
		}
		cfg = previousConfig
		cfgFile = previousConfigFile
		homeDir = previousHomeDir
		logger = previousLogger
		logResult = previousLogResult
	})
	path, loaded := providerSetupConfigFile(t)
	cfg = nil
	cfgFile = path
	homeDir = ""
	logResult = nil
	deps := defaultPersonProviderCommandDeps()

	st := testutil.NewSQLiteTestStore(t)
	checker := &fixedPersonProviderChecker{response: peoplesweep.StructuredResponse{
		Output: []byte(`{"ok":true}`), ProviderVersion: peoplesweep.OpenAICompatibleProviderVersion,
		ModelVersion: "live-model-v1",
	}}
	deps.openStore = func() (personProviderStore, func(), error) { return st, func() {}, nil }
	deps.openReadStore = func() (personProviderStore, func(), error) { return st, func() {}, nil }
	deps.newChecker = func(peoplesweep.Config, personProviderStore) (personProviderChecker, error) {
		return checker, nil
	}
	deps.isDaemonSubprocess = func() bool { return true }
	deps.setup.catalog = nil
	deps.setup.negotiate = func(
		_ context.Context,
		_ peoplesweep.ProviderConfig,
		credential peoplesweep.Credential,
	) (peoplesweep.NegotiatedCapabilities, error) {
		assert.Equal(t, providerSetupSecretCanary, credential.Value())
		return peoplesweep.NegotiatedCapabilities{
			OutputMode: peoplesweep.OutputModeJSONObject, TokenLimitParameter: "max_tokens",
			DriverVersion: peoplesweep.OpenAICompatibleProviderVersion,
		}, nil
	}

	root := &cobra.Command{Use: "msgvault", PersistentPreRunE: rootCmd.PersistentPreRunE}
	person := &cobra.Command{Use: "person"}
	person.AddCommand(newPersonProviderCommand(deps))
	root.AddCommand(person)
	root.SetIn(strings.NewReader(providerSetupSecretCanary + "\n"))
	var output bytes.Buffer
	root.SetOut(&output)
	root.SetErr(&output)
	root.SetArgs([]string{
		"person", "provider", "add", "live-stored", "--custom",
		"--protocol", "openai_chat", "--endpoint", "https://live.example.test/v1",
		"--model", "live-model", "--auth", "bearer", "--api-key-stdin",
		"--retention-posture", "zero_retention", "--training-posture", "no_training",
		"--source", "conversation_text", "--source-since", "2025-01-01", "--yes",
	})
	require.NoError(t, root.ExecuteContext(t.Context()))
	assert.NotContains(t, output.String(), providerSetupSecretCanary)

	credentialPath := filepath.Join(loaded.TokensDir(), "people-providers", "live-stored.json")
	credentialData, err := os.ReadFile(credentialPath)
	require.NoError(t, err)
	assert.Contains(t, string(credentialData), providerSetupSecretCanary)
	credentialInfo, err := os.Stat(credentialPath)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), credentialInfo.Mode().Perm())
	for _, directory := range []string{loaded.TokensDir(), filepath.Dir(credentialPath)} {
		info, statErr := os.Stat(directory)
		require.NoError(t, statErr)
		assert.Equal(t, os.FileMode(0o700), info.Mode().Perm())
	}
	configData, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.NotContains(t, string(configData), providerSetupSecretCanary)
	assert.Contains(t, string(configData), `credential = "stored"`)

	removeRoot := &cobra.Command{Use: "msgvault", PersistentPreRunE: rootCmd.PersistentPreRunE}
	removePerson := &cobra.Command{Use: "person"}
	removePerson.AddCommand(newPersonProviderCommand(deps))
	removeRoot.AddCommand(removePerson)
	removeRoot.SetOut(&output)
	removeRoot.SetErr(&output)
	removeRoot.SetArgs([]string{"person", "provider", "remove", "live-stored"})
	require.NoError(t, removeRoot.ExecuteContext(t.Context()))
	assert.Contains(t, output.String(), `Removed people provider profile "live-stored"`)
	_, err = peoplesweep.NewFileCredentialStore(loaded.TokensDir()).Load("live-stored")
	require.ErrorIs(t, err, peoplesweep.ErrCredentialNotFound)
	tombstoneInfo, err := os.Stat(credentialPath)
	require.NoError(t, err)
	assert.Zero(t, tombstoneInfo.Size())
	assert.Equal(t, os.FileMode(0o600), tombstoneInfo.Mode().Perm())
}

// TestPersonProviderAddCustomStdinKeepsSecretLocal catches an add operation
// serializing a key into Cobra arguments, output, TOML, or config recovery
// artifacts instead of publishing it only to the private credential store.
func TestPersonProviderAddCustomStdinKeepsSecretLocal(t *testing.T) {
	path, loaded := providerSetupConfigFile(t)
	checker := &fixedPersonProviderChecker{response: peoplesweep.StructuredResponse{
		Output: []byte(`{"ok":true}`), ProviderVersion: peoplesweep.OpenAICompatibleProviderVersion,
		ModelVersion: "new-model-v1",
	}}
	deps := providerSetupCommandDeps(t, path, loaded, checker)

	root := &bytes.Buffer{}
	root.WriteString(providerSetupSecretCanary + "\n")
	output, err := executePersonProviderCommandWithInput(t, deps, root,
		"add", "new-provider", "--custom", "--protocol", "openai_chat",
		"--endpoint", "https://new.example.test/v1", "--model", "new-model",
		"--auth", "bearer", "--api-key-stdin", "--retention-posture", "zero_retention",
		"--training-posture", "no_training", "--source", "conversation_text",
		"--source-since", "2025-01-01", "--yes")
	require.NoError(t, err)
	assert.NotContains(t, output, providerSetupSecretCanary)
	content, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.NotContains(t, string(content), providerSetupSecretCanary)
	assert.Contains(t, string(content), `[people.sweep.providers.new-provider]`)
	assert.Contains(t, string(content), `provider = "new-provider" # selector formatting must survive rollback`)
	credential, err := peoplesweep.NewFileCredentialStore(loaded.TokensDir()).Load("new-provider")
	require.NoError(t, err)
	assert.Equal(t, providerSetupSecretCanary, credential.Value())
	recoveries, err := filepath.Glob(filepath.Join(filepath.Dir(path), ".msgvault-config-recovery-*"))
	require.NoError(t, err)
	for _, recovery := range recoveries {
		recovered, readErr := os.ReadFile(recovery)
		require.NoError(t, readErr)
		assert.NotContains(t, string(recovered), providerSetupSecretCanary)
	}

	_, err = executePersonProviderCommand(t, deps, "add", "bad", "--api-key", providerSetupSecretCanary)
	require.Error(t, err)
	assert.NotContains(t, err.Error(), providerSetupSecretCanary)
}

func executePersonProviderCommandWithInput(
	t *testing.T,
	deps personProviderCommandDeps,
	input io.Reader,
	args ...string,
) (string, error) {
	t.Helper()
	root := &cobra.Command{Use: "msgvault"}
	person := &cobra.Command{Use: "person"}
	person.AddCommand(newPersonProviderCommand(deps))
	root.AddCommand(person)
	root.SetArgs(append([]string{"person", "provider"}, args...))
	root.SetIn(input)
	var output bytes.Buffer
	root.SetOut(&output)
	root.SetErr(&output)
	err := root.ExecuteContext(t.Context())
	return output.String(), err
}

// TestPersonProviderAddReadsOnlyExactEnvironmentOrMaskedTerminal catches setup
// falling back to a broader environment variable or echoing masked input.
func TestPersonProviderAddReadsOnlyExactEnvironmentOrMaskedTerminal(t *testing.T) {
	t.Run("exact environment", func(t *testing.T) {
		path, loaded := providerSetupConfigFile(t)
		checker := &fixedPersonProviderChecker{response: peoplesweep.StructuredResponse{
			Output: []byte(`{"ok":true}`), ProviderVersion: peoplesweep.OpenAICompatibleProviderVersion,
			ModelVersion: "env-model-v1",
		}}
		deps := providerSetupCommandDeps(t, path, loaded, checker)
		t.Setenv("EXACT_PROVIDER_KEY", providerSetupSecretCanary)
		t.Setenv("OPENAI_API_KEY", "broader-secret-must-not-be-read")
		var lookedUp []string
		deps.setup.lookupEnv = func(name string) (string, bool) {
			lookedUp = append(lookedUp, name)
			return os.LookupEnv(name)
		}

		output, err := executePersonProviderCommand(t, deps,
			"add", "env-provider", "--custom", "--protocol", "openai_chat",
			"--endpoint", "https://env.example.test/v1", "--model", "env-model",
			"--auth", "bearer", "--credential-env", "EXACT_PROVIDER_KEY",
			"--retention-posture", "zero_retention", "--training-posture", "no_training",
			"--source", "conversation_text", "--source-since", "2025-01-01", "--yes")
		require.NoError(t, err)
		assert.Equal(t, []string{"EXACT_PROVIDER_KEY"}, lookedUp)
		assert.NotContains(t, output, providerSetupSecretCanary)
		content, err := os.ReadFile(path)
		require.NoError(t, err)
		assert.Contains(t, string(content), `credential_env = "EXACT_PROVIDER_KEY"`)
		assert.NotContains(t, string(content), providerSetupSecretCanary)
		_, err = deps.setup.credentials.Load("env-provider")
		assert.ErrorIs(t, err, peoplesweep.ErrCredentialNotFound)
	})

	t.Run("masked terminal", func(t *testing.T) {
		path, loaded := providerSetupConfigFile(t)
		checker := &fixedPersonProviderChecker{response: peoplesweep.StructuredResponse{
			Output: []byte(`{"ok":true}`), ProviderVersion: peoplesweep.OpenAICompatibleProviderVersion,
			ModelVersion: "masked-model-v1",
		}}
		deps := providerSetupCommandDeps(t, path, loaded, checker)
		input, err := os.Open(os.DevNull)
		require.NoError(t, err)
		defer func() { _ = input.Close() }()
		deps.setup.isTerminal = func(uintptr) bool { return true }
		deps.setup.readMasked = func(file *os.File, limit int) ([]byte, error) {
			assert.Same(t, input, file)
			assert.Equal(t, maxProviderCredentialBytes, limit)
			return []byte(providerSetupSecretCanary), nil
		}

		output, err := executePersonProviderCommandWithInput(t, deps, input,
			"add", "masked-provider", "--custom", "--protocol", "openai_chat",
			"--endpoint", "https://masked.example.test/v1", "--model", "masked-model",
			"--auth", "x_api_key", "--retention-posture", "zero_retention",
			"--training-posture", "no_training", "--source", "conversation_text",
			"--source-since", "2025-01-01", "--yes")
		require.NoError(t, err)
		assert.NotContains(t, output, providerSetupSecretCanary)
		credential, err := deps.setup.credentials.Load("masked-provider")
		require.NoError(t, err)
		assert.Equal(t, peoplesweep.AuthXAPIKey, credential.Scheme)
		assert.Equal(t, providerSetupSecretCanary, credential.Value())
	})
}

// TestPersonProviderCatalogPricesRequireExplicitAcceptance catches mutable
// catalog data silently refreshing configured budget prices.
func TestPersonProviderCatalogPricesRequireExplicitAcceptance(t *testing.T) {
	for _, test := range []struct {
		name       string
		accept     bool
		wantInput  string
		wantOutput string
	}{
		{name: "rejected", wantInput: "111", wantOutput: "222"},
		{name: "accepted", accept: true, wantInput: "700000", wantOutput: "900000"},
	} {
		t.Run(test.name, func(t *testing.T) {
			path, loaded := providerSetupConfigFile(t)
			checker := &fixedPersonProviderChecker{response: peoplesweep.StructuredResponse{
				Output: []byte(`{"ok":true}`), ProviderVersion: peoplesweep.OpenAICompatibleProviderVersion,
				ModelVersion: "catalog-model-v1",
			}}
			deps := providerSetupCommandDeps(t, path, loaded, checker)
			inputPrice, outputPrice := int64(700000), int64(900000)
			deps.setup.catalog = func(context.Context) ([]peoplesweep.ProviderSuggestion, error) {
				return []peoplesweep.ProviderSuggestion{{
					ID: "hint-provider", Name: "Hint Provider", Endpoint: "https://catalog.example.test/v1",
					Models: []peoplesweep.ModelSuggestion{{
						ID: "catalog-model", Name: "Catalog Model",
						InputCostMicroUSDPerMillionTokens:  &inputPrice,
						OutputCostMicroUSDPerMillionTokens: &outputPrice,
					}},
				}}, nil
			}
			args := []string{
				"add", "catalog-provider", "--protocol", "openai_chat",
				"--endpoint", "https://catalog.example.test/v1", "--model", "catalog-model",
				"--auth", "none", "--retention-posture", "local_only",
				"--training-posture", "local_only", "--source", "conversation_text",
				"--source-since", "2025-01-01", "--yes",
			}
			if test.accept {
				args = append(args, "--accept-catalog-prices")
			}
			_, err := executePersonProviderCommand(t, deps, args...)
			require.ErrorContains(t, err, "loopback")
			// Use authenticated HTTPS after proving catalog labels never imply
			// anonymous policy or alter the explicit values.
			args = append(args, "--credential-env", "CATALOG_KEY")
			t.Setenv("CATALOG_KEY", providerSetupSecretCanary)
			args[9] = "bearer"
			_, err = executePersonProviderCommand(t, deps, args...)
			require.NoError(t, err)
			content, err := os.ReadFile(path)
			require.NoError(t, err)
			assert.Contains(t, string(content),
				"input_cost_microusd_per_million_tokens = "+test.wantInput+" # operator price")
			assert.Contains(t, string(content),
				"output_cost_microusd_per_million_tokens = "+test.wantOutput)
		})
	}
}

// TestPersonProviderRemoveRevokesAndDeletesOnlyExactCredential catches profile
// removal deleting audit history, a sibling credential, or the active enabled
// selector.
func TestPersonProviderRemoveRevokesAndDeletesOnlyExactCredential(t *testing.T) {
	path, loaded := providerSetupConfigFile(t)
	snapshot, err := config.ReadConfigFile(path)
	require.NoError(t, err)
	oldProvider := configuredPersonProvider(loaded.People.Sweep)
	oldProvider.Endpoint = "https://old.example.test/v1"
	oldProvider.Model = "old-model"
	oldProvider.Credential = peoplesweep.CredentialStored
	oldProvider.CredentialEnv = ""
	after, err := config.EditConfigTables(path, snapshot.ETag, []config.TableEdit{{
		Path:   []string{"people", "sweep", "providers", "old"},
		Values: personProviderTableValues(oldProvider),
	}})
	require.NoError(t, err)
	loaded, err = config.LoadConfigFile(after, "")
	require.NoError(t, err)
	credentialStore := peoplesweep.NewFileCredentialStore(loaded.TokensDir())
	require.NoError(t, credentialStore.Save("old", peoplesweep.NewCredential(
		peoplesweep.AuthBearer, providerSetupSecretCanary)))
	require.NoError(t, credentialStore.Save("sibling", peoplesweep.NewCredential(
		peoplesweep.AuthBearer, "sibling-secret")))
	st := testutil.NewSQLiteTestStore(t)
	selected := loaded.People.Sweep
	selected.Enabled = true
	selected.Provider = peoplesweep.ProviderSelection{Name: "old"}
	profile, err := selected.Profile()
	require.NoError(t, err)
	_, err = st.EnsurePersonInferenceProfile(t.Context(), profile)
	require.NoError(t, err)
	_, _, err = st.GrantPersonInferenceConsent(t.Context(), profile.Fingerprint, "cli")
	require.NoError(t, err)
	deps := localPersonProviderDeps(loaded.People.Sweep, st, nil)
	deps.config = func() peoplesweep.Config {
		current, loadErr := config.Load(path, "")
		require.NoError(t, loadErr)
		return current.People.Sweep
	}
	deps.readConfigFile = func() (config.ConfigFile, error) { return config.ReadConfigFile(path) }
	deps.editConfigTables = func(etag string, edits []config.TableEdit) (config.ConfigFile, error) {
		return config.EditConfigTables(path, etag, edits)
	}
	deps.restoreConfigFile = func(published, before config.ConfigFile) (config.ConfigFile, error) {
		return config.RestoreConfigFile(path, published, before)
	}
	deps.setup.credentials = credentialStore

	output, err := executePersonProviderCommand(t, deps, "remove", "old")
	require.NoError(t, err)
	assert.Contains(t, output, "old")
	content, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.NotContains(t, string(content), "[people.sweep.providers.old]")
	_, err = credentialStore.Load("old")
	require.ErrorIs(t, err, peoplesweep.ErrCredentialNotFound)
	sibling, err := credentialStore.Load("sibling")
	require.NoError(t, err)
	assert.Equal(t, "sibling-secret", sibling.Value())
	active, err := st.HasActivePersonInferenceConsent(t.Context(), profile.Fingerprint)
	require.NoError(t, err)
	assert.False(t, active)
	profiles, err := st.ListPersonInferenceProfiles(t.Context())
	require.NoError(t, err)
	assert.Len(t, profiles, 1, "immutable audit profile must remain")

	activePath, activeLoaded := providerSetupConfigFile(t)
	activeSnapshot, err := config.ReadConfigFile(activePath)
	require.NoError(t, err)
	_, err = config.EditConfigFile(activePath, activeSnapshot.ETag, []config.Edit{{
		Key: "people.sweep.enabled", Value: true,
	}})
	require.NoError(t, err)
	activeDeps := localPersonProviderDeps(activeLoaded.People.Sweep, st, nil)
	activeDeps.readConfigFile = func() (config.ConfigFile, error) { return config.ReadConfigFile(activePath) }
	activeDeps.editConfigTables = func(etag string, edits []config.TableEdit) (config.ConfigFile, error) {
		return config.EditConfigTables(activePath, etag, edits)
	}
	activeDeps.restoreConfigFile = func(published, before config.ConfigFile) (config.ConfigFile, error) {
		return config.RestoreConfigFile(activePath, published, before)
	}
	_, err = executePersonProviderCommand(t, activeDeps, "remove", "default")
	require.ErrorContains(t, err, "active")
}

func TestPersonProviderRemoveUsesOneFreshConfigSnapshotForAllSideEffects(t *testing.T) {
	path, loaded := providerSetupConfigFile(t)
	initial, err := config.ReadConfigFile(path)
	require.NoError(t, err)
	old := configuredPersonProvider(loaded.People.Sweep)
	old.Endpoint = "https://old-snapshot.example.test/v1"
	old.Model = "startup-old-model"
	old.Credential = peoplesweep.CredentialStored
	old.CredentialEnv = ""
	withOld, err := config.EditConfigTables(path, initial.ETag, []config.TableEdit{{
		Path:   []string{"people", "sweep", "providers", "old"},
		Values: personProviderTableValues(old), InsertOnly: true,
	}})
	require.NoError(t, err)
	startupLoaded, err := config.LoadConfigFile(withOld, "")
	require.NoError(t, err)
	startup := startupLoaded.People.Sweep
	credentialStore := peoplesweep.NewFileCredentialStore(startupLoaded.TokensDir())
	require.NoError(t, credentialStore.Save("old", peoplesweep.NewCredential(
		peoplesweep.AuthBearer, providerSetupSecretCanary)))
	staleSelection := startup
	staleSelection.Enabled = true
	staleSelection.Provider = peoplesweep.ProviderSelection{Name: "old"}
	staleProfile, err := staleSelection.Profile()
	require.NoError(t, err)
	st := testutil.NewSQLiteTestStore(t)
	_, err = st.EnsurePersonInferenceProfile(t.Context(), staleProfile)
	require.NoError(t, err)
	_, _, err = st.GrantPersonInferenceConsent(t.Context(), staleProfile.Fingerprint, "cli")
	require.NoError(t, err)

	current := old
	current.Model = "operator-current-model"
	current.Credential = peoplesweep.CredentialEnv
	current.CredentialEnv = "OPERATOR_CURRENT_KEY"
	currentSnapshot, err := config.ReadConfigFile(path)
	require.NoError(t, err)
	_, err = config.EditConfigTables(path, currentSnapshot.ETag, []config.TableEdit{{
		Path:   []string{"people", "sweep", "providers", "old"},
		Values: personProviderTableValues(current),
	}})
	require.NoError(t, err)

	deps := localPersonProviderDeps(startup, st, nil)
	deps.config = func() peoplesweep.Config { return startup }
	deps.readConfigFile = func() (config.ConfigFile, error) { return config.ReadConfigFile(path) }
	deps.editConfigTables = func(etag string, edits []config.TableEdit) (config.ConfigFile, error) {
		return config.EditConfigTables(path, etag, edits)
	}
	deps.restoreConfigFile = func(published, before config.ConfigFile) (config.ConfigFile, error) {
		return config.RestoreConfigFile(path, published, before)
	}
	deps.setup.credentials = credentialStore

	_, err = executePersonProviderCommand(t, deps, "remove", "old")
	require.NoError(t, err)
	stillActive, err := st.HasActivePersonInferenceConsent(t.Context(), staleProfile.Fingerprint)
	require.NoError(t, err)
	assert.True(t, stillActive, "stale startup fingerprint must not be revoked")
	credential, err := credentialStore.Load("old")
	require.NoError(t, err)
	assert.Equal(t, providerSetupSecretCanary, credential.Value())
	finalConfig, err := config.Load(path, "")
	require.NoError(t, err)
	_, exists := finalConfig.People.Sweep.Providers["old"]
	assert.False(t, exists)
}

func TestPersonProviderRemoveConfigConflictHasNoConsentOrCredentialSideEffects(t *testing.T) {
	path, loaded := providerSetupConfigFile(t)
	initial, err := config.ReadConfigFile(path)
	require.NoError(t, err)
	old := configuredPersonProvider(loaded.People.Sweep)
	old.Model = "conflict-old-model"
	old.Credential = peoplesweep.CredentialStored
	old.CredentialEnv = ""
	withOld, err := config.EditConfigTables(path, initial.ETag, []config.TableEdit{{
		Path:   []string{"people", "sweep", "providers", "old"},
		Values: personProviderTableValues(old), InsertOnly: true,
	}})
	require.NoError(t, err)
	current, err := config.LoadConfigFile(withOld, "")
	require.NoError(t, err)
	credentialStore := peoplesweep.NewFileCredentialStore(current.TokensDir())
	require.NoError(t, credentialStore.Save("old", peoplesweep.NewCredential(
		peoplesweep.AuthBearer, providerSetupSecretCanary)))
	selected := current.People.Sweep
	selected.Enabled = true
	selected.Provider = peoplesweep.ProviderSelection{Name: "old"}
	profile, err := selected.Profile()
	require.NoError(t, err)
	st := testutil.NewSQLiteTestStore(t)
	_, err = st.EnsurePersonInferenceProfile(t.Context(), profile)
	require.NoError(t, err)
	_, _, err = st.GrantPersonInferenceConsent(t.Context(), profile.Fingerprint, "cli")
	require.NoError(t, err)

	deps := localPersonProviderDeps(current.People.Sweep, st, nil)
	deps.readConfigFile = func() (config.ConfigFile, error) { return config.ReadConfigFile(path) }
	deps.editConfigTables = func(etag string, edits []config.TableEdit) (config.ConfigFile, error) {
		operator, readErr := config.ReadConfigFile(path)
		if readErr != nil {
			return config.ConfigFile{}, readErr
		}
		if _, editErr := config.EditConfigFile(path, operator.ETag, []config.Edit{{
			Key: "web.theme", Value: "dark",
		}}); editErr != nil {
			return config.ConfigFile{}, editErr
		}
		return config.EditConfigTables(path, etag, edits)
	}
	deps.restoreConfigFile = func(published, before config.ConfigFile) (config.ConfigFile, error) {
		return config.RestoreConfigFile(path, published, before)
	}
	deps.setup.credentials = credentialStore

	_, err = executePersonProviderCommand(t, deps, "remove", "old")
	require.ErrorIs(t, err, config.ErrConfigConflict)
	active, err := st.HasActivePersonInferenceConsent(t.Context(), profile.Fingerprint)
	require.NoError(t, err)
	assert.True(t, active)
	credential, err := credentialStore.Load("old")
	require.NoError(t, err)
	assert.Equal(t, providerSetupSecretCanary, credential.Value())
	finalConfig, err := config.Load(path, "")
	require.NoError(t, err)
	assert.Equal(t, "conflict-old-model", finalConfig.People.Sweep.Providers["old"].Model)
	assert.Equal(t, "dark", finalConfig.Web.Theme)
}

// TestPersonProviderAddRollsBackExactConfigAndNewCredential catches a failed
// final saved-profile check leaving a selector/profile or deleting an existing
// sibling credential during rollback.
func TestPersonProviderAddRollsBackExactConfigAndNewCredential(t *testing.T) {
	path, loaded := providerSetupConfigFile(t)
	before, err := os.ReadFile(path)
	require.NoError(t, err)
	credentialStore := peoplesweep.NewFileCredentialStore(loaded.TokensDir())
	require.NoError(t, credentialStore.Save("sibling", peoplesweep.NewCredential(
		peoplesweep.AuthBearer, "sibling-secret")))
	checker := &fixedPersonProviderChecker{err: errors.New("synthetic check failed")}
	deps := providerSetupCommandDeps(t, path, loaded, checker)

	_, err = executePersonProviderCommandWithInput(t, deps,
		bytes.NewBufferString(providerSetupSecretCanary+"\n"),
		"add", "will-rollback", "--custom", "--protocol", "openai_chat",
		"--endpoint", "https://rollback.example.test/v1", "--model", "rollback-model",
		"--auth", "bearer", "--api-key-stdin", "--retention-posture", "zero_retention",
		"--training-posture", "no_training", "--source", "conversation_text",
		"--source-since", "2025-01-01", "--yes")
	require.ErrorContains(t, err, "synthetic check failed")
	after, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, before, after)
	_, err = credentialStore.Load("will-rollback")
	require.ErrorIs(t, err, peoplesweep.ErrCredentialNotFound)
	sibling, err := credentialStore.Load("sibling")
	require.NoError(t, err)
	assert.Equal(t, "sibling-secret", sibling.Value())
	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, fs.FileMode(0o640), info.Mode().Perm())
}

type replacingSaveNewCredentialStore struct {
	*peoplesweep.FileCredentialStore

	afterSaveNew func()
}

func (s *replacingSaveNewCredentialStore) SaveNew(
	profileName string,
	credential peoplesweep.Credential,
) (peoplesweep.CredentialCleanupGuard, bool, error) {
	guard, created, err := s.FileCredentialStore.SaveNew(profileName, credential)
	if err == nil && created && s.afterSaveNew != nil {
		s.afterSaveNew()
	}
	return guard, created, err
}

type callbackPersonProviderChecker func(context.Context) (peoplesweep.StructuredResponse, error)

func (check callbackPersonProviderChecker) Check(
	ctx context.Context,
) (peoplesweep.StructuredResponse, error) {
	return check(ctx)
}

func replaceNewPersonProviderCredential(
	t *testing.T,
	store *peoplesweep.FileCredentialStore,
	tokensDir, profileName string,
) (string, string, []byte, []byte) {
	t.Helper()
	credentialPath := filepath.Join(tokensDir, "people-providers", profileName+".json")
	retainedPath := credentialPath + ".retained"
	require.NoError(t, os.Rename(credentialPath, retainedPath))
	require.NoError(t, store.Save(profileName, peoplesweep.NewCredential(
		peoplesweep.AuthBearer, providerSetupSecretCanary+"-replacement",
	)))
	original, err := os.ReadFile(retainedPath)
	require.NoError(t, err)
	replacement, err := os.ReadFile(credentialPath)
	require.NoError(t, err)
	return retainedPath, credentialPath, original, replacement
}

func assertNewPersonProviderCredentialReplacementUntouched(
	t *testing.T,
	retainedPath, credentialPath string,
	wantOriginal, wantReplacement []byte,
) {
	t.Helper()
	gotOriginal, err := os.ReadFile(retainedPath)
	require.NoError(t, err)
	gotReplacement, err := os.ReadFile(credentialPath)
	require.NoError(t, err)
	assert.Equal(t, wantOriginal, gotOriginal)
	assert.Equal(t, wantReplacement, gotReplacement)
}

func TestPersonProviderAddConfigFailureRejectsReplacementCredentialCleanup(t *testing.T) {
	path, loaded := providerSetupConfigFile(t)
	before, err := os.ReadFile(path)
	require.NoError(t, err)
	st := testutil.NewSQLiteTestStore(t)
	baseStore := peoplesweep.NewFileCredentialStore(loaded.TokensDir())
	var retainedPath, credentialPath string
	var original, replacement []byte
	credentialStore := &replacingSaveNewCredentialStore{FileCredentialStore: baseStore}
	credentialStore.afterSaveNew = func() {
		retainedPath, credentialPath, original, replacement = replaceNewPersonProviderCredential(
			t, baseStore, loaded.TokensDir(), "config-race",
		)
	}
	deps := providerSetupCommandDeps(t, path, loaded, &fixedPersonProviderChecker{})
	deps.setup.credentials = credentialStore
	deps.openStore = func() (personProviderStore, func(), error) { return st, func() {}, nil }
	deps.editConfigTables = func(string, []config.TableEdit) (config.ConfigFile, error) {
		return config.ConfigFile{}, errors.New("injected config publication failure")
	}

	_, err = executePersonProviderCommandWithInput(t, deps,
		bytes.NewBufferString(providerSetupSecretCanary+"\n"),
		"add", "config-race", "--custom", "--protocol", "openai_chat",
		"--endpoint", "https://config-race.example.test/v1", "--model", "config-race-model",
		"--auth", "bearer", "--api-key-stdin", "--retention-posture", "zero_retention",
		"--training-posture", "no_training", "--source", "conversation_text",
		"--source-since", "2025-01-01", "--yes")
	require.ErrorContains(t, err, "injected config publication failure")
	require.ErrorContains(t, err, "credential cleanup conflict")
	after, readErr := os.ReadFile(path)
	require.NoError(t, readErr)
	assert.Equal(t, before, after)
	assertNewPersonProviderCredentialReplacementUntouched(
		t, retainedPath, credentialPath, original, replacement,
	)
	credential, loadErr := baseStore.Load("config-race")
	require.NoError(t, loadErr)
	assert.Equal(t, providerSetupSecretCanary+"-replacement", credential.Value())
	var consentCount int
	require.NoError(t, st.DB().QueryRowContext(t.Context(),
		`SELECT COUNT(*) FROM person_inference_consents`).Scan(&consentCount))
	assert.Zero(t, consentCount)
}

func TestPersonProviderAddFinalCheckFailureRejectsReplacementCredentialCleanup(t *testing.T) {
	path, loaded := providerSetupConfigFile(t)
	before, err := os.ReadFile(path)
	require.NoError(t, err)
	st := testutil.NewSQLiteTestStore(t)
	credentialStore := peoplesweep.NewFileCredentialStore(loaded.TokensDir())
	var retainedPath, credentialPath string
	var original, replacement []byte
	checker := callbackPersonProviderChecker(func(context.Context) (peoplesweep.StructuredResponse, error) {
		retainedPath, credentialPath, original, replacement = replaceNewPersonProviderCredential(
			t, credentialStore, loaded.TokensDir(), "check-race",
		)
		return peoplesweep.StructuredResponse{}, errors.New("synthetic final check failed")
	})
	deps := providerSetupCommandDeps(t, path, loaded, checker)
	deps.setup.credentials = credentialStore
	deps.openStore = func() (personProviderStore, func(), error) { return st, func() {}, nil }

	_, err = executePersonProviderCommandWithInput(t, deps,
		bytes.NewBufferString(providerSetupSecretCanary+"\n"),
		"add", "check-race", "--custom", "--protocol", "openai_chat",
		"--endpoint", "https://check-race.example.test/v1", "--model", "check-race-model",
		"--auth", "bearer", "--api-key-stdin", "--retention-posture", "zero_retention",
		"--training-posture", "no_training", "--source", "conversation_text",
		"--source-since", "2025-01-01", "--yes")
	require.ErrorContains(t, err, "synthetic final check failed")
	require.ErrorContains(t, err, "credential cleanup conflict")
	after, readErr := os.ReadFile(path)
	require.NoError(t, readErr)
	assert.Equal(t, before, after)
	assertNewPersonProviderCredentialReplacementUntouched(
		t, retainedPath, credentialPath, original, replacement,
	)
	credential, loadErr := credentialStore.Load("check-race")
	require.NoError(t, loadErr)
	assert.Equal(t, providerSetupSecretCanary+"-replacement", credential.Value())
	var consentCount int
	require.NoError(t, st.DB().QueryRowContext(t.Context(),
		`SELECT COUNT(*) FROM person_inference_consents`).Scan(&consentCount))
	assert.Zero(t, consentCount)
}

func TestPersonProviderAddConfigConflictKeepsConcurrentEditAndDeletesOnlyNewCredential(t *testing.T) {
	path, loaded := providerSetupConfigFile(t)
	checker := &fixedPersonProviderChecker{response: peoplesweep.StructuredResponse{
		Output: []byte(`{"ok":true}`), ProviderVersion: peoplesweep.OpenAICompatibleProviderVersion,
		ModelVersion: "conflict-model-v1",
	}}
	deps := providerSetupCommandDeps(t, path, loaded, checker)
	editTables := deps.editConfigTables
	deps.editConfigTables = func(etag string, edits []config.TableEdit) (config.ConfigFile, error) {
		current, err := config.ReadConfigFile(path)
		if err != nil {
			return config.ConfigFile{}, err
		}
		if _, err := config.EditConfigFile(path, current.ETag, []config.Edit{{
			Key: "web.theme", Value: "dark",
		}}); err != nil {
			return config.ConfigFile{}, err
		}
		return editTables(etag, edits)
	}

	output, err := executePersonProviderCommandWithInput(t, deps,
		bytes.NewBufferString(providerSetupSecretCanary+"\n"),
		"add", "conflicted", "--custom", "--protocol", "openai_chat",
		"--endpoint", "https://conflict.example.test/v1", "--model", "conflict-model",
		"--auth", "bearer", "--api-key-stdin", "--retention-posture", "zero_retention",
		"--training-posture", "no_training", "--source", "conversation_text",
		"--source-since", "2025-01-01", "--yes")
	require.ErrorIs(t, err, config.ErrConfigConflict)
	assert.NotContains(t, err.Error(), providerSetupSecretCanary)
	content, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Contains(t, string(content), `theme = "dark"`)
	assert.NotContains(t, string(content), "providers.conflicted")
	assert.NotContains(t, string(content), providerSetupSecretCanary)
	assert.NotContains(t, output, providerSetupSecretCanary)
	_, err = deps.setup.credentials.Load("conflicted")
	assert.ErrorIs(t, err, peoplesweep.ErrCredentialNotFound)
}

func TestPersonProviderAddRollsBackExactUncertainPublication(t *testing.T) {
	path, loaded := providerSetupConfigFile(t)
	beforeBytes, err := os.ReadFile(path)
	require.NoError(t, err)
	checker := &fixedPersonProviderChecker{response: peoplesweep.StructuredResponse{
		Output: []byte(`{"ok":true}`), ProviderVersion: peoplesweep.OpenAICompatibleProviderVersion,
		ModelVersion: "uncertain-model-v1",
	}}
	deps := providerSetupCommandDeps(t, path, loaded, checker)
	deps.editConfigTables = func(etag string, edits []config.TableEdit) (config.ConfigFile, error) {
		after, editErr := config.EditConfigTables(path, etag, edits)
		if editErr != nil {
			return after, editErr
		}
		return after, errors.Join(config.ErrConfigChanged, errors.New("injected cleanup uncertainty"))
	}

	_, err = executePersonProviderCommandWithInput(t, deps,
		bytes.NewBufferString(providerSetupSecretCanary+"\n"),
		"add", "uncertain", "--custom", "--protocol", "openai_chat",
		"--endpoint", "https://uncertain.example.test/v1", "--model", "uncertain-model",
		"--auth", "bearer", "--api-key-stdin", "--retention-posture", "zero_retention",
		"--training-posture", "no_training", "--source", "conversation_text",
		"--source-since", "2025-01-01", "--yes")
	require.ErrorIs(t, err, config.ErrConfigChanged)
	afterBytes, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, beforeBytes, afterBytes)
	_, err = deps.setup.credentials.Load("uncertain")
	assert.ErrorIs(t, err, peoplesweep.ErrCredentialNotFound)
}

func TestPersonProviderAddFailedCheckRestoresOriginallyMissingConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	startup := config.NewDefaultConfig()
	startup.HomeDir = dir
	st := testutil.NewSQLiteTestStore(t)
	deps := localPersonProviderDeps(startup.People.Sweep, st,
		&fixedPersonProviderChecker{err: errors.New("synthetic final check failed")})
	deps.config = func() peoplesweep.Config { return startup.People.Sweep }
	deps.configHomeDir = func() string { return dir }
	deps.readConfigFile = func() (config.ConfigFile, error) { return config.ReadConfigFile(path) }
	deps.editConfigTables = func(etag string, edits []config.TableEdit) (config.ConfigFile, error) {
		return config.EditConfigTables(path, etag, edits)
	}
	deps.restoreConfigFile = func(published, before config.ConfigFile) (config.ConfigFile, error) {
		return config.RestoreConfigFile(path, published, before)
	}
	deps.setup = personProviderSetupDeps{
		negotiate: func(context.Context, peoplesweep.ProviderConfig, peoplesweep.Credential) (peoplesweep.NegotiatedCapabilities, error) {
			return peoplesweep.NegotiatedCapabilities{
				OutputMode: peoplesweep.OutputModeJSONObject, TokenLimitParameter: "max_tokens",
				DriverVersion: peoplesweep.OpenAICompatibleProviderVersion,
			}, nil
		},
		credentials: peoplesweep.NewFileCredentialStore(filepath.Join(dir, "tokens")),
	}

	_, err := executePersonProviderCommandWithInput(t, deps,
		bytes.NewBufferString(providerSetupSecretCanary+"\n"),
		"add", "first", "--custom", "--protocol", "openai_chat",
		"--endpoint", "https://first.example.test/v1", "--model", "first-model",
		"--auth", "bearer", "--api-key-stdin", "--retention-posture", "zero_retention",
		"--training-posture", "no_training", "--source", "conversation_text",
		"--source-since", "2025-01-01", "--yes")
	require.ErrorContains(t, err, "synthetic final check failed")
	_, statErr := os.Stat(path)
	require.ErrorIs(t, statErr, fs.ErrNotExist)
	_, credentialErr := deps.setup.credentials.Load("first")
	require.ErrorIs(t, credentialErr, peoplesweep.ErrCredentialNotFound)
	recoveries, globErr := filepath.Glob(filepath.Join(dir, ".config-retired-*"))
	require.NoError(t, globErr)
	require.Len(t, recoveries, 1)
	recovered, readErr := os.ReadFile(recoveries[0])
	require.NoError(t, readErr)
	assert.NotContains(t, string(recovered), providerSetupSecretCanary)
}

func TestPersonProviderConcurrentExactAddNeverReadsSecretOrOverwrites(t *testing.T) {
	tests := []struct {
		name       string
		auth       string
		endpoint   string
		credential []string
	}{
		{name: "stored", auth: "bearer", endpoint: "https://candidate.example.test/v1",
			credential: []string{"--api-key-stdin"}},
		{name: "environment", auth: "bearer", endpoint: "https://candidate.example.test/v1",
			credential: []string{"--credential-env", "EXACT_RACE_KEY"}},
		{name: "anonymous", auth: "none", endpoint: "http://127.0.0.1:11434/v1"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path, loaded := providerSetupConfigFile(t)
			deps := providerSetupCommandDeps(t, path, loaded, &fixedPersonProviderChecker{})
			startup := loaded.People.Sweep
			deps.config = func() peoplesweep.Config { return startup }
			concurrent := configuredPersonProvider(startup)
			concurrent.Model = "operator-raced-model"
			concurrent.Credential = peoplesweep.CredentialEnv
			concurrent.CredentialEnv = "OPERATOR_RACED_KEY"
			installed := false
			deps.readConfigFile = func() (config.ConfigFile, error) {
				if !installed {
					installed = true
					before, err := config.ReadConfigFile(path)
					if err != nil {
						return config.ConfigFile{}, err
					}
					if _, err := config.EditConfigTables(path, before.ETag, []config.TableEdit{{
						Path:   []string{"people", "sweep", "providers", "raced"},
						Values: personProviderTableValues(concurrent), InsertOnly: true,
					}}); err != nil {
						return config.ConfigFile{}, err
					}
				}
				return config.ReadConfigFile(path)
			}
			lookups := 0
			deps.setup.lookupEnv = func(string) (string, bool) {
				lookups++
				return providerSetupSecretCanary, true
			}
			negotiations := 0
			deps.setup.negotiate = func(
				context.Context, peoplesweep.ProviderConfig, peoplesweep.Credential,
			) (peoplesweep.NegotiatedCapabilities, error) {
				negotiations++
				return peoplesweep.NegotiatedCapabilities{
					OutputMode: peoplesweep.OutputModeJSONObject, TokenLimitParameter: "max_tokens",
					DriverVersion: peoplesweep.OpenAICompatibleProviderVersion,
				}, nil
			}
			input := &countingProviderReader{data: bytes.NewReader([]byte(providerSetupSecretCanary + "\n"))}
			args := []string{
				"add", "raced", "--custom", "--protocol", "openai_chat", "--endpoint", test.endpoint,
				"--model", "candidate-model", "--auth", test.auth,
				"--retention-posture", "local_only", "--training-posture", "local_only",
				"--source", "conversation_text", "--source-since", "2025-01-01", "--yes",
			}
			args = append(args, test.credential...)

			_, err := executePersonProviderCommandWithInput(t, deps, input, args...)
			require.ErrorContains(t, err, "already exists")
			assert.Zero(t, input.reads)
			assert.Zero(t, lookups)
			assert.Zero(t, negotiations)
			current, loadErr := config.Load(path, "")
			require.NoError(t, loadErr)
			assert.Equal(t, "operator-raced-model", current.People.Sweep.Providers["raced"].Model)
			_, credentialErr := deps.setup.credentials.Load("raced")
			assert.ErrorIs(t, credentialErr, peoplesweep.ErrCredentialNotFound)
		})
	}
}

func TestPersonProviderAddRefusesToOverwriteExactCredential(t *testing.T) {
	path, loaded := providerSetupConfigFile(t)
	checker := &fixedPersonProviderChecker{response: peoplesweep.StructuredResponse{
		Output: []byte(`{"ok":true}`), ProviderVersion: peoplesweep.OpenAICompatibleProviderVersion,
		ModelVersion: "occupied-model-v1",
	}}
	deps := providerSetupCommandDeps(t, path, loaded, checker)
	require.NoError(t, deps.setup.credentials.Save("occupied", peoplesweep.NewCredential(
		peoplesweep.AuthBearer, "preexisting-credential")))

	output, err := executePersonProviderCommandWithInput(t, deps,
		bytes.NewBufferString(providerSetupSecretCanary+"\n"),
		"add", "occupied", "--custom", "--protocol", "openai_chat",
		"--endpoint", "https://occupied.example.test/v1", "--model", "occupied-model",
		"--auth", "bearer", "--api-key-stdin", "--retention-posture", "zero_retention",
		"--training-posture", "no_training", "--source", "conversation_text",
		"--source-since", "2025-01-01", "--yes")
	require.ErrorContains(t, err, "already exists")
	assert.NotContains(t, err.Error(), providerSetupSecretCanary)
	credential, err := deps.setup.credentials.Load("occupied")
	require.NoError(t, err)
	assert.Equal(t, "preexisting-credential", credential.Value())
	assert.NotContains(t, output, providerSetupSecretCanary)
}
