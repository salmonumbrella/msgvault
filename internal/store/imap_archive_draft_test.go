package store_test

import (
	"database/sql"
	"fmt"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/emailtags"
	"go.kenn.io/msgvault/internal/store"
	"os"
	"testing"
)

func TestIMAPArchiveMovesManagedDraftReceipt(t *testing.T) {
	st, source, draft, _ := newReviewManagedDraft(t, "terra-move", 21, "original")
	if os.Getenv("MSGVAULT_TEST_DB") != "" {
		require.True(t, st.IsPostgreSQL())
	}
	_, err := st.DB().ExecContext(t.Context(), st.Rebind(`INSERT INTO imap_folder_state(source_id, mailbox, uidvalidity, uidnext) VALUES(?,?,?,?)`), source.ID, draft.CurrentReceipt.Mailbox, draft.CurrentReceipt.UIDValidity, 22)
	require.NoError(t, err)
	target, err := st.EmailTagTargetContext(t.Context(), draft.CurrentMessageID, "")
	require.NoError(t, err)
	move := store.IMAPPendingMove{SourceIdentifier: source.Identifier, Target: target, Destination: "Archiv", DestinationUIDValidity: 88}
	require.NoError(t, st.BeginIMAPMoveContext(t.Context(), move))
	move.DestinationUID = 9
	require.NoError(t, st.SaveIMAPMoveMappingContext(t.Context(), move))
	dbPath := store.DBPathForTest(st)
	require.NoError(t, st.Close())
	reopened, err := store.Open(dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = reopened.Close() })
	require.NoError(t, reopened.ReconcileIMAPMoveContext(t.Context(), move))
	saved, err := reopened.GetIMAPDraftContext(t.Context(), draft.DraftID)
	require.NoError(t, err)
	current, err := reopened.EmailTagTargetContext(t.Context(), draft.CurrentMessageID, "")
	require.NoError(t, err)
	require.Equal(t, current.Mailbox, saved.CurrentReceipt.Mailbox, "draft edit/delete must use the relocated copy")
	require.Equal(t, current.UIDValidity, saved.CurrentReceipt.UIDValidity)
	require.Equal(t, current.UID, saved.CurrentReceipt.UID)
	move = store.IMAPPendingMove{SourceIdentifier: source.Identifier, Target: current, Destination: "INBOX", DestinationUIDValidity: 99}
	require.NoError(t, reopened.BeginIMAPMoveContext(t.Context(), move))
	move.DestinationUID = 10
	require.NoError(t, reopened.SaveIMAPMoveMappingContext(t.Context(), move))
	require.NoError(t, reopened.ReconcileIMAPMoveContext(t.Context(), move))
	restored, err := reopened.GetIMAPDraftContext(t.Context(), draft.DraftID)
	require.NoError(t, err)
	require.Equal(t, "INBOX", restored.CurrentReceipt.Mailbox)
	require.EqualValues(t, 99, restored.CurrentReceipt.UIDValidity)
	require.EqualValues(t, 10, restored.CurrentReceipt.UID)
}

func archiveRefusesPendingDraftDelete(t *testing.T) {
	t.Helper()
	st, source, draft, _ := newReviewManagedDraft(t, "terra-pending", 21, "original")
	if os.Getenv("MSGVAULT_TEST_DB") != "" {
		require.True(t, st.IsPostgreSQL())
	}
	_, err := st.DB().ExecContext(t.Context(), st.Rebind(`INSERT INTO imap_folder_state(source_id, mailbox, uidvalidity, uidnext) VALUES(?,?,?,?)`), source.ID, draft.CurrentReceipt.Mailbox, draft.CurrentReceipt.UIDValidity, 22)
	require.NoError(t, err)
	target, err := st.EmailTagTargetContext(t.Context(), draft.CurrentMessageID, "")
	require.NoError(t, err)
	_, err = st.ClaimIMAPDraftContext(t.Context(), draft.DraftID, draft.Revision, store.IMAPDraftOperationDelete, nil)
	require.NoError(t, err)
	move := store.IMAPPendingMove{SourceIdentifier: source.Identifier, Target: target, Destination: "Archiv", DestinationUIDValidity: 88}
	err = st.BeginIMAPMoveContext(t.Context(), move)
	var failure *emailtags.MessageTagError
	require.ErrorAs(t, err, &failure)
	require.Equal(t, "stale_identity", failure.Code)
	require.ErrorIs(t, err, store.ErrIMAPDraftPending)
}

