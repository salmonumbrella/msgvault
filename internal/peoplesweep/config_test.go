package peoplesweep_test

import (
	"bytes"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/peoplesweep"
)

func validConfig() peoplesweep.Config {
	config := peoplesweep.Config{
		Enabled:  true,
		Provider: peoplesweep.ProviderSelection{Name: "default"},
		Providers: map[string]peoplesweep.ProviderConfig{"default": {
			Protocol:            peoplesweep.ProtocolOpenAIChat,
			Endpoint:            "https://api.example.test/v1/",
			Model:               "gpt-test",
			Auth:                peoplesweep.AuthBearer,
			Credential:          peoplesweep.CredentialEnv,
			CredentialEnv:       "TEST_KEY",
			OutputMode:          peoplesweep.OutputModeNativeJSONSchema,
			TokenLimitParameter: "max_completion_tokens",
			RetentionPosture:    "zero_retention",
			TrainingPosture:     "no_training",
			AllowedSources: []peoplesweep.SourceClass{
				peoplesweep.SourceMeetingText,
				peoplesweep.SourceConversationText,
			},
			SourceSince:    "2025-01-01",
			SourceUntil:    "2025-12-31",
			RequestTimeout: 45 * time.Second,
		}},
	}
	config.ApplyDefaults()
	return config
}

func activeProvider(config peoplesweep.Config) peoplesweep.ProviderConfig {
	return config.Providers[config.Provider.Name]
}

func setActiveProvider(config *peoplesweep.Config, provider peoplesweep.ProviderConfig) {
	providers := make(map[string]peoplesweep.ProviderConfig, len(config.Providers))
	maps.Copy(providers, config.Providers)
	providers[config.Provider.Name] = provider
	config.Providers = providers
}

func mutateActiveProvider(config *peoplesweep.Config, mutate func(*peoplesweep.ProviderConfig)) {
	provider := activeProvider(*config)
	provider.AllowedSources = slices.Clone(provider.AllowedSources)
	mutate(&provider)
	setActiveProvider(config, provider)
}

func cloneConfig(config peoplesweep.Config) peoplesweep.Config {
	mutateActiveProvider(&config, func(*peoplesweep.ProviderConfig) {})
	return config
}

func providerMutation(mutate func(*peoplesweep.ProviderConfig)) func(*peoplesweep.Config) {
	return func(config *peoplesweep.Config) { mutateActiveProvider(config, mutate) }
}

func configWithProvider(provider peoplesweep.ProviderConfig) peoplesweep.Config {
	config := peoplesweep.Config{
		Enabled: true, Provider: peoplesweep.ProviderSelection{Name: "default"},
		Providers: map[string]peoplesweep.ProviderConfig{"default": provider},
	}
	config.ApplyDefaults()
	return config
}

func TestConfigDefaultsStayDisabled(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	var config peoplesweep.Config
	config.ApplyDefaults()
	provider := activeProvider(config)

	assert.False(config.Enabled)
	assert.Equal(peoplesweep.ProtocolOpenAIChat, provider.Protocol)
	assert.Equal("https://api.openai.com/v1", provider.Endpoint)
	assert.Equal("OPENAI_API_KEY", provider.CredentialEnv)
	assert.Equal(time.Minute, provider.RequestTimeout)
	require.NoError(config.Validate())
	_, err := config.Profile()
	require.ErrorContains(err, "disabled")
}

func TestConfigRejectsUnsafeActiveAndInactiveProviderProfileNames(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*peoplesweep.Config)
	}{
		{name: "active", mutate: func(config *peoplesweep.Config) {
			provider := config.Providers[config.Provider.Name]
			delete(config.Providers, config.Provider.Name)
			config.Provider.Name = "bad\nname"
			config.Providers[config.Provider.Name] = provider
		}},
		{name: "inactive", mutate: func(config *peoplesweep.Config) {
			config.Providers["--help"] = config.Providers[config.Provider.Name]
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			configured := validConfig()
			test.mutate(&configured)

			err := configured.Validate()
			require.ErrorContains(t, err, "invalid people provider profile name")
			assert.NotContains(t, err.Error(), "bad\nname")
			assert.NotContains(t, err.Error(), "--help")
		})
	}
}

