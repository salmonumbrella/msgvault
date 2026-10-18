package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/calrecur"
)

const (
	meetingSeriesKind      = "series"
	meetingCancelledStatus = "cancelled"
)

// OccurrenceRef identifies an event, or one original occurrence of a series.
type OccurrenceRef struct {
	MessageID int64
	Key       string
}

type meetingIdentity struct {
	ICalUID        string
	EventMessageID int64
	OccurrenceKey  string
}

type linkOccurrence struct {
	Ref        OccurrenceRef
	Row        CalendarEventRow
	Identity   meetingIdentity
	Start, End time.Time
}

type reboundReader struct {
	reader SQLReader
	rebind func(string) string
}

func (r reboundReader) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return r.reader.QueryContext(ctx, r.rebind(query), args...)
}
func (r reboundReader) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	return r.reader.QueryRowContext(ctx, r.rebind(query), args...)
}

func spendMeetingBudget(ctx context.Context, budget *int, count int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	*budget -= count
	if *budget < 0 {
		return ErrQueryTooBroad
	}
	return nil
}

func meetingEventRows(ctx context.Context, r SQLReader, predicate string, args []any, budget *int) ([]CalendarEventRow, error) {
	rows, err := queryCalendarEventRowsWithinBudget(ctx, r, predicate, args, min(200, *budget))
	if err != nil {
		return nil, err
	}
	*budget -= len(rows)
	return rows, nil
}

func occurrenceFromRow(row CalendarEventRow, key string) linkOccurrence {
	id := meetingIdentity{ICalUID: row.ICalUID, OccurrenceKey: key}
	if id.ICalUID == "" {
		id.EventMessageID = row.MessageID
	}
	o := linkOccurrence{Ref: OccurrenceRef{MessageID: row.MessageID, Key: key}, Row: row, Identity: id}
	if row.Event.Start != nil {
		o.Start = *row.Event.Start
	}
	if row.Event.End != nil {
		o.End = *row.Event.End
	}
	return o
}

func expandMeetingSeries(ctx context.Context, r SQLReader, master CalendarEventRow, from, to time.Time, budget *int) ([]linkOccurrence, error) {
	predicate, args := calendarExceptionWindow([]CalendarEventRow{master}, from, to)
	predicate += ` AND m.source_id = ? AND e.series_provider_id = ?`
	args = append(args, master.SourceID, master.Event.ProviderEventID)
	related, err := meetingEventRows(ctx, r, predicate, args, budget)
	if err != nil {
		return nil, err
	}
	expanded, count, reason, err := expandCalendarSeries(ctx, master, related, from, to, *budget)
	*budget -= count
	if err != nil {
		return nil, err
	}
	var expansionErr error
	if reason != "" {
		if *budget <= 0 || reason == calrecur.ErrBudgetExceeded.Error() {
			return nil, calendarRecurrenceBudgetError()
		}
		expansionErr = fmt.Errorf("%w: %s", calrecur.ErrUnsupportedRecurrence, reason)
	}
	byID := make(map[int64]CalendarEventRow, len(related))
	for _, row := range related {
		byID[row.MessageID] = row
	}
	out := make([]linkOccurrence, 0, len(expanded))
	for _, instance := range expanded {
		o := occurrenceFromRow(master, instance.Original.Key())
		if instance.HasException {
			o.Row = byID[instance.ExceptionRef]
			if instance.Original == (calrecur.Identity{}) {
				o = occurrenceFromRow(o.Row, "")
			}
			if o.Row.Event.Status == meetingCancelledStatus {
				o.Row, err = inheritCalendarCancellation(ctx, r, o.Row, master)
				if err != nil {
					return nil, err
				}
			}
		}
		o.Start, o.End = instance.Start, instance.End
		if instance.Cancelled || master.Event.Status == meetingCancelledStatus {
			o.Row.Event.Status = meetingCancelledStatus
		}
		out = append(out, o)
	}
	return out, expansionErr
}

