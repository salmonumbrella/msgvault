package store

import (
	"fmt"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestListCalendarOccurrences(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st := newRFC822IDBackfillBackendStore(t)
	_, master := calendarFixture(t, st, "master", `{"id":"weekly","status":"confirmed","start":{"dateTime":"2027-03-02T17:00:00Z"},"end":{"dateTime":"2027-03-02T18:00:00Z"},"recurrence":["RRULE:FREQ=WEEKLY;COUNT=3"],"attendees":[{"email":"guest@example.com","responseStatus":"accepted"}]}`)
	_, cancelled := calendarFixture(t, st, "cancelled", `{"id":"cancelled","recurringEventId":"weekly","originalStartTime":{"dateTime":"2027-03-09T17:00:00Z"},"status":"cancelled"}`)
	_, moved := calendarFixture(t, st, "moved", `{"id":"moved","recurringEventId":"weekly","status":"confirmed","originalStartTime":{"dateTime":"2027-03-16T17:00:00Z"},"start":{"dateTime":"2027-04-02T19:00:00Z"},"end":{"dateTime":"2027-04-02T20:00:00Z"}}`)
	_, err := st.db.Exec(`UPDATE messages SET subject = '' WHERE id = ?`, cancelled)
	requirements.NoError(err)
	_, single := calendarFixture(t, st, "single", calendarRaw)
	from, err := time.Parse(time.RFC3339, "2027-03-01T00:00:00Z")
	requirements.NoError(err)
	page, err := st.ListCalendarOccurrencesContext(t.Context(), from, from.AddDate(0, 1, 0))
	requirements.NoError(err)
	requirements.Len(page.Occurrences, 3)
	assertions.Empty(page.Unexpandable)
	assertions.Equal(master, page.Occurrences[0].MessageID)
	assertions.Equal(single, page.Occurrences[1].MessageID)
	assertions.Equal("cancelled", page.Occurrences[2].Status)
	assertions.Equal(master, page.Occurrences[2].SeriesMessageID)
	assertions.Equal(cancelled, page.Occurrences[2].MessageID)
	assertions.Equal("Example event", page.Occurrences[2].Subject)
	assertions.Equal(page.Occurrences[0].Attendees, page.Occurrences[2].Attendees)
	assertions.Equal("accepted", page.Occurrences[0].Attendees[0].RSVP)
	page, err = st.ListCalendarOccurrencesContext(t.Context(), from.AddDate(0, 1, 0), from.AddDate(0, 2, 0))
	requirements.NoError(err)
	requirements.Len(page.Occurrences, 1)
	assertions.Equal(master, page.Occurrences[0].SeriesMessageID)
	assertions.Equal(moved, page.Occurrences[0].MessageID)
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

func TestListCalendarOccurrencesOrphanExceptions(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, complete := calendarFixture(t, st, "orphan", `{"id":"orphan","recurringEventId":"missing","status":"confirmed","originalStartTime":{"dateTime":"2027-03-02T17:00:00Z"},"start":{"dateTime":"2027-03-03T17:00:00Z"},"end":{"dateTime":"2027-03-03T18:00:00Z"}}`)
	_, incomplete := calendarFixture(t, st, "sparse-orphan", `{"id":"sparse-orphan","recurringEventId":"missing","status":"cancelled","originalStartTime":{"dateTime":"2027-03-04T17:00:00Z"}}`)
	_, unidentified := calendarFixture(t, st, "unidentified-orphan", `{"id":"unidentified-orphan","recurringEventId":"missing","status":"cancelled"}`)
	from := time.Date(2027, 3, 1, 0, 0, 0, 0, time.UTC)
	page, err := st.ListCalendarOccurrencesContext(t.Context(), from, from.AddDate(0, 1, 0))
	requirements.NoError(err)
	requirements.Len(page.Occurrences, 1)
	assertions.Equal(complete, page.Occurrences[0].MessageID)
	assertions.Zero(page.Occurrences[0].SeriesMessageID)
	assertions.Equal("2027-03-03T17:00:00Z", page.Occurrences[0].Start.Format(time.RFC3339))
	assertions.ElementsMatch([]CalendarUnexpandable{{MessageID: incomplete, Reason: "missing series master and event bounds"}, {MessageID: unidentified, Reason: "missing series master and event bounds"}}, page.Unexpandable)
}

func TestListCalendarOccurrencesAllDayQueryZone(t *testing.T) {
	for _, recurring := range []bool{false, true} {
		t.Run(strconv.FormatBool(recurring), func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			recurrence := ""
			if recurring {
				recurrence = `,"recurrence":["RRULE:FREQ=DAILY;COUNT=2"]`
			}
			data, id := calendarFixture(t, st, "all-day", `{"id":"all-day","start":{"date":"2027-03-02"},"end":{"date":"2027-03-03"}`+recurrence+`}`)
			requirements.NoError(st.UpdateSourceSyncConfig(data.Message.SourceID, `{"time_zone":"Pacific/Auckland"}`))
			zone, err := time.LoadLocation("America/Los_Angeles")
			requirements.NoError(err)
			from := time.Date(2027, 3, 1, 0, 0, 0, 0, zone)
			page, err := st.ListCalendarOccurrencesContext(t.Context(), from, from.AddDate(0, 0, 1))
			requirements.NoError(err)
			assertions.Empty(page.Occurrences, "March 2 is outside March 1")
			from = from.AddDate(0, 0, 1)
			page, err = st.ListCalendarOccurrencesContext(t.Context(), from, from.AddDate(0, 0, 1))
			requirements.NoError(err)
			requirements.Len(page.Occurrences, 1)
			assertions.Equal(id, page.Occurrences[0].MessageID)
			assertions.Equal("2027-03-02", page.Occurrences[0].StartDate)
		})
	}
}

func TestListCalendarOccurrencesCancellationSuppliedFields(t *testing.T) {
	for _, tc := range []struct {
		name, fields, subject string
		attendees             int
	}{
		{"omitted", "", "Example event", 1},
		{"supplied", `,"summary":"Cancelled planning","attendees":[{"email":"other@example.com","responseStatus":"declined"}]`, "Cancelled planning", 1},
		{"cleared", `,"summary":"","attendees":[]`, "", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			_, master := calendarFixture(t, st, "master", `{"id":"weekly","status":"confirmed","start":{"dateTime":"2027-03-02T17:00:00Z"},"end":{"dateTime":"2027-03-02T18:00:00Z"},"recurrence":["RRULE:FREQ=WEEKLY;COUNT=2"],"attendees":[{"email":"guest@example.com","responseStatus":"accepted"}]}`)
			_, cancelled := calendarFixture(t, st, "cancelled", `{"id":"cancelled","recurringEventId":"weekly","originalStartTime":{"dateTime":"2027-03-09T17:00:00Z"},"status":"cancelled"`+tc.fields+`}`)
			subject := tc.subject
			if tc.name == "omitted" {
				subject = ""
			}
			_, err := st.db.Exec(`UPDATE messages SET subject = ? WHERE id = ?`, subject, cancelled)
			requirements.NoError(err)
			from := time.Date(2027, 3, 9, 0, 0, 0, 0, time.UTC)
			page, err := st.ListCalendarOccurrencesContext(t.Context(), from, from.AddDate(0, 0, 1))
			requirements.NoError(err)
			requirements.Len(page.Occurrences, 1)
			o := page.Occurrences[0]
			assertions.Equal(cancelled, o.MessageID)
			assertions.Equal(master, o.SeriesMessageID)
			assertions.Equal(tc.subject, o.Subject)
			assertions.Equal("cancelled", o.Status)
			requirements.Len(o.Attendees, tc.attendees)
			if tc.name == "supplied" {
				assertions.Equal("other@example.com", o.Attendees[0].Email)
			}
		})
	}
}

