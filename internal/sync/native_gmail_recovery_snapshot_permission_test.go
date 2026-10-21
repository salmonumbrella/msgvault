package sync

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/gmail"
	"go.kenn.io/msgvault/internal/store"
	testemail "go.kenn.io/msgvault/internal/testutil/email"
	"golang.org/x/oauth2"
)

type deniedGmailCompleteSnapshotAPI struct {
	grantedGmailProvenanceAPI
	denial error
}

func (a deniedGmailCompleteSnapshotAPI) ListCompleteMessageSnapshot(context.Context, string) (*gmail.MessageListResponse, error) {
	return nil, a.denial
}

func TestGmailCompleteSnapshotDenialRetiresRetainedEventsAndPendingDelivery(t *testing.T) {
	for _, tc := range []struct {
		name   string
		denial error
	}{
		{name: "unauthorized", denial: &gmail.StatusError{StatusCode: http.StatusUnauthorized}},
		{name: "forbidden", denial: &gmail.StatusError{StatusCode: http.StatusForbidden}},
		{name: "invalid grant", denial: &oauth2.RetrieveError{ErrorCode: "invalid_grant"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			env := newTestEnv(t)
			source := env.CreateSourceWithHistory(t, "1000")
			const principal = "owner:synthetic-gmail-recovery"
			const family = "msgvault.source_message_archived"
			_, err := env.Store.ConfigureMCPEvents(t.Context(), store.MCPEventsConfig{
				Enabled: true, Principal: principal, Capabilities: []store.MCPEventCapability{
					{Family: family, SourceType: "gmail", Kinds: []string{"message"}},
				},
			})
			require.NoError(err)

			resource, err := env.Store.GetMCPResource(t.Context(), "source", source.ID)
			require.NoError(err)
			permission := store.NativeSourcePermission{SourceID: source.ID, SourceReference: resource.Reference, SourceType: "gmail", NativeIdentity: source.Identifier, Known: true, MessageRead: true}
			require.NoError(env.Store.ObserveNativeSourcePermission(t.Context(), permission))
			_, binding, err := env.Store.ResolveMCPSubscriptionResource(t.Context(), family, "source", source.ID, []string{"message"}, resource.Reference)
			require.NoError(err)
			input := store.MCPSubscription{
				ID: "sub_gmail_recovery_snapshot", Principal: principal, Name: family, Arguments: []byte(`{}`),
				ScopeKind: "source", ScopeID: source.ID, SourceID: source.ID,
				CallbackURL: "https://receiver.example.net/events", SecretEnc: []byte("synthetic-secret-ciphertext"),
				SecretRevision: 1, VerifiedRevision: 1, ExpiresAt: time.Now().UTC().Add(time.Hour),
			}
			require.NoError(env.Store.BindMCPSubscriptionScope(t.Context(), &input))
			active, _, err := env.Store.ActivateMCPSubscription(t.Context(), store.MCPActivation{Subscription: input, Now: time.Now().UTC()})
			require.NoError(err)
			require.NotNil(active)

			env.Mock.AddMessage("seed", testemail.NewMessage().From("sender@example.test").To("owner@example.test").Body("Synthetic retained Gmail event").Bytes(), []string{"INBOX"})
			env.SetHistory(1001, historyAdded("seed"))
			env.Syncer = New(grantedGmailProvenanceAPI{env.Mock}, env.Store, nil)
			_, err = env.Syncer.Incremental(env.Context, source)
			require.NoError(err)

			var seq int64
			require.NoError(env.Store.DB().QueryRow(env.Store.Rebind(`SELECT seq FROM mcp_event_log WHERE family=? AND source_id=? ORDER BY seq DESC LIMIT 1`), family, source.ID).Scan(&seq))
			require.NoError(env.Store.CheckMCPResourceBinding(t.Context(), binding))
			now := time.Now().UTC()
			_, err = env.Store.GetMCPEvent(t.Context(), active.ID, seq, principal, now)
			require.NoError(err, "the event is readable while the native grant is current")
			pending, err := env.Store.PrepareMCPDelivery(t.Context(), active.ID, active.Generation, now, func(_ store.MCPSubscription, event store.MCPEvent) ([]byte, error) {
				return event.Data, nil
			})
			require.NoError(err)
			require.NotNil(pending)
			assert.Equal(seq, pending.Event.Seq)
			queued, err := env.Store.GetMCPSubscription(t.Context(), active.ID)
			require.NoError(err)
			assert.Equal(seq, queued.PendingSeq)
			assert.NotEmpty(queued.PendingEnvelope)

			worker := New(deniedGmailCompleteSnapshotAPI{grantedGmailProvenanceAPI: grantedGmailProvenanceAPI{env.Mock}, denial: tc.denial}, env.Store, nil)
			_, err = worker.nativeProfileWithPermission(t.Context(), source)
			require.NoError(err)
			// Complete snapshot recovery can run with only its pinned Gmail lifetime.
			// The common adapter observer may be absent, so this error must use the
			// nativeMailPermissionFailure fallback.
			worker.nativeSourcePermission = nil
			_, err = worker.listCompleteMessageSnapshot(t.Context())
			require.Error(err)

			allowed, err := env.Store.NativeSourceMessagePermission(t.Context(), source.ID)
			require.NoError(err)
			assert.False(allowed, "snapshot authorization failure retires the pinned source grant")
			current, err := env.Store.GetMCPResource(t.Context(), "source", source.ID)
			require.NoError(err)
			assert.NotEqual(resource.Reference, current.Reference)
			_, err = env.Store.GetMCPEvent(t.Context(), active.ID, seq, principal, now.Add(time.Second))
			require.Error(err, "a retained event receipt is unavailable after the grant is revoked")
			delivery, err := env.Store.PrepareMCPDelivery(t.Context(), active.ID, active.Generation, now.Add(time.Second), func(_ store.MCPSubscription, event store.MCPEvent) ([]byte, error) {
				return event.Data, nil
			})
			require.NoError(err)
			assert.Nil(delivery, "queued delivery cannot use the retired source lifetime")
			stopped, err := env.Store.GetMCPSubscription(t.Context(), active.ID)
			require.NoError(err)
			assert.Equal("stopped", stopped.State)
			assert.Equal("scope_removed", stopped.StopReason)
			assert.Zero(stopped.PendingSeq)
			assert.Empty(stopped.PendingEnvelope)
		})
	}
}
