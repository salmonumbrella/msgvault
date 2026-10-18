package store

import (
	"fmt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"strings"
	"testing"
	"time"
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
	st := newRFC822IDBackfillBackendStore(t)
	data, _ := calendarFixture(t, st, "cap-master", `{"id":"cap-series","start":{"date":"1970-01-01"},"end":{"date":"1970-01-02"},"recurrence":["RRULE:FREQ=DAILY"]}`)
	bulkCalendarCandidates(t, st, data.Message.SourceID, data.Message.ConversationID, "exception", 20001)
	rows, err := calendarWindow(t, st, "2027-03-14T00:00:00Z", "2027-03-15T00:00:00Z")
	require.ErrorIs(t, err, ErrQueryTooBroad)
	assertions.Empty(rows)
}

func TestCalendarEventRowsHardSeriesCap(t *testing.T) {
	assertions := assert.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	data, _ := calendarFixture(t, st, "cap-master", calendarRaw)
	bulkCalendarCandidates(t, st, data.Message.SourceID, data.Message.ConversationID, "series", 2001)
	rows, err := calendarWindow(t, st, "2027-03-14T00:00:00Z", "2027-03-15T00:00:00Z")
	require.ErrorIs(t, err, ErrQueryTooBroad)
	assertions.Empty(rows)
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
