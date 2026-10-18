package store

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/calrecur"
)

const linkSeriesRaw = `{"id":"series","iCalUID":"weekly@example.org","start":{"dateTime":"2027-03-07T10:00:00Z","timeZone":"UTC"},"end":{"dateTime":"2027-03-07T11:00:00Z"},"recurrence":["RRULE:FREQ=WEEKLY;COUNT=6"]}`

func singleLinkRaw(id, uid, start, end string) string {
	return fmt.Sprintf(`{"id":%q,"iCalUID":%q,"start":{"dateTime":%q},"end":{"dateTime":%q}}`, id, uid, start, end)
}
func granolaLinkFixture(t *testing.T, st *Store, key, event string) int64 {
	t.Helper()
	_, id := projectionFixture(t, st, key, "granola_json", fmt.Sprintf(`{"owner":{"email":"calendar@example.com"},"calendar_event":{"calendar_event_id":%q,"scheduled_start_time":"2027-03-14T10:00:00Z"}}`, event))
	return id
}
func exportedMeetingKeys(t *testing.T, st *Store, ids ...int64) map[int64]string {
	t.Helper()
	ctx, release, err := st.BeginReadSnapshotContext(t.Context())
	require.NoError(t, err)
	defer release()
	keys, err := st.ExportLogicalMeetingKeysContext(ctx, ReadDBContext(ctx, st.db.DB), ids)
	require.NoError(t, err)
	return keys
}

func TestMeetingLinksGranolaOwnCalendar(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	raw := singleLinkRaw("event", "own@example.org", "2027-03-14T10:00:00Z", "2027-03-14T11:00:00Z")
	_, other := calendarFixture(t, st, "other", raw, "other@example.com")
	note := granolaLinkFixture(t, st, "note", "event")
	assertions.Equal(fmt.Sprintf("msg:%d", note), exportedMeetingKeys(t, st, note)[note])
	suggestions, err := st.SuggestMeetingLinksContext(t.Context(), fmt.Sprintf("msg:%d", note), 10)
	requirements.NoError(err)
	requirements.Len(suggestions, 1)
	assertions.Equal(fmt.Sprintf("evt:%d", other), suggestions[0].Ref)
	calendarFixture(t, st, "own", raw)
	assertions.Equal(`["own@example.org",0,""]`, exportedMeetingKeys(t, st, note)[note])
}

func TestMeetingLinksIdentityHasSeparateFields(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st := newRFC822IDBackfillBackendStore(t)
	_, single := calendarFixture(t, st, "single", singleLinkRaw("single", "x@20270314T100000Z", "2027-03-14T10:00:00Z", "2027-03-14T11:00:00Z"))
	_, series := calendarFixture(t, st, "series", strings.ReplaceAll(linkSeriesRaw, "weekly@example.org", "x"))
	_, note := projectionFixture(t, st, "note", "plaud_json", `{}`)
	requirements.NoError(st.LinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: single}))
	requirements.NoError(st.LinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: series, Key: "20270314T100000Z"}))
	assertions.Equal(fmt.Sprintf("msg:%d", note), exportedMeetingKeys(t, st, note)[note])
	requirements.NoError(st.UnlinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: single}))
	assertions.Equal(`["x",0,"20270314T100000Z"]`, exportedMeetingKeys(t, st, note)[note])
}

