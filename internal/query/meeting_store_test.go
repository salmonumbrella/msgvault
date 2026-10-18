package query

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/meetingarchive"
	"go.kenn.io/msgvault/internal/store"
)

func newMeetingCacheStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.OpenForTest(filepath.Join(t.TempDir(), "archive.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, st.Close()) })
	require.NoError(t, st.InitSchema())
	return st
}

// Feed persisted messages and edges through the existing Parquet fixture builder.
func addStoredMeeting(t *testing.T, b *TestDataBuilder, st *store.Store, id int64) {
	t.Helper()
	var opt MessageOpt
	var sender sql.NullInt64
	require.NoError(t, st.DB().QueryRow(`SELECT source_id, conversation_id, message_type, sender_id FROM messages WHERE id = ?`, id).Scan(&opt.SourceID, &opt.ConversationID, &opt.MessageType, &sender))
	if sender.Valid {
		opt.SenderID = &sender.Int64
	}
	opt.ConversationType = "meeting"
	b.nextMsgID = id
	b.AddMessage(opt)
	rows, err := st.DB().Query(`SELECT participant_id, recipient_type, coalesce(display_name, '') FROM message_recipients WHERE message_id = ?`, id)
	require.NoError(t, err)
	defer func() { require.NoError(t, rows.Close()) }()
	for rows.Next() {
		var participant int64
		var role, name string
		require.NoError(t, rows.Scan(&participant, &role, &name))
		b.AddRecipient(id, participant, role, name)
	}
	require.NoError(t, rows.Err())
}

func TestMeetingCountsAcrossCalendarCopySources(t *testing.T) {
	requirements := require.New(t)
	st := newMeetingCacheStore(t)
	b := NewTestDataBuilder(t)
	person, err := st.EnsureParticipant("person@example.org", "Person", "example.org")
	requirements.NoError(err)
	requirements.Equal(person, b.AddParticipant("person@example.org", "example.org", "Person"))
	owner, err := st.EnsureParticipant("owner@example.org", "Owner", "example.org")
	requirements.NoError(err)
	requirements.Equal(owner, b.AddParticipant("owner@example.org", "example.org", "Owner"))
	var sources []int64
	for _, account := range []string{"first@example.org", "second@example.org", "notes@example.org"} {
		kind, format := "gcal", "gcal_json"
		raw := `{"id":"event","iCalUID":"shared@example.org","start":{"dateTime":"2025-03-14T10:00:00Z"},"end":{"dateTime":"2025-03-14T11:00:00Z"}}`
		messageType := "calendar_event"
		if account == "notes@example.org" {
			kind, format, messageType = "twenty", "twenty_json", "meeting_transcript"
			raw = `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"shared@example.org"}}`
		}
		source, err := st.GetOrCreateSource(kind, account)
		requirements.NoError(err)
		requirements.Equal(source.ID, b.AddSourceWithType(account, kind))
		b.AddOwnerParticipant(source.ID, owner)
		sources = append(sources, source.ID)
		id, err := st.PersistMessage(&store.MessagePersistData{
			Message:      &store.Message{SourceID: source.ID, SourceMessageID: "meeting", MessageType: messageType},
			Conversation: &store.ConversationPersistData{SourceConversationID: "meeting", ConversationType: "meeting"},
			Metadata:     &sql.NullString{String: `{"ical_uid":"shared@example.org"}`, Valid: true},
			RawFormat:    format, RawMIME: []byte(raw),
			Recipients: []store.RecipientSet{{Type: "to", ParticipantIDs: []int64{person, owner}}},
		})
		requirements.NoError(err)
		addStoredMeeting(t, b, st, id)
	}
	keys, _, err := st.MeetingCountKeysContext(t.Context())
	requirements.NoError(err)
	for id, key := range keys {
		b.meetingKeys = append(b.meetingKeys, fmt.Sprintf("(%d, '%s')", id, key))
	}
	engine := b.BuildEngine()
	for _, scope := range [][]int64{nil, {sources[0], sources[1]}, {sources[0], sources[2]}, {sources[1], sources[2]}, sources} {
		people, err := engine.Relationships(t.Context(), RelationshipsRequest{Context: Context{SourceIDs: scope}, Now: time.Date(2024, 1, 16, 0, 0, 0, 0, time.UTC)})
		requirements.NoError(err)
		requirements.Len(people.Rows, 1)
		assert.Equal(t, int64(1), people.Rows[0].Signals.MeetingCount, "sources %v", scope)
	}
}

func TestResourceFlagContactRemainsInMeetingPeopleLists(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newMeetingCacheStore(t)
	b := NewTestDataBuilder(t)
	notes, err := st.GetOrCreateSource("meeting_import", "notes@example.org")
	requirements.NoError(err)
	requirements.Equal(notes.ID, b.AddSourceWithType("notes@example.org", "meeting_import"))
	person, err := st.EnsureParticipant("person@example.org", "Person", "example.org")
	requirements.NoError(err)
	requirements.Equal(person, b.AddParticipant("person@example.org", "example.org", "Person"))
	_, err = st.PersistMessage(&store.MessagePersistData{
		Message:      &store.Message{SourceID: notes.ID, SourceMessageID: "email", MessageType: "email", SenderID: sql.NullInt64{Int64: person, Valid: true}},
		Conversation: &store.ConversationPersistData{SourceConversationID: "email", ConversationType: "email"},
	})
	requirements.NoError(err)
	archive := meetingarchive.New(st)
	snapshot := meetingarchive.Snapshot{SourceID: notes.ID, SourceMessageID: "before", Attendees: []meetingarchive.Person{{Email: "person@example.org"}}}
	before, err := archive.Upsert(t.Context(), snapshot, meetingarchive.UpsertOptions{})
	requirements.NoError(err)
	calendar, err := st.GetOrCreateSource("gcal", "calendar@example.org")
	requirements.NoError(err)
	_, err = st.PersistMessage(&store.MessagePersistData{
		Message:      &store.Message{SourceID: calendar.ID, SourceMessageID: "invite", MessageType: "calendar_event"},
		Conversation: &store.ConversationPersistData{SourceConversationID: "invite", ConversationType: "calendar"},
		RawFormat:    "gcal_json", RawMIME: []byte(`{"id":"invite","start":{"dateTime":"2025-03-14T10:00:00Z"},"end":{"dateTime":"2025-03-14T11:00:00Z"},"attendees":[{"email":"person@example.org","resource":true},{"email":"unseen@example.org","resource":true}]}`),
	})
	requirements.NoError(err)
	snapshot.SourceMessageID = "after"
	snapshot.Attendees = append(snapshot.Attendees, meetingarchive.Person{Email: "unseen@example.org"})
	after, err := archive.Upsert(t.Context(), snapshot, meetingarchive.UpsertOptions{})
	requirements.NoError(err)
	for _, id := range []int64{before.MessageID, after.MessageID} {
		addStoredMeeting(t, b, st, id)
	}
	engine := b.BuildEngine()
	people, err := engine.SearchPeople(t.Context(), PersonSearchRequest{})
	requirements.NoError(err)
	requirements.Len(people.Rows, 1)
	assertions.Equal(person, people.Rows[0].ID)
	assertions.Equal(int64(2), people.Rows[0].MeetingCount)
}
