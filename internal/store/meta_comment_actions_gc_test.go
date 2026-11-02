package store_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestGCCommentActionRetainsUnresolvedTargetAndAncestors(t *testing.T) {
	for _, state := range []string{"claimed", "dispatched", "unknown", "succeeded", "failed"} {
		t.Run(state, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			st := testutil.NewSQLiteTestStore(t)
			src, err := st.GetOrCreateSource("facebook_comments", "app:9:page:100")
			require.NoError(err)
			conv, err := st.EnsureConversation(src.ID, "100_200", "Example post")
			require.NoError(err)
			var ids []int64
			for _, nativeID := range []string{"100_200", "100_201", "100_202", "100_203"} {
				id, upsertErr := st.UpsertMessage(&store.Message{SourceID: src.ID, ConversationID: conv, SourceMessageID: nativeID, MessageType: "facebook_comments"})
				require.NoError(upsertErr)
				ids = append(ids, id)
				require.NoError(st.MarkMessageDeleted(src.ID, nativeID))
			}
			require.NoError(st.SetReplyTo(src.ID, "100_201", "100_200"))
			require.NoError(st.SetReplyTo(src.ID, "100_202", "100_201"))
			row := claimAction(t, st, store.MetaCommentActionClaim{ActionKey: "retained-action", SourceID: src.ID, SourceIdentifier: src.Identifier, MessageID: ids[2], TargetCommentID: "100_202", Action: "hide"})
			if state == "dispatched" {
				row, err = st.MarkMetaCommentActionDispatchedContext(t.Context(), row.ID, row.RowRevision)
				require.NoError(err)
			} else if state != "claimed" {
				row = finishAction(t, st, row, state)
			}
			plan, err := st.PlanGCContext(t.Context())
			require.NoError(err)
			unresolved := state == "claimed" || state == "dispatched" || state == "unknown"
			want := int64(4)
			if unresolved {
				want = 1
				assert.Equal([]int64{ids[3]}, plan.SourceDeletedIDs)
			}
			assert.Equal(want, plan.SourceDeleted)
			deleted, err := st.ExecuteGCContext(t.Context(), plan)
			require.NoError(err)
			assert.Equal(want, deleted)
			if unresolved {
				assert.Equal(ids[:3], remainingGCMessageIDs(t, st))
				target, readErr := st.MetaCommentTargetContext(t.Context(), ids[2])
				require.NoError(readErr)
				assert.Equal(ids[1], target.ParentMessageID)
				assert.Equal("100_201", target.ParentCommentID)
			} else {
				assert.Empty(remainingGCMessageIDs(t, st))
			}
			saved, err := st.GetMetaCommentActionByKeyContext(t.Context(), "retained-action")
			require.NoError(err)
			assert.Equal(row.ID, saved.ID)
			assert.Equal(unresolved, saved.MessageID.Valid)
		})
	}
}