func TestMeetingLinksGranolaOccurrences(t *testing.T) {
	for _, tc := range []struct {
		name, raw, exception string
		want                 bool
	}{
		{name: "generated", raw: linkSeriesRaw, want: true},
		{name: "exdate", raw: strings.Replace(linkSeriesRaw, `"RRULE:FREQ=WEEKLY;COUNT=6"`, `"RRULE:FREQ=WEEKLY;COUNT=6","EXDATE:20270314T100000Z"`, 1)},
		{name: "cancelled override", raw: strings.Replace(linkSeriesRaw, `"RRULE:FREQ=WEEKLY;COUNT=6"`, `"RRULE:FREQ=WEEKLY;COUNT=6","EXDATE:20270314T100000Z"`, 1), exception: `{"id":"series_20270314T100000Z","recurringEventId":"series","status":"cancelled","originalStartTime":{"dateTime":"2027-03-14T10:00:00Z"}}`, want: true},
		{name: "moved", raw: linkSeriesRaw, exception: `{"id":"series_20270314T100000Z","recurringEventId":"series","originalStartTime":{"dateTime":"2027-03-14T10:00:00Z"},"start":{"dateTime":"2027-04-01T12:00:00Z"},"end":{"dateTime":"2027-04-01T13:00:00Z"}}`, want: true},
		{name: "non UTC", raw: `{"id":"series","iCalUID":"weekly@example.org","start":{"dateTime":"2027-03-07T05:00:00-05:00","timeZone":"America/New_York"},"end":{"dateTime":"2027-03-07T06:00:00-05:00"},"recurrence":["RRULE:FREQ=WEEKLY;COUNT=6"]}`},
		{name: "non UTC after DST", raw: `{"id":"series","iCalUID":"weekly@example.org","start":{"dateTime":"2027-03-07T06:00:00-05:00","timeZone":"America/New_York"},"end":{"dateTime":"2027-03-07T07:00:00-05:00"},"recurrence":["RRULE:FREQ=WEEKLY;COUNT=6"]}`, want: true},
		{name: "all day", raw: `{"id":"series","iCalUID":"weekly@example.org","start":{"date":"2027-03-07"},"end":{"date":"2027-03-08"},"recurrence":["RRULE:FREQ=WEEKLY;COUNT=6"]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := newRFC822IDBackfillBackendStore(t)
			calendarFixture(t, st, "master", tc.raw)
			if tc.exception != "" {
				calendarFixture(t, st, "exception", tc.exception)
			}
			note := granolaLinkFixture(t, st, "note", "series_20270314T100000Z")
			want := fmt.Sprintf("msg:%d", note)
			if tc.want {
				want = `["weekly@example.org",0,"20270314T100000Z"]`
			}
			assert.Equal(t, want, exportedMeetingKeys(t, st, note)[note])
		})
	}
}

func TestMeetingLinksAllDayInstance(t *testing.T) {
	st := newRFC822IDBackfillBackendStore(t)
	calendarFixture(t, st, "series", `{"id":"series","iCalUID":"day@example.org","start":{"date":"2027-03-07"},"end":{"date":"2027-03-08"},"recurrence":["RRULE:FREQ=WEEKLY;COUNT=6"]}`)
	note := granolaLinkFixture(t, st, "note", "series_20270314")
	assert.Equal(t, `["day@example.org",0,"20270314"]`, exportedMeetingKeys(t, st, note)[note])
}

func TestMeetingLinksTwentyCopiesAndAmbiguity(t *testing.T) {
	for _, kind := range []string{"single copies", "series copies", "different occurrences"} {
		t.Run(kind, func(t *testing.T) {
			st := newRFC822IDBackfillBackendStore(t)
			raw := linkSeriesRaw
			want := `["weekly@example.org",0,"20270314T100000Z"]`
			if kind == "single copies" {
				raw = singleLinkRaw("one", "weekly@example.org", "2027-03-14T10:00:00Z", "2027-03-14T11:00:00Z")
				want = `["weekly@example.org",0,""]`
			}
			calendarFixture(t, st, "one", raw)
			calendarFixture(t, st, "copy", raw, "copy@example.com")
			_, note := projectionFixture(t, st, "twenty", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"weekly@example.org","startsAt":"2027-03-14T10:00:00Z"}}`)
			if kind == "different occurrences" {
				calendarFixture(t, st, "single", singleLinkRaw("single", "weekly@example.org", "2027-03-14T10:00:00Z", "2027-03-14T11:00:00Z"))
				want = fmt.Sprintf("msg:%d", note)
			}
			assert.Equal(t, want, exportedMeetingKeys(t, st, note)[note])
		})
	}
}

func TestMeetingLinksUserDecisions(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st := newRFC822IDBackfillBackendStore(t)
	raw := singleLinkRaw("event", "same@example.org", "2027-03-14T10:00:00Z", "2027-03-14T11:00:00Z")
	_, event := calendarFixture(t, st, "one", raw)
	_, copyID := calendarFixture(t, st, "copy", raw, "copy@example.com")
	_, other := calendarFixture(t, st, "other", singleLinkRaw("other", "other@example.org", "2027-03-14T10:00:00Z", "2027-03-14T11:00:00Z"))
	note := granolaLinkFixture(t, st, "note", "event")
	fallback := fmt.Sprintf("msg:%d", note)
	requirements.NoError(st.UnlinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: copyID}))
	assertions.Equal(fallback, exportedMeetingKeys(t, st, note)[note])
	requirements.NoError(st.LinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: event}))
	assertions.Equal(`["same@example.org",0,""]`, exportedMeetingKeys(t, st, note)[note])
	requirements.NoError(st.UnlinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: copyID}))
	assertions.Equal(fallback, exportedMeetingKeys(t, st, note)[note])
	requirements.NoError(st.LinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: other}))
	assertions.Equal(`["other@example.org",0,""]`, exportedMeetingKeys(t, st, note)[note])
	requirements.ErrorIs(st.LinkMeetingContext(t.Context(), event, OccurrenceRef{MessageID: other}), sql.ErrNoRows)
	requirements.ErrorIs(st.LinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: 999999}), sql.ErrNoRows)
}

