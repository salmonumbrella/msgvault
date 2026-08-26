package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/peoplesweep"
)

func TestPeopleSweepConfigDefaultsDisabled(t *testing.T) {
	assert := assert.New(t)
	config := NewDefaultConfig().People.Sweep
	_, provider, err := config.ActiveProviderConfig()
	require.NoError(t, err)

	assert.False(config.Enabled)
	assert.Equal(peoplesweep.ProtocolOpenAIChat, provider.Protocol)
	assert.Equal("https://api.openai.com/v1", provider.Endpoint)
	assert.Equal("OPENAI_API_KEY", provider.CredentialEnv)
	assert.Equal(time.Minute, provider.RequestTimeout)
}

func TestLoadPeopleSweepProviderConfig(t *testing.T) {
	assert := assert.New(t)
	path := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(t, os.WriteFile(path, []byte(`
[people.sweep]
enabled = true

[people.sweep.provider]
kind = "openai_compatible"
endpoint = "https://api.example.test/v1/"
model = "gpt-test"
api_key_env = "TEST_KEY"
allow_anonymous = false
retention_posture = "zero_retention"
training_posture = "no_training"
allowed_sources = ["meeting_text", "conversation_text"]
source_since = "2025-01-01"
source_until = "2025-12-31"
allow_sensitive = true
request_timeout = "45s"
`), 0o600))

	loaded, err := Load(path, "")
	require.NoError(t, err)
	name, provider, err := loaded.People.Sweep.ActiveProviderConfig()
	require.NoError(t, err)
	assert.True(loaded.People.Sweep.Enabled)
	assert.Equal("default", name)
	assert.Equal(peoplesweep.ProtocolOpenAIChat, provider.Protocol)
	assert.Equal("https://api.example.test/v1/", provider.Endpoint)
	assert.Equal("gpt-test", provider.Model)
	assert.Equal("TEST_KEY", provider.CredentialEnv)
	assert.Equal(peoplesweep.AuthBearer, provider.Auth)
	assert.Equal("zero_retention", provider.RetentionPosture)
	assert.Equal("no_training", provider.TrainingPosture)
	assert.Equal([]peoplesweep.SourceClass{
		peoplesweep.SourceMeetingText,
		peoplesweep.SourceConversationText,
	}, provider.AllowedSources)
	assert.Equal("2025-01-01", provider.SourceSince)
	assert.Equal("2025-12-31", provider.SourceUntil)
	assert.True(provider.AllowSensitive)
	assert.Equal(45*time.Second, provider.RequestTimeout)
}

func TestLoadRejectsInvalidEnabledPeopleSweepProvider(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(t, os.WriteFile(path, []byte(`
[people.sweep]
enabled = true

[people.sweep.provider]
model = "gpt-test"
retention_posture = "zero_retention"
training_posture = "no_training"
allowed_sources = ["raw_image"]
source_since = "2025-01-01"
`), 0o600))

	_, err := Load(path, "")
	require.ErrorContains(t, err, "allowed_sources")
}

func TestLoadDoesNotReplaceExplicitEmptyPeopleProviderKeyEnv(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(t, os.WriteFile(path, []byte(`
[people.sweep]
enabled = true

[people.sweep.provider]
model = "gpt-test"
api_key_env = ""
retention_posture = "zero_retention"
training_posture = "no_training"
allowed_sources = ["conversation_text"]
source_since = "2025-01-01"
`), 0o600))

	_, err := Load(path, "")
	require.ErrorContains(t, err, "credential_env")
}

func TestLoadAllowsAnonymousLoopbackPeopleProviderWithoutKeyEnv(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	path := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(os.WriteFile(path, []byte(`
[people.sweep]
enabled = true

[people.sweep.provider]
endpoint = "http://127.0.0.1:11434/v1"
model = "local-model"
allow_anonymous = true
retention_posture = "local_only"
training_posture = "local_only"
allowed_sources = ["conversation_text"]
source_since = "2025-01-01"
`), 0o600))

	loaded, err := Load(path, "")
	require.NoError(err)
	_, provider, err := loaded.People.Sweep.ActiveProviderConfig()
	require.NoError(err)
	assert.Equal(peoplesweep.AuthNone, provider.Auth)
	assert.Empty(provider.CredentialEnv)
}

