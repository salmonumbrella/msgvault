package store_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/store"
)

func TestMergeSourcesRebuildsAccountAttribution(t *testing.T) {
	t.Run("incoming alias changes destination mail", func(t *testing.T) {
		require := require.New(t)
		assert := assert.New(t)
		live := newAttrFixture(t, "mbox", "live")
		history := newAttrFixtureOn(t, live.st, "mbox", "history")
		history.confirm("alias@example.test")
		id := live.persist(attrMail{raw: "To: alias@example.test\r\n\r\nbody", to: []string{"alias@example.test"}})
		address, path := attribution(t, live.st, id)
		require.False(address.Valid)
		require.True(path.Valid, "mail was derived before the alias was known")

		req := store.MergeSourcesRequest{FromSourceID: history.source.ID, IntoSourceID: live.source.ID, DryRun: true}
		_, err := live.st.MergeSourcesContext(t.Context(), req)
		require.NoError(err)
		afterAddress, afterPath := attribution(t, live.st, id)
		assert.Equal(address, afterAddress)
		assert.Equal(path, afterPath)

		req.DryRun = false
		_, err = live.st.MergeSourcesContext(t.Context(), req)
		require.NoError(err)
		assert.Equal([]int64{id}, searchIDs(t, live.st, "received:alias@example.test"))

		// A repair interrupted after the merge committed leaves pending rows.
		// Retrying the merge must finish those rows without repeating the merge.
		clearAccountPath(t, live.st, id)
		req.DryRun = true
		_, err = live.st.MergeSourcesContext(t.Context(), req)
		require.NoError(err)
		_, path = attribution(t, live.st, id)
		assert.False(path.Valid, "a dry run must not repair pending rows")
		req.DryRun = false
		result, err := live.st.MergeSourcesContext(t.Context(), req)
		require.NoError(err)
		assert.True(result.AlreadyMerged)
		assert.Equal([]int64{id}, searchIDs(t, live.st, "received:alias@example.test"))
	})

	t.Run("moved mail uses the destination mailbox", func(t *testing.T) {
		require := require.New(t)
		assert := assert.New(t)
		live := newAttrFixture(t, "gmail", "live@example.test")
		history := newAttrFixtureOn(t, live.st, "gmail", "history@example.test")
		live.confirm("live@example.test")
		history.confirm("history@example.test")
		id := history.persist(attrMail{raw: "From: friend@example.test\r\nTo: list@example.test\r\n\r\nbody",
			from: []string{"friend@example.test"}, to: []string{"list@example.test"}})
		require.Equal([]int64{id}, searchIDs(t, live.st, "received:history@example.test"))

		_, err := live.st.MergeSourcesContext(t.Context(), store.MergeSourcesRequest{
			FromSourceID: history.source.ID, IntoSourceID: live.source.ID,
		})
		require.NoError(err)
		assert.Equal([]int64{id}, searchIDs(t, live.st, "received:live@example.test"))
		assert.Empty(searchIDs(t, live.st, "received:history@example.test"))
	})

	t.Run("incoming Sent folder changes destination mail", func(t *testing.T) {
		require := require.New(t)
		assert := assert.New(t)
		live := newAttrFixture(t, "imap", "imaps://live%40example.test@mail.example.test:993")
		history := newAttrFixtureOn(t, live.st, "imap", "imaps://history%40example.test@mail.example.test:993")
		live.confirm("live@example.test")
		id := live.persist(attrMail{raw: "From: live@example.test\r\nTo: list@example.test\r\n\r\nbody",
			from: []string{"live@example.test"}, to: []string{"list@example.test"}})
		_, path := attribution(t, live.st, id)
		require.Equal("sent", path.String)
		_, err := live.st.EnsureLabelsBatch(history.source.ID, map[string]store.LabelInfo{
			"Sent": {Name: "Sent", Type: "system", SystemRole: store.LabelSystemRoleSent},
		})
		require.NoError(err)

		_, err = live.st.MergeSourcesContext(t.Context(), store.MergeSourcesRequest{
			FromSourceID: history.source.ID, IntoSourceID: live.source.ID,
		})
		require.NoError(err)
		assert.Equal([]int64{id}, searchIDs(t, live.st, "received:live@example.test"))
	})

	t.Run("moved calendar events use the destination account", func(t *testing.T) {
		require := require.New(t)
		assert := assert.New(t)
		live := newAttrFixture(t, "gcal", "live-calendar")
		history := newAttrFixtureOn(t, live.st, "gcal", "history-calendar")
		require.NoError(live.st.UpdateSourceSyncConfig(live.source.ID,
			`{"account_email":"live@example.test","calendar_id":"primary"}`))
		require.NoError(live.st.UpdateSourceSyncConfig(history.source.ID,
			`{"account_email":"history@example.test","calendar_id":"primary"}`))
		live.confirm("live@example.test")
		history.confirm("history@example.test")
		id, err := live.st.UpsertMessage(&store.Message{
			SourceID: history.source.ID, ConversationID: history.conv,
			SourceMessageID: "historical-event", MessageType: "calendar_event",
		})
		require.NoError(err)
		require.Equal([]int64{id}, searchIDs(t, live.st, "account:history@example.test"))

		_, err = live.st.MergeSourcesContext(t.Context(), store.MergeSourcesRequest{
			FromSourceID: history.source.ID, IntoSourceID: live.source.ID,
		})
		require.NoError(err)
		assert.Equal([]int64{id}, searchIDs(t, live.st, "account:live@example.test"))
		assert.Empty(searchIDs(t, live.st, "account:history@example.test"))
	})
}
