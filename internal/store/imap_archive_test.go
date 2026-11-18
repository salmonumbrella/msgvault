package store_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

func TestIMAPArchiveMovePersistence(t *testing.T) {
	st, source, id := tagFixture(t, "imap", "INBOX|7")
	tagMembership(t, st, source.ID, id, "INBOX", 77, 7)
	target, err := st.IMAPArchiveTargetContext(t.Context(), id)
	require.NoError(t, err)
	m := store.IMAPPendingMove{SourceIdentifier: source.Identifier, Target: target, Destination: "Archiv", DestinationUIDValidity: 88}
	require.NoError(t, st.BeginIMAPMoveContext(t.Context(), m))
	pending, err := st.IMAPPendingMoveContext(t.Context(), source.ID, id)
	require.NoError(t, err)
	require.NotNil(t, pending)
	assert.Zero(t, pending.DestinationUID)
	require.Error(t, st.BeginIMAPMoveContext(t.Context(), m))
	other, err := st.GetOrCreateSource("imap", "other@example.test")
	require.NoError(t, err)
	_, err = st.MergeSourcesContext(t.Context(), store.MergeSourcesRequest{FromSourceID: source.ID, IntoSourceID: other.ID})
	require.ErrorIs(t, err, store.ErrSourceMergeInvalid)
	m.DestinationUID = 9
	require.NoError(t, st.SaveIMAPMoveMappingContext(t.Context(), m))
	pending, err = st.IMAPPendingMoveContext(t.Context(), source.ID, id)
	require.NoError(t, err)
	assert.Equal(t, m, pendingValue(pending))
	_, err = st.MergeSourcesContext(t.Context(), store.MergeSourcesRequest{FromSourceID: other.ID, IntoSourceID: source.ID})
	require.ErrorIs(t, err, store.ErrSourceMergeInvalid)
	require.NoError(t, st.ReconcileIMAPMoveContext(t.Context(), m))
	target, err = st.IMAPArchiveTargetContext(t.Context(), id)
	require.NoError(t, err)
	assert.Equal(t, "Archiv", target.Mailbox)
	assert.Equal(t, uint32(88), target.UIDValidity)
	assert.Equal(t, uint32(9), target.UID)
	assert.Equal(t, "INBOX|7", target.SourceMessageID)
	pending, err = st.IMAPPendingMoveContext(t.Context(), source.ID, id)
	require.NoError(t, err)
	assert.Nil(t, pending)
	labels, err := st.MessageLabelIDsContext(t.Context(), id)
	require.NoError(t, err)
	label, err := st.LabelIDContext(t.Context(), source.ID, "Archiv")
	require.NoError(t, err)
	assert.Contains(t, labels, label)
}

func pendingValue(p *store.IMAPPendingMove) store.IMAPPendingMove {
	if p == nil {
		return store.IMAPPendingMove{}
	}
	return *p
}

func TestIMAPArchiveRejectsAmbiguousAndChangedEpoch(t *testing.T) {
	st, source, id := tagFixture(t, "imap", "INBOX|7")
	tagMembership(t, st, source.ID, id, "INBOX", 77, 7)
	target, err := st.IMAPArchiveTargetContext(t.Context(), id)
	require.NoError(t, err)
	m := store.IMAPPendingMove{SourceIdentifier: source.Identifier, Target: target, Destination: "Archiv", DestinationUIDValidity: 88}
	tagMembership(t, st, source.ID, id, "Other", 20, 2)
	_, err = st.IMAPArchiveTargetContext(t.Context(), id)
	require.Error(t, err)
	require.Error(t, st.BeginIMAPMoveContext(t.Context(), m))
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`DELETE FROM imap_message_memberships WHERE source_id=? AND mailbox=?`), source.ID, "Other")
	require.NoError(t, err)
	require.NoError(t, st.BeginIMAPMoveContext(t.Context(), m))
	m.DestinationUID = 9
	require.NoError(t, st.SaveIMAPMoveMappingContext(t.Context(), m))
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE imap_folder_state SET uidvalidity=78 WHERE source_id=? AND mailbox=?`), source.ID, "INBOX")
	require.NoError(t, err)
	require.Error(t, st.ReconcileIMAPMoveContext(t.Context(), m))
	pending, err := st.IMAPPendingMoveContext(t.Context(), source.ID, id)
	require.NoError(t, err)
	require.NotNil(t, pending)
	assert.Equal(t, uint32(9), pending.DestinationUID)
}

func TestIMAPArchiveReconcileConflictKeepsIntent(t *testing.T) {
	st, source, id := tagFixture(t, "imap", "INBOX|7")
	tagMembership(t, st, source.ID, id, "INBOX", 77, 7)
	conv, err := st.EnsureConversation(t.Context(), source.ID, "other-thread", "Other")
	require.NoError(t, err)
	other, err := st.UpsertMessage(&store.Message{SourceID: source.ID, ConversationID: conv, SourceMessageID: "Archiv|9", MessageType: "email"})
	require.NoError(t, err)
	tagMembership(t, st, source.ID, other, "Archiv", 88, 9)
	target, err := st.IMAPArchiveTargetContext(t.Context(), id)
	require.NoError(t, err)
	m := store.IMAPPendingMove{SourceIdentifier: source.Identifier, Target: target, Destination: "Archiv", DestinationUIDValidity: 88}
	require.NoError(t, st.BeginIMAPMoveContext(t.Context(), m))
	m.DestinationUID = 9
	require.NoError(t, st.SaveIMAPMoveMappingContext(t.Context(), m))
	require.Error(t, st.ReconcileIMAPMoveContext(t.Context(), m))
	got, err := st.IMAPArchiveTargetContext(t.Context(), id)
	require.NoError(t, err)
	assert.Equal(t, target, got)
	pending, err := st.IMAPPendingMoveContext(t.Context(), source.ID, id)
	require.NoError(t, err)
	require.NotNil(t, pending)
	assert.Equal(t, uint32(9), pending.DestinationUID)
}

func TestIMAPArchiveStaleDestinationRefusedBeforeIntent(t *testing.T) {
	st, source, id := tagFixture(t, "imap", "INBOX|7")
	tagMembership(t, st, source.ID, id, "INBOX", 77, 7)
	_, err := st.DB().ExecContext(t.Context(), st.Rebind(`INSERT INTO imap_folder_state(source_id,mailbox,uidvalidity,uidnext) VALUES(?,?,?,?)`), source.ID, "Archiv", 88, 1)
	require.NoError(t, err)
	target, err := st.IMAPArchiveTargetContext(t.Context(), id)
	require.NoError(t, err)
	move := store.IMAPPendingMove{SourceIdentifier: source.Identifier, Target: target, Destination: "Archiv", DestinationUIDValidity: 89}
	require.ErrorContains(t, st.BeginIMAPMoveContext(t.Context(), move), "epoch changed")
	pending, err := st.IMAPPendingMoveContext(t.Context(), source.ID, id)
	require.NoError(t, err)
	assert.Nil(t, pending)
}
