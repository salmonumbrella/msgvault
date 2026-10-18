package store

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMeetingCountKeysContext(t *testing.T) {
	for _, tc := range []struct {
		name string
		mode string
		want string
	}{
		{"shared calendar copies", "shared", "ical:shared@example.org|"},
		{"unlinked", "unlinked", ""},
		{"ambiguous decisions", "ambiguous", ""},
		{"split", "split", ""},
		{"event without UID", "event", "event"},
		{"series master", "series", "ical:shared@example.org|20270314T100000Z"},
		{"unsupported recurrence", "unsupported", ""},
		{"over budget", "budget", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			_, note := projectionFixture(t, st, "note", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"shared@example.org"}}`)
			var event, copyID int64
			switch tc.mode {
			case "series":
				_, event = calendarFixture(t, st, "event", `{"id":"event","iCalUID":"shared@example.org","start":{"dateTime":"2027-03-07T10:00:00Z"},"end":{"dateTime":"2027-03-07T11:00:00Z"},"recurrence":["RRULE:FREQ=WEEKLY"]}`)
				_, note = projectionFixture(t, st, "note", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"shared@example.org","startsAt":"2027-03-14T10:00:00Z"}}`)
			case "unsupported":
				calendarFixture(t, st, "event", `{"id":"event","iCalUID":"shared@example.org","start":{"dateTime":"2027-03-07T10:00:00Z"},"end":{"dateTime":"2027-03-07T11:00:00Z"},"recurrence":["RRULE:FREQ=HOURLY"]}`)
				_, note = projectionFixture(t, st, "note", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"shared@example.org","startsAt":"2027-03-14T10:00:00Z"}}`)
			case "budget":
				// The resolver caps one UID's candidate event rows at 200.
				for i := range 201 {
					calendarFixture(t, st, fmt.Sprintf("copy-%d", i), singleLinkRaw(fmt.Sprintf("copy-%d", i), "shared@example.org"), fmt.Sprintf("copy-%d@example.org", i))
				}
			case "unlinked":
			default:
				uid := "shared@example.org"
				if tc.mode == "event" {
					uid = ""
				}
				_, event = calendarFixture(t, st, "event", singleLinkRaw("event", uid))
				if tc.mode != "shared" {
					requirements.NoError(st.LinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: event}))
				}
				if tc.mode == "shared" {
					_, copyID = calendarFixture(t, st, "copy", singleLinkRaw("copy", uid), "copy@example.org")
				}
				if tc.mode == "ambiguous" {
					_, other := calendarFixture(t, st, "other", singleLinkRaw("other", "other@example.org"))
					requirements.NoError(st.LinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: other}))
				}
				if tc.mode == "split" {
					requirements.NoError(st.UnlinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: event}))
				}
			}
			keys, _, err := st.MeetingCountKeysContext(t.Context())
			requirements.NoError(err)
			want := tc.want
			if want == "event" {
				want = fmt.Sprintf("event:%d|", event)
			}
			assertions.Equal(want, keys[note])
			if tc.mode == "series" {
				assertions.NotContains(keys, event, "series master stays a separate calendar item")
			}
			if tc.mode == "shared" || tc.mode == "event" {
				assertions.Equal(want, keys[event])
			}
			if tc.mode == "shared" {
				assertions.Equal(want, keys[copyID], "each effective calendar copy receives the shared count key")
			}
		})
	}
}

func TestCalendarSourceZoneChangeJournalsMeetingLinks(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	uid := "zone-change@example.org"
	data, event := calendarFixture(t, st, "zone-event", `{"id":"zone-event","iCalUID":"zone-change@example.org","start":{"date":"2027-03-14"},"end":{"date":"2027-03-15"},"recurrence":["RRULE:FREQ=DAILY;COUNT=2"]}`)
	_, note := projectionFixture(t, st, "zone-note", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"zone-change@example.org","startsAt":"2027-03-14T00:00:00Z"}}`)
	keys, seq, err := st.MeetingCountKeysContext(t.Context())
	requirements.NoError(err)
	requirements.Equal("ical:"+uid+"|20270314", keys[note])
	requirements.NotContains(keys, event, "series masters keep their own count")

	requirements.NoError(st.UpdateSourceSyncConfig(data.Message.SourceID, `{"calendar_id":"calendar@example.com","time_zone":"Asia/Tokyo"}`))
	keys, _, err = st.MeetingCountKeysContext(t.Context())
	requirements.NoError(err)
	assertions.NotContains(keys, note, "a schedule no longer matching the reprojected local date must lose its automatic link")
	assertions.NotContains(keys, event)
	if !st.IsPostgreSQL() {
		var journaled int
		requirements.NoError(st.db.QueryRow(`SELECT count(*) FROM cache_related_change_journal
         WHERE dataset = 'meeting_links' AND message_id = ? AND seq > ?`, note, seq).Scan(&journaled))
		assertions.Equal(1, journaled, "the source-config transaction invalidates cached meeting keys")
	}
}