func resolveMeetingOccurrence(ctx context.Context, r SQLReader, ref OccurrenceRef, budget *int) (linkOccurrence, error) {
	rows, err := meetingEventRows(ctx, r, `e.message_id = ?`, []any{ref.MessageID}, budget)
	if err != nil {
		return linkOccurrence{}, err
	}
	if len(rows) != 1 {
		return linkOccurrence{}, sql.ErrNoRows
	}
	row := rows[0]
	if row.Event.Kind == "exception" && row.Event.Original != nil && ref.Key == "" {
		masters, err := meetingEventRows(ctx, r, `e.kind = 'series' AND m.source_id = ? AND e.provider_event_id = ?`, []any{row.SourceID, row.Event.SeriesProviderID}, budget)
		if err != nil {
			return linkOccurrence{}, err
		}
		if len(masters) == 1 {
			if row.Event.Start != nil && row.Event.End != nil {
				o := occurrenceFromRow(masters[0], row.Event.Original.Key())
				o.Row, o.Start, o.End = row, *row.Event.Start, *row.Event.End
				if masters[0].Event.Status == meetingCancelledStatus {
					o.Row.Event.Status = meetingCancelledStatus
				}
				return o, nil
			}
			row, ref = masters[0], OccurrenceRef{MessageID: masters[0].MessageID, Key: row.Event.Original.Key()}
		} else {
			if row.Event.Start == nil || row.Event.End == nil {
				return linkOccurrence{}, fmt.Errorf("%s: %w", calendarMissingBoundsReason(row.Event), sql.ErrNoRows)
			}
			o := occurrenceFromRow(row, row.Event.Original.Key())
			o.Ref.Key = ""
			return o, nil
		}
	}
	if ref.Key == "" {
		if row.Event.Kind == meetingSeriesKind {
			return linkOccurrence{}, sql.ErrNoRows
		}
		if row.Event.Start == nil || row.Event.End == nil {
			return linkOccurrence{}, fmt.Errorf("%s: %w", calendarMissingBoundsReason(row.Event), sql.ErrNoRows)
		}
		return occurrenceFromRow(row, ""), nil
	}
	if row.Event.Kind != meetingSeriesKind {
		return linkOccurrence{}, sql.ErrNoRows
	}
	_, id, ok := calrecur.ParseInstanceID("event_" + ref.Key)
	if !ok {
		return linkOccurrence{}, fmt.Errorf("invalid occurrence key %q", ref.Key)
	}
	at := id.Instant
	if id.AllDay {
		zone, err := calendarEventZone(row.Event)
		if err != nil {
			return linkOccurrence{}, err
		}
		at, _, err = calrecur.CivilDateBoundary(id.Date, zone)
		if err != nil {
			return linkOccurrence{}, err
		}
	}
	// A moved exception is expanded at its effective start, retaining Original.
	originalColumn, originalValue := "e.original_start_at", any(id.Instant)
	if id.AllDay {
		originalColumn, originalValue = "e.original_start_date", id.Date
	}
	moved, err := meetingEventRows(ctx, r, `e.kind = 'exception' AND m.source_id = ? AND e.series_provider_id = ? AND `+originalColumn+` = ?`, []any{row.SourceID, row.Event.ProviderEventID, originalValue}, budget)
	if err != nil {
		return linkOccurrence{}, err
	}
	for _, x := range moved {
		if x.Event.Start != nil && x.Event.End != nil {
			o := occurrenceFromRow(row, ref.Key)
			o.Row, o.Start, o.End = x, *x.Event.Start, *x.Event.End
			if row.Event.Status == meetingCancelledStatus {
				o.Row.Event.Status = meetingCancelledStatus
			}
			return o, nil
		}
		if x.Event.Start != nil {
			at = *x.Event.Start
		}
	}
	occurrences, err := expandMeetingSeries(ctx, r, row, at.Add(-calendarCandidateMargin), at.Add(calendarCandidateMargin), budget)
	if err != nil && !errors.Is(err, calrecur.ErrUnsupportedRecurrence) {
		return linkOccurrence{}, err
	}
	for _, o := range occurrences {
		if o.Ref.Key == ref.Key {
			return o, nil
		}
	}
	if err != nil {
		return linkOccurrence{}, err
	}
	return linkOccurrence{}, sql.ErrNoRows
}

func memberLinkKey(ctx context.Context, r SQLReader, id int64) (meetingLinkKey, error) {
	var key meetingLinkKey
	var at sql.NullTime
	err := r.QueryRowContext(ctx, `SELECT kind,value,calendar_id,scheduled_at FROM meeting_link_key WHERE message_id = ?`, id).Scan(&key.Kind, &key.Value, &key.CalendarID, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return key, nil
	}
	if at.Valid {
		key.ScheduledAt = &at.Time
	}
	return key, err
}

func (s *Store) calendarUIDPredicate() string {
	uidExpr := "json_extract(m.metadata, '$.ical_uid')"
	if s.IsPostgreSQL() {
		uidExpr = "m.metadata ->> 'ical_uid'"
	}
	return uidExpr + ` = ? AND m.message_type = 'calendar_event'`
}