func TestLoadCodexPeopleProviderUsesCodexOnlyDefaults(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	path := filepath.Join(t.TempDir(), "config.toml")
	requirements.NoError(os.WriteFile(path, []byte(`
[people.sweep]
enabled = true

[people.sweep.provider]
kind = "codex_app_server"
model = "gpt-test"
reasoning_effort = "high"
retention_posture = "zero_retention"
training_posture = "no_training"
allowed_sources = ["conversation_text"]
source_since = "2025-01-01"
`), 0o600))

	loaded, err := Load(path, "")
	requirements.NoError(err)
	_, provider, err := loaded.People.Sweep.ActiveProviderConfig()
	requirements.NoError(err)
	checks.Equal(peoplesweep.ProtocolCodexAppServer, provider.Protocol)
	checks.Empty(provider.Endpoint)
	checks.Empty(provider.CredentialEnv)
	checks.Equal("codex", provider.Executable)
	checks.Equal(peoplesweep.CodexExecutionBoundaryV1, provider.ExecutionBoundary)
}

func TestLoadRejectsAnonymousPeopleProviderWithExplicitKeyEnv(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	path := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(os.WriteFile(path, []byte(`
[people.sweep]
enabled = true

[people.sweep.provider]
endpoint = "http://127.0.0.1:11434/v1"
model = "local-model"
api_key_env = "LOCAL_KEY"
allow_anonymous = true
retention_posture = "local_only"
training_posture = "local_only"
allowed_sources = ["conversation_text"]
source_since = "2025-01-01"
`), 0o600))

	_, err := Load(path, "")
	assert.ErrorContains(err, "anonymous mode cannot also configure api_key_env")
}

func TestConfigLoadsNamedProviderProfiles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(t, os.WriteFile(path, []byte(`
[people.sweep]
enabled = true
provider = "glm"

[people.sweep.providers.glm]
protocol = "openai_chat"
endpoint = "https://api.z.ai/api/paas/v4"
model = "glm-5.3"
auth = "bearer"
credential = "env"
credential_env = "ZAI_API_KEY"
output_mode = "json_object"
token_limit_parameter = "max_tokens"
reasoning_effort = "max"
retention_posture = "provider-declared"
training_posture = "provider-declared"
allowed_sources = ["conversation_text"]
source_since = "2026-01-01"
`), 0o600))

	loaded, err := Load(path, "")
	require.NoError(t, err)
	name, provider, err := loaded.People.Sweep.ActiveProviderConfig()
	require.NoError(t, err)
	assert.Equal(t, "glm", name)
	assert.Equal(t, peoplesweep.ProtocolOpenAIChat, provider.Protocol)
	assert.Equal(t, "https://api.z.ai/api/paas/v4", provider.Endpoint)
	assert.Equal(t, "glm-5.3", provider.Model)
	assert.Equal(t, peoplesweep.AuthBearer, provider.Auth)
	assert.Equal(t, peoplesweep.CredentialEnv, provider.Credential)
	assert.Equal(t, "ZAI_API_KEY", provider.CredentialEnv)
	assert.Equal(t, peoplesweep.OutputModeJSONObject, provider.OutputMode)
	assert.Equal(t, "max_tokens", provider.TokenLimitParameter)
	assert.Equal(t, "max", provider.ReasoningEffort)
}

func TestConfigMigratesLegacyProviderTable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(t, os.WriteFile(path, []byte(`
[people.sweep]
enabled = true

[people.sweep.provider]
kind = "openai_compatible"
endpoint = "https://api.example.test/v1"
model = "gpt-test"
api_key_env = "TEST_KEY"
retention_posture = "zero_retention"
training_posture = "no_training"
allowed_sources = ["conversation_text"]
source_since = "2025-01-01"
`), 0o600))

	loaded, err := Load(path, "")
	require.NoError(t, err)
	name, provider, err := loaded.People.Sweep.ActiveProviderConfig()
	require.NoError(t, err)
	assert.Equal(t, "default", name)
	assert.Equal(t, "default", loaded.People.Sweep.Provider.Name)
	assert.Len(t, loaded.People.Sweep.Providers, 1)
	assert.Equal(t, peoplesweep.ProtocolOpenAIChat, provider.Protocol)
	assert.Equal(t, peoplesweep.AuthBearer, provider.Auth)
	assert.Equal(t, peoplesweep.CredentialEnv, provider.Credential)
	assert.Equal(t, "TEST_KEY", provider.CredentialEnv)
	assert.Equal(t, peoplesweep.OutputModeNativeJSONSchema, provider.OutputMode)
	assert.Equal(t, "max_completion_tokens", provider.TokenLimitParameter)
}

func TestConfigRejectsMixedProviderShapes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(t, os.WriteFile(path, []byte(`
[people.sweep.provider]
kind = "openai_compatible"

[people.sweep.providers.glm]
protocol = "openai_chat"
`), 0o600))

	_, err := Load(path, "")
	require.ErrorContains(t, err, "legacy")
}

func TestConfigRejectsMissingActiveProvider(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(t, os.WriteFile(path, []byte(`
[people.sweep]
provider = "missing"

[people.sweep.providers.glm]
protocol = "openai_chat"
`), 0o600))

	_, err := Load(path, "")
	require.ErrorContains(t, err, "missing")
}
