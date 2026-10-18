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
	_, err = st.PersistMessage(&store.MessagePersistData{
		Message:      &store.Message{SourceID: source.ID, SourceMessageID: "calendar", MessageType: "calendar_event"},
		Conversation: &store.ConversationPersistData{SourceConversationID: "calendar", ConversationType: "calendar"},
		RawFormat:    "gcal_json", RawMIME: []byte(`{"id":"calendar","start":{"dateTime":"2026-08-01T10:00:00Z"},"end":{"dateTime":"2026-08-01T11:00:00Z"},"attendees":[{"email":"stored-room@example.org","resource":true}]}`),
	})
	requirements.NoError(err)
	snapshot := testSnapshot(source.ID)
	snapshot.Organizer = &Person{Email: "room@c.resource.calendar.google.com", Name: "Room", Anchor: "test:room"}
	snapshot.Attendees = []Person{
		{Email: "stored-room@example.org", Anchor: "test:stored-room"},
		{Email: "room@c.resource.calendar.google.com", Name: "Room", Anchor: "test:room"},
		{Email: "human@example.org", Name: "Human", OtherEmails: []string{"alias@resource.calendar.google.com", "stored-room@example.org"}, Anchor: "test:human"},
	}
	archiver := New(st)
	_, err = archiver.LinkIdentities(t.Context(), source.ID, append(snapshot.Attendees, *snapshot.Organizer))
	requirements.NoError(err)
	result, err := archiver.Upsert(t.Context(), snapshot, UpsertOptions{})
	requirements.NoError(err)
	var count int
	requirements.NoError(st.DB().QueryRow(`SELECT count(*) FROM participants WHERE email_address LIKE '%resource.calendar.google.com' OR email_address = 'stored-room@example.org'`).Scan(&count))
	assertions.Zero(count)
	requirements.NoError(st.DB().QueryRow(st.Rebind(`SELECT count(*) FROM message_recipients WHERE message_id = ? AND recipient_type = 'to'`), result.MessageID).Scan(&count))
	assertions.Equal(1, count)
	requirements.NoError(st.DB().QueryRow(st.Rebind(`SELECT count(*) FROM messages WHERE id = ? AND sender_id IS NOT NULL`), result.MessageID).Scan(&count))
	assertions.Zero(count)
}