func (s *Store) automaticMeetingOccurrences(ctx context.Context, r SQLReader, key meetingLinkKey, budget *int) ([]linkOccurrence, error) {
	predicate, args := s.calendarUIDPredicate(), []any{key.Value}
	base, instance, _ := calrecur.ParseInstanceID(key.Value)
	if key.Kind == "google_event_id" {
		// Parse the same source configuration used by calendar persistence.
		sources, err := r.QueryContext(ctx, `SELECT id,sync_config FROM sources WHERE source_type = 'gcal'`)
		if err != nil {
			return nil, err
		}
		defer func() { _ = sources.Close() }()
		var sourceIDs []any
		for sources.Next() {
			var id int64
			var config sql.NullString
			if err := sources.Scan(&id, &config); err != nil {
				return nil, err
			}
			if NormalizeIdentifierForCompare(strings.TrimSpace(decodeCalendarSourceConfig(config).CalendarID)) == key.CalendarID {
				sourceIDs = append(sourceIDs, id)
			}
		}
		err = errors.Join(sources.Err(), sources.Close())
		if err != nil {
			return nil, err
		}
		if len(sourceIDs) == 0 {
			return nil, nil
		}
		predicate = `m.source_id IN (` + placeholders(len(sourceIDs)) + `) AND (e.provider_event_id = ? OR (e.kind = 'series' AND e.provider_event_id = ?))`
		args = sourceIDs
		args = append(args, key.Value, base)
	} else if key.ScheduledAt != nil {
		window, windowArgs := calendarExceptionWindow(nil, key.ScheduledAt.Add(-calendarCandidateMargin), key.ScheduledAt.Add(calendarCandidateMargin))
		predicate += ` AND (e.kind != 'exception' OR (` + window + `))`
		args = append(args, windowArgs...)
	} else {
		predicate += ` AND e.kind != 'exception'`
	}
	rows, err := meetingEventRows(ctx, r, predicate, args, budget)
	if err != nil {
		return nil, err
	}
	var out []linkOccurrence
candidatesLoop:
	for _, row := range rows {
		if row.Event.Kind == "exception" {
			for _, master := range rows {
				if master.Event.Kind == meetingSeriesKind && master.SourceID == row.SourceID && master.Event.ProviderEventID == row.Event.SeriesProviderID {
					continue candidatesLoop
				}
			}
		}
		var candidates []linkOccurrence
		if key.Kind == "google_event_id" {
			ref := OccurrenceRef{MessageID: row.MessageID}
			if row.Event.Kind == meetingSeriesKind {
				if base == "" {
					continue
				}
				ref.Key = instance.Key()
			}
			o, err := resolveMeetingOccurrence(ctx, r, ref, budget)
			if errors.Is(err, sql.ErrNoRows) || errors.Is(err, calrecur.ErrUnsupportedRecurrence) {
				continue
			}
			if err != nil {
				return nil, err
			}
			candidates = append(candidates, o)
		} else if row.Event.Kind == meetingSeriesKind {
			if key.ScheduledAt == nil {
				continue
			}
			// PostgreSQL rounds a nanosecond window to zero; compare exact starts below.
			candidates, err = expandMeetingSeries(ctx, r, row, *key.ScheduledAt, key.ScheduledAt.Add(time.Second), budget)
			if err != nil && !errors.Is(err, calrecur.ErrUnsupportedRecurrence) {
				return nil, err
			}
		} else {
			o, err := resolveMeetingOccurrence(ctx, r, OccurrenceRef{MessageID: row.MessageID}, budget)
			if errors.Is(err, sql.ErrNoRows) || errors.Is(err, calrecur.ErrUnsupportedRecurrence) {
				continue
			}
			if err != nil {
				return nil, err
			}
			candidates = append(candidates, o)
		}
		for _, o := range candidates {
			if key.Kind == "ical_uid" && row.Event.Kind != "single" {
				if key.ScheduledAt == nil || !o.Start.Equal(*key.ScheduledAt) {
					continue
				}
			}
			out = append(out, o)
		}
	}
	for _, o := range out {
		if o.Identity != out[0].Identity {
			return nil, nil
		}
	}
	return out, nil
}

type meetingLinkDecision struct {
	Ref       OccurrenceRef
	Decision  string
	CreatedAt time.Time
}

func (s *Store) meetingMemberOccurrences(ctx context.Context, r SQLReader, memberID int64, budget *int) ([]linkOccurrence, error) {
	decisions, err := readMeetingLinkDecisions(ctx, r, memberID)
	if err != nil {
		return nil, err
	}
	type resolvedDecision struct {
		meetingLinkDecision

		target linkOccurrence
	}
	var resolved []resolvedDecision
	splits := map[meetingIdentity]time.Time{}
	for _, d := range decisions {
		o, err := resolveMeetingOccurrence(ctx, r, d.Ref, budget)
		if errors.Is(err, sql.ErrNoRows) || errors.Is(err, calrecur.ErrUnsupportedRecurrence) {
			continue
		}
		if err != nil {
			return nil, err
		}
		resolved = append(resolved, resolvedDecision{d, o})
		if d.Decision == "split" && !d.CreatedAt.Before(splits[o.Identity]) {
			splits[o.Identity] = d.CreatedAt
		}
	}
	var out []linkOccurrence
	for _, d := range resolved {
		if d.Decision == "link" && d.CreatedAt.After(splits[d.target.Identity]) {
			out = append(out, d.target)
		}
	}
	if len(out) > 0 {
		return out, nil
	}
	key, err := memberLinkKey(ctx, r, memberID)
	if err != nil {
		return nil, err
	}
	if key.Value != "" {
		matches, err := s.automaticMeetingOccurrences(ctx, r, key, budget)
		if err != nil {
			return nil, err
		}
		for _, o := range matches {
			if _, split := splits[o.Identity]; !split {
				out = append(out, o)
			}
		}
	}
	return out, nil
}

func (s *Store) meetingReader(ctx context.Context) SQLReader {
	return reboundReader{reader: ReadDBContext(ctx, s.db.DB), rebind: s.dialect.Rebind}
}
