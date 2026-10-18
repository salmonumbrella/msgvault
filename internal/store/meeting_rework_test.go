package store

import (
	"context"
	"database/sql/driver"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMeetingLinksUnsupportedMasterException(t *testing.T) {
	st := newRFC822IDBackfillBackendStore(t)
	_, master := calendarFixture(t, st, "master", strings.ReplaceAll(linkSeriesRaw, "FREQ=WEEKLY;COUNT=6", "FREQ=HOURLY;COUNT=6"))
	_, exception := calendarFixture(t, st, "exception", `{"id":"edited","recurringEventId":"series","originalStartTime":{"dateTime":"2027-03-07T10:00:00Z"},"start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"}}`)
	_, note := projectionFixture(t, st, "note", "plaud_json", `{}`)
	from := time.Date(2027, 3, 14, 10, 0, 0, 0, time.UTC)
	page, err := st.ListCalendarOccurrencesContext(t.Context(), from, from.Add(time.Hour))
	require.NoError(t, err)
	require.Len(t, page.Occurrences, 1)
	assert.Equal(t, fmt.Sprintf("evt:%d@20270307T100000Z", master), page.Occurrences[0].Ref)
	require.NoError(t, st.LinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: master, Key: "20270307T100000Z"}))
	links, err := st.MeetingLinksContext(t.Context(), note)
	require.NoError(t, err)
	require.Len(t, links, 1)
	assert.Equal(t, page.Occurrences[0].Ref, links[0].Ref)
	assert.Equal(t, from, links[0].Start)
	require.NoError(t, st.UnlinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: exception}))
	links, err = st.MeetingLinksContext(t.Context(), note)
	require.NoError(t, err)
	assert.Empty(t, links)
	_, otherNote := projectionFixture(t, st, "other-note", "plaud_json", `{}`)
	require.NoError(t, st.LinkMeetingContext(t.Context(), otherNote, OccurrenceRef{MessageID: exception}))
	links, err = st.MeetingLinksContext(t.Context(), otherNote)
	require.NoError(t, err)
	require.Len(t, links, 1)
	assert.Equal(t, page.Occurrences[0].Ref, links[0].Ref)
}

func TestMeetingLinksSuggestionWindowPlan(t *testing.T) {
	st := newRFC822IDBackfillBackendStore(t)
	_, event := calendarFixture(t, st, "event", singleLinkRaw("event", "window@example.org"))
	// Enough historical notes make a narrow range cheaper than a population scan.
	_, note := projectionFixture(t, st, "note", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"window@example.org","startsAt":"2027-03-14T10:00:00Z"}}`)
	_, err := st.db.Exec(`INSERT INTO messages(source_id,conversation_id,source_message_id,message_type,sent_at) SELECT source_id,conversation_id, 'history-' || n, 'meeting_transcript', '2020-01-01 00:00:00+00:00' FROM messages CROSS JOIN `+func() string {
		if st.IsPostgreSQL() {
			return `generate_series(1,3000) AS n`
		}
		return `(WITH RECURSIVE nums(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM nums WHERE n<3000) SELECT n FROM nums)`
	}()+` WHERE id = ?`, note)
	require.NoError(t, err)
	_, err = st.db.Exec(`INSERT INTO meeting_link_key(message_id,kind,value,calendar_id,scheduled_at) SELECT id,'ical_uid','old@example.org','','2020-01-01 00:00:00+00:00' FROM messages WHERE source_message_id LIKE 'history-%'`)
	require.NoError(t, err)
	_, err = st.db.Exec(`ANALYZE`)
	require.NoError(t, err)
	var query string
	var args []any
	observed := gatedMeetingProjectionStore(t, st, &meetingProjectionGate{before: func(_ context.Context, q string, values []driver.NamedValue) error {
		if strings.Contains(q, "WITH window_ids AS") {
			query = q
			for _, v := range values {
				args = append(args, v.Value)
			}
		}
		return nil
	}})
	suggestions, err := observed.SuggestMeetingLinksContext(t.Context(), MeetingLinkRef{Kind: "evt", OccurrenceRef: OccurrenceRef{MessageID: event}}, 10)
	require.NoError(t, err)
	require.Len(t, suggestions, 1)
	require.NotEmpty(t, query)
	explain := "EXPLAIN QUERY PLAN "
	if st.IsPostgreSQL() {
		explain = "EXPLAIN "
	}
	rows, err := st.db.QueryContext(t.Context(), explain+query, args...)
	require.NoError(t, err)
	defer rows.Close()
	var plan []string
	for rows.Next() {
		var detail string
		if st.IsPostgreSQL() {
			require.NoError(t, rows.Scan(&detail))
		} else {
			var id, parent, unused int
			require.NoError(t, rows.Scan(&id, &parent, &unused, &detail))
		}
		plan = append(plan, detail)
	}
	require.NoError(t, rows.Err())
	text := strings.Join(plan, "\n")
	assert.Contains(t, text, "idx_meeting_link_key_scheduled")
	assert.Contains(t, text, "idx_messages_meeting_start")
	if st.IsPostgreSQL() {
		assert.Contains(t, text, "Index Cond:")
	}
}
