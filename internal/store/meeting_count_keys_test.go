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
			requirements := require.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			_, note := projectionFixture(t, st, "note", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"shared@example.org"}}`)
			var event int64
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
					calendarFixture(t, st, "copy", singleLinkRaw("copy", uid), "copy@example.org")
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
			assert.Equal(t, want, keys[note])
			if tc.mode == "series" {
				assert.NotContains(t, keys, event, "series master stays a separate calendar item")
			}
			if tc.mode == "shared" || tc.mode == "event" {
				assert.Equal(t, want, keys[event])
			}
		})
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
