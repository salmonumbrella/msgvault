package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMeetingLinksSuggestionsBatchParticipants(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st := newRFC822IDBackfillBackendStore(t)
	_, event := calendarFixture(t, st, "event", `{"id":"event","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"},"attendees":[{"email":"guest@example.com"}]}`)
	participant, err := st.EnsureParticipant("guest@example.com", "Guest Example", "")
	requirements.NoError(err)
	var notes []int64
	for i := range 3 {
		data, id := projectionFixture(t, st, fmt.Sprintf("note-%d", i), "circleback_json", `{"meeting":{"url":"https://meet.example.com/room"}}`)
		data.Message.SentAt.Time = time.Date(2027, 3, 14, 10, 0, 0, 0, time.UTC)
		data.Message.SentAt.Valid = true
		_, err := st.UpsertMessage(data.Message)
		requirements.NoError(err)
		requirements.NoError(st.ReplaceMessageRecipients(id, "to", []int64{participant}, nil))
		notes = append(notes, id)
	}
	var queries atomic.Int64
	observed := gatedMeetingProjectionStore(t, st, &meetingProjectionGate{before: func(_ context.Context, query string, _ []driver.NamedValue) error {
		if strings.Contains(query, "FROM message_recipients mr JOIN participants p") {
			queries.Add(1)
		}
		return nil
	}})
	suggestions, err := observed.SuggestMeetingLinksContext(t.Context(), fmt.Sprintf("evt:%d", event), 10)
	requirements.NoError(err)
	requirements.Len(suggestions, 3)
	assertions.Equal(int64(1), queries.Load())
	for _, suggestion := range suggestions {
		assertions.Equal(30, suggestion.OverlapMinutes)
		assertions.Equal(1, suggestion.SharedAttendees)
	}
	// Circleback's room URL stays a suggestion, including when the times overlap.
	assertions.Equal(fmt.Sprintf("msg:%d", notes[0]), exportedMeetingKeys(t, st, notes[0])[notes[0]])
	reverse, err := observed.SuggestMeetingLinksContext(t.Context(), fmt.Sprintf("msg:%d", notes[0]), 1)
	requirements.NoError(err)
	requirements.Len(reverse, 1)
	assertions.Equal(fmt.Sprintf("evt:%d", event), reverse[0].Ref)
	assertions.Equal(int64(2), queries.Load())
	requirements.NoError(st.UpsertMessageRawWithFormat(event, []byte(`{"id":"event","status":"cancelled","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"}}`), "gcal_json"))
	suggestions, err = observed.SuggestMeetingLinksContext(t.Context(), fmt.Sprintf("evt:%d", event), 10)
	requirements.NoError(err)
	assertions.Empty(suggestions)
	reverse, err = observed.SuggestMeetingLinksContext(t.Context(), fmt.Sprintf("msg:%d", notes[0]), 10)
	requirements.NoError(err)
	assertions.Empty(reverse)
	var count int
	requirements.NoError(st.db.QueryRow(`SELECT count(*) FROM meeting_link_decisions`).Scan(&count))
	assertions.Zero(count)
}

func TestMeetingLinksSuggestionsTimeWindow(t *testing.T) {
	requirements := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, event := calendarFixture(t, st, "event", singleLinkRaw("event", "window@example.org"))
	var want []int64
	for _, tc := range []struct {
		name, sent, received, internal, raw string
		include                             bool
	}{
		{name: "scheduled", sent: "2020-01-01T00:00:00Z", raw: `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"window@example.org","startsAt":"2027-03-14T10:00:00Z"}}`, include: true},
		{name: "long recording", sent: "2027-03-12T10:00:00Z", raw: `{"started_at":"2027-03-12T10:00:00Z","ended_at":"2027-03-14T10:30:00Z"}`, include: true},
		{name: "received", received: "2027-03-14T10:00:00Z", raw: `{}`, include: true},
		{name: "internal", internal: "2027-03-14T10:00:00Z", raw: `{}`, include: true},
		{name: "past", sent: "2020-01-01T00:00:00Z", raw: `{}`},
		{name: "future", sent: "2030-01-01T00:00:00Z", raw: `{}`},
		{name: "scheduled overrides sent", sent: "2027-03-14T10:00:00Z", raw: `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"window@example.org","startsAt":"2030-01-01T00:00:00Z"}}`},
		{name: "ends at start", sent: "2027-03-14T09:30:00Z", raw: `{}`},
		{name: "starts at end", sent: "2027-03-14T11:00:00Z", raw: `{}`},
	} {
		format := "meeting_json"
		if strings.Contains(tc.raw, "schema_version") {
			format = "twenty_json"
		}
		data, id := projectionFixture(t, st, tc.name, format, tc.raw)
		for field, value := range map[*sql.NullTime]string{&data.Message.SentAt: tc.sent, &data.Message.ReceivedAt: tc.received, &data.Message.InternalDate: tc.internal} {
			if value != "" {
				at, err := time.Parse(time.RFC3339, value)
				requirements.NoError(err)
				*field = sql.NullTime{Time: at, Valid: true}
			}
		}
		_, err := st.UpsertMessage(data.Message)
		requirements.NoError(err)
		if tc.include {
			want = append(want, id)
		}
	}
	suggestions, err := st.SuggestMeetingLinksContext(t.Context(), fmt.Sprintf("evt:%d", event), 100)
	requirements.NoError(err)
	var got []int64
	for _, suggestion := range suggestions {
		got = append(got, suggestion.MemberMessageID)
	}
	assert.ElementsMatch(t, want, got)
}
