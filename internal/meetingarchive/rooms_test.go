package meetingarchive

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestArchiverExcludesRooms(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("meeting_import", "rooms@example.org")
	requirements.NoError(err)
	snapshot := testSnapshot(source.ID)
	snapshot.Organizer = &Person{Email: "room@c.resource.calendar.google.com", Name: "Room", Anchor: "test:room"}
	snapshot.Attendees = []Person{
		{Email: "room@c.resource.calendar.google.com", Name: "Room", Anchor: "test:room"},
		{Email: "human@example.org", Name: "Human", OtherEmails: []string{"alias@example.org"}, Anchor: "test:human"},
	}
	archiver := New(st)
	_, err = archiver.LinkIdentities(t.Context(), source.ID, append(snapshot.Attendees, *snapshot.Organizer))
	requirements.NoError(err)
	result, err := archiver.Upsert(t.Context(), snapshot, UpsertOptions{})
	requirements.NoError(err)
	var count int
	requirements.NoError(st.DB().QueryRow(`SELECT count(*) FROM participants WHERE email_address LIKE '%resource.calendar.google.com'`).Scan(&count))
	assertions.Zero(count)
	requirements.NoError(st.DB().QueryRow(st.Rebind(`SELECT count(*) FROM message_recipients WHERE message_id = ? AND recipient_type = 'to'`), result.MessageID).Scan(&count))
	assertions.Equal(1, count)
	requirements.NoError(st.DB().QueryRow(`SELECT count(*) FROM conversation_participants`).Scan(&count))
	assertions.Equal(1, count, "only the human joins the conversation roster")
	requirements.NoError(st.DB().QueryRow(st.Rebind(`SELECT count(*) FROM messages WHERE id = ? AND sender_id IS NOT NULL`), result.MessageID).Scan(&count))
	assertions.Zero(count)
}

func TestArchiverDropsWholeRoomIdentity(t *testing.T) {
	for _, tc := range []struct {
		name   string
		person Person
		stored bool
	}{
		{"stored resource", Person{Email: " ROOM@example.org ", Phone: "+15550100001", Anchor: "test:room"}, true},
		{"resource with phone", Person{Email: "c@resource.calendar.google.com", Phone: "+15550100001", Anchor: "test:room"}, false},
		{"resource with participant", Person{Email: "c@resource.calendar.google.com", ParticipantID: 1}, false},
		{"secondary room email", Person{Email: "alias@example.org", OtherEmails: []string{"c@resource.calendar.google.com"}, Anchor: "test:room"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			st := testutil.NewTestStore(t)
			source, err := st.GetOrCreateSource("meeting_import", "rooms@example.org")
			requirements.NoError(err)
			pid, err := st.EnsureParticipant("existing@example.org", "Existing", "")
			requirements.NoError(err)
			if tc.person.ParticipantID != 0 {
				tc.person.ParticipantID = pid
			}
			if tc.stored {
				_, err := st.PersistMessage(&store.MessagePersistData{
					Message:      &store.Message{SourceID: source.ID, SourceMessageID: "calendar", MessageType: "calendar_event"},
					Conversation: &store.ConversationPersistData{SourceConversationID: "calendar", ConversationType: "calendar"},
					RawFormat:    "gcal_json", RawMIME: []byte(`{"id":"calendar","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"},"attendees":[{"email":"room@example.org","resource":true}]}`),
				})
				requirements.NoError(err)
			}
			snapshot := testSnapshot(source.ID)
			snapshot.Organizer = &tc.person
			snapshot.Attendees = []Person{tc.person}
			archiver := New(st)
			_, err = archiver.LinkIdentities(t.Context(), source.ID, snapshot.Attendees)
			requirements.NoError(err)
			result, err := archiver.Upsert(t.Context(), snapshot, UpsertOptions{})
			requirements.NoError(err)
			var count int
			requirements.NoError(st.DB().QueryRow(`SELECT count(*) FROM participants`).Scan(&count))
			assertions.Equal(1, count, "room identifiers must not create participants")
			requirements.NoError(st.DB().QueryRow(st.Rebind(`SELECT count(*) FROM message_recipients WHERE message_id = ?`), result.MessageID).Scan(&count))
			assertions.Zero(count)
			requirements.NoError(st.DB().QueryRow(`SELECT count(*) FROM conversation_participants`).Scan(&count))
			assertions.Zero(count)
			requirements.NoError(st.DB().QueryRow(st.Rebind(`SELECT count(*) FROM messages WHERE id = ? AND sender_id IS NOT NULL`), result.MessageID).Scan(&count))
			assertions.Zero(count)
		})
	}
}