func TestMeetingCountKeysJournalSnapshot(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, note := projectionFixture(t, st, "note", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"shared@example.org"}}`)
	_, event := calendarFixture(t, st, "event", singleLinkRaw("event", "shared@example.org"))
	require.NoError(st.LinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: event}))
	ctx, release, err := st.BeginReadSnapshotContext(t.Context())
	require.NoError(err)
	defer release()
	before, seq, err := st.MeetingCountKeysContext(ctx)
	require.NoError(err)
	require.Equal("ical:shared@example.org|", before[note])
	require.NoError(st.UnlinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: event}))
	pinned, pinnedSeq, err := st.MeetingCountKeysContext(ctx)
	require.NoError(err)
	assert.Equal(before, pinned)
	assert.Equal(seq, pinnedSeq)
	current, currentSeq, err := st.MeetingCountKeysContext(t.Context())
	require.NoError(err)
	assert.NotContains(current, note)
	if !st.IsPostgreSQL() {
		assert.Greater(currentSeq, pinnedSeq)
	}
}

func TestCalendarTimezoneMeetingJournalRollsBackWithProjection(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	if st.IsPostgreSQL() {
		t.Skip("SQLite cache journal")
	}
	data, event := calendarFixture(t, st, "event", singleLinkRaw("event", "shared@example.org"))
	_, note := projectionFixture(t, st, "note", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"shared@example.org"}}`)
	_, seq, err := st.MeetingCountKeysContext(t.Context())
	requirements.NoError(err)
	_, err = st.DB().Exec(`CREATE TRIGGER reject_meeting_journal BEFORE INSERT ON cache_related_change_journal WHEN NEW.dataset = 'meeting_links' BEGIN SELECT RAISE(ABORT, 'journal blocked'); END`)
	requirements.NoError(err)
	err = st.UpdateSourceSyncConfig(data.Message.SourceID, `{"calendar_id":"calendar@example.com","time_zone":"America/New_York"}`)
	requirements.ErrorContains(err, "journal blocked")
	source, err := st.GetSourceByID(data.Message.SourceID)
	requirements.NoError(err)
	assertions.Equal("UTC", calendarConfigZone(source.SyncConfig))
	assertions.Equal("UTC", calendarRows(t, st, event)[0].Event.CalendarTimeZone)
	keys, after, err := st.MeetingCountKeysContext(t.Context())
	requirements.NoError(err)
	assertions.Equal(seq, after)
	assertions.Equal("ical:shared@example.org|", keys[note])
}

func TestMeetingCountKeysGranolaCalendarCopies(t *testing.T) {
	st := newRFC822IDBackfillBackendStore(t)
	_, own := calendarFixture(t, st, "own", singleLinkRaw("event", "shared@example.org"))
	_, copyID := calendarFixture(t, st, "copy", singleLinkRaw("event", "shared@example.org"), "copy@example.org")
	_, note := projectionFixture(t, st, "note", "granola_json", `{"owner":{"email":"calendar@example.com"},"calendar_event":{"calendar_event_id":"event","scheduled_start_time":"2027-03-14T10:00:00Z"}}`)
	keys, _, err := st.MeetingCountKeysContext(t.Context())
	require.NoError(t, err)
	for _, id := range []int64{note, own, copyID} {
		assert.Equal(t, "ical:shared@example.org|", keys[id], "message %d", id)
	}
}
