package store

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func calendarWindow(t *testing.T, st *Store, from, to string) ([]CalendarEventRow, error) {
	t.Helper()
	start, err := time.Parse(time.RFC3339, from)
	require.NoError(t, err)
	end, err := time.Parse(time.RFC3339, to)
	require.NoError(t, err)
	ctx, release, err := st.BeginReadSnapshotContext(t.Context())
	require.NoError(t, err)
	defer release()
	return calendarEventRowsInWindow(ctx, st.db, start, end)
}
func bulkCalendarCandidates(t *testing.T, st *Store, sourceID, conversationID int64, kind string, count int) {
	t.Helper()
	require.NoError(t, st.withTxContext(t.Context(), func(tx *loggedTx) error {
		for offset := 0; offset < count; offset += 500 {
			size := min(500, count-offset)
			messageValues, eventValues := make([]string, 0, size), make([]string, 0, size)
			messageArgs, eventArgs := make([]any, 0, size*5), make([]any, 0, size*14)
			for i := range size {
				id := int64(100000 + offset + i)
				providerID := fmt.Sprintf("bulk-%s-%d", kind, id)
				seriesID := providerID
				if kind == "exception" {
					seriesID = "cap-series"
				}
				date := time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC).AddDate(0, 0, offset+i).Format(time.DateOnly)
				if kind != "exception" {
					date = ""
				}
				messageValues = append(messageValues, "(?, ?, ?, ?, ?)")
				messageArgs = append(messageArgs, id, sourceID, conversationID, providerID, "calendar_event")
				eventValues = append(eventValues, "(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)")
				eventArgs = append(eventArgs, id, 1, "fixture", providerID, seriesID, kind, date, "cancelled", true, "UTC", `[]`)
			}
			override := ""
			if st.IsPostgreSQL() {
				override = " OVERRIDING SYSTEM VALUE"
			}
			if _, err := tx.Exec(`INSERT INTO messages (id,source_id,conversation_id,source_message_id,message_type)`+override+` VALUES `+strings.Join(messageValues, ","), messageArgs...); err != nil {
				return err
			}
			if _, err := tx.Exec(`INSERT INTO calendar_events (message_id,projection_version,content_hash,provider_event_id,series_provider_id,kind,original_start_date,status,all_day,calendar_time_zone,recurrence_json) VALUES `+strings.Join(eventValues, ","), eventArgs...); err != nil {
				return err
			}
		}
		return nil
	}))
}

func TestCalendarEventRowsManyExceptionsBounded(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	data, _ := calendarFixture(t, st, "cap-master", `{"id":"cap-series","start":{"date":"1970-01-01"},"end":{"date":"1970-01-02"},"recurrence":["RRULE:FREQ=DAILY"]}`)
	bulkCalendarCandidates(t, st, data.Message.SourceID, data.Message.ConversationID, "exception", 20001)
	rows, err := calendarWindow(t, st, "2027-03-14T00:00:00Z", "2027-03-15T00:00:00Z")
	requirements.NoError(err)
	assertions.Len(rows, 1)
	from := time.Date(2027, 3, 14, 0, 0, 0, 0, time.UTC)
	page, err := st.ListCalendarOccurrencesContext(t.Context(), from, from.AddDate(0, 0, 1))
	requirements.NoError(err)
	assertions.Len(page.Occurrences, 1)
	// The same budget still rejects exceptions that actually affect the window.
	_, err = st.db.Exec(`UPDATE calendar_events SET original_start_date = '2027-03-14' WHERE kind = 'exception'`)
	requirements.NoError(err)
	_, err = st.ListCalendarOccurrencesContext(t.Context(), from, from.AddDate(0, 0, 1))
	requirements.ErrorIs(err, ErrQueryTooBroad)
	assertions.Contains(err.Error(), "narrow the window")
}

