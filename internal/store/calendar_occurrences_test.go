package store

import (
	"fmt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"testing"
	"time"
)

func TestListCalendarOccurrences(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st := newRFC822IDBackfillBackendStore(t)
	_, master := calendarFixture(t, st, "master", `{"id":"weekly","status":"confirmed","start":{"dateTime":"2027-03-02T17:00:00Z"},"end":{"dateTime":"2027-03-02T18:00:00Z"},"recurrence":["RRULE:FREQ=WEEKLY;COUNT=3"],"attendees":[{"email":"guest@example.com","responseStatus":"accepted"}]}`)
	calendarFixture(t, st, "cancelled", `{"id":"cancelled","recurringEventId":"weekly","originalStartTime":{"dateTime":"2027-03-09T17:00:00Z"},"status":"cancelled"}`)
	calendarFixture(t, st, "moved", `{"id":"moved","recurringEventId":"weekly","status":"confirmed","originalStartTime":{"dateTime":"2027-03-16T17:00:00Z"},"start":{"dateTime":"2027-04-02T19:00:00Z"},"end":{"dateTime":"2027-04-02T20:00:00Z"}}`)
	_, single := calendarFixture(t, st, "single", calendarRaw)
	from, err := time.Parse(time.RFC3339, "2027-03-01T00:00:00Z")
	requirements.NoError(err)
	page, err := st.ListCalendarOccurrencesContext(t.Context(), from, from.AddDate(0, 1, 0))
	requirements.NoError(err)
	requirements.Len(page.Occurrences, 3)
	assertions.Empty(page.Unexpandable)
	assertions.Equal(master, page.Occurrences[0].MessageID)
	assertions.Equal(single, page.Occurrences[1].MessageID)
	assertions.True(page.Occurrences[2].Cancelled)
	assertions.Equal("cancelled", page.Occurrences[2].Status)
	assertions.Equal(master, page.Occurrences[2].SeriesMessageID)
	assertions.Equal("accepted", page.Occurrences[0].Attendees[0].RSVP)
	page, err = st.ListCalendarOccurrencesContext(t.Context(), from.AddDate(0, 1, 0), from.AddDate(0, 2, 0))
	requirements.NoError(err)
	requirements.Len(page.Occurrences, 1)
	assertions.Equal(master, page.Occurrences[0].SeriesMessageID)
	assertions.Equal("2027-04-02T19:00:00Z", page.Occurrences[0].Start.Format(time.RFC3339))
}
func TestListCalendarOccurrencesUnexpandable(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st := newRFC822IDBackfillBackendStore(t)
	_, id := calendarFixture(t, st, "unsupported", `{"id":"unsupported","start":{"date":"2027-03-01"},"end":{"date":"2027-03-02"},"recurrence":["RRULE:FREQ=HOURLY"]}`)
	from, err := time.Parse(time.DateOnly, "2027-03-01")
	requirements.NoError(err)
	page, err := st.ListCalendarOccurrencesContext(t.Context(), from, from.AddDate(0, 1, 0))
	requirements.NoError(err)
	assertions.Empty(page.Occurrences)
	requirements.Len(page.Unexpandable, 1)
	assertions.Equal(id, page.Unexpandable[0].MessageID)
}

func TestListCalendarOccurrencesSharesRequestBudget(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	from, err := time.Parse(time.DateOnly, "2027-03-01")
	requirements.NoError(err)
	for i := range 10 {
		calendarFixture(t, st, fmt.Sprintf("long-%d", i), fmt.Sprintf(`{"id":"long-%d","start":{"dateTime":"1800-01-01T17:00:00Z"},"end":{"dateTime":"1800-01-01T18:00:00Z"},"recurrence":["RRULE:FREQ=DAILY"]}`, i))
		if i == 0 {
			page, err := st.ListCalendarOccurrencesContext(t.Context(), from, from.AddDate(0, 1, 0))
			requirements.NoError(err)
			requirements.Len(page.Unexpandable, 1)
			assertions.Contains(page.Unexpandable[0].Reason, "budget")
		}
	}
	_, err = st.ListCalendarOccurrencesContext(t.Context(), from, from.AddDate(0, 1, 0))
	assertions.ErrorIs(err, ErrQueryTooBroad)
}
