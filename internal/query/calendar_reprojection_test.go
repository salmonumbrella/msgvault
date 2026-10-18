package query

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/calsync"
	"go.kenn.io/msgvault/internal/gcal"
	"go.kenn.io/msgvault/internal/store"
)

func TestCalendarDiscoveryTimezoneChangeJournalsMeetingKeys(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newMeetingCacheStore(t)
	mock := gcal.NewMockAPI()
	mock.Calendars = []gcal.Calendar{{ID: "primary", AccessRole: "owner", TimeZone: "UTC"}}
	syncer := calsync.New(mock, st, calsync.Options{AccountEmail: "owner@example.org"})
	_, err := syncer.RegisterCalendars(t.Context())
	requirements.NoError(err)
	source, err := st.GetSourceByIdentifier("owner@example.org/primary")
	requirements.NoError(err)
	_, err = st.PersistMessage(&store.MessagePersistData{
		Message:      &store.Message{SourceID: source.ID, SourceMessageID: "series", MessageType: "calendar_event"},
		Conversation: &store.ConversationPersistData{SourceConversationID: "series", ConversationType: "calendar"},
		Metadata:     &sql.NullString{String: `{"ical_uid":"shared@example.org"}`, Valid: true},
		RawFormat:    "gcal_json", RawMIME: []byte(`{"id":"series","iCalUID":"shared@example.org","start":{"date":"2025-03-14"},"end":{"date":"2025-03-15"},"recurrence":["RRULE:FREQ=DAILY;COUNT=2"]}`),
	})
	requirements.NoError(err)
	notes, err := st.GetOrCreateSource("twenty", "notes@example.org")
	requirements.NoError(err)
	note, err := st.PersistMessage(&store.MessagePersistData{
		Message:      &store.Message{SourceID: notes.ID, SourceMessageID: "note", MessageType: "meeting_transcript"},
		Conversation: &store.ConversationPersistData{SourceConversationID: "note", ConversationType: "meeting"},
		RawFormat:    "twenty_json", RawMIME: []byte(`{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"shared@example.org","startsAt":"2025-03-14T00:00:00Z"}}`),
	})
	requirements.NoError(err)
	keys, seq, err := st.MeetingCountKeysContext(t.Context())
	requirements.NoError(err)
	requirements.Equal("ical:shared@example.org|20250314", keys[note])
	mock.Calendars[0].TimeZone = "America/New_York"
	_, err = syncer.RegisterCalendars(t.Context())
	requirements.NoError(err)
	keys, _, err = st.MeetingCountKeysContext(t.Context())
	requirements.NoError(err)
	assertions.NotContains(keys, note, "the occurrence instant no longer matches the transcript")
	var pending int
	requirements.NoError(st.DB().QueryRow(`SELECT count(*) FROM cache_related_change_journal WHERE dataset = 'meeting_links' AND seq > ? AND message_id = ?`, seq, note).Scan(&pending))
	assertions.Positive(pending, "calendar discovery journals the transcript for derived refresh")
}