func TestCodexProviderFingerprintIncludesExecutionBoundaryAndEffort(t *testing.T) {
	base := validConfig()
	setActiveProvider(&base, peoplesweep.ProviderConfig{
		Protocol:          peoplesweep.ProtocolCodexAppServer,
		Auth:              peoplesweep.AuthNone,
		Credential:        peoplesweep.CredentialNone,
		OutputMode:        peoplesweep.OutputModeNativeJSONSchema,
		Model:             "gpt-test",
		RetentionPosture:  "zero_retention",
		TrainingPosture:   "no_training",
		AllowedSources:    []peoplesweep.SourceClass{peoplesweep.SourceConversationText},
		SourceSince:       "2025-01-01",
		ReasoningEffort:   "high",
		ExecutionBoundary: peoplesweep.CodexExecutionBoundaryV1,
	})
	base.ApplyDefaults()
	want, err := base.Profile()
	require.NoError(t, err)

	for _, mutation := range []struct {
		name   string
		mutate func(*peoplesweep.Config)
	}{
		{"effort", providerMutation(func(p *peoplesweep.ProviderConfig) { p.ReasoningEffort = "medium" })},
		{"boundary", providerMutation(func(p *peoplesweep.ProviderConfig) { p.ExecutionBoundary = "different-boundary" })},
	} {
		t.Run(mutation.name, func(t *testing.T) {
			checks := assert.New(t)
			requirements := require.New(t)
			gotConfig := cloneConfig(base)
			mutation.mutate(&gotConfig)
			got, profileErr := gotConfig.Profile()
			if mutation.name == "boundary" {
				requirements.ErrorContains(profileErr, "execution_boundary")
				return
			}
			requirements.NoError(profileErr)
			checks.NotEqual(want.Fingerprint, got.Fingerprint)
			checks.False(bytes.Equal(want.PolicyJSON, got.PolicyJSON))
		})
	}
}

// TestCodexProviderExecutableCannotSelfAttest catches a configured executable
// path being incorporated as if it were a released binary identity.
func TestCodexProviderExecutableCannotSelfAttest(t *testing.T) {
	base := validConfig()
	setActiveProvider(&base, peoplesweep.ProviderConfig{
		Protocol: peoplesweep.ProtocolCodexAppServer, Model: "gpt-test", ReasoningEffort: "high",
		Auth: peoplesweep.AuthNone, Credential: peoplesweep.CredentialNone,
		OutputMode:       peoplesweep.OutputModeNativeJSONSchema,
		RetentionPosture: "zero_retention", TrainingPosture: "no_training",
		AllowedSources: []peoplesweep.SourceClass{peoplesweep.SourceConversationText},
		SourceSince:    "2025-01-01", Executable: "/synthetic/path/codex-one",
		ExecutionBoundary: peoplesweep.CodexExecutionBoundaryV1,
	})
	base.ApplyDefaults()
	want, err := base.Profile()
	require.NoError(t, err)

	mutateActiveProvider(&base, func(p *peoplesweep.ProviderConfig) { p.Executable = "/synthetic/path/codex-two" })
	got, err := base.Profile()
	require.NoError(t, err)
	assert.Equal(t, want, got)
}

func TestPositiveCostBudgetRequiresPrices(t *testing.T) {
	config := validConfig()
	config.Budgets.MaxEstimatedCostMicroUSDPerDay = 1
	config.Budgets.InputCostMicroUSDPerMillionTokens = 0
	config.Budgets.OutputCostMicroUSDPerMillionTokens = 0

	assert.ErrorContains(t, config.Validate(), "cost prices are required")
}

