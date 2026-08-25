package api

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/peoplesweep"
)

func configureAPIProvider(cfg *config.Config, provider peoplesweep.ProviderConfig) {
	cfg.People.Sweep.Provider = peoplesweep.ProviderSelection{Name: "default"}
	cfg.People.Sweep.Providers = map[string]peoplesweep.ProviderConfig{"default": provider}
}

func TestCLIRunCommandAllowedPermitsExactPersonProviderCommands(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want bool
	}{
		{name: "status", args: []string{"person", "provider", "status"}, want: true},
		{name: "status flags", args: []string{"person", "provider", "status", "--json"}, want: true},
		{name: "consent", args: []string{"person", "provider", "consent", "--yes"}, want: true},
		{name: "revoke", args: []string{"person", "provider", "revoke"}, want: true},
		{name: "check", args: []string{"person", "provider", "check", "--json"}, want: true},
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
		checks.True(cliRunCommandAllowed(args), args)
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
