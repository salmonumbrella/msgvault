package meta

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestActionsUnknownResolutionSurvivesGC(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, src, id, actions, graph := actionFixture(t)
	graph.status, graph.response = 500, `{}`
	unknown, err := actions.Run(t.Context(), ActionRequest{MessageID: id, Action: ActionHide})
	_ = actionCode(t, err, "outcome_unknown")
	require.NotNil(unknown.Receipt)
	graph.status, graph.response = 200, `{"success":true}`
	_, err = actionRun(t, actions, id, ActionDelete)
	require.NoError(err)
	require.NoError(st.MarkMessageDeleted(src.ID, "100_200"))

	plan, err := st.PlanGCContext(t.Context())
	require.NoError(err)
	_, err = st.ExecuteGCContext(t.Context(), plan)
	require.NoError(err)
	resolved, err := actions.Run(t.Context(), ActionRequest{MessageID: id, Action: ActionHide, ResolveUnknown: "not-sent"})
	require.NoError(err, "cleanup must retain the target and parent needed for local resolution")
	require.NotNil(resolved.Receipt)
	assert.Equal(unknown.Receipt.ID, resolved.Receipt.ID)
	assert.Equal("failed", resolved.Receipt.State)
	assert.Equal(2, graph.count(), "resolution must not contact the provider")

	plan, err = st.PlanGCContext(t.Context())
	require.NoError(err)
	assert.Equal(int64(2), plan.SourceDeleted, "resolved receipts no longer retain deleted rows")
	deleted, err := st.ExecuteGCContext(t.Context(), plan)
	require.NoError(err)
	assert.Equal(int64(2), deleted)
}
