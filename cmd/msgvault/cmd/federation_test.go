package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/clirun"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/federation"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFederationEnrollOutput(t *testing.T) {
	setup := func(t *testing.T) (*store.Store, *cobra.Command, context.Context) {
		t.Helper()
		st := testutil.NewTestStore(t)
		serverCfg := config.NewDefaultConfig()
		serverCfg.Data.DataDir = t.TempDir()
		serverCfg.Federation.Role = "hub"
		serverCfg.Server.APIKey = "owner-fixture"
		require.NoError(t, st.ConfigureFederationHubContext(t.Context()))
		apiServer := api.NewServerWithOptions(api.ServerOptions{Config: serverCfg, Store: st, FederationStore: st, Logger: slog.New(slog.DiscardHandler)})
		t.Cleanup(func() { require.NoError(t, apiServer.Shutdown(context.WithoutCancel(t.Context()))) })
		srv := httptest.NewServer(apiServer.Router())
		t.Cleanup(srv.Close)
		clientCfg := config.NewDefaultConfig()
		clientCfg.Data.DataDir = t.TempDir()
		clientCfg.Remote = config.RemoteConfig{URL: srv.URL, APIKey: "owner-fixture", AllowInsecure: true}
		ctx := withStoreResolverConfig(t, clientCfg)
		template, _, err := rootCmd.Find([]string{"federation", "enroll"})
		require.NoError(t, err)
		command := &cobra.Command{Use: template.Use, Args: template.Args, RunE: template.RunE}
		silenceUsageInRunE(command)
		command.Flags().AddFlagSet(template.Flags())
		command.Flags().VisitAll(func(flag *pflag.Flag) { require.NoError(t, flag.Value.Set(flag.DefValue)); flag.Changed = false })
		t.Cleanup(func() {
			command.Flags().VisitAll(func(flag *pflag.Flag) { assert.NoError(t, flag.Value.Set(flag.DefValue)); flag.Changed = false })
		})
		return st, command, ctx
	}
	t.Run("token and join hint", func(t *testing.T) {
		for _, tc := range []struct {
			name, spokeName string
		}{{"matching", ""}, {"distinct", "laptop"}} {
			t.Run(tc.name, func(t *testing.T) {
				assertions := assert.New(t)
				_, command, ctx := setup(t)
				stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
				command.SetOut(stdout)
				command.SetErr(stderr)
				command.SetArgs([]string{"--spoke-instance", strings.Repeat("a", 64), "--member", "alice", "--spoke-name", tc.spokeName})
				require.NoError(t, command.ExecuteContext(ctx))
				token := strings.TrimSpace(stdout.String())
				assertions.Regexp(`^mvf1_[A-Za-z0-9_-]+$`, token)
				assertions.Equal(token+"\n", stdout.String())
				assertions.NotContains(stderr.String(), token)
				assertions.Contains(stderr.String(), "Member: alice\n")
				if tc.spokeName == "" {
					assertions.NotContains(stderr.String(), "Spoke:")
				} else {
					assertions.Contains(stderr.String(), "Spoke: laptop\n")
				}
				assertions.Contains(stderr.String(), "msgvault federation join <hub-url>")
				assertions.NotContains(stderr.String(), "http://")
			})
		}
	})
	t.Run("token write failure", func(t *testing.T) {
		st, command, ctx := setup(t)
		var stderr bytes.Buffer
		command.SetOut(failingSearchWriter{})
		command.SetErr(&stderr)
		command.SetArgs([]string{"--spoke-instance", strings.Repeat("a", 64), "--member", "alice"})
		err := command.ExecuteContext(ctx)
		require.ErrorIs(t, err, io.ErrClosedPipe)
		enrollments, listErr := st.ListFederationEnrollmentsContext(t.Context())
		require.NoError(t, listErr)
		require.Len(t, enrollments, 1)
		assert.Contains(t, err.Error(), enrollments[0].EnrollmentUID)
		assert.Contains(t, err.Error(), "revoke")
		assert.Contains(t, err.Error(), "re-enroll")
		assert.NotContains(t, err.Error(), "mvf1_")
		assert.NotContains(t, stderr.String(), "mvf1_")
		assert.NotContains(t, stderr.String(), "msgvault federation join")
	})
	t.Run("list", func(t *testing.T) {
		requirements := require.New(t)
		st, _, ctx := setup(t)
		matching, _, err := st.CreateFederationEnrollmentContext(t.Context(), strings.Repeat("a", 64), "alice", "")
		requirements.NoError(err)
		secondMatching, _, err := st.CreateFederationEnrollmentContext(t.Context(), strings.Repeat("c", 64), "alice", "")
		requirements.NoError(err)
		distinct, _, err := st.CreateFederationEnrollmentContext(t.Context(), strings.Repeat("b", 64), "bob", "laptop")
		requirements.NoError(err)
		template, _, err := rootCmd.Find([]string{"federation", "enrollments", "list"})
		requirements.NoError(err)
		list := &cobra.Command{Use: template.Use, Args: template.Args, RunE: template.RunE}
		list.Flags().AddFlagSet(template.Flags())
		var output bytes.Buffer
		list.SetOut(&output)
		requirements.NoError(list.ExecuteContext(ctx))
		assert.Contains(t, output.String(), matching.EnrollmentUID+"\t"+strings.Repeat("a", 64)+"\talice\tactive\n")
		assert.Contains(t, output.String(), secondMatching.EnrollmentUID+"\t"+strings.Repeat("c", 64)+"\talice\tactive\n")
		assert.Contains(t, output.String(), distinct.EnrollmentUID+"\t"+strings.Repeat("b", 64)+"\tbob (laptop)\tactive\n")
	})
	t.Run("server error", func(t *testing.T) {
		_, command, ctx := setup(t)
		command.SetArgs([]string{"--spoke-instance", strings.Repeat("a", 64), "--member", " "})
		assert.ErrorContains(t, command.ExecuteContext(ctx), "member")
	})
}

