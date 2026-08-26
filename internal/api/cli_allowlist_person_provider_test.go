package api

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/peoplesweep"
)

func configureAPIProvider(cfg *config.Config, provider peoplesweep.ProviderConfig) {
	cfg.People.Sweep.Provider = peoplesweep.ProviderSelection{Name: "default"}
	cfg.People.Sweep.Providers = map[string]peoplesweep.ProviderConfig{"default": provider}
}

func completeAPIProvider(keyEnv, model string) peoplesweep.ProviderConfig {
	return peoplesweep.ProviderConfig{
		Protocol: peoplesweep.ProtocolOpenAIChat, Endpoint: "https://provider.example/v1",
		Model: model, Auth: peoplesweep.AuthBearer,
		Credential: peoplesweep.CredentialEnv, CredentialEnv: keyEnv,
		OutputMode:          peoplesweep.OutputModeNativeJSONSchema,
		TokenLimitParameter: "max_completion_tokens",
		RetentionPosture:    "zero_data_retention", TrainingPosture: "no_training",
		AllowedSources: []peoplesweep.SourceClass{peoplesweep.SourceMeetingText},
		SourceSince:    "2025-01-01", RequestTimeout: time.Second,
	}
}

func TestCLIRunCommandAllowedPermitsExactPersonProviderCommands(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want bool
	}{
		{name: "status", args: []string{"person", "provider", "status"}, want: true},
		{name: "status flags", args: []string{"person", "provider", "status", "--json"}, want: true},
		{name: "named status", args: []string{"person", "provider", "status", "alpha", "--json"}, want: true},
		{name: "list", args: []string{"person", "provider", "list", "--json"}, want: true},
		{name: "consent", args: []string{"person", "provider", "consent", "--yes"}, want: true},
		{name: "revoke", args: []string{"person", "provider", "revoke"}, want: true},
		{name: "check", args: []string{"person", "provider", "check", "--json"}, want: true},
		{name: "named check", args: []string{"person", "provider", "check", "alpha", "--json"}, want: true},
		{name: "history", args: []string{"person", "provider", "history", "alpha", "--limit", "20"}, want: true},
		{name: "add is local", args: []string{"person", "provider", "add", "alpha"}},
		{name: "use is local", args: []string{"person", "provider", "use", "alpha"}},
		{name: "remove is local", args: []string{"person", "provider", "remove", "alpha"}},
		{name: "login is local", args: []string{"person", "provider", "login"}},
		{name: "models are local", args: []string{"person", "provider", "models"}},
		{name: "secret flag smuggling", args: []string{"person", "provider", "check", "--api-key=secret-canary"}},
		{name: "extra positional smuggling", args: []string{"person", "provider", "check", "alpha", "beta"}},
		{name: "list positional smuggling", args: []string{"person", "provider", "list", "alpha"}},
		{name: "missing operation", args: []string{"person", "provider"}},
		{name: "unknown operation", args: []string{"person", "provider", "run"}},
		{name: "ordinary person mutation", args: []string{"person", "delete", "7"}},
		{name: "different nested group", args: []string{"person", "attributes", "list", "7"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert.Equal(t, test.want, cliRunCommandAllowed(test.args))
		})
	}
}

func TestCLIRunEnvAllowedPermitsConfiguredPeopleProviderKeyOnly(t *testing.T) {
	checks := assert.New(t)
	srv := &Server{cfg: &config.Config{}}
	configureAPIProvider(srv.cfg, peoplesweep.ProviderConfig{
		Protocol: peoplesweep.ProtocolOpenAIChat, Credential: peoplesweep.CredentialEnv,
		CredentialEnv: "MSGVAULT_PEOPLE_PROVIDER_KEY",
	})

	checks.True(srv.cliRunEnvAllowed("MSGVAULT_PEOPLE_PROVIDER_KEY"))
	checks.False(srv.cliRunEnvAllowed("OPENAI_API_KEY"))

	unconfigured := &Server{cfg: &config.Config{}}
	checks.False(unconfigured.cliRunEnvAllowed("MSGVAULT_PEOPLE_PROVIDER_KEY"))

	codex := &Server{cfg: &config.Config{}}
	configureAPIProvider(codex.cfg, peoplesweep.ProviderConfig{
		Protocol: peoplesweep.ProtocolCodexAppServer, Credential: peoplesweep.CredentialNone,
	})
	checks.False(codex.cliRunEnvAllowed("MSGVAULT_PEOPLE_PROVIDER_KEY"))
}