func TestOpenAIProviderProfileOperationalFieldsExcluded(t *testing.T) {
	base := validConfig()
	want, err := base.Profile()
	require.NoError(t, err)

	for _, mutation := range []struct {
		name   string
		mutate func(*peoplesweep.Config)
	}{
		{"timeout", providerMutation(func(p *peoplesweep.ProviderConfig) { p.RequestTimeout = 2 * time.Minute })},
		{"executable", providerMutation(func(p *peoplesweep.ProviderConfig) { p.Executable = "other-codex" })},
		{"schedule", func(c *peoplesweep.Config) { c.Schedule = "0 3 * * *" }},
		{"lease", func(c *peoplesweep.Config) { c.LeaseDuration = 30 * time.Minute }},
		{"retry", func(c *peoplesweep.Config) { c.RetryBase = 2 * time.Minute }},
	} {
		t.Run(mutation.name, func(t *testing.T) {
			gotConfig := cloneConfig(base)
			mutation.mutate(&gotConfig)
			got, profileErr := gotConfig.Profile()
			require.NoError(t, profileErr)
			assert.Equal(t, want, got)
		})
	}
}

func TestProviderFingerprintIncludesEgressPolicy(t *testing.T) {
	base := validConfig()
	want, err := base.Profile()
	require.NoError(t, err)

	for _, mutation := range []struct {
		name   string
		mutate func(*peoplesweep.Config)
	}{
		{"endpoint", providerMutation(func(p *peoplesweep.ProviderConfig) { p.Endpoint = "https://other.example.test/v1" })},
		{"model", providerMutation(func(p *peoplesweep.ProviderConfig) { p.Model = "other-model" })},
		{"key environment", providerMutation(func(p *peoplesweep.ProviderConfig) { p.CredentialEnv = "OTHER_KEY" })},
		{"retention", providerMutation(func(p *peoplesweep.ProviderConfig) { p.RetentionPosture = "provider-policy" })},
		{"training", providerMutation(func(p *peoplesweep.ProviderConfig) { p.TrainingPosture = "provider-policy" })},
		{"sources", providerMutation(func(p *peoplesweep.ProviderConfig) {
			p.AllowedSources = []peoplesweep.SourceClass{peoplesweep.SourceDocumentText}
		})},
		{"source since", providerMutation(func(p *peoplesweep.ProviderConfig) { p.SourceSince = "2024-01-01" })},
		{"source until", providerMutation(func(p *peoplesweep.ProviderConfig) { p.SourceUntil = "2026-01-01" })},
		{"sensitive", providerMutation(func(p *peoplesweep.ProviderConfig) { p.AllowSensitive = true })},
	} {
		t.Run(mutation.name, func(t *testing.T) {
			gotConfig := cloneConfig(base)
			mutation.mutate(&gotConfig)
			got, profileErr := gotConfig.Profile()
			require.NoError(t, profileErr)
			assert.NotEqual(t, want.Fingerprint, got.Fingerprint)
		})
	}
}