func TestMeetingLinksOrphanDecisionAfterMasterArrives(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st := newRFC822IDBackfillBackendStore(t)
	_, orphan := calendarFixture(t, st, "orphan", `{"id":"series_20270314T100000Z","recurringEventId":"series","originalStartTime":{"dateTime":"2027-03-14T10:00:00Z"},"start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"}}`)
	_, note := projectionFixture(t, st, "note", "plaud_json", `{}`)
	requirements.NoError(st.LinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: orphan}))
	assertions.Equal(fmt.Sprintf(`["",%d,"20270314T100000Z"]`, orphan), exportedMeetingKeys(t, st, note)[note])
	_, master := calendarFixture(t, st, "master", linkSeriesRaw)
	assertions.Equal(`["weekly@example.org",0,"20270314T100000Z"]`, exportedMeetingKeys(t, st, note)[note])
	requirements.NoError(st.UnlinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: master, Key: "20270314T100000Z"}))
	assertions.Equal(fmt.Sprintf("msg:%d", note), exportedMeetingKeys(t, st, note)[note])
}

func TestMeetingLinksActualRecurrenceBudget(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st := newRFC822IDBackfillBackendStore(t)
	_, master := calendarFixture(t, st, "master", `{"id":"series","iCalUID":"gaps@example.org","start":{"dateTime":"2027-03-07T02:30:00-08:00","timeZone":"America/Los_Angeles"},"end":{"dateTime":"2027-03-07T03:30:00-08:00"},"recurrence":["RRULE:FREQ=WEEKLY;COUNT=260"]}`)
	ctx, release, err := st.BeginReadSnapshotContext(t.Context())
	requirements.NoError(err)
	defer release()
	r := st.meetingReader(ctx)
	budget := calrecur.RequestReadBudget
	ref := OccurrenceRef{MessageID: master, Key: "20320404T093000Z"}
	o, err := resolveMeetingOccurrence(ctx, r, ref, &budget)
	requirements.NoError(err)
	assertions.Equal("20320404T093000Z", o.Ref.Key)
	spent := calrecur.RequestReadBudget - budget
	assertions.Greater(spent, 260)
	budget = spent
	_, err = resolveMeetingOccurrence(ctx, r, ref, &budget)
	requirements.NoError(err)
	assertions.Zero(budget)
	budget = spent - 1
	_, err = resolveMeetingOccurrence(ctx, r, ref, &budget)
	requirements.ErrorIs(err, ErrQueryTooBroad)
}

func TestMeetingLinksFallbackWithoutUIDAndBatchLimit(t *testing.T) {
	st := newRFC822IDBackfillBackendStore(t)
	_, event := calendarFixture(t, st, "single", singleLinkRaw("event", "", "2027-03-14T10:00:00Z", "2027-03-14T11:00:00Z"))
	note := granolaLinkFixture(t, st, "note", "event")
	assert.Equal(t, fmt.Sprintf(`["",%d,""]`, event), exportedMeetingKeys(t, st, note)[note])
	ctx, release, err := st.BeginReadSnapshotContext(t.Context())
	require.NoError(t, err)
	defer release()
	_, err = st.ExportLogicalMeetingKeysContext(ctx, ReadDBContext(ctx, st.db.DB), make([]int64, 101))
	require.ErrorContains(t, err, "100")
}

