package activity

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"go.kenn.io/msgvault/internal/store"
)

func TestClassifyDirectionRolesAndEvidence(t *testing.T) {
	tests := []struct {
		name          string
		candidate     store.ActivityCandidate
		maxDirect     int
		wantRefKind   store.ActivityRefKind
		wantChannel   store.ActivityChannel
		wantDirection store.ActivityDirection
		wantOwner     string
		wantPersons   []store.ActivityEventPerson
	}{
		{
			name: "inbound email credits only the sender",
			candidate: store.ActivityCandidate{
				MessageID:        1,
				SourceID:         7,
				MessageType:      "email",
				ConversationType: "email_thread",
				Counterparts: []store.ActivityCounterpart{
					{ParticipantID: 10, PersonID: new(int64(100)), RecipientType: "from"},
					{ParticipantID: 11, RecipientType: "to", IsOwner: true, OwnerAddress: "me@example.com"},
					{ParticipantID: 12, PersonID: new(int64(101)), RecipientType: "cc"},
				},
			},
			maxDirect:     25,
			wantRefKind:   store.RefKindMessage,
			wantChannel:   store.ChannelEmail,
			wantDirection: store.DirectionInbound,
			wantOwner:     "me@example.com",
			wantPersons: []store.ActivityEventPerson{
				{PersonID: 100, Role: store.RoleSender, Evidence: store.EvidenceDirect},
				{PersonID: 101, Role: store.RoleAddressed, Evidence: store.EvidenceCoPresence},
			},
		},
		{
			name: "outbound email treats to cc and bcc alike below threshold",
			candidate: store.ActivityCandidate{
				MessageID:        2,
				SourceID:         7,
				MessageType:      "email",
				ConversationType: "email_thread",
				Counterparts: []store.ActivityCounterpart{
					{ParticipantID: 11, RecipientType: "from", IsOwner: true, OwnerAddress: "me@example.com"},
					{ParticipantID: 12, PersonID: new(int64(101)), RecipientType: "to"},
					{ParticipantID: 13, PersonID: new(int64(102)), RecipientType: "cc"},
					{ParticipantID: 14, PersonID: new(int64(103)), RecipientType: "bcc"},
				},
			},
			maxDirect:     3,
			wantRefKind:   store.RefKindMessage,
			wantChannel:   store.ChannelEmail,
			wantDirection: store.DirectionOutbound,
			wantOwner:     "me@example.com",
			wantPersons: []store.ActivityEventPerson{
				{PersonID: 101, Role: store.RoleAddressed, Evidence: store.EvidenceDirect},
				{PersonID: 102, Role: store.RoleAddressed, Evidence: store.EvidenceDirect},
				{PersonID: 103, Role: store.RoleAddressed, Evidence: store.EvidenceDirect},
			},
		},
		{
			name: "outbound broadcast above threshold is co-presence",
			candidate: store.ActivityCandidate{
				MessageID:        3,
				MessageType:      "chat",
				ConversationType: "group_chat",
				Counterparts: []store.ActivityCounterpart{
					{ParticipantID: 11, RecipientType: "from", IsOwner: true},
					{ParticipantID: 12, PersonID: new(int64(101)), RecipientType: "member"},
					{ParticipantID: 13, PersonID: new(int64(102)), RecipientType: "member"},
				},
			},
			maxDirect:     1,
			wantRefKind:   store.RefKindMessage,
			wantChannel:   store.ChannelChat,
			wantDirection: store.DirectionOutbound,
			wantPersons: []store.ActivityEventPerson{
				{PersonID: 101, Role: store.RoleMember, Evidence: store.EvidenceCoPresence},
				{PersonID: 102, Role: store.RoleMember, Evidence: store.EvidenceCoPresence},
			},
		},
		{
			name: "observed chat is co-presence",
			candidate: store.ActivityCandidate{
				MessageID:        4,
				MessageType:      "chat",
				ConversationType: "direct_chat",
				Counterparts: []store.ActivityCounterpart{
					{ParticipantID: 20, PersonID: new(int64(200)), RecipientType: "from"},
					{ParticipantID: 21, PersonID: new(int64(201)), RecipientType: "member"},
				},
			},
			maxDirect:     25,
			wantRefKind:   store.RefKindMessage,
			wantChannel:   store.ChannelChat,
			wantDirection: store.DirectionObserved,
			wantPersons: []store.ActivityEventPerson{
				{PersonID: 200, Role: store.RoleSender, Evidence: store.EvidenceCoPresence},
				{PersonID: 201, Role: store.RoleMember, Evidence: store.EvidenceCoPresence},
			},
		},
		{
			name: "owner-organized meeting credits attendees",
			candidate: store.ActivityCandidate{
				MessageID:   5,
				SourceID:    8,
				MessageType: "calendar_event",
				Counterparts: []store.ActivityCounterpart{
					{ParticipantID: 30, RecipientType: "from", IsOwner: true, OwnerAddress: "owner@example.com"},
					{ParticipantID: 31, PersonID: new(int64(300)), RecipientType: "to"},
				},
			},
			maxDirect:     25,
			wantRefKind:   store.RefKindMeeting,
			wantChannel:   store.ChannelMeeting,
			wantDirection: store.DirectionOutbound,
			wantOwner:     "owner@example.com",
			wantPersons: []store.ActivityEventPerson{
				{PersonID: 300, Role: store.RoleAttendee, Evidence: store.EvidenceDirect},
			},
		},
		{
			name: "counterpart-organized meeting credits only organizer",
			candidate: store.ActivityCandidate{
				MessageID:   6,
				SourceID:    8,
				MessageType: "calendar_event",
				Counterparts: []store.ActivityCounterpart{
					{ParticipantID: 30, PersonID: new(int64(300)), RecipientType: "from"},
					{ParticipantID: 31, RecipientType: "to", IsOwner: true, OwnerAddress: "owner@example.com"},
					{ParticipantID: 32, PersonID: new(int64(301)), RecipientType: "to"},
				},
			},
			maxDirect:     25,
			wantRefKind:   store.RefKindMeeting,
			wantChannel:   store.ChannelMeeting,
			wantDirection: store.DirectionInbound,
			wantOwner:     "owner@example.com",
			wantPersons: []store.ActivityEventPerson{
				{PersonID: 300, Role: store.RoleOrganizer, Evidence: store.EvidenceDirect},
				{PersonID: 301, Role: store.RoleAttendee, Evidence: store.EvidenceCoPresence},
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert := assert.New(t)
			got := Classify(test.candidate, test.maxDirect)
			assert.Equal(test.wantRefKind, got.RefKind)
			assert.Equal(test.wantChannel, got.Channel)
			assert.Equal(test.wantDirection, got.Direction)
			assert.Equal(test.wantOwner, got.OwnerAddress)
			assert.Equal(test.wantPersons, got.Persons)
			if test.wantDirection == store.DirectionObserved {
				assert.Nil(got.OwnerSourceID)
			} else {
				requireSourceID := test.candidate.SourceID
				if requireSourceID == 0 {
					assert.Nil(got.OwnerSourceID)
				} else if assert.NotNil(got.OwnerSourceID) {
					assert.Equal(requireSourceID, *got.OwnerSourceID)
				}
			}
		})
	}
}