func TestProviderFingerprintIncludesProtocolCapabilities(t *testing.T) {
	base := peoplesweep.Config{
		Enabled:  true,
		Provider: peoplesweep.ProviderSelection{Name: "glm"},
		Providers: map[string]peoplesweep.ProviderConfig{
			"glm": {
				Protocol: peoplesweep.ProtocolOpenAIChat, Endpoint: "https://api.example.test/v1",
				Model: "gpt-test", Auth: peoplesweep.AuthBearer,
				Credential: peoplesweep.CredentialEnv, CredentialEnv: "TEST_KEY",
				OutputMode: peoplesweep.OutputModeJSONObject, TokenLimitParameter: "max_tokens",
				ReasoningEffort: "high", DriverVersion: "openai-chat-v1",
				RetentionPosture: "zero_retention", TrainingPosture: "no_training",
				AllowedSources: []peoplesweep.SourceClass{peoplesweep.SourceConversationText},
				SourceSince:    "2025-01-01", RequestTimeout: time.Minute,
			},
		},
	}
	base.ApplyDefaults()
	encodedSelection, err := base.Provider.MarshalTOML()
	require.NoError(t, err)
	assert.Equal(t, `"glm"`, string(encodedSelection))
	want, err := base.Profile()
	require.NoError(t, err)

	for _, mutation := range []struct {
		name   string
		mutate func(*peoplesweep.ProviderConfig)
	}{
		{"protocol", func(p *peoplesweep.ProviderConfig) {
			p.Protocol = peoplesweep.ProtocolOpenAIResponses
			p.TokenLimitParameter = ""
		}},
		{"endpoint", func(p *peoplesweep.ProviderConfig) { p.Endpoint = "https://other.example.test/v1" }},
		{"model", func(p *peoplesweep.ProviderConfig) { p.Model = "other-model" }},
		{"auth", func(p *peoplesweep.ProviderConfig) { p.Auth = peoplesweep.AuthXAPIKey }},
		{"credential source", func(p *peoplesweep.ProviderConfig) { p.Credential = peoplesweep.CredentialStored; p.CredentialEnv = "" }},
		{"credential reference", func(p *peoplesweep.ProviderConfig) { p.CredentialEnv = "OTHER_KEY" }},
		{"output mode", func(p *peoplesweep.ProviderConfig) { p.OutputMode = peoplesweep.OutputModePromptJSON }},
		{"token parameter", func(p *peoplesweep.ProviderConfig) { p.TokenLimitParameter = "max_completion_tokens" }},
		{"reasoning effort", func(p *peoplesweep.ProviderConfig) { p.ReasoningEffort = "medium" }},
		{"reasoning mode", func(p *peoplesweep.ProviderConfig) { p.ReasoningMode = "enabled" }},
		{"driver version", func(p *peoplesweep.ProviderConfig) { p.DriverVersion = "openai-chat-v2" }},
		{"privacy", func(p *peoplesweep.ProviderConfig) { p.AllowSensitive = true }},
	} {
		t.Run(mutation.name, func(t *testing.T) {
			changed := base
			provider := changed.Providers["glm"]
			provider.AllowedSources = slices.Clone(provider.AllowedSources)
			mutation.mutate(&provider)
			changed.Providers = map[string]peoplesweep.ProviderConfig{"glm": provider}
			got, profileErr := changed.Profile()
			require.NoError(t, profileErr)
			assert.NotEqual(t, want.Fingerprint, got.Fingerprint)
		})
	}

	t.Run("operational values", func(t *testing.T) {
		changed := base
		provider := changed.Providers["glm"]
		provider.RequestTimeout = 2 * time.Minute
		changed.Providers = map[string]peoplesweep.ProviderConfig{"glm": provider}
		got, profileErr := changed.Profile()
		require.NoError(t, profileErr)
		assert.Equal(t, want.Fingerprint, got.Fingerprint)
	})

	assert.Contains(t, string(want.PolicyJSON), peoplesweep.PacketRendererPolicyV1)
	assert.Contains(t, string(want.PolicyJSON), peoplesweep.ProgramFingerprint())
	mutated := want
	mutated.PacketRendererPolicy = "other-renderer"
	assert.Error(t, mutated.Validate())
	mutated = want
	mutated.ProgramFingerprint = "other-program"
	assert.Error(t, mutated.Validate())
}

func TestPeopleSweepDefaults(t *testing.T) {
	checks := assert.New(t)
	var config peoplesweep.Config
	config.ApplyDefaults()

	checks.Equal("15 2 * * *", config.Schedule)
	checks.Equal(25, config.WorkBatchSize)
	checks.Equal(256, config.ChangeBatchSize)
	checks.Equal(2_000, config.HistoricalMessageCap)
	checks.Equal(8, config.ContextPerTarget)
	checks.Equal(131_072, config.EvidenceMaxBytes)
	checks.Equal(200, config.EvidenceMaxItems)
	checks.Equal(15*time.Minute, config.LeaseDuration)
	checks.Equal(24*time.Hour, config.BackstopInterval)
	checks.Equal(time.Minute, config.RetryBase)
	checks.Equal(6*time.Hour, config.RetryMax)
	checks.Equal(peoplesweep.BudgetConfig{
		MaxRequestsPerPerson: 4, MaxInputTokensPerPerson: 200_000, MaxOutputTokensPerPerson: 16_000,
		MaxRequestsPerRun: 100, MaxInputTokensPerRun: 1_000_000, MaxOutputTokensPerRun: 160_000,
		MaxRequestsPerDay: 500, MaxInputTokensPerDay: 5_000_000, MaxOutputTokensPerDay: 800_000,
	}, config.Budgets)
}

