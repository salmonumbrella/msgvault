package cmd

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

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
	deps.restoreConfigFile = func(etag string, before config.ConfigFile) (config.ConfigFile, error) {
		return config.RestoreConfigFile(path, etag, before)
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
		deps.setup.isTerminal = func(uintptr) bool { return true }
		deps.setup.readMasked = func(uintptr) ([]byte, error) {
			return []byte(providerSetupSecretCanary), nil
		}
		input, err := os.Open(os.DevNull)
		require.NoError(t, err)
		defer func() { _ = input.Close() }()

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
	deps.restoreConfigFile = func(etag string, before config.ConfigFile) (config.ConfigFile, error) {
		return config.RestoreConfigFile(path, etag, before)
	}
	deps.setup.credentials = credentialStore

	output, err := executePersonProviderCommand(t, deps, "remove", "old")
	require.NoError(t, err)
	assert.Contains(t, output, "old")
	content, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.NotContains(t, string(content), "[people.sweep.providers.old]")
	_, err = credentialStore.Load("old")
	assert.ErrorIs(t, err, peoplesweep.ErrCredentialNotFound)
	sibling, err := credentialStore.Load("sibling")
	require.NoError(t, err)
	assert.Equal(t, "sibling-secret", sibling.Value())
	active, err := st.HasActivePersonInferenceConsent(t.Context(), profile.Fingerprint)
	require.NoError(t, err)
	assert.False(t, active)
	profiles, err := st.ListPersonInferenceProfiles(t.Context())
	require.NoError(t, err)
	assert.Len(t, profiles, 1, "immutable audit profile must remain")

	enabled := loaded.People.Sweep
	enabled.Enabled = true
	activeDeps := localPersonProviderDeps(enabled, st, nil)
	_, err = executePersonProviderCommand(t, activeDeps, "remove", "default")
	require.ErrorContains(t, err, "active")
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
	assert.ErrorIs(t, err, peoplesweep.ErrCredentialNotFound)
	sibling, err := credentialStore.Load("sibling")
	require.NoError(t, err)
	assert.Equal(t, "sibling-secret", sibling.Value())
	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, fs.FileMode(0o640), info.Mode().Perm())
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
