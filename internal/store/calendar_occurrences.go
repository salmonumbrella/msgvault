package store

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"go.kenn.io/msgvault/internal/calevent"
	"go.kenn.io/msgvault/internal/calrecur"
)

type CalendarOccurrence struct {
	MessageID         int64               `json:"message_id"`
	SeriesMessageID   int64               `json:"series_message_id"`
	SourceID          int64               `json:"source_id"`
	Subject           string              `json:"subject"`
	Start             time.Time           `json:"start"`
	End               time.Time           `json:"end"`
	AllDay            bool                `json:"all_day"`
	StartDate         string              `json:"start_date"`
	EndDate           string              `json:"end_date"`
	Status            string              `json:"status"`
	DeletedFromSource bool                `json:"deleted_from_source"`
	Attendees         []calevent.Attendee `json:"attendees"`
}
type CalendarUnexpandable struct {
	MessageID int64  `json:"message_id"`
	Reason    string `json:"reason"`
}
type CalendarOccurrencePage struct {
	Occurrences  []CalendarOccurrence   `json:"occurrences"`
	Unexpandable []CalendarUnexpandable `json:"unexpandable"`
}

// ListCalendarOccurrencesContext expands archived series in a half-open window.
func (s *Store) ListCalendarOccurrencesContext(ctx context.Context, from, to time.Time) (CalendarOccurrencePage, error) {
	page := CalendarOccurrencePage{Occurrences: []CalendarOccurrence{}, Unexpandable: []CalendarUnexpandable{}}
	if !to.After(from) {
		return page, errors.New("to must be after from")
	}
	ctx, release, err := s.BeginReadSnapshotContext(ctx)
	if err != nil {
		return page, err
	}
	defer release()
	rows, err := calendarEventRowsInWindow(ctx, s.db, from, to)
	if err != nil {
		return page, err
	}
	type seriesKey struct {
		source int64
		id     string
	}
	exceptions := map[seriesKey][]CalendarEventRow{}
	masters := map[seriesKey]bool{}
	byID := make(map[int64]CalendarEventRow, len(rows))
	for _, row := range rows {
		byID[row.MessageID] = row
		if row.Event.Kind == "series" {
			masters[seriesKey{row.SourceID, row.Event.ProviderEventID}] = true
		}
		if row.Event.Kind == "exception" {
			key := seriesKey{row.SourceID, row.Event.SeriesProviderID}
			exceptions[key] = append(exceptions[key], row)
		}
	}
	remaining := calrecur.RequestReadBudget - len(rows)
	if remaining < 0 {
		return page, ErrQueryTooBroad
	}
	add := func(row CalendarEventRow, seriesID int64, o calrecur.Occurrence) {
		e := row.Event
		if !calendarOccurrenceOverlaps(o, from, to) {
			return
		}
		status := e.Status
		if o.Cancelled {
			status = "cancelled"
		}
		attendees := e.Attendees
		if attendees == nil {
			attendees = []calevent.Attendee{}
		}
		page.Occurrences = append(page.Occurrences, CalendarOccurrence{MessageID: row.MessageID, SeriesMessageID: seriesID, SourceID: row.SourceID, Subject: row.Subject, Start: o.Start, End: o.End, AllDay: o.AllDay, StartDate: o.StartDate, EndDate: o.EndDate, Status: status, DeletedFromSource: row.DeletedFromSource, Attendees: attendees})
	}
	for _, row := range rows {
		if err := ctx.Err(); err != nil {
			return page, err
		}
		e := row.Event
		switch e.Kind {
		case "exception", "single":
			if e.Kind == "exception" && masters[seriesKey{row.SourceID, e.SeriesProviderID}] {
				continue
			}
			if e.Start != nil && e.End != nil {
				add(row, 0, calrecur.Occurrence{Start: *e.Start, End: *e.End, AllDay: e.AllDay, StartDate: e.StartDate, EndDate: e.EndDate, Cancelled: e.Status == "cancelled"})
			} else {
				reason := "missing event bounds"
				if e.Kind == "exception" {
					reason = "missing series master and event bounds"
				}
				page.Unexpandable = append(page.Unexpandable, CalendarUnexpandable{MessageID: row.MessageID, Reason: reason})
			}
		case "series":
			fail := func() {
				page.Unexpandable = append(page.Unexpandable, CalendarUnexpandable{MessageID: row.MessageID, Reason: "unsupported recurrence"})
			}
			if row.RecurrenceError != "" || e.Start == nil || e.End == nil {
				fail()
				continue
			}
			set, duration, days, err := prepareCalendarRecurrence(e)
			if err != nil {
				fail()
				continue
			}
			var exc []calrecur.Exception
			invalid := false
			for _, x := range exceptions[seriesKey{row.SourceID, e.ProviderEventID}] {
				if x.Event.Original == nil {
					invalid = true
					break
				}
				xzone, err := calendarEventZone(x.Event)
				if err != nil {
					invalid = true
					break
				}
				exc = append(exc, calrecur.Exception{Original: *x.Event.Original, Start: x.Event.Start, End: x.Event.End, StartDate: x.Event.StartDate, EndDate: x.Event.EndDate, Zone: xzone, Cancelled: x.Event.Status == "cancelled", Ref: x.MessageID})
			}
			if invalid {
				fail()
				continue
			}
			occurrences, count, err := calrecur.Expand(ctx, set, duration, days, exc, from.Add(-calendarCandidateMargin), to.Add(calendarCandidateMargin), min(remaining, calrecur.SeriesReadBudget))
			remaining -= count
			if err != nil {
				if ctx.Err() != nil {
					return page, ctx.Err()
				}
				if errors.Is(err, calrecur.ErrBudgetExceeded) {
					if remaining < 0 {
						return page, ErrQueryTooBroad
					}
					page.Unexpandable = append(page.Unexpandable, CalendarUnexpandable{MessageID: row.MessageID, Reason: err.Error()})
					continue
				}
				fail()
				continue
			}
			for _, o := range occurrences {
				effective := row
				if o.ExceptionRef != 0 {
					effective = byID[o.ExceptionRef]
					if effective.Event.Status == "cancelled" {
						effective, err = inheritCalendarCancellation(ctx, s.db, effective, row)
						if err != nil {
							return page, err
						}
					}
				}
				if e.Status == "cancelled" {
					o.Cancelled = true
				}
				add(effective, row.MessageID, o)
			}
		}
	}
	slices.SortFunc(page.Occurrences, func(a, b CalendarOccurrence) int {
		if c := a.Start.Compare(b.Start); c != 0 {
			return c
		}
		return cmp.Compare(a.MessageID, b.MessageID)
	})
	return page, nil
}
func calendarEventZone(e calevent.Event) (*time.Location, error) {
	name := e.EventTimeZone
	if name == "" {
		name = e.CalendarTimeZone
	}
	if name == "" {
		return time.UTC, nil
	}
	zone, err := time.LoadLocation(name)
	if err != nil {
		return nil, fmt.Errorf("calendar time zone: %w", err)
	}
	return zone, nil
}