func TestProviderProfileHasStableCanonicalPolicy(t *testing.T) {
	assert := assert.New(t)
	profile, err := validConfig().Profile()
	require.NoError(t, err)

	assert.Equal("https://api.example.test/v1", profile.Endpoint)
	assert.Equal([]peoplesweep.SourceClass{
		peoplesweep.SourceConversationText,
		peoplesweep.SourceMeetingText,
	}, profile.AllowedSources)
	assert.JSONEq(strings.ReplaceAll(`{
		"protocol":"openai_chat",
		"endpoint":"https://api.example.test/v1",
		"model":"gpt-test",
		"auth":"bearer",
		"credential":"env",
		"credential_ref":"TEST_KEY",
		"output_mode":"native_json_schema",
		"token_limit_parameter":"max_completion_tokens",
		"reasoning_effort":"",
		"reasoning_mode":"",
		"driver_version":"openai-chat-completions-json-schema-v1",
		"retention_posture":"zero_retention",
		"training_posture":"no_training",
		"allowed_sources":["conversation_text","meeting_text"],
		"source_since":"2025-01-01",
		"source_until":"2025-12-31",
		"allow_sensitive":false,
		"execution_boundary":"",
		"packet_renderer_policy":"person-sweep-packet-v1",
		"program_fingerprint":"PROGRAM_FINGERPRINT",
		"disclosed_packet_fields":["person_id","program_identity","catalog","current_projection","unresolved_claims","seed_evidence","retrieved_context"]
	}`, "PROGRAM_FINGERPRINT", peoplesweep.ProgramFingerprint()), string(profile.PolicyJSON))
	assert.Contains(string(profile.PolicyJSON), peoplesweep.ProgramFingerprint())
	assert.NoError(profile.Validate())
}

func TestProviderProfileFingerprintCoversConsentPolicy(t *testing.T) {
	base := validConfig()
	want, err := base.Profile()
	require.NoError(t, err)

	mutations := []struct {
		name   string
		mutate func(*peoplesweep.Config)
	}{
		{"protocol", providerMutation(func(p *peoplesweep.ProviderConfig) { p.Protocol = "other" })},
		{"endpoint", providerMutation(func(p *peoplesweep.ProviderConfig) { p.Endpoint = "https://other.example.test/v1" })},
		{"model", providerMutation(func(p *peoplesweep.ProviderConfig) { p.Model = "another-model" })},
		{"key env", providerMutation(func(p *peoplesweep.ProviderConfig) { p.CredentialEnv = "OTHER_API_KEY" })},
		{"anonymous", providerMutation(func(p *peoplesweep.ProviderConfig) {
			p.Endpoint = "http://127.0.0.1:11434/v1"
			p.Auth = peoplesweep.AuthNone
			p.Credential = peoplesweep.CredentialNone
			p.CredentialEnv = ""
		})},
		{"retention", providerMutation(func(p *peoplesweep.ProviderConfig) { p.RetentionPosture = "provider_policy" })},
		{"training", providerMutation(func(p *peoplesweep.ProviderConfig) { p.TrainingPosture = "provider_policy" })},
		{"sources", providerMutation(func(p *peoplesweep.ProviderConfig) {
			p.AllowedSources = append(p.AllowedSources, peoplesweep.SourceDocumentText)
		})},
		{"source since", providerMutation(func(p *peoplesweep.ProviderConfig) { p.SourceSince = "2024-01-01" })},
		{"source until", providerMutation(func(p *peoplesweep.ProviderConfig) { p.SourceUntil = "2026-01-01" })},
		{"sensitive", providerMutation(func(p *peoplesweep.ProviderConfig) { p.AllowSensitive = true })},
	}
	for _, mutation := range mutations {
		t.Run(mutation.name, func(t *testing.T) {
			gotConfig := cloneConfig(base)
			mutation.mutate(&gotConfig)
			got, profileErr := gotConfig.Profile()
			if mutation.name == "protocol" {
				require.Error(t, profileErr)
				return
			}
			require.NoError(t, profileErr)
			assert.NotEqual(t, want.Fingerprint, got.Fingerprint)
		})
	}

	t.Run("source order is canonical", func(t *testing.T) {
		reordered := cloneConfig(base)
		mutateActiveProvider(&reordered, func(p *peoplesweep.ProviderConfig) {
			p.AllowedSources = []peoplesweep.SourceClass{
				peoplesweep.SourceConversationText,
				peoplesweep.SourceMeetingText,
			}
		})
		got, profileErr := reordered.Profile()
		require.NoError(t, profileErr)
		assert.Equal(t, want.Fingerprint, got.Fingerprint)
	})

	t.Run("timeout is operational", func(t *testing.T) {
		changed := cloneConfig(base)
		mutateActiveProvider(&changed, func(p *peoplesweep.ProviderConfig) { p.RequestTimeout = 2 * time.Minute })
		got, profileErr := changed.Profile()
		require.NoError(t, profileErr)
		assert.Equal(t, want.Fingerprint, got.Fingerprint)
	})
}

