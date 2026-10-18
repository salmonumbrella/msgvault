package query

import (
	"database/sql"
	"encoding/json/v2"
	"fmt"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/calsync"
	"go.kenn.io/msgvault/internal/gcal"
	"go.kenn.io/msgvault/internal/store"
)

func TestCalendarTimezoneChangeInvalidatesPublishedMeetingKeys(t *testing.T) {
	for _, register := range []bool{false, true} {
		t.Run(map[bool]string{false: "sync config", true: "calendar discovery"}[register], func(t *testing.T) {
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
			b := NewTestDataBuilder(t)
			b.AddSourceWithType("owner@example.org/primary", "gcal")
			b.AddSourceWithType("notes@example.org", "twenty")
			b.AddMessage(MessageOpt{SourceID: source.ID, MessageType: "calendar_event"})
			requirements.Equal(note, b.AddMessage(MessageOpt{SourceID: notes.ID, MessageType: "meeting_transcript"}))
			b.meetingKeys = append(b.meetingKeys, fmt.Sprintf("(%d, '%s')", note, keys[note]))
			engine := b.BuildEngine()
			state, err := ReadCacheSyncState(engine.analyticsDir)
			requirements.NoError(err)
			state.LastRelatedChangeSeq = seq
			marker, err := json.Marshal(state)
			requirements.NoError(err)
			requirements.NoError(os.WriteFile(CacheStatePath(engine.analyticsDir), marker, 0o600))
			if register {
				mock.Calendars[0].TimeZone = "America/New_York"
				_, err = syncer.RegisterCalendars(t.Context())
			} else {
				err = st.UpdateSourceSyncConfig(source.ID, `{"account_email":"owner@example.org","calendar_id":"primary","time_zone":"America/New_York"}`)
			}
			requirements.NoError(err)
			keys, _, err = st.MeetingCountKeysContext(t.Context())
			requirements.NoError(err)
			assertions.NotContains(keys, note, "the occurrence instant no longer matches the transcript")
			published, err := ReadCacheSyncState(engine.analyticsDir)
			requirements.NoError(err)
			var pending int
			requirements.NoError(st.DB().QueryRow(`SELECT count(*) FROM cache_related_change_journal WHERE dataset = 'meeting_links' AND seq > ? AND message_id = ?`, published.LastRelatedChangeSeq, note).Scan(&pending))
			assertions.Positive(pending, "the published cache needs a derived refresh for this transcript")
		})
	}
}
