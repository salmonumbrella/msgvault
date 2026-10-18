package store

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/calrecur"
)

const linkSeriesRaw = `{"id":"series","iCalUID":"weekly@example.org","start":{"dateTime":"2027-03-07T10:00:00Z","timeZone":"UTC"},"end":{"dateTime":"2027-03-07T11:00:00Z"},"recurrence":["RRULE:FREQ=WEEKLY;COUNT=6"]}`

func TestMeetingLinksCalendarUIDLookupPlan(t *testing.T) {
	st := newRFC822IDBackfillBackendStore(t)
	_, id := calendarFixture(t, st, "event", singleLinkRaw("event", "shared@example.org"))
	// Metadata is the UID source even if the archived provider payload differs.
	require.NoError(t, st.SetMessageMetadata(id, sql.NullString{String: `{"ical_uid":"metadata@example.org"}`, Valid: true}))
	_, note := projectionFixture(t, st, "note", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"metadata@example.org"}}`)
	assert.Equal(t, `["metadata@example.org",0,""]`, exportedMeetingKeys(t, st, note)[note])

	explain := "EXPLAIN QUERY PLAN "
	if st.IsPostgreSQL() {
		explain = "EXPLAIN "
	}
	rows, err := st.db.QueryContext(t.Context(), explain+calendarEventSelect+"("+st.calendarUIDPredicate()+" AND e.kind != 'exception') ORDER BY e.message_id LIMIT 201", "metadata@example.org")
	require.NoError(t, err)
	defer func() { require.NoError(t, rows.Close()) }()
	var plan []string
	for rows.Next() {
		var detail string
		if st.IsPostgreSQL() {
			require.NoError(t, rows.Scan(&detail))
		} else {
			var id, parent, unused int
			require.NoError(t, rows.Scan(&id, &parent, &unused, &detail))
		}
		t.Log(detail)
		plan = append(plan, detail)
	}
	require.NoError(t, rows.Err())
	assert.Contains(t, strings.Join(plan, "\n"), "idx_messages_calendar_ical_uid")
}

func TestMeetingLinksWindowedExceptions(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	raw := strings.ReplaceAll(strings.ReplaceAll(linkSeriesRaw, "2027-03-07", "2026-01-04"), "FREQ=WEEKLY;COUNT=6", "FREQ=DAILY;COUNT=1000")
	_, master := calendarFixture(t, st, "master", raw)
	for i := range 201 {
		at := time.Date(2026, 1, 4, 10, 0, 0, 0, time.UTC).AddDate(0, 0, i)
		calendarFixture(t, st, fmt.Sprintf("old-%d", i), fmt.Sprintf(`{"id":"old-%d","iCalUID":"weekly@example.org","recurringEventId":"series","status":"cancelled","originalStartTime":{"dateTime":%q}}`, i, at.Format(time.RFC3339)))
	}
	from := time.Date(2027, 3, 14, 10, 0, 0, 0, time.UTC)
	page, err := st.ListCalendarOccurrencesContext(t.Context(), from, from.Add(time.Hour))
	requirements.NoError(err)
	requirements.Len(page.Occurrences, 1)
	note := granolaLinkFixture(t, st, "note", "series_20270314T100000Z")
	assertions.Equal(`["weekly@example.org",0,"20270314T100000Z"]`, exportedMeetingKeys(t, st, note)[note])
	_, twenty := projectionFixture(t, st, "twenty", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"weekly@example.org","startsAt":"2027-03-14T10:00:00Z"}}`)
	assertions.Equal(`["weekly@example.org",0,"20270314T100000Z"]`, exportedMeetingKeys(t, st, twenty)[twenty])
	ref := OccurrenceRef{MessageID: master, Key: "20270314T100000Z"}
	requirements.NoError(st.LinkMeetingContext(t.Context(), note, ref))
	requirements.NoError(st.UnlinkMeetingContext(t.Context(), note, ref))
	assertions.Equal(fmt.Sprintf("msg:%d", note), exportedMeetingKeys(t, st, note)[note])

	// An old exception moved into this window retains its original identity.
	calendarFixture(t, st, "old-1", `{"id":"old-1","iCalUID":"weekly@example.org","recurringEventId":"series","originalStartTime":{"dateTime":"2026-01-05T10:00:00Z"},"start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"}}`)
	raw = strings.Replace(raw, `"RRULE:FREQ=DAILY;COUNT=1000"`, `"RRULE:FREQ=DAILY;COUNT=1000","EXDATE:20270314T100000Z"`, 1)
	requirements.NoError(st.UpsertMessageRawWithFormat(master, []byte(raw), "gcal_json"))
	assertions.Equal(`["weekly@example.org",0,"20260105T100000Z"]`, exportedMeetingKeys(t, st, twenty)[twenty])
	ref.Key = "20260105T100000Z"
	requirements.NoError(st.LinkMeetingContext(t.Context(), note, ref))
	requirements.NoError(st.UnlinkMeetingContext(t.Context(), note, ref))
}

