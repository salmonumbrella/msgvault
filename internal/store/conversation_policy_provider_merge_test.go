package store_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/conversationpolicy"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestParticipantMergeKeepsProviderIdentityBoundToFullObservation(t *testing.T) {
	for _, test := range []struct {
		name           string
		fullIsSurvivor bool
		fullProviderID *string
		coldProviderID string
		wantProviderID *string
	}{
		{
			name:           "cold binding cannot fill a Full survivor",
			fullIsSurvivor: true,
			coldProviderID: "cold-provider-user",
		},
		{
			name:           "Full binding replaces cold survivor binding",
			fullProviderID: new("full-provider-user"),
			coldProviderID: "cold-provider-user",
			wantProviderID: new("full-provider-user"),
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			s := testutil.NewTestStore(t)
			source, err := s.GetOrCreateSource("beeper", "participant-provider-merge")
			requirements.NoError(err)
			fullRoom, err := s.EnsureConversation(source.ID, "full", "Full")
			requirements.NoError(err)
			coldRoom, err := s.EnsureConversation(source.ID, "cold", "Cold")
			requirements.NoError(err)
			requirements.NoError(s.SetConversationPolicyOverride(t.Context(), source.ID, "full", conversationpolicy.Full))
			requirements.NoError(s.SetConversationPolicyOverride(t.Context(), source.ID, "cold", conversationpolicy.Cold))

			fullParticipant, err := s.EnsureParticipantByIdentifier("beeper", "full-provider-merge", "Full Participant")
			requirements.NoError(err)
			coldParticipant, err := s.EnsureParticipantByIdentifier("beeper", "cold-provider-merge", "Cold Participant")
			requirements.NoError(err)
			fullInput := store.ParticipantContactObservationInput{
				SourceID: &source.ID, ConversationID: &fullRoom,
				AddressKind: store.ContactAddressEmail, OriginalValue: "shared@example.test",
				ProviderUserID: test.fullProviderID,
				Envelope:       store.ValueEnvelopeInput{Source: store.ProvenanceArchiveObservation},
			}
			_, err = s.RecordContactObservationContext(t.Context(), fullParticipant, fullInput)
			requirements.NoError(err)
			coldInput := fullInput
			coldInput.ConversationID = &coldRoom
			coldInput.ProviderUserID = new(test.coldProviderID)
			_, err = s.RecordContactObservationContext(t.Context(), coldParticipant, coldInput)
			requirements.NoError(err)

			absorbed, survivor := coldParticipant, fullParticipant
			if !test.fullIsSurvivor {
				absorbed, survivor = fullParticipant, coldParticipant
			}
			requirements.NoError(s.MergeParticipantsContext(t.Context(), absorbed, survivor))
			observations, err := s.ListFullParticipantObservationsContext(t.Context(), survivor)
			requirements.NoError(err)
			requirements.Len(observations, 1)
			assertions.Equal(test.wantProviderID, observations[0].ProviderUserID,
				"the provider binding must retain its own Full-room support")
		})
	}
}

func TestSourceMergeKeepsProviderIdentityBoundToFullObservation(t *testing.T) {
	for _, test := range []struct {
		name           string
		fromProviderID *string
		intoProviderID *string
		wantProviderID *string
	}{
		{
			name:           "Full address support cannot authorize a cold destination binding",
			intoProviderID: new("cold-provider-user"),
		},
		{
			name:           "Full source binding survives source merge",
			fromProviderID: new("full-provider-user"),
			wantProviderID: new("full-provider-user"),
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			s := testutil.NewTestStore(t)
			from, err := s.GetOrCreateSource("beeper", "provider-merge-from")
			requirements.NoError(err)
			into, err := s.GetOrCreateSource("beeper", "provider-merge-into")
			requirements.NoError(err)
			fromFullRoom, err := s.EnsureConversation(from.ID, "shared", "Shared")
			requirements.NoError(err)
			_, err = s.EnsureConversation(into.ID, "shared", "Shared")
			requirements.NoError(err)
			intoColdRoom, err := s.EnsureConversation(into.ID, "cold-support", "Cold Support")
			requirements.NoError(err)
			requirements.NoError(s.SetConversationPolicyOverride(t.Context(), from.ID, "shared", conversationpolicy.Full))
			requirements.NoError(s.SetConversationPolicyOverride(t.Context(), into.ID, "shared", conversationpolicy.Full))
			requirements.NoError(s.SetConversationPolicyOverride(t.Context(), into.ID, "cold-support", conversationpolicy.Cold))
			participantID, err := s.EnsureParticipantByIdentifier("beeper", "source-provider-merge", "Source Merge Participant")
			requirements.NoError(err)

			fromInput := store.ParticipantContactObservationInput{
				SourceID: &from.ID, ConversationID: &fromFullRoom,
				AddressKind: store.ContactAddressEmail, OriginalValue: "shared@example.test",
				ProviderUserID: test.fromProviderID,
				Envelope:       store.ValueEnvelopeInput{Source: store.ProvenanceArchiveObservation},
			}
			_, err = s.RecordContactObservationContext(t.Context(), participantID, fromInput)
			requirements.NoError(err)
			intoInput := fromInput
			intoInput.SourceID = &into.ID
			intoInput.ConversationID = &intoColdRoom
			intoInput.ProviderUserID = test.intoProviderID
			_, err = s.RecordContactObservationContext(t.Context(), participantID, intoInput)
			requirements.NoError(err)

			_, err = s.MergeSourcesContext(t.Context(), store.MergeSourcesRequest{FromSourceID: from.ID, IntoSourceID: into.ID})
			requirements.NoError(err)
			observations, err := s.ListFullParticipantObservationsContext(t.Context(), participantID)
			requirements.NoError(err)
			requirements.Len(observations, 1)
			assertions.Equal(test.wantProviderID, observations[0].ProviderUserID,
				"source merge must preserve only provider IDs with Full-origin evidence")
		})
	}
}