func TestFederationJoinForwardsTokenInEnvironment(t *testing.T) {
	const token = "mvf1_synthetic_fixture"
	server, calls := newDaemonCLIRunnerTestServer(t, func(req daemonCLIRunTestRequest) {
		assert.Equal(t, token, req.Env[clirun.EnvFederationToken])
		assert.NotContains(t, strings.Join(req.Args, " "), token)
		assert.Equal(t, "federation", req.Args[0])
		assert.Equal(t, "join", req.Args[1])
	}, `{"type":"complete"}`)
	ctx := configureRemoteDaemonForTest(t, server.URL)
	root := &cobra.Command{Use: "msgvault"}
	group := &cobra.Command{Use: "federation"}
	group.AddCommand(newFederationJoinCmd())
	root.AddCommand(group)
	root.SetIn(strings.NewReader(token))
	root.SetArgs([]string{"federation", "join", "https://hub.example.com", "--hub-instance", strings.Repeat("a", 64), "--token-file", "-"})
	require.NoError(t, root.ExecuteContext(ctx))
	assert.EqualValues(t, 1, calls.Load())
}

func TestFederationCommandsRequireSpoke(t *testing.T) {
	markDaemonCLISubprocessForTest(t)
	cfg := config.NewDefaultConfig()
	cfg.Data.DataDir = t.TempDir()
	ctx := withStoreResolverConfig(t, cfg)
	for _, cmd := range []*cobra.Command{newFederationStatusCmd(), newFederationLeaveCmd()} {
		if strings.HasPrefix(cmd.Use, "leave") {
			cmd.SetArgs([]string{"https://hub.example.com"})
		}
		assert.ErrorContains(t, cmd.ExecuteContext(ctx), "role must be spoke")
	}
}

func TestFederationStatusJSON(t *testing.T) {
	var out bytes.Buffer
	links := []federation.LinkStatus{{HubOrigin: "https://example.com", Member: "alice", State: "enrolled", Shares: 0, Policy: config.NewDefaultConfig().Federation.Hub}}
	require.NoError(t, writeFederationStatus(&out, links, true))
	var decoded []map[string]any
	require.NoError(t, json.Unmarshal(out.Bytes(), &decoded))
	require.Len(t, decoded, 1)
	assert.Equal(t, "enrolled", decoded[0]["state"])
	assert.Equal(t, float64(0), decoded[0]["shares"])
	assert.NotContains(t, out.String(), "mvf1_")
}
