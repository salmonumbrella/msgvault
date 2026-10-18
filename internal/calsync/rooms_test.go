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

func TestCalsyncLateRoomEvidencePreservesContactEdges(t *testing.T) {
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
	// Ordinary email history establishes this address as a person.
	_, err = st.PersistMessage(&store.MessagePersistData{
		Message:      &store.Message{SourceID: source.ID, SourceMessageID: "email", MessageType: "email", SenderID: sql.NullInt64{Int64: roomID, Valid: true}},
		Conversation: &store.ConversationPersistData{SourceConversationID: "email", ConversationType: "email", Participants: []store.ConversationParticipantRef{{ParticipantID: roomID, Role: "member"}}},
		Recipients:   []store.RecipientSet{{Type: "to", ParticipantIDs: []int64{roomID}}},
	})
	requirements.NoError(err)
	_, err = syncer.Full(t.Context())
	requirements.NoError(err)
	for _, tc := range []struct {
		query string
		want  int
	}{
		{`SELECT count(*) FROM message_recipients WHERE participant_id = ?`, 4},
		{`SELECT count(*) FROM conversation_participants WHERE participant_id = ?`, 2},
		{`SELECT count(*) FROM messages WHERE sender_id = ?`, 2},
	} {
		var count int
		requirements.NoError(st.DB().QueryRow(st.Rebind(tc.query), roomID).Scan(&count))
		assertions.Equal(tc.want, count, "contact edges remain: %s", tc.query)
	}
	var humanCount int
	requirements.NoError(st.DB().QueryRow(`SELECT count(*) FROM message_recipients r JOIN participants p ON p.id = r.participant_id WHERE p.email_address = 'human@example.org'`).Scan(&humanCount))
	assertions.Equal(1, humanCount)
}

func TestCalsyncResourceFlagPreservesContactHistory(t *testing.T) {
	for _, edge := range []string{"sender", "recipient", "conversation", "chat"} {
		t.Run(edge, func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			mock := gcal.NewMockAPI()
			mock.Calendars = []gcal.Calendar{{ID: "primary", AccessRole: "owner", TimeZone: "UTC"}}
			event := timedEvent("planning", "Planning", gcal.Attendee{Email: "person@example.org", Resource: true, Organizer: true}, gcal.Attendee{Email: "unseen@example.org", Resource: true})
			event.Organizer = gcal.Person{Email: "person@example.org"}
			mock.FullEvents["primary"] = [][]gcal.Event{{event}}
			syncer, st := newSyncer(t, mock, Options{})
			source, err := st.GetOrCreateSource("meeting_import", "notes@example.org")
			requirements.NoError(err)
			personID, err := st.EnsureParticipant("person@example.org", "Person", "example.org")
			requirements.NoError(err)
			data := &store.MessagePersistData{
				Message:      &store.Message{SourceID: source.ID, SourceMessageID: "history", MessageType: "email"},
				Conversation: &store.ConversationPersistData{SourceConversationID: "history", ConversationType: "email"},
			}
			switch edge {
			case "sender":
				data.Message.SenderID = sql.NullInt64{Int64: personID, Valid: true}
			case "recipient":
				data.Recipients = []store.RecipientSet{{Type: "to", ParticipantIDs: []int64{personID}}}
			case "conversation", "chat":
				data.Conversation.Participants = []store.ConversationParticipantRef{{ParticipantID: personID, Role: "member"}}
				if edge == "chat" {
					data.Message.MessageType = "chat"
					data.Conversation.ConversationType = "direct_chat"
				}
			}
			_, err = st.PersistMessage(data)
			requirements.NoError(err)
			person := meetingarchive.Person{Email: "person@example.org"}
			archive := meetingarchive.New(st)
			snapshot := meetingarchive.Snapshot{SourceID: source.ID, SourceMessageID: "before", Organizer: &person, Attendees: []meetingarchive.Person{person}}
			before, err := archive.Upsert(t.Context(), snapshot, meetingarchive.UpsertOptions{})
			requirements.NoError(err)
			_, err = syncer.Full(t.Context())
			requirements.NoError(err)
			snapshot.SourceMessageID = "after"
			snapshot.Attendees = append(snapshot.Attendees, meetingarchive.Person{Email: "unseen@example.org"})
			after, err := archive.Upsert(t.Context(), snapshot, meetingarchive.UpsertOptions{})
			requirements.NoError(err)
			for _, id := range []int64{before.MessageID, after.MessageID} {
				var sender sql.NullInt64
				requirements.NoError(st.DB().QueryRow(st.Rebind(`SELECT sender_id FROM messages WHERE id = ?`), id).Scan(&sender))
				assertions.Equal(sql.NullInt64{Int64: personID, Valid: true}, sender)
				var recipients int
				requirements.NoError(st.DB().QueryRow(st.Rebind(`SELECT count(*) FROM message_recipients WHERE message_id = ? AND participant_id = ?`), id, personID).Scan(&recipients))
				assertions.Equal(2, recipients)
			}
			var sender sql.NullInt64
			requirements.NoError(st.DB().QueryRow(`SELECT sender_id FROM messages WHERE message_type = 'calendar_event'`).Scan(&sender))
			assertions.Equal(sql.NullInt64{Int64: personID, Valid: true}, sender)
			var unseen int
			requirements.NoError(st.DB().QueryRow(`SELECT count(*) FROM participants WHERE email_address = 'unseen@example.org'`).Scan(&unseen))
			assertions.Equal(1, unseen, "an event resource flag must not suppress an attendee in another meeting")
		})
	}
}