func TestClassifyKeepsOneStrongestDeterministicLinkPerPerson(t *testing.T) {
	got := Classify(store.ActivityCandidate{
		MessageID:        7,
		MessageType:      "email",
		ConversationType: "email_thread",
		Counterparts: []store.ActivityCounterpart{
			{ParticipantID: 40, PersonID: new(int64(400)), RecipientType: "from"},
			{ParticipantID: 41, RecipientType: "to", IsOwner: true},
			{ParticipantID: 42, PersonID: new(int64(400)), RecipientType: "cc"},
			{ParticipantID: 43, PersonID: new(int64(401)), RecipientType: "member"},
			{ParticipantID: 44, PersonID: new(int64(401)), RecipientType: "to"},
			{ParticipantID: 45, PersonID: new(int64(999)), RecipientType: "to", IsOwner: true},
		},
	}, 25)

	assert.Equal(t, []store.ActivityEventPerson{
		{PersonID: 400, Role: store.RoleSender, Evidence: store.EvidenceDirect},
		{PersonID: 401, Role: store.RoleAddressed, Evidence: store.EvidenceCoPresence},
	}, got.Persons)
}

func TestClassifyDefaultsInvalidThresholdAndOtherChannel(t *testing.T) {
	counterparts := []store.ActivityCounterpart{
		{ParticipantID: 1, RecipientType: "from", IsOwner: true},
	}
	for id := int64(2); id <= DefaultMaxDirectCounterparts+1; id++ {
		counterparts = append(counterparts, store.ActivityCounterpart{
			ParticipantID: id,
			PersonID:      new(id),
			RecipientType: "to",
		})
	}

	got := Classify(store.ActivityCandidate{
		MessageID:        8,
		MessageType:      "unknown",
		ConversationType: "unknown",
		Counterparts:     counterparts,
	}, 0)

	assert.Equal(t, store.ChannelOther, got.Channel)
	assert.Len(t, got.Persons, DefaultMaxDirectCounterparts)
	for _, link := range got.Persons {
		assert.Equal(t, store.EvidenceDirect, link.Evidence)
	}
}