func TestMeetingLinksTwentyExactInstant(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, master := calendarFixture(t, st, "master", linkSeriesRaw)
	_, note := projectionFixture(t, st, "note", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"weekly@example.org","startsAt":"2027-03-14T10:00:30Z"}}`)
	assertions.Equal(fmt.Sprintf("msg:%d", note), exportedMeetingKeys(t, st, note)[note])
	suggestions, err := st.SuggestMeetingLinksContext(t.Context(), MeetingLinkRef{Kind: "msg", OccurrenceRef: OccurrenceRef{MessageID: note}}, 10)
	requirements.NoError(err)
	requirements.Len(suggestions, 1)
	assertions.Equal(fmt.Sprintf("evt:%d@20270314T100000Z", master), suggestions[0].Ref)
}

func TestMeetingLinksBatchMemberFailure(t *testing.T) {
	for _, failure := range []string{"dense UID", "invalid recurrence"} {
		t.Run(failure, func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			calendarFixture(t, st, "good", singleLinkRaw("good", "good@example.org"))
			_, bad := projectionFixture(t, st, "bad-note", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"bad@example.org"}}`)
			good := granolaLinkFixture(t, st, "good-note", "good")
			_, unlinked := projectionFixture(t, st, "unlinked", "plaud_json", `{}`)
			count := 201
			if failure == "invalid recurrence" {
				count = 1
			}
			for i := range count {
				_, id := calendarFixture(t, st, fmt.Sprintf("bad-%d", i), singleLinkRaw(fmt.Sprintf("bad-%d", i), "bad@example.org"))
				if failure == "invalid recurrence" {
					_, err := st.db.Exec(`UPDATE calendar_events SET recurrence_json = 'broken' WHERE message_id = ?`, id)
					requirements.NoError(err)
				}
			}
			ctx, release, err := st.BeginReadSnapshotContext(t.Context())
			requirements.NoError(err)
			defer release()
			keys, err := st.ExportLogicalMeetingKeysContext(ctx, ReadDBContext(ctx, st.db.DB), []int64{bad, good, unlinked})
			if failure == "invalid recurrence" {
				requirements.ErrorContains(err, "decode calendar recurrence")
				requirements.ErrorContains(err, fmt.Sprintf("message %d", bad))
			} else {
				requirements.NoError(err)
			}
			assertions.Equal(`["good@example.org",0,""]`, keys[good])
			assertions.Equal(fmt.Sprintf("msg:%d", bad), keys[bad])
			assertions.Equal(fmt.Sprintf("msg:%d", unlinked), keys[unlinked])
		})
	}
}

func TestMeetingLinksSplitSurvivesReingest(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	eventData, event := calendarFixture(t, st, "event", singleLinkRaw("event", "same@example.org"))
	noteData, note := projectionFixture(t, st, "note", "granola_json", `{"owner":{"email":"calendar@example.com"},"calendar_event":{"calendar_event_id":"event"}}`)
	assertions.Equal(`["same@example.org",0,""]`, exportedMeetingKeys(t, st, note)[note])
	requirements.NoError(st.UnlinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: event}))
	for _, tc := range []struct {
		data *MessagePersistData
		id   int64
	}{{noteData, note}, {eventData, event}} {
		id, err := st.PersistMessage(tc.data)
		requirements.NoError(err)
		assertions.Equal(tc.id, id)
	}
	assertions.Equal(fmt.Sprintf("msg:%d", note), exportedMeetingKeys(t, st, note)[note])
}