func TestCalendarEventRowsHardSeriesCap(t *testing.T) {
	assertions := assert.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	data, _ := calendarFixture(t, st, "cap-master", calendarRaw)
	bulkCalendarCandidates(t, st, data.Message.SourceID, data.Message.ConversationID, "series", 2001)
	_, err := st.db.Exec(`UPDATE calendar_events SET start_at = '2027-03-01 00:00:00+00:00', end_at = '2027-03-02 00:00:00+00:00', start_date = '2027-03-01', end_date = '2027-03-02', recurrence_json = '["RRULE:FREQ=DAILY"]' WHERE kind = 'series'`)
	require.NoError(t, err)
	from := time.Date(2027, 3, 14, 0, 0, 0, 0, time.UTC)
	for _, width := range []time.Duration{24 * time.Hour, time.Hour} {
		_, err := st.ListCalendarOccurrencesContext(t.Context(), from, from.Add(width))
		require.ErrorIs(t, err, ErrQueryTooBroad)
		assertions.Contains(err.Error(), "more than 2000 recurring series overlap the window")
		assertions.Contains(err.Error(), "a narrower window will not reduce open-ended series")
		assertions.NotContains(err.Error(), "narrow the window")
	}
}

func TestCalendarEventRowsBudgetRejectsBeforeDecoding(t *testing.T) {
	st := newRFC822IDBackfillBackendStore(t)
	calendarFixture(t, st, "one", calendarRaw)
	calendarFixture(t, st, "two", calendarRaw)
	_, err := st.db.Exec(`UPDATE calendar_events SET recurrence_json = 'invalid'`)
	require.NoError(t, err)
	ctx, release, err := st.BeginReadSnapshotContext(t.Context())
	require.NoError(t, err)
	defer release()
	_, err = queryCalendarEventRowsWithinBudget(ctx, st.db, "1 = 1", nil, 1)
	assert.ErrorIs(t, err, ErrQueryTooBroad)
}
func TestCalendarEventSourceZoneRebuild(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st := newRFC822IDBackfillBackendStore(t)
	data, id := calendarFixture(t, st, "zone", `{"id":"all-day","start":{"date":"2027-03-14"},"end":{"date":"2027-03-15"}}`)
	requirements.NoError(st.UpdateSourceSyncConfig(data.Message.SourceID, `{"time_zone":"America/Los_Angeles"}`))
	row := calendarRows(t, st, id)[0]
	requirements.NotNil(row.Event.Start)
	assertions.Equal("2027-03-14T08:00:00Z", row.Event.Start.Format(time.RFC3339))
	dest, err := st.GetOrCreateSource("gcal", "destination@example.com")
	requirements.NoError(err)
	requirements.NoError(st.UpdateSourceSyncConfig(dest.ID, `{"time_zone":"Asia/Tokyo"}`))
	_, err = st.MergeSourcesContext(t.Context(), MergeSourcesRequest{FromSourceID: data.Message.SourceID, IntoSourceID: dest.ID})
	requirements.NoError(err)
	row = calendarRows(t, st, id)[0]
	assertions.Equal(dest.ID, row.SourceID)
	assertions.Equal("2027-03-13T15:00:00Z", row.Event.Start.Format(time.RFC3339))
}

func TestCalendarExceptionsOriginalAndEffectiveWindows(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st := newRFC822IDBackfillBackendStore(t)
	_, master := calendarFixture(t, st, "master", `{"id":"weekly","start":{"dateTime":"2027-03-02T09:00:00Z"},"end":{"dateTime":"2027-03-02T10:00:00Z"},"recurrence":["RRULE:FREQ=WEEKLY;COUNT=2"]}`)
	_, moved := calendarFixture(t, st, "moved", `{"id":"moved","recurringEventId":"weekly","originalStartTime":{"dateTime":"2027-03-02T09:00:00Z"},"start":{"dateTime":"2027-04-02T09:00:00Z"},"end":{"dateTime":"2027-04-02T10:00:00Z"}}`)
	from := time.Date(2027, 3, 2, 0, 0, 0, 0, time.UTC)
	page, err := st.ListCalendarOccurrencesContext(t.Context(), from, from.AddDate(0, 0, 1))
	requirements.NoError(err)
	assertions.Empty(page.Occurrences, "the original occurrence moved outside the window")
	from = from.AddDate(0, 1, 0)
	page, err = st.ListCalendarOccurrencesContext(t.Context(), from, from.AddDate(0, 0, 1))
	requirements.NoError(err)
	requirements.Len(page.Occurrences, 1)
	assertions.Equal(moved, page.Occurrences[0].MessageID)
	assertions.Equal(master, page.Occurrences[0].SeriesMessageID)
}