func TestCLIAllowlistPermitsExactPersonSweepCommands(t *testing.T) {
	tests := []struct {
		args []string
		want bool
	}{
		{args: []string{"person", "sweep", "run"}, want: true},
		{args: []string{"person", "sweep", "status", "--json"}, want: true},
		{args: []string{"person", "sweep", "history", "--limit", "20"}, want: true},
		{args: []string{"person", "sweep"}},
		{args: []string{"person", "sweep", "delete"}},
		{args: []string{"person", "sweep", "run-everything"}},
	}
	for _, test := range tests {
		assert.Equal(t, test.want, cliRunCommandAllowed(test.args), test.args)
	}
}

func TestCLIAllowlistReloadsSavedNamedProviderEnvironment(t *testing.T) {
	cfg := config.NewDefaultConfig()
	cfg.HomeDir = t.TempDir()
	cfg.People.Sweep.Enabled = true
	cfg.People.Sweep.Provider = peoplesweep.ProviderSelection{Name: "default"}
	cfg.People.Sweep.Providers = map[string]peoplesweep.ProviderConfig{
		"default": completeAPIProvider("STALE_ACTIVE_KEY", "active-model"),
	}
	require.NoError(t, cfg.Save())

	latest, err := config.Load(cfg.ConfigFilePath(), "")
	require.NoError(t, err)
	latest.People.Sweep.Providers["named"] = completeAPIProvider("EXACT_NAMED_KEY", "named-model")
	require.NoError(t, latest.Save())

	srv := &Server{cfg: cfg}
	args := []string{"person", "provider", "check", "named", "--json"}
	assert.True(t, srv.cliRunEnvAllowedForCommand(args, "EXACT_NAMED_KEY"))
	assert.False(t, srv.cliRunEnvAllowedForCommand(args, "STALE_ACTIVE_KEY"))
	assert.False(t, srv.cliRunEnvAllowedForCommand(args, "OPENAI_API_KEY"))
}

func TestCLIAllowlistPersonSweepForwardsExactCredentialOnly(t *testing.T) {
	checks := assert.New(t)
	srv := &Server{cfg: &config.Config{}}
	configureAPIProvider(srv.cfg, peoplesweep.ProviderConfig{
		Protocol: peoplesweep.ProtocolOpenAIChat, Credential: peoplesweep.CredentialEnv,
		CredentialEnv: "PEOPLE_SWEEP_KEY",
	})
	srv.cfg.Vector.Embeddings.APIKeyEnv = "EMBEDDINGS_KEY"
	srv.cfg.Attachments.Documents.APIKeyEnv = "DOCUMENT_KEY"

	checks.True(srv.cliRunEnvAllowedForCommand(
		[]string{"person", "sweep", "run"}, "PEOPLE_SWEEP_KEY"))
	checks.False(srv.cliRunEnvAllowedForCommand(
		[]string{"person", "sweep", "run"}, "EMBEDDINGS_KEY"))
	checks.False(srv.cliRunEnvAllowedForCommand(
		[]string{"person", "sweep", "run"}, "DOCUMENT_KEY"))
	checks.False(srv.cliRunEnvAllowedForCommand(
		[]string{"person", "sweep", "status"}, "PEOPLE_SWEEP_KEY"))
	checks.False(srv.cliRunEnvAllowedForCommand(
		[]string{"person", "sweep", "history"}, "PEOPLE_SWEEP_KEY"))
	checks.False(srv.cliRunEnvAllowedForCommand(
		[]string{"add-account"}, "PEOPLE_SWEEP_KEY"))
}

func TestCLIAllowlistCodexProviderOperations(t *testing.T) {
	checks := assert.New(t)
	for _, operation := range []string{"login", "models"} {
		args := []string{"person", "provider", operation}
		checks.False(cliRunCommandAllowed(args), args)
	}
	for _, operation := range []string{"logout", "delete", "exec", "account"} {
		args := []string{"person", "provider", operation}
		checks.False(cliRunCommandAllowed(args), args)
	}

	srv := &Server{cfg: &config.Config{}}
	configureAPIProvider(srv.cfg, peoplesweep.ProviderConfig{
		Protocol: peoplesweep.ProtocolCodexAppServer, Credential: peoplesweep.CredentialNone,
	})
	for _, operation := range []string{"login", "models", "status", "check"} {
		checks.False(srv.cliRunEnvAllowedForCommand(
			[]string{"person", "provider", operation}, "MUST_NOT_FORWARD"), operation)
	}
}
