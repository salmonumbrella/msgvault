package calsync

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/gcal"
	"go.kenn.io/msgvault/internal/meetingarchive"
	"go.kenn.io/msgvault/internal/store"
)

func TestCalsyncSkipsRoomAttendeesAndOrganizer(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	mock := gcal.NewMockAPI()
	mock.Calendars = []gcal.Calendar{{ID: "primary", AccessRole: "owner", TimeZone: "UTC"}}
	event := timedEvent("planning", "Planning",
		gcal.Attendee{Email: "resource@example.com", Resource: true, Organizer: true},
		gcal.Attendee{Email: "room@c.resource.calendar.google.com"},
		gcal.Attendee{Email: "human@example.org", DisplayName: "Example Human"})
	event.Organizer = gcal.Person{Email: "RESOURCE@example.com", DisplayName: "Example Room"}
	mock.FullEvents["primary"] = [][]gcal.Event{{event}}
	syncer, st := newSyncer(t, mock, Options{})
	_, err := syncer.Full(t.Context())
	requirements.NoError(err)
	var count int
	requirements.NoError(st.DB().QueryRow(`SELECT count(*) FROM participants WHERE lower(email_address) IN ('resource@example.com','room@c.resource.calendar.google.com')`).Scan(&count))
	assertions.Zero(count)
	requirements.NoError(st.DB().QueryRow(`SELECT count(*) FROM message_recipients WHERE recipient_type = 'to'`).Scan(&count))
	assertions.Equal(1, count)
	requirements.NoError(st.DB().QueryRow(`SELECT count(*) FROM calendar_event_attendees WHERE role = 'room'`).Scan(&count))
	assertions.Equal(2, count)
	requirements.NoError(st.DB().QueryRow(`SELECT count(*) FROM messages WHERE sender_id IS NOT NULL`).Scan(&count))
	assertions.Zero(count)
	if !st.IsPostgreSQL() {
		requirements.NoError(st.DB().QueryRow(`SELECT count(*) FROM messages_fts WHERE messages_fts MATCH 'to_addr:resource'`).Scan(&count))
		assertions.Equal(1, count, "room addresses remain searchable")
	}
}

func TestCalsyncLateRoomEvidenceRemovesMeetingEdges(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	mock := gcal.NewMockAPI()
	mock.Calendars = []gcal.Calendar{{ID: "primary", AccessRole: "owner", TimeZone: "UTC"}}
	mock.FullEvents["primary"] = [][]gcal.Event{{timedEvent("planning", "Planning", gcal.Attendee{Email: "ROOM@example.org", Resource: true})}}
	syncer, st := newSyncer(t, mock, Options{})
	source, err := st.GetOrCreateSource("meeting_import", "notes@example.org")
	requirements.NoError(err)
	room := meetingarchive.Person{Email: "room@example.org"}
	_, err = meetingarchive.New(st).Upsert(t.Context(), meetingarchive.Snapshot{
		SourceID: source.ID, SourceMessageID: "note", Organizer: &room,
		Attendees: []meetingarchive.Person{room, {Email: "human@example.org"}},
	}, meetingarchive.UpsertOptions{})
	requirements.NoError(err)
	var roomID int64
	requirements.NoError(st.DB().QueryRow(`SELECT id FROM participants WHERE email_address = 'room@example.org'`).Scan(&roomID))
	// An ordinary email to the same address must survive the calendar evidence.
	_, err = st.PersistMessage(&store.MessagePersistData{
		Message:      &store.Message{SourceID: source.ID, SourceMessageID: "email", MessageType: "email", SenderID: sql.NullInt64{Int64: roomID, Valid: true}},
		Conversation: &store.ConversationPersistData{SourceConversationID: "email", ConversationType: "email", Participants: []store.ConversationParticipantRef{{ParticipantID: roomID, Role: "member"}}},
		Recipients:   []store.RecipientSet{{Type: "to", ParticipantIDs: []int64{roomID}}},
	})
	requirements.NoError(err)
	_, err = syncer.Full(t.Context())
	requirements.NoError(err)
	for _, query := range []string{
		`SELECT count(*) FROM message_recipients WHERE participant_id = ?`,
		`SELECT count(*) FROM conversation_participants WHERE participant_id = ?`,
		`SELECT count(*) FROM messages WHERE sender_id = ?`,
	} {
		var count int
		requirements.NoError(st.DB().QueryRow(st.Rebind(query), roomID).Scan(&count))
		assertions.Equal(1, count, "only the email edge remains: %s", query)
	}
	var humanCount int
	requirements.NoError(st.DB().QueryRow(`SELECT count(*) FROM message_recipients r JOIN participants p ON p.id = r.participant_id WHERE p.email_address = 'human@example.org'`).Scan(&humanCount))
	assertions.Equal(1, humanCount)
}