func TestListCalendarOccurrencesAllDayPartialWindow(t *testing.T) {
	for _, tc := range []struct {
		name, endDate, from, to string
		want                    int
	}{
		{"inside", "2027-03-03", "2027-03-02T09:00:00Z", "2027-03-02T17:00:00Z", 1},
		{"end midnight", "2027-03-03", "2027-03-01T09:00:00Z", "2027-03-02T00:00:00Z", 0},
		{"start midday", "2027-03-03", "2027-03-02T17:00:00Z", "2027-03-03T00:00:00Z", 1},
		{"after", "2027-03-03", "2027-03-03T00:00:00Z", "2027-03-03T17:00:00Z", 0},
		{"multi day", "2027-03-05", "2027-03-04T09:00:00-05:00", "2027-03-04T17:00:00-05:00", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requirements := require.New(t)

			st := newRFC822IDBackfillBackendStore(t)
			calendarFixture(t, st, "partial", fmt.Sprintf(`{"id":"partial","start":{"date":"2027-03-02"},"end":{"date":%q}}`, tc.endDate))
			from, err := time.Parse(time.RFC3339, tc.from)
			requirements.NoError(err)
			to, err := time.Parse(time.RFC3339, tc.to)
			requirements.NoError(err)
			page, err := st.ListCalendarOccurrencesContext(t.Context(), from, to)
			requirements.NoError(err)
			assert.Len(t, page.Occurrences, tc.want)
		})
	}
}

func TestListCalendarOccurrencesMultiDayExceptions(t *testing.T) {
	for _, allDay := range []bool{false, true} {
		for _, moved := range []bool{false, true} {
			t.Run(fmt.Sprintf("allDay=%t/moved=%t", allDay, moved), func(t *testing.T) {
				assertions, requirements := assert.New(t), require.New(t)
				st := newRFC822IDBackfillBackendStore(t)
				start, end := `{"dateTime":"2027-03-01T09:00:00Z"}`, `{"dateTime":"2027-03-08T09:00:00Z"}`
				newStart, newEnd := `{"dateTime":"2027-04-01T09:00:00Z"}`, `{"dateTime":"2027-04-08T09:00:00Z"}`
				if allDay {
					start, end = `{"date":"2027-03-01"}`, `{"date":"2027-03-08"}`
					newStart, newEnd = `{"date":"2027-04-01"}`, `{"date":"2027-04-08"}`
				}
				_, master := calendarFixture(t, st, "master", fmt.Sprintf(`{"id":"weekly","status":"confirmed","start":%s,"end":%s,"recurrence":["RRULE:FREQ=WEEKLY;COUNT=2"]}`, start, end))
				fields := `"status":"cancelled"`
				if moved {
					fields = fmt.Sprintf(`"status":"confirmed","start":%s,"end":%s`, newStart, newEnd)
				}
				_, exception := calendarFixture(t, st, "exception", fmt.Sprintf(`{"id":"exception","recurringEventId":"weekly","originalStartTime":%s,%s}`, start, fields))
				from := time.Date(2027, 3, 5, 0, 0, 0, 0, time.UTC)
				page, err := st.ListCalendarOccurrencesContext(t.Context(), from, from.AddDate(0, 0, 1))
				requirements.NoError(err)
				assertions.Empty(page.Unexpandable)
				if moved {
					assertions.Empty(page.Occurrences, "the week-long occurrence moved to April")
					return
				}
				requirements.Len(page.Occurrences, 1)
				assertions.Equal(exception, page.Occurrences[0].MessageID)
				assertions.Equal(master, page.Occurrences[0].SeriesMessageID)
				assertions.Equal("cancelled", page.Occurrences[0].Status)
			})
		}
	}
}
