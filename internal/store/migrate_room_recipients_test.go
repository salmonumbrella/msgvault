package store

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func seedLegacyRoomEdges(t *testing.T, st *Store) {
	t.Helper()
	requirements := require.New(t)
	source, err := st.GetOrCreateSource("meeting_import", "legacy@example.org")
	requirements.NoError(err)
	room, err := st.EnsureParticipant("room@c.resource.calendar.google.com", "Room", "c.resource.calendar.google.com")
	requirements.NoError(err)
	resource, err := st.EnsureParticipant("resource@example.org", "Resource", "example.org")
	requirements.NoError(err)
	human, err := st.EnsureParticipant("human@example.org", "Human", "example.org")
	requirements.NoError(err)
	for _, kind := range []string{"email", "calendar_event", "meeting_transcript"} {
		id, err := st.PersistMessage(&MessagePersistData{
			Message:      &Message{SourceID: source.ID, SourceMessageID: kind, MessageType: kind, SenderID: sql.NullInt64{Int64: room, Valid: true}},
			Conversation: &ConversationPersistData{SourceConversationID: kind, ConversationType: "meeting"},
		})
		requirements.NoError(err)
		var conversationID int64
		requirements.NoError(st.DB().QueryRow(st.Rebind(`SELECT conversation_id FROM messages WHERE id = ?`), id).Scan(&conversationID))
		for _, participant := range []int64{room, resource, human} {
			requirements.NoError(st.EnsureConversationParticipant(conversationID, participant, "member"))
		}
		for _, role := range []string{"from", "to"} {
			requirements.NoError(st.ReplaceMessageRecipients(id, role, []int64{room, resource, human}, []string{"Room", "Resource", "Human"}))
		}
		if kind == "calendar_event" {
			requirements.NoError(st.UpsertMessageRawWithFormat(id, []byte(`{"id":"event","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"},"attendees":[{"email":"resource@example.org","resource":true}]}`), "gcal_json"))
		}
	}
}

func assertRepairedRoomEdges(t *testing.T, st *Store) {
	t.Helper()
	assertions, requirements := assert.New(t), require.New(t)
	var count int
	requirements.NoError(st.DB().QueryRow(`SELECT count(*) FROM message_recipients mr JOIN messages m ON m.id = mr.message_id WHERE m.message_type IN ('calendar_event','meeting_transcript')`).Scan(&count))
	assertions.Equal(8, count, "human and established contact edges remain on both messages")
	requirements.NoError(st.DB().QueryRow(`SELECT count(*) FROM message_recipients mr JOIN messages m ON m.id = mr.message_id WHERE m.message_type = 'email'`).Scan(&count))
	assertions.Equal(6, count, "email room edges remain")
	requirements.NoError(st.DB().QueryRow(`SELECT count(*) FROM messages WHERE message_type IN ('calendar_event','meeting_transcript') AND sender_id IS NOT NULL`).Scan(&count))
	assertions.Zero(count)
	requirements.NoError(st.DB().QueryRow(`SELECT count(*) FROM conversation_participants cp WHERE EXISTS (SELECT 1 FROM messages m WHERE m.conversation_id = cp.conversation_id AND m.message_type IN ('calendar_event','meeting_transcript'))`).Scan(&count))
	assertions.Equal(4, count, "human and established contact roster edges remain on both conversations")
	requirements.NoError(st.DB().QueryRow(`SELECT count(*) FROM conversation_participants cp WHERE EXISTS (SELECT 1 FROM messages m WHERE m.conversation_id = cp.conversation_id AND m.message_type = 'email')`).Scan(&count))
	assertions.Equal(3, count, "email roster remains")
}

func TestRoomRecipientsMigration(t *testing.T) {
	requirements := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	seedLegacyRoomEdges(t, st)
	_, err := st.DB().Exec(st.Rebind(`DELETE FROM applied_migrations WHERE name = ?`), migrationRoomRecipientsV1)
	requirements.NoError(err)
	requirements.NoError(st.InitSchemaContext(t.Context()))
	assertRepairedRoomEdges(t, st)
	requirements.NoError(st.InitSchemaContext(t.Context()))
	assertRepairedRoomEdges(t, st)
	if !st.IsPostgreSQL() {
		var count int
		requirements.NoError(st.DB().QueryRow(`SELECT count(*) FROM cache_related_change_journal WHERE dataset = 'message_facts'`).Scan(&count))
		assert.Positive(t, count)
	}
}

func TestRoomRecipientsSubsetRepair(t *testing.T) {
	requirements := require.New(t)
	path := filepath.Join(t.TempDir(), "source.db")
	st, err := OpenForTest(path)
	requirements.NoError(err)
	t.Cleanup(func() { requirements.NoError(st.Close()) })
	requirements.NoError(st.InitSchema())
	seedLegacyRoomEdges(t, st)
	destination := t.TempDir()
	_, err = CopySubset(path, destination, 3, false)
	requirements.NoError(err)
	subset, err := OpenForTest(filepath.Join(destination, "msgvault.db"))
	requirements.NoError(err)
	defer func() { requirements.NoError(subset.Close()) }()
	assertRepairedRoomEdges(t, subset)
}

func TestRoomRecipientsMigrationPreservesContactHistory(t *testing.T) {
	requirements := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	seedLegacyRoomEdges(t, st)
	_, err := st.DB().Exec(st.Rebind(`DELETE FROM applied_migrations WHERE name = ?`), migrationRoomRecipientsV1)
	requirements.NoError(err)
	requirements.NoError(st.InitSchemaContext(t.Context()))
	var count int
	requirements.NoError(st.DB().QueryRow(`SELECT count(*) FROM message_recipients r JOIN participants p ON p.id = r.participant_id JOIN messages m ON m.id = r.message_id WHERE p.email_address = 'resource@example.org' AND m.message_type IN ('calendar_event','meeting_transcript')`).Scan(&count))
	assert.Equal(t, 4, count, "an invite's resource flag must preserve a contact's meeting edges")
}