// All-day dates belong to the query's civil days, independent of the calendar zone.
func calendarOccurrenceOverlaps(o calrecur.Occurrence, from, to time.Time) bool {
	if o.AllDay {
		endDate := to.Format(time.DateOnly)
		if to.Hour() != 0 || to.Minute() != 0 || to.Second() != 0 || to.Nanosecond() != 0 {
			endDate = to.AddDate(0, 0, 1).Format(time.DateOnly)
		}
		return o.StartDate < endDate && o.EndDate > from.Format(time.DateOnly)
	}
	return o.Start.Before(to) && o.End.After(from)
}

func prepareCalendarRecurrence(e calevent.Event) (calrecur.Set, time.Duration, int, error) {
	if e.Start == nil || e.End == nil {
		return calrecur.Set{}, 0, 0, errors.New("missing event bounds")
	}
	zone, err := calendarEventZone(e)
	if err != nil {
		return calrecur.Set{}, 0, 0, err
	}
	set, err := calrecur.ParseSet(*e.Start, e.AllDay, e.StartDate, zone, e.Recurrence)
	if err != nil {
		return set, 0, 0, err
	}
	days := 0
	if e.AllDay {
		first, err := time.Parse(time.DateOnly, e.StartDate)
		if err != nil {
			return set, 0, 0, fmt.Errorf("calendar start date: %w", err)
		}
		last, err := time.Parse(time.DateOnly, e.EndDate)
		if err != nil {
			return set, 0, 0, fmt.Errorf("calendar end date: %w", err)
		}
		days = int(last.Sub(first) / (24 * time.Hour))
	}
	return set, e.End.Sub(*e.Start), days, nil
}