func singleLinkRaw(id, uid string) string {
	return fmt.Sprintf(`{"id":%q,"iCalUID":%q,"start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"}}`, id, uid)
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
	raw := singleLinkRaw("event", "own@example.org")
	_, other := calendarFixture(t, st, "other", raw, "other@example.com")
	note := granolaLinkFixture(t, st, "note", "event")
	assertions.Equal(fmt.Sprintf("msg:%d", note), exportedMeetingKeys(t, st, note)[note])
	suggestions, err := st.SuggestMeetingLinksContext(t.Context(), MeetingLinkRef{Kind: "msg", OccurrenceRef: OccurrenceRef{MessageID: note}}, 10)
	requirements.NoError(err)
	requirements.Len(suggestions, 1)
	assertions.Equal(fmt.Sprintf("evt:%d", other), suggestions[0].Ref)
	calendarFixture(t, st, "own", raw)
	assertions.Equal(`["own@example.org",0,""]`, exportedMeetingKeys(t, st, note)[note])
}

func TestMeetingLinksGranolaEmailCase(t *testing.T) {
	st := newRFC822IDBackfillBackendStore(t)
	calendarFixture(t, st, "own", singleLinkRaw("event", "own@example.org"), " Calendar@Example.COM ")
	_, note := projectionFixture(t, st, "note", "granola_json", `{"owner":{"email":" CALENDAR@example.com "},"calendar_event":{"calendar_event_id":"event"}}`)
	assert.Equal(t, `["own@example.org",0,""]`, exportedMeetingKeys(t, st, note)[note])
}

func TestMeetingLinksIdentityHasSeparateFields(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st := newRFC822IDBackfillBackendStore(t)
	_, single := calendarFixture(t, st, "single", singleLinkRaw("single", "x@20270314T100000Z"))
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
				raw = singleLinkRaw("one", "weekly@example.org")
				want = `["weekly@example.org",0,""]`
			}
			calendarFixture(t, st, "one", raw)
			calendarFixture(t, st, "copy", raw, "copy@example.com")
			_, note := projectionFixture(t, st, "twenty", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"weekly@example.org","startsAt":"2027-03-14T10:00:00Z"}}`)
			if kind == "different occurrences" {
				calendarFixture(t, st, "single", singleLinkRaw("single", "weekly@example.org"))
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
	raw := singleLinkRaw("event", "same@example.org")
	_, event := calendarFixture(t, st, "one", raw)
	_, copyID := calendarFixture(t, st, "copy", raw, "copy@example.com")
	_, other := calendarFixture(t, st, "other", singleLinkRaw("other", "other@example.org"))
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
	_, event := calendarFixture(t, st, "single", singleLinkRaw("event", ""))
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
	calendarFixture(t, st, "event", singleLinkRaw("event", "valid@example.org"))
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
	calendarFixture(t, st, "event", singleLinkRaw("event", "upgraded@example.org"))
	note := granolaLinkFixture(t, st, "note", "event")
	_, err := st.db.Exec(`DROP TABLE meeting_link_decisions; DROP TABLE meeting_link_keys; DROP INDEX idx_messages_calendar_ical_uid`)
	requirements.NoError(err)
	_, err = st.db.Exec(`UPDATE meeting_details SET projection_version = 1; UPDATE archive_metadata SET value = '11' WHERE key = 'schema_version'`)
	requirements.NoError(err)
	_, err = st.db.Exec(`UPDATE applied_migrations SET version = 1 WHERE name = ?`, migrationMeetingProjectionV1)
	requirements.NoError(err)
	requirements.NoError(st.InitSchemaContext(t.Context()))
	assertions.Equal(`["upgraded@example.org",0,""]`, exportedMeetingKeys(t, st, note)[note])
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

func TestMeetingLinksBreadthBeforeDecode(t *testing.T) {
	st := newRFC822IDBackfillBackendStore(t)
	_, note := projectionFixture(t, st, "note", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"dense@example.org"}}`)
	for i := range 201 {
		_, event := calendarFixture(t, st, fmt.Sprintf("event-%d", i), singleLinkRaw(fmt.Sprintf("event-%d", i), "dense@example.org"))
		_, err := st.db.Exec(`UPDATE calendar_events SET recurrence_json = 'broken' WHERE message_id = ?`, event)
		require.NoError(t, err)
	}
	ctx, release, err := st.BeginReadSnapshotContext(t.Context())
	require.NoError(t, err)
	defer release()
	keys, err := st.ExportLogicalMeetingKeysContext(ctx, ReadDBContext(ctx, st.db.DB), []int64{note})
	require.NoError(t, err)
	assert.Equal(t, fmt.Sprintf("msg:%d", note), keys[note])
}
