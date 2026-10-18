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
		{"unsupported recurrence", "unsupported", ""},
		{"over budget", "budget", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requirements := require.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			_, note := projectionFixture(t, st, "note", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"shared@example.org"}}`)
			var event int64
			switch tc.mode {
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
				requirements.NoError(st.LinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: event}))
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
			keys, err := st.MeetingCountKeysContext(t.Context())
			requirements.NoError(err)
			want := tc.want
			if want == "event" {
				want = fmt.Sprintf("event:%d|", event)
			}
			assert.Equal(t, want, keys[note])
		})
	}
}
