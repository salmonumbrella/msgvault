package api

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/clirun"
)

func TestCLIRunCommandAllowedInlineCommands(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{
		{"add-inline", "--chat-id", "123"},
		{"add-inline", "--transport", "cli", "--chat-id", "123"},
		{"sync-inline"},
		{"sync-inline", "api.inline.chat:user:42", "--full"},
		{"backfill-inline-media"},
	} {
		assert.True(t, cliRunCommandAllowed(args), "%v must reach the daemon", args)
	}
	assert.False(t, cliRunCommandAllowed([]string{"inline"}))
}

func TestInlineSourceSchedulerClassification(t *testing.T) {
	assertions := assert.New(t)

	t.Parallel()
	name, ok := SchedulerJobNameForSource("inline", "api.inline.chat:user:42")
	require.True(t, ok)
	assertions.Equal(InlineJobName, name)
	classification := classifySourceScheduling("inline", "api.inline.chat:user:42")
	assertions.Equal(sourceScheduleGeneric, classification.kind)
	assertions.Equal(name, classification.jobName)
}

func TestInlineOAuthHandoffIsOnlyAcceptedForAccountSetup(t *testing.T) {
	assertions := assert.New(t)

	t.Parallel()
	srv := &Server{}
	assertions.True(srv.cliRunEnvAllowedForCommand([]string{"add-inline"}, clirun.EnvInlineOAuth))
	assertions.False(srv.cliRunEnvAllowedForCommand([]string{"sync-inline"}, clirun.EnvInlineOAuth))
	assertions.False(srv.cliRunEnvAllowedForCommand([]string{"export-messages"}, clirun.EnvInlineOAuth))
	assertions.False(srv.cliRunEnvAllowedForCommand(nil, clirun.EnvInlineOAuth))
}

func TestFederationCLIAdmission(t *testing.T) {
	srv := &Server{}
	for _, sub := range []string{"join", "status", "leave"} {
		assert.True(t, cliRunCommandAllowed([]string{"federation", sub}))
		assert.Equal(t, sub == "join", srv.cliRunEnvAllowedForCommand([]string{"federation", sub}, clirun.EnvFederationToken))
	}
	for _, sub := range []string{"enroll", "identity", "revoke", "enrollments"} {
		assert.False(t, cliRunCommandAllowed([]string{"federation", sub}))
		assert.False(t, srv.cliRunEnvAllowedForCommand([]string{"federation", sub}, clirun.EnvFederationToken))
	}
}
