package store

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMeetingCountKeys(t *testing.T) {
	t.Run("copies and exact targets", func(t *testing.T) {
		assertions, requirements := assert.New(t), require.New(t)
		st := newRFC822IDBackfillBackendStore(t)
		_, event := calendarFixture(t, st, "event", singleLinkRaw("event", "meeting@example.test"))
		_, copyID := calendarFixture(t, st, "copy", singleLinkRaw("copy", "meeting@example.test"), "other@example.test")
		_, first := projectionFixture(t, st, "first", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"meeting@example.test"}}`)
		source, err := st.GetOrCreateSource("meeting_import", "other-notes@example.test")
		requirements.NoError(err)
		second, err := st.PersistMessage(&MessagePersistData{
			Message:      &Message{SourceID: source.ID, SourceMessageID: "second", MessageType: "meeting_transcript"},
			Conversation: &ConversationPersistData{SourceConversationID: "second", ConversationType: "meeting"},
			RawFormat:    "plaud_json", RawMIME: []byte(`{}`),
		})
		requirements.NoError(err)
		requirements.NoError(st.LinkMeetingContext(t.Context(), second, OccurrenceRef{MessageID: copyID}))
		_, noUID := calendarFixture(t, st, "no-uid", singleLinkRaw("no-uid", ""))
		_, manual := projectionFixture(t, st, "manual", "plaud_json", `{}`)
		requirements.NoError(st.LinkMeetingContext(t.Context(), manual, OccurrenceRef{MessageID: noUID}))
		_, master := calendarFixture(t, st, "series", linkSeriesRaw)
		_, recurring := projectionFixture(t, st, "recurring", "plaud_json", `{}`)
		requirements.NoError(st.LinkMeetingContext(t.Context(), recurring, OccurrenceRef{MessageID: master, Key: "20270321T100000Z"}))
		keys, seq, err := st.MeetingCountKeysContext(t.Context())
		requirements.NoError(err)
		assertions.Equal(map[int64]string{
			first: "uid:meeting@example.test/", second: "uid:meeting@example.test/",
			event: "uid:meeting@example.test/", copyID: "uid:meeting@example.test/",
			manual: fmt.Sprintf("evt:%d/", noUID), noUID: fmt.Sprintf("evt:%d/", noUID),
			recurring: "uid:weekly@example.org/20270321T100000Z",
		}, keys)
		if st.IsPostgreSQL() {
			assertions.Zero(seq)
		} else {
			assertions.Positive(seq)
			reader, err := OpenReadOnlyContext(t.Context(), st.dbPath)
			requirements.NoError(err)
			defer func() { requirements.NoError(reader.Close()) }()
			readKeys, readSeq, err := reader.MeetingCountKeysContext(t.Context())
			requirements.NoError(err)
			assertions.Equal(keys, readKeys)
			assertions.Equal(seq, readSeq)
			_, err = st.db.Exec(`DELETE FROM cache_related_change_journal`)
			requirements.NoError(err)
			_, prunedSeq, err := st.MeetingCountKeysContext(t.Context())
			requirements.NoError(err)
			assertions.Equal(seq, prunedSeq, "pruning keeps the journal high-water mark")
		}
		requirements.NoError(st.UnlinkMeetingContext(t.Context(), first, OccurrenceRef{MessageID: event}))
		keys, _, err = st.MeetingCountKeysContext(t.Context())
		requirements.NoError(err)
		assertions.NotContains(keys, first)
		assertions.Equal("uid:meeting@example.test/", keys[second])
		_, err = st.db.Exec(`UPDATE messages SET deleted_at=CURRENT_TIMESTAMP WHERE id=?`, second)
		requirements.NoError(err)
		keys, _, err = st.MeetingCountKeysContext(t.Context())
		requirements.NoError(err)
		assertions.NotContains(keys, second)
	})
	t.Run("unresolved notes", func(t *testing.T) {
		assertions, requirements := assert.New(t), require.New(t)
		st := newRFC822IDBackfillBackendStore(t)
		_, _ = projectionFixture(t, st, "unlinked", "plaud_json", `{}`)
		_, _ = projectionFixture(t, st, "absent", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"absent@example.test"}}`)
		_, master := calendarFixture(t, st, "series", linkSeriesRaw)
		_, ambiguous := projectionFixture(t, st, "ambiguous", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"weekly@example.org"}}`)
		keys, _, err := st.MeetingCountKeysContext(t.Context())
		requirements.NoError(err)
		assertions.Empty(keys, "unlinked, absent, and ambiguous targets stay unkeyed")
		_, note := projectionFixture(t, st, "saved", "plaud_json", `{}`)
		requirements.NoError(st.LinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: master, Key: "20270314T100000Z"}))
		for _, rule := range []string{"RRULE:FREQ=WEEKLY;COUNT=1", "RRULE:FREQ=HOURLY", "RRULE:FREQ=DAILY;COUNT=100000"} {
			raw := strings.Replace(linkSeriesRaw, "RRULE:FREQ=WEEKLY;COUNT=6", rule, 1)
			if strings.Contains(rule, "100000") {
				raw = strings.NewReplacer("2027-03-07", "1800-01-01").Replace(raw)
			}
			requirements.NoError(st.UpsertMessageRawWithFormat(master, []byte(raw), "gcal_json"))
			keys, _, err := st.MeetingCountKeysContext(t.Context())
			requirements.NoError(err)
			assertions.NotContains(keys, note, rule)
			if rule != "RRULE:FREQ=WEEKLY;COUNT=1" {
				assertions.NotContains(keys, ambiguous, rule)
			}
		}
		_, err = st.db.Exec(`DELETE FROM calendar_events WHERE message_id=?`, master)
		requirements.NoError(err)
		keys, _, err = st.MeetingCountKeysContext(t.Context())
		requirements.NoError(err)
		assertions.Empty(keys, "saved decisions with absent targets stay unkeyed")
	})
	t.Run("cancellation", func(t *testing.T) {
		requirements := require.New(t)
		st := newRFC822IDBackfillBackendStore(t)
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		_, _, err := st.MeetingCountKeysContext(ctx)
		requirements.ErrorIs(err, context.Canceled)
	})
}

func TestMeetingCountKeysIncompleteCopies(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, event := calendarFixture(t, st, "event", singleLinkRaw("event", "meeting@example.test"))
	_, note := projectionFixture(t, st, "note", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"meeting@example.test"}}`)
	// This later copy cannot resolve its own bounds. A partial copy map would
	// leave the earlier event keyed even though this identity's mapping failed.
	_, incomplete := calendarFixture(t, st, "incomplete", `{"id":"incomplete","iCalUID":"meeting@example.test","status":"cancelled"}`, "other@example.test")
	keys, _, err := st.MeetingCountKeysContext(t.Context())
	requirements.NoError(err)
	assertions.Equal(map[int64]string{note: "uid:meeting@example.test/"}, keys)
	assertions.NotContains(keys, event)
	assertions.NotContains(keys, incomplete)
}
