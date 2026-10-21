package store_test

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/conversationpolicy"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestCuratedContactPointMutationInvalidatesParticipantProjectionAndContextEmbeddings(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	s := testutil.NewTestStore(t)

	source, err := s.GetOrCreateSource("beeper", "curated-contact-invalidation")
	requirements.NoError(err)
	room, err := s.EnsureConversation(source.ID, "room", "Room")
	requirements.NoError(err)
	requirements.NoError(s.SetConversationPolicyOverride(t.Context(), source.ID, "room", conversationpolicy.Full))
	participantID, err := s.EnsureParticipantByIdentifier("beeper", "synthetic:curated-contact", "")
	requirements.NoError(err)
	address := "curated-contact@example.test"
	_, err = s.DB().ExecContext(t.Context(), s.Rebind(
		`UPDATE participants SET email_address=? WHERE id=?`), address, participantID)
	requirements.NoError(err)
	messageID, err := s.UpsertMessage(&store.Message{
		SourceID: source.ID, ConversationID: room, SourceMessageID: "context-message",
		MessageType: "beeper", SenderID: sql.NullInt64{Int64: participantID, Valid: true},
	})
	requirements.NoError(err)
	requirements.NoError(s.UpsertMessageBodyContext(t.Context(), messageID,
		sql.NullString{String: "Synthetic context message", Valid: true}, sql.NullString{}))
	person, _, err := s.CreatePersonFromParticipantContext(t.Context(), participantID)
	requirements.NoError(err)
	assertions.Contains(person.ParticipantIDs, participantID)
	requirements.NoError(s.EnableEmbeddingChangeJournal(t.Context()))

	readEmailEligibility := func() bool {
		var eligible bool
		requirements.NoError(s.DB().QueryRowContext(t.Context(), s.Rebind(`
			SELECT `+store.FullParticipantValueWhere("p.id", "'email'", "p.email_address")+`
			FROM participants p WHERE p.id=?`), participantID).Scan(&eligible))
		return eligible
	}
	readDisplay := func() string {
		var display string
		requirements.NoError(s.DB().QueryRowContext(t.Context(), s.Rebind(
			`SELECT `+store.FullParticipantDisplaySQL("p")+` FROM participants p WHERE p.id=?`), participantID).Scan(&display))
		return display
	}
	readDisplayRevision := func() int64 {
		value, err := s.ParticipantDisplayNameRevisionContext(t.Context())
		requirements.NoError(err)
		return value
	}
	readEmbeddingSequence := func() int64 {
		value, err := s.LatestEmbeddingChangeSequence(t.Context())
		requirements.NoError(err)
		return value
	}
	hasParticipantDisplayChange := func(changes []store.EmbeddingChange) bool {
		for _, change := range changes {
			if change.Kind == store.EmbeddingChangeParticipantDisplayName &&
				change.ParticipantID.Valid && change.ParticipantID.Int64 == participantID {
				return true
			}
		}
		return false
	}
	assertions.False(readEmailEligibility(), "a curated contact has not been added yet")
	assertions.Empty(readDisplay())
	beforeRevision := readDisplayRevision()
	beforeSequence := readEmbeddingSequence()

	point, err := s.AddPersonContactPointContext(t.Context(), person.ID, store.PersonContactPointInput{
		AddressKind: store.ContactAddressEmail, OriginalValue: address,
		Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser},
	})
	requirements.NoError(err)
	assertions.Equal(store.ProvenanceUser, point.Envelope.Source)
	addedRevision := readDisplayRevision()
	assertions.Greater(addedRevision, beforeRevision,
		"curated contact support changes the participant projection's visible email")
	assertions.True(readEmailEligibility())
	assertions.Equal(address, readDisplay())
	addedChanges, err := s.ScanEmbeddingChanges(t.Context(), beforeSequence, 20)
	requirements.NoError(err)
	assertions.True(hasParticipantDisplayChange(addedChanges))

	beforeSupersedeSequence := readEmbeddingSequence()
	requirements.NoError(s.SupersedePersonContactPointContext(t.Context(), person.ID, point.Envelope.ID, nil))
	assertions.Greater(readDisplayRevision(), addedRevision)
	assertions.False(readEmailEligibility())
	assertions.Empty(readDisplay())
	withdrawnChanges, err := s.ScanEmbeddingChanges(t.Context(), beforeSupersedeSequence, 20)
	requirements.NoError(err)
	assertions.True(hasParticipantDisplayChange(withdrawnChanges))
}