func TestIMAPDraftClaimRefusesPendingMove(t *testing.T) {
	for _, mapped := range []bool{false, true} {
		t.Run(fmt.Sprintf("mapped=%t", mapped), func(t *testing.T) {
			st, source, draft, _ := newReviewManagedDraft(t, "move-claim-delete", 31, "original")
			if os.Getenv("MSGVAULT_TEST_DB") != "" {
				require.True(t, st.IsPostgreSQL())
			}
			_, err := st.DB().ExecContext(t.Context(), st.Rebind(`INSERT INTO imap_folder_state(source_id, mailbox, uidvalidity, uidnext) VALUES(?,?,?,?)`), source.ID, draft.CurrentReceipt.Mailbox, draft.CurrentReceipt.UIDValidity, 32)
			require.NoError(t, err)
			target, err := st.EmailTagTargetContext(t.Context(), draft.CurrentMessageID, "")
			require.NoError(t, err)
			move := store.IMAPPendingMove{SourceIdentifier: source.Identifier, Target: target, Destination: "Archiv", DestinationUIDValidity: 88}
			require.NoError(t, st.BeginIMAPMoveContext(t.Context(), move))
			if mapped {
				move.DestinationUID = 9
				require.NoError(t, st.SaveIMAPMoveMappingContext(t.Context(), move))
			}
			_, err = st.ClaimIMAPDraftContext(t.Context(), draft.DraftID, draft.Revision, store.IMAPDraftOperationDelete, nil)
			require.ErrorIs(t, err, store.ErrIMAPDraftPending, "draft mutations must not invalidate pending MOVE evidence")
		})
	}
}

func archiveRefusesPendingDraftReplacement(t *testing.T) {
	t.Helper()
	st, source, draft, conversationID := newReviewManagedDraft(t, "pending-replacement", 21, "original")
	if os.Getenv("MSGVAULT_TEST_DB") != "" {
		require.True(t, st.IsPostgreSQL())
	}
	_, err := st.DB().ExecContext(t.Context(), st.Rebind(`INSERT INTO imap_folder_state(source_id, mailbox, uidvalidity, uidnext) VALUES(?,?,?,?)`), source.ID, "Drafts", 1, 23)
	require.NoError(t, err)
	raw := []byte("From: alice@example.com\r\nTo: bob@example.com\r\n\r\ncandidate\r\n")
	_, err = st.ClaimIMAPDraftContext(t.Context(), draft.DraftID, draft.Revision, store.IMAPDraftOperationEdit, raw)
	require.NoError(t, err)
	replacement := store.IMAPDraftReceipt{SourceID: source.ID, Mailbox: "Drafts", UIDValidity: 1, UID: 22}
	require.NoError(t, st.RecordIMAPDraftOutcomeContext(t.Context(), draft.DraftID, draft.Revision, "append_uidplus", &replacement))
	published, err := st.PublishIMAPDraftReplacementContext(t.Context(), draft.DraftID, draft.Revision, nil, func([]int64) *store.MessagePersistData {
		return &store.MessagePersistData{Message: &store.Message{SourceID: source.ID, SourceMessageID: store.IMAPDraftSourceMessageID(replacement), MessageType: store.MessageTypeEmail, ConversationID: conversationID}, BodyText: sql.NullString{String: "candidate", Valid: true}, RawMIME: raw}
	})
	require.NoError(t, err)
	require.NotNil(t, published.Pending)
	for _, id := range []int64{draft.CurrentMessageID, published.CurrentMessageID} {
		target, targetErr := st.EmailTagTargetContext(t.Context(), id, "")
		require.NoError(t, targetErr)
		err = st.BeginIMAPMoveContext(t.Context(), store.IMAPPendingMove{SourceIdentifier: source.Identifier, Target: target, Destination: "Archiv", DestinationUIDValidity: 88})
		var failure *emailtags.MessageTagError
		require.ErrorAs(t, err, &failure)
		require.Equal(t, "stale_identity", failure.Code)
		require.ErrorIs(t, err, store.ErrIMAPDraftPending, "neither pending edit copy may be moved")
	}
}

func TestIMAPArchiveRefusesPendingDraft(t *testing.T) {
	t.Run("delete", archiveRefusesPendingDraftDelete)
	t.Run("edit_with_replacement", archiveRefusesPendingDraftReplacement)
}
