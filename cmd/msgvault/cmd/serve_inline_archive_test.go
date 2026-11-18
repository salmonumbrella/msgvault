package cmd

import (
	"context"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/agentgrant"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/emailtags"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestInlineArchiveStatefulCLI(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fixture executable requires POSIX shell")
	}
	// Synthetic JSON follows Inline b3b96187460631e3de96dae79d52737331b99fde:
	// cli/src/main.rs emits GetChat/GetMessage JSON and core.proto defines Dialog and peers.

	for _, tc := range []struct {
		name, initial, peer, dialog, account, transport, mode, chatKey, messageKey string
		desired, dry, restricted, noWhole, filter, syncActive                      bool
		wantCode                                                                   string
		writes                                                                     int
	}{
		{name: "DM collision", initial: "false", peer: "user", desired: true, writes: 1},
		{name: "omitted archive bit", initial: "omitted", peer: "user", desired: true, writes: 1},
		{name: "restore", initial: "true", peer: "user", writes: 1},
		{name: "already", initial: "true", peer: "user", desired: true},
		{name: "preview", initial: "false", peer: "user", desired: true, dry: true},
		{name: "while syncing", initial: "false", peer: "user", desired: true, syncActive: true, writes: 1},
		{name: "merged chat key", initial: "false", peer: "user", chatKey: "msgvault-archive:7", desired: true, wantCode: "stale_identity"},
		{name: "wrong message key", initial: "false", peer: "user", messageKey: "chat:6:message:8", desired: true, wantCode: "stale_identity"},
		{name: "child", initial: "false", peer: "child", desired: true, writes: 1},
		{name: "missing dialog", initial: "false", peer: "user", dialog: "missing", desired: true, wantCode: "archive_unsupported"},
		{name: "wrong account", initial: "false", peer: "user", account: "99", desired: true, wantCode: "provider_unavailable"},
		{name: "restricted", initial: "false", peer: "user", restricted: true, desired: true, wantCode: "message_not_found"},
		{name: "no whole chat", initial: "false", peer: "user", noWhole: true, desired: true, wantCode: "invalid_request"},
		{name: "outside filter", initial: "false", peer: "user", filter: true, desired: true, wantCode: "stale_identity"},
		{name: "unsupported binary", initial: "false", peer: "user", mode: "unsupported", desired: true, wantCode: "provider_unavailable"},
		{name: "deleted message", initial: "false", peer: "user", mode: "missing-message", desired: true, wantCode: "stale_identity"},
		{name: "unsupported server", initial: "false", peer: "user", mode: "server", desired: true, wantCode: "archive_unsupported"},
		{name: "account changed", initial: "false", peer: "user", mode: "account-change", desired: true, wantCode: "remote_unknown", writes: 1},
		{name: "timeout after write", initial: "false", peer: "user", mode: "timeout", desired: true, wantCode: "remote_unknown", writes: 1},
		{name: "lost response", initial: "false", peer: "user", mode: "lost", desired: true, wantCode: "remote_unknown", writes: 1},
		{name: "mismatched readback", initial: "false", peer: "user", mode: "mismatch", desired: true, wantCode: "remote_unknown", writes: 1},
		{name: "OAuth", initial: "false", peer: "user", transport: "mcp", desired: true, wantCode: "archive_unsupported"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("INLINE_FIXTURE", dir)
			require.NoError(t, os.WriteFile(filepath.Join(dir, "state"), []byte(tc.initial), 0600))
			require.NoError(t, os.WriteFile(filepath.Join(dir, "collision-state"), []byte("false"), 0600))
			require.NoError(t, os.WriteFile(filepath.Join(dir, "mode"), []byte(tc.mode), 0600))
			require.NoError(t, os.WriteFile(filepath.Join(dir, "peer"), []byte(tc.peer), 0600))
			require.NoError(t, os.WriteFile(filepath.Join(dir, "dialog"), []byte(tc.dialog), 0600))
			account := tc.account
			if account == "" {
				account = "42"
			}
			require.NoError(t, os.WriteFile(filepath.Join(dir, "account"), []byte(account), 0600))
			script := `#!/bin/sh
set -eu
d="$INLINE_FIXTURE"
echo "$1 ${2-}" >> "$d/calls"
mode=$(cat "$d/mode")
case "$1 ${2-}" in
 '--version ') if [ "$mode" = unsupported ]; then exit 1; fi; echo 'inline 1.0.0' ;;
 'doctor --json') echo '{"config":{"apiBaseUrl":"https://api.inline.chat","realtimeUrl":"wss://api.inline.chat"}}' ;;
 'auth me') if [ "$mode" = account-change ] && [ -f "$d/me" ]; then echo '{"id":99}'; else touch "$d/me"; printf '{"id":%s}' "$(cat "$d/account")"; fi ;;
 'messages get') if [ "$mode" = missing-message ]; then exit 1; fi; echo '{"id":8,"chat_id":7,"media":{"media":{"FutureVariant":{}}},"reactions":{"reactions":[{"user_id":0}]}}' ;;
 'chats get')
  if [ "$mode" = server ]; then exit 1; fi
  if [ "$(cat "$d/peer")" = child ]; then chat='{"id":7,"parent_chat_id":6,"parent_message_id":3,"peer_id":{"type":{"Chat":{"chat_id":7}}}}'; peer='{"type":{"Chat":{"chat_id":7}}}'; else chat='{"id":7,"peer_id":{"type":{"User":{"user_id":9}}}}'; peer='{"type":{"User":{"user_id":9}}}'; fi
  state=$(cat "$d/state"); if [ "$state" = omitted ]; then bit=''; else bit=",\"archived\":$state"; fi
  if [ "$mode" = mismatch ] && [ -f "$d/writes" ]; then bit=',"archived":false'; fi
  if [ "$(cat "$d/dialog")" = missing ]; then printf '{"chat":%s,"dialog":null}\n' "$chat"; else printf '{"chat":%s,"dialog":{"peer":%s,"chat_id":7%s}}\n' "$chat" "$peer" "$bit"; fi ;;
 'chats archive'|'chats unarchive')
  target="$d/state"
  if [ "$(cat "$d/peer")" = child ]; then [ "$3" = --chat-id ] && [ "$4" = 7 ]; elif [ "$3" = --chat-id ] && [ "$4" = 7 ]; then target="$d/collision-state"; else [ "$3" = --user-id ] && [ "$4" = 9 ]; fi
  if [ "$2" = archive ]; then echo true > "$target"; else echo false > "$target"; fi; echo "$2" >> "$d/writes"; if [ "$mode" = lost ]; then exit 1; fi; if [ "$mode" = timeout ]; then exec sleep 30; fi; echo '{"updates":[]}' ;;
 *) exit 1 ;;
esac
`
			executable := filepath.Join(dir, "inline-fixture")
			require.NoError(t, os.WriteFile(executable, []byte(script), 0700))
			st := testutil.NewTestStore(t)
			source, err := st.GetOrCreateSource("inline", "api.inline.chat:user:42")
			require.NoError(t, err)
			if tc.syncActive {
				execution, lockErr := st.AcquireSyncExecutionContext(t.Context(), source.ID)
				require.NoError(t, lockErr)
				t.Cleanup(func() { assert.NoError(t, execution.Release()) })
			}
			chatKey := tc.chatKey
			if chatKey == "" {
				chatKey = "chat:7"
			}
			messageKey := tc.messageKey
			if messageKey == "" {
				messageKey = "chat:7:message:8"
			}
			conv, err := st.EnsureConversation(t.Context(), source.ID, chatKey, "Fixture")
			require.NoError(t, err)
			id, err := st.UpsertMessage(&store.Message{SourceID: source.ID, ConversationID: conv, SourceMessageID: messageKey, MessageType: "inline"})
			require.NoError(t, err)
			cfg := &config.Config{Data: config.DataConfig{DataDir: dir}}
			transport := tc.transport
			if transport == "" {
				transport = "cli"
			}
			cfg.Inline.Accounts = []config.InlineAccount{{Identifier: source.Identifier, Transport: transport, CLIPath: executable}}
			if tc.filter {
				cfg.Inline.Accounts[0].ChatIDs = []string{"9"}
			}
			ref := agentgrant.SourceRef{Type: "inline", Identifier: source.Identifier}
			if tc.restricted {
				ref.IncomingSenderKeys = []string{"fixture@example.test"}
			}
			grant := &agentgrant.Grant{Permissions: []agentgrant.Permission{agentgrant.PermissionMessageArchive}, Sources: []agentgrant.SourceRef{ref}}
			adapter := &storeAPIAdapter{store: st, config: cfg}
			gates := 0
			ctx := t.Context()
			if tc.mode == "timeout" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				done := make(chan struct{})
				go func() {
					defer close(done)
					ticker := time.NewTicker(10 * time.Millisecond)
					defer ticker.Stop()
					timer := time.NewTimer(30 * time.Second)
					defer timer.Stop()
					for {
						select {
						case <-ticker.C:
							if _, err := os.Stat(filepath.Join(dir, "writes")); err == nil {
								cancel()
								return
							}
						case <-timer.C:
							cancel()
							return
						case <-ctx.Done():
							return
						}
					}
				}()
				defer func() { cancel(); <-done }()
			}
			result, err := adapter.ArchiveMessage(ctx, id, emailtags.ArchiveChange{Archived: &tc.desired, DryRun: tc.dry, WholeChat: !tc.noWhole}, grant, func(context.Context) (func(), error) { gates++; return func() {}, nil })
			if tc.wantCode != "" {
				var failure *emailtags.MessageTagError
				require.ErrorAs(t, err, &failure)
				assert.Equal(t, tc.wantCode, failure.Code)
			} else {
				require.NoError(t, err)
				assert.Equal(t, (tc.initial == "true") != tc.desired, result.Changed)
				assert.Equal(t, !tc.dry, result.Verified)
			}
			writes, err := os.ReadFile(filepath.Join(dir, "writes"))
			if tc.writes == 0 {
				assert.ErrorIs(t, err, os.ErrNotExist)
			} else {
				require.NoError(t, err)
				assert.Len(t, strings.Fields(string(writes)), tc.writes)
			}
			if tc.writes > 0 {
				assert.Equal(t, 1, gates)
			}
			collision, collisionErr := os.ReadFile(filepath.Join(dir, "collision-state"))
			require.NoError(t, collisionErr)
			assert.Equal(t, "false", string(collision))
			if tc.restricted || tc.noWhole || tc.transport == "mcp" || tc.filter || tc.chatKey != "" || tc.messageKey != "" {
				_, callErr := os.Stat(filepath.Join(dir, "calls"))
				assert.ErrorIs(t, callErr, os.ErrNotExist)
			}
			state, err := os.ReadFile(filepath.Join(dir, "state"))
			require.NoError(t, err)
			if tc.writes > 0 {
				assert.Equal(t, map[bool]string{true: "true\n", false: "false\n"}[tc.desired], string(state))
			} else {
				assert.Equal(t, tc.initial, string(state))
			}
			if tc.name == "DM collision" {
				cfg.Server.APIKey = "owner"
				cfg.Server.AgentAccess = true
				server := api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: adapter, Engine: query.NewEngine(st.DB(), st.IsPostgreSQL()), Logger: slog.New(slog.DiscardHandler)})
				t.Cleanup(func() { assert.NoError(t, server.Shutdown(context.WithoutCancel(t.Context()))) })
				httpServer := httptest.NewServer(server.Router())
				t.Cleanup(httpServer.Close)
				owner, clientErr := daemonclient.New(daemonclient.Config{URL: httpServer.URL, APIKey: "owner", AllowInsecure: true})
				require.NoError(t, clientErr)
				t.Cleanup(func() { assert.NoError(t, owner.Close()) })
				remote, callErr := owner.ArchiveMessage(t.Context(), id, emailtags.ArchiveChange{Archived: &tc.desired, WholeChat: true})
				require.NoError(t, callErr)
				assert.False(t, remote.Changed)
				assert.True(t, remote.Verified)
				calls, readErr := os.ReadFile(filepath.Join(dir, "writes"))
				require.NoError(t, readErr)
				assert.Len(t, strings.Fields(string(calls)), 1)
				token, tokenErr := owner.IssueAgentToken(t.Context(), "fixture", []string{"message.archive"}, []int64{source.ID}, nil, nil, time.Time{})
				require.NoError(t, tokenErr)
				delegated, clientErr := daemonclient.New(daemonclient.Config{URL: httpServer.URL, AgentToken: token.Secret, AllowInsecure: true})
				require.NoError(t, clientErr)
				t.Cleanup(func() { assert.NoError(t, delegated.Close()) })
				opts, optsErr := delegatedMCPServeOptions(t.Context(), delegated)
				require.NoError(t, optsErr)
				session, closeSession := archiveMCPSession(t, opts)
				defer closeSession()
				mcpResult, callErr := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "archive_message", Arguments: map[string]any{"message_id": id, "archived": false, "whole_chat": true}})
				require.NoError(t, callErr)
				require.False(t, mcpResult.IsError, "%v", mcpResult.Content)
				actual, readErr := os.ReadFile(filepath.Join(dir, "state"))
				require.NoError(t, readErr)
				assert.Equal(t, "false\n", string(actual))
				calls, readErr = os.ReadFile(filepath.Join(dir, "writes"))
				require.NoError(t, readErr)
				assert.Equal(t, []string{"archive", "unarchive"}, strings.Fields(string(calls)))
				collision, readErr = os.ReadFile(filepath.Join(dir, "collision-state"))
				require.NoError(t, readErr)
				assert.Equal(t, "false", string(collision))

			}
			saved, err := st.GetMessageContext(t.Context(), id)
			require.NoError(t, err)
			assert.Equal(t, messageKey, saved.SourceMessageID)
		})
	}
}