func TestProviderProfileValidateRejectsTampering(t *testing.T) {
	profile, err := validConfig().Profile()
	require.NoError(t, err)

	changedField := profile
	changedField.Model = "changed"
	require.ErrorContains(t, changedField.Validate(), "fingerprint")

	changedPolicy := profile
	changedPolicy.PolicyJSON = []byte(`{"different":true}`)
	require.ErrorContains(t, changedPolicy.Validate(), "policy")
}

func TestProviderProfileValidateRejectsNonCanonicalPublicFields(t *testing.T) {
	profile, err := validConfig().Profile()
	require.NoError(t, err)

	mutations := []struct {
		name   string
		mutate func(*peoplesweep.ProviderProfile)
	}{
		{"endpoint slash", func(p *peoplesweep.ProviderProfile) { p.Endpoint += "/" }},
		{"model whitespace", func(p *peoplesweep.ProviderProfile) { p.Model = " " + p.Model }},
		{"retention whitespace", func(p *peoplesweep.ProviderProfile) { p.RetentionPosture += " " }},
		{"training whitespace", func(p *peoplesweep.ProviderProfile) { p.TrainingPosture += " " }},
		{"source order", func(p *peoplesweep.ProviderProfile) {
			p.AllowedSources = []peoplesweep.SourceClass{
				peoplesweep.SourceMeetingText,
				peoplesweep.SourceConversationText,
			}
		}},
	}
	for _, mutation := range mutations {
		t.Run(mutation.name, func(t *testing.T) {
			changed := profile
			changed.AllowedSources = slices.Clone(profile.AllowedSources)
			mutation.mutate(&changed)
			assert.ErrorContains(t, changed.Validate(), "canonical")
		})
	}
}