func TestMeetingLinksProjectionEnvelopeAndRepair(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st := newRFC822IDBackfillBackendStore(t)
	calendarFixture(t, st, "event", singleLinkRaw("event", "valid@example.org", "2027-03-14T10:00:00Z", "2027-03-14T11:00:00Z"))
	data, note := projectionFixture(t, st, "note", "twenty_json", `{"schema_version":2,"recording":{},"calendar_event":{"iCalUid":"valid@example.org"}}`)
	requirements.NoError(st.SetMessageMetadata(note, sql.NullString{String: `{"platform":"twenty","calendar_ical_uid":"valid@example.org"}`, Valid: true}))
	assertions.Equal(fmt.Sprintf("msg:%d", note), exportedMeetingKeys(t, st, note)[note])
	valid := `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"valid@example.org"}}`
	requirements.NoError(st.UpsertMessageRawWithFormat(note, []byte(valid), "twenty_json"))
	assertions.Equal(`["valid@example.org",0,""]`, exportedMeetingKeys(t, st, note)[note])
	// Only the shared ID changes; the content remains identical.
	before, hash := readProjection(t, st, note)
	requirements.NoError(st.UpsertMessageRawWithFormat(note, []byte(strings.ReplaceAll(valid, "valid@example.org", "changed@example.org")), "twenty_json"))
	after, newHash := readProjection(t, st, note)
	assertions.Equal(before, after)
	assertions.NotEqual(hash, newHash)
	assertions.Equal(fmt.Sprintf("msg:%d", note), exportedMeetingKeys(t, st, note)[note])
	requirements.NoError(st.UpsertMessageRawWithFormat(note, []byte(`{broken`), "twenty_json"))
	content, _ := readProjection(t, st, note)
	assertions.Equal("invalid_raw", content.Transcript.Reason)
	var count int
	requirements.NoError(st.db.QueryRow(`SELECT count(*) FROM meeting_link_keys WHERE message_id = ?`, note).Scan(&count))
	assertions.Zero(count)
	requirements.NoError(st.UpsertMessageRawWithFormat(note, []byte(valid), "twenty_json"))
	assertions.Equal(`["valid@example.org",0,""]`, exportedMeetingKeys(t, st, note)[note])
	data.Message.MessageType = "email"
	_, err := st.UpsertMessage(data.Message)
	requirements.NoError(err)
	assertions.Empty(exportedMeetingKeys(t, st, note))
	requirements.NoError(st.db.QueryRow(`SELECT count(*) FROM meeting_link_keys WHERE message_id = ?`, note).Scan(&count))
	assertions.Zero(count)
}

func TestMeetingLinksUpgradeFromSchema11(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st := newRFC822IDBackfillBackendStore(t)
	_, event := calendarFixture(t, st, "event", singleLinkRaw("event", "upgraded@example.org", "2027-03-14T10:00:00Z", "2027-03-14T11:00:00Z"))
	note := granolaLinkFixture(t, st, "note", "event")
	_, err := st.db.Exec(`DROP TABLE meeting_link_decisions; DROP TABLE meeting_link_keys; DROP INDEX idx_calendar_events_ical_uid; ALTER TABLE calendar_events DROP COLUMN ical_uid`)
	requirements.NoError(err)
	_, err = st.db.Exec(`UPDATE calendar_events SET projection_version = 1; UPDATE meeting_details SET projection_version = 1; UPDATE archive_metadata SET value = '11' WHERE key = 'schema_version'`)
	requirements.NoError(err)
	_, err = st.db.Exec(`UPDATE applied_migrations SET version = 1 WHERE name IN (?,?)`, migrationCalendarEventProjectionV1, migrationMeetingProjectionV1)
	requirements.NoError(err)
	requirements.NoError(st.InitSchemaContext(t.Context()))
	assertions.Equal(`["upgraded@example.org",0,""]`, exportedMeetingKeys(t, st, note)[note])
	var uid string
	requirements.NoError(st.db.QueryRow(`SELECT ical_uid FROM calendar_events WHERE message_id = ?`, event).Scan(&uid))
	assertions.Equal("upgraded@example.org", uid)
	requirements.NoError(st.InitSchemaContext(t.Context()))
	assertions.Equal(`["upgraded@example.org",0,""]`, exportedMeetingKeys(t, st, note)[note])
}

func TestMeetingLinksIgnoreUnresolvableDecision(t *testing.T) {
	requirements := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, master := calendarFixture(t, st, "master", linkSeriesRaw)
	_, note := projectionFixture(t, st, "note", "plaud_json", `{}`)
	requirements.NoError(st.LinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: master, Key: "20270314T100000Z"}))
	requirements.NoError(st.UpsertMessageRawWithFormat(master, []byte(`{"id":"series","recurrence":["RRULE:FREQ=WEEKLY"]}`), "gcal_json"))
	assert.Equal(t, fmt.Sprintf("msg:%d", note), exportedMeetingKeys(t, st, note)[note])
}