func TestConfigValidationRejectsUnsafeOrAmbiguousPolicies(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*peoplesweep.Config)
		want   string
	}{
		{"missing model", providerMutation(func(p *peoplesweep.ProviderConfig) { p.Model = "" }), "model"},
		{"unsupported protocol", providerMutation(func(p *peoplesweep.ProviderConfig) { p.Protocol = "other" }), "protocol"},
		{"remote http", providerMutation(func(p *peoplesweep.ProviderConfig) { p.Endpoint = "http://api.example.test/v1" }), "HTTPS"},
		{"URL credentials", providerMutation(func(p *peoplesweep.ProviderConfig) { p.Endpoint = "https://user:pass@api.example.test/v1" }), "credentials"},
		{"URL query", providerMutation(func(p *peoplesweep.ProviderConfig) { p.Endpoint = "https://api.example.test/v1?x=1" }), "query"},
		{"URL fragment", providerMutation(func(p *peoplesweep.ProviderConfig) { p.Endpoint = "https://api.example.test/v1#x" }), "fragment"},
		{"invalid key env", providerMutation(func(p *peoplesweep.ProviderConfig) { p.CredentialEnv = "bad-name" }), "credential_env"},
		{"missing credential", providerMutation(func(p *peoplesweep.ProviderConfig) { p.CredentialEnv = "" }), "credential_env"},
		{"anonymous remote", providerMutation(func(p *peoplesweep.ProviderConfig) {
			p.CredentialEnv = ""
			p.Auth = peoplesweep.AuthNone
			p.Credential = peoplesweep.CredentialNone
		}), "loopback"},
		{"missing retention", providerMutation(func(p *peoplesweep.ProviderConfig) { p.RetentionPosture = "" }), "retention"},
		{"unknown retention", providerMutation(func(p *peoplesweep.ProviderConfig) { p.RetentionPosture = "unknown" }), "retention"},
		{"missing training", providerMutation(func(p *peoplesweep.ProviderConfig) { p.TrainingPosture = "" }), "training"},
		{"unknown training", providerMutation(func(p *peoplesweep.ProviderConfig) { p.TrainingPosture = "unknown" }), "training"},
		{"missing sources", providerMutation(func(p *peoplesweep.ProviderConfig) { p.AllowedSources = nil }), "allowed_sources"},
		{"unknown source", providerMutation(func(p *peoplesweep.ProviderConfig) {
			p.AllowedSources = []peoplesweep.SourceClass{"raw_image"}
		}), "allowed_sources"},
		{"attachment caption without hydration", providerMutation(func(p *peoplesweep.ProviderConfig) {
			p.AllowedSources = []peoplesweep.SourceClass{peoplesweep.SourceAttachmentCaption}
		}), "not yet supported"},
		{"attachment OCR without hydration", providerMutation(func(p *peoplesweep.ProviderConfig) {
			p.AllowedSources = []peoplesweep.SourceClass{peoplesweep.SourceAttachmentOCR}
		}), "not yet supported"},
		{"duplicate source", providerMutation(func(p *peoplesweep.ProviderConfig) {
			p.AllowedSources = []peoplesweep.SourceClass{
				peoplesweep.SourceConversationText,
				peoplesweep.SourceConversationText,
			}
		}), "duplicate"},
		{"missing start", providerMutation(func(p *peoplesweep.ProviderConfig) { p.SourceSince = "" }), "source_since"},
		{"invalid start", providerMutation(func(p *peoplesweep.ProviderConfig) { p.SourceSince = "2025-02-30" }), "source_since"},
		{"invalid end", providerMutation(func(p *peoplesweep.ProviderConfig) { p.SourceUntil = "tomorrow" }), "source_until"},
		{"reversed dates", providerMutation(func(p *peoplesweep.ProviderConfig) { p.SourceUntil = "2024-12-31" }), "before"},
		{"zero timeout", providerMutation(func(p *peoplesweep.ProviderConfig) { p.RequestTimeout = 0 }), "request_timeout"},
		{"negative timeout", providerMutation(func(p *peoplesweep.ProviderConfig) { p.RequestTimeout = -time.Second }), "request_timeout"},
		{"output budget below one request", func(c *peoplesweep.Config) {
			c.Budgets.MaxOutputTokensPerPerson = 4095
		}, "at least 4096"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := validConfig()
			test.mutate(&config)
			_, err := config.Profile()
			assert.ErrorContains(t, err, test.want)
		})
	}
}

func TestConfigAllowsExplicitAnonymousLoopback(t *testing.T) {
	for _, endpoint := range []string{
		"http://127.0.0.1:11434/v1",
		"http://[::1]:11434/v1",
		"http://localhost:11434/v1",
	} {
		t.Run(endpoint, func(t *testing.T) {
			config := validConfig()
			mutateActiveProvider(&config, func(p *peoplesweep.ProviderConfig) {
				p.Endpoint = endpoint
				p.Auth = peoplesweep.AuthNone
				p.Credential = peoplesweep.CredentialNone
				p.CredentialEnv = ""
			})

			profile, err := config.Profile()
			require.NoError(t, err)
			assert.Equal(t, peoplesweep.AuthNone, profile.Auth)
			assert.Empty(t, profile.CredentialRef)
		})
	}
}

func TestConfigAllowsAuthenticatedLoopbackHTTP(t *testing.T) {
	config := validConfig()
	mutateActiveProvider(&config, func(p *peoplesweep.ProviderConfig) {
		p.Endpoint = "http://127.0.0.1:11434/v1"
	})

	assert.NoError(t, config.Validate())
}
