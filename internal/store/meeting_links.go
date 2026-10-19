package store

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
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
	reader  SQLReader
	rebind  func(string) string
	dialect Dialect
}

func (r reboundReader) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return r.reader.QueryContext(ctx, r.rebind(query), args...)
}
func (r reboundReader) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	return r.reader.QueryRowContext(ctx, r.rebind(query), args...)
}

func meetingEventRows(ctx context.Context, r reboundReader, predicate string, args []any, budget *int) ([]CalendarEventRow, error) {
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

func expandMeetingSeries(ctx context.Context, r reboundReader, master CalendarEventRow, from, to time.Time, budget *int) ([]linkOccurrence, error) {
	predicate, args := calendarExceptionWindow(r.dialect, from, to)
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
		effective, instance, err := materializeCalendarOccurrence(ctx, r, master, byID, instance)
		if err != nil {
			return nil, err
		}
		o := occurrenceFromRow(master, instance.Original.Key())
		if instance.HasException && instance.Original == (calrecur.Identity{}) {
			o = occurrenceFromRow(effective, "")
		}
		o.Row = effective
		o.Start, o.End = instance.Start, instance.End
		out = append(out, o)
	}
	return out, expansionErr
}

func resolveMeetingOccurrence(ctx context.Context, r reboundReader, ref OccurrenceRef, budget *int) (linkOccurrence, error) {
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
				effective, err := resolveCompleteCalendarException(masters[0], row)
				if err != nil {
					return linkOccurrence{}, err
				}
				o := occurrenceFromRow(masters[0], row.Event.Original.Key())
				o.Row, o.Start, o.End = row, effective.Start, effective.End
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
			effective, err := resolveCompleteCalendarException(row, x)
			if err != nil {
				return linkOccurrence{}, err
			}
			o := occurrenceFromRow(row, ref.Key)
			o.Row, o.Start, o.End = x, effective.Start, effective.End
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
	return calendarUIDExpression(s.dialect) + ` = ? AND m.message_type = 'calendar_event'`
}

func (s *Store) automaticMeetingOccurrences(ctx context.Context, r reboundReader, key meetingLinkKey, budget *int) ([]linkOccurrence, error) {
	if key.Kind != "google_event_id" && key.Kind != "ical_uid" {
		return nil, nil
	}
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
			if calendarMailbox(config) == key.CalendarID {
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
		window, windowArgs := calendarExceptionWindow(r.dialect, key.ScheduledAt.Add(-calendarCandidateMargin), key.ScheduledAt.Add(calendarCandidateMargin))
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
	Identity  meetingIdentity
	Decision  string
	CreatedAt time.Time
}

func (s *Store) meetingMemberOccurrences(ctx context.Context, r reboundReader, memberID int64, budget *int) ([]linkOccurrence, error) {
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
		if d.Decision == "split" {
			// Alias expansion is optional during a read. Keep the saved identity
			// and its budget available for other links when a long or unsupported
			// recurrence prevents discovering every alias.
			aliasBudget := *budget
			identities, err := s.meetingSplitIdentities(ctx, r, d.Identity, &aliasBudget)
			if errors.Is(err, ErrQueryTooBroad) || errors.Is(err, calrecur.ErrUnsupportedRecurrence) {
				identities = []meetingIdentity{d.Identity}
			} else if err != nil {
				return nil, err
			} else {
				*budget = aliasBudget
			}
			for _, identity := range identities {
				if !d.CreatedAt.Before(splits[identity]) {
					splits[identity] = d.CreatedAt
				}
			}
			continue
		}
		targets, err := s.resolveMeetingIdentity(ctx, r, d.Identity, budget)
		if err != nil {
			return nil, err
		}
		// Keep an absent target's identity so its decision still overrides automatic matches.
		if len(targets) == 0 {
			targets = []linkOccurrence{{Identity: d.Identity}}
		}
		for _, o := range targets {
			resolved = append(resolved, resolvedDecision{d, o})
		}
	}
	var out []linkOccurrence
	hasManualLink := false
	for _, d := range resolved {
		if d.Decision == "link" && d.CreatedAt.After(splits[d.target.Identity]) {
			hasManualLink = true
			if d.target.Ref.MessageID != 0 {
				out = append(out, d.target)
			}
		}
	}
	if hasManualLink {
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

func (s *Store) meetingReader(ctx context.Context) reboundReader {
	return reboundReader{reader: ReadDBContext(ctx, s.db.DB), rebind: s.dialect.Rebind, dialect: s.dialect}
}

// Choose the lowest canonical reference for each identity, in reference order.
func uniqueMeetingOccurrences(out []linkOccurrence) []linkOccurrence {
	slices.SortFunc(out, func(a, b linkOccurrence) int {
		if c := cmp.Compare(a.Ref.MessageID, b.Ref.MessageID); c != 0 {
			return c
		}
		return cmp.Compare(a.Ref.Key, b.Ref.Key)
	})
	seen := make(map[meetingIdentity]bool, len(out))
	return slices.DeleteFunc(out, func(o linkOccurrence) bool {
		if seen[o.Identity] {
			return true
		}
		seen[o.Identity] = true
		return false
	})
}

func (s *Store) meetingIdentityRows(ctx context.Context, r reboundReader, identity meetingIdentity, budget *int) ([]CalendarEventRow, error) {
	predicate, args := s.calendarUIDPredicate(), []any{identity.ICalUID}
	if identity.ICalUID == "" {
		predicate, args = "e.message_id = ?", []any{identity.EventMessageID}
	}
	if identity.ICalUID != "" {
		if identity.OccurrenceKey == "" {
			predicate += ` AND (e.kind = 'single' OR (e.kind = 'exception' AND e.original_start_at IS NULL AND e.original_start_date = ''))`
		} else {
			_, id, ok := calrecur.ParseInstanceID("event_" + identity.OccurrenceKey)
			if !ok {
				return nil, nil
			}
			column, value := "e.original_start_at", any(id.Instant)
			if id.AllDay {
				column, value = "e.original_start_date", id.Date
			}
			predicate += ` AND (e.kind = 'series' OR (e.kind = 'exception' AND ` + column + ` = ?))`
			args = append(args, value)
		}
	}

	return meetingEventRows(ctx, r, predicate, args, budget)
}

// Splits retain their saved identity even when a series can no longer expand.
// Complete exceptions can acquire a master's identity without expansion.
func (s *Store) meetingSplitIdentities(ctx context.Context, r reboundReader, identity meetingIdentity, budget *int) ([]meetingIdentity, error) {
	rows, err := s.meetingIdentityRows(ctx, r, identity, budget)
	if err != nil {
		return nil, err
	}
	identities := []meetingIdentity{identity}
	if identity.ICalUID != "" && identity.OccurrenceKey == "" {
		var singles []CalendarEventRow
		for _, row := range rows {
			if row.Event.Kind == "single" {
				singles = append(singles, row)
			}
		}
		if len(singles) > 0 {
			series, err := meetingEventRows(ctx, r, s.calendarUIDPredicate()+` AND e.kind = 'series'`, []any{identity.ICalUID}, budget)
			if err != nil {
				return nil, err
			}
			for _, single := range singles {
				singleOccurrence, err := resolveMeetingOccurrence(ctx, r, OccurrenceRef{MessageID: single.MessageID}, budget)
				if errors.Is(err, sql.ErrNoRows) || errors.Is(err, calrecur.ErrUnsupportedRecurrence) {
					continue
				}
				if err != nil {
					return nil, err
				}
				var startIdentity calrecur.Identity
				if single.Event.AllDay {
					if single.Event.StartDate == "" {
						continue
					}
					startIdentity = calrecur.Identity{Date: single.Event.StartDate, AllDay: true}
				} else if single.Event.Start != nil {
					startIdentity = calrecur.Identity{Instant: *single.Event.Start}
				}
				key := startIdentity.Key()
				if key != "" {
					for _, master := range series {
						occurrence, err := resolveMeetingOccurrence(ctx, r, OccurrenceRef{MessageID: master.MessageID, Key: key}, budget)
						if errors.Is(err, sql.ErrNoRows) || errors.Is(err, calrecur.ErrUnsupportedRecurrence) {
							continue
						}
						if err != nil {
							return nil, err
						}
						if occurrence.Start.Equal(singleOccurrence.Start) {
							identities = append(identities, occurrence.Identity)
						}
					}
				}
				exceptionPredicate := s.calendarUIDPredicate() + ` AND e.kind = 'exception'`
				exceptionArgs := []any{identity.ICalUID}
				if single.Event.AllDay && single.Event.StartDate != "" {
					exceptionPredicate += ` AND e.start_date = ?`
					exceptionArgs = append(exceptionArgs, single.Event.StartDate)
				} else if single.Event.Start != nil {
					exceptionPredicate += ` AND e.start_at = ?`
					exceptionArgs = append(exceptionArgs, *single.Event.Start)
				} else {
					continue
				}
				exceptions, err := meetingEventRows(ctx, r, exceptionPredicate, exceptionArgs, budget)
				if err != nil {
					return nil, err
				}
				for _, exception := range exceptions {
					occurrence, err := resolveMeetingOccurrence(ctx, r, OccurrenceRef{MessageID: exception.MessageID}, budget)
					if errors.Is(err, sql.ErrNoRows) || errors.Is(err, calrecur.ErrUnsupportedRecurrence) {
						continue
					}
					if err != nil {
						return nil, err
					}
					if occurrence.Start.Equal(singleOccurrence.Start) {
						identities = append(identities, occurrence.Identity)
					}
				}
			}
		}
	}
	if identity.ICalUID != "" && identity.OccurrenceKey != "" {
		_, instance, ok := calrecur.ParseInstanceID("event_" + identity.OccurrenceKey)
		if ok {
			exceptionRows := make([]CalendarEventRow, 0, len(rows))
			predicate := s.calendarUIDPredicate() + ` AND e.kind = 'single' AND (`
			args := []any{identity.ICalUID}
			appendStartPredicate := func(column string, value any) {
				if len(args) > 1 {
					predicate += ` OR `
				}
				predicate += column + ` = ?`
				args = append(args, value)
			}
			if instance.AllDay {
				appendStartPredicate("e.start_date", instance.Date)
			} else {
				appendStartPredicate("e.start_at", instance.Instant)
			}
			for _, row := range rows {
				if row.Event.Kind != "exception" || row.Event.Original == nil || row.Event.Original.Key() != identity.OccurrenceKey {
					continue
				}
				exceptionRows = append(exceptionRows, row)
				if row.Event.AllDay && row.Event.StartDate != "" {
					appendStartPredicate("e.start_date", row.Event.StartDate)
				} else if row.Event.Start != nil {
					appendStartPredicate("e.start_at", *row.Event.Start)
				}
			}
			predicate += `)`
			singles, err := meetingEventRows(ctx, r, predicate, args, budget)
			if err != nil {
				return nil, err
			}
			for _, copy := range singles {
				matches := copy.Event.AllDay && instance.AllDay && copy.Event.StartDate == instance.Date
				if !matches && copy.Event.Start != nil {
					matches = !instance.AllDay && copy.Event.Start.Equal(instance.Instant)
					if !matches {
						for _, exception := range exceptionRows {
							if exception.Event.AllDay && copy.Event.AllDay && exception.Event.StartDate != "" {
								if exception.Event.StartDate == copy.Event.StartDate {
									matches = true
									break
								}
								continue
							}
							if exception.Event.Start != nil && exception.Event.Start.Equal(*copy.Event.Start) {
								matches = true
								break
							}
						}
					}
				}
				if matches {
					identities = append(identities, occurrenceFromRow(copy, "").Identity)
				}
			}
		}
	}
	for _, row := range rows {
		// A split saved without a UID follows the event when it gains one.
		if identity.ICalUID == "" && row.ICalUID != "" {
			identities = append(identities, meetingIdentity{ICalUID: row.ICalUID, OccurrenceKey: identity.OccurrenceKey})
		}
		if row.Event.Kind != "exception" || row.Event.Start == nil || row.Event.End == nil {
			continue
		}
		if row.Event.Original != nil && row.Event.Original.Key() != identity.OccurrenceKey {
			continue
		}
		target, err := resolveMeetingOccurrence(ctx, r, OccurrenceRef{MessageID: row.MessageID}, budget)
		if err != nil {
			return nil, err
		}
		identities = append(identities, target.Identity)
	}
	unique := make([]meetingIdentity, 0, len(identities))
	seen := make(map[meetingIdentity]struct{}, len(identities))
	for _, candidate := range identities {
		if _, ok := seen[candidate]; ok {
			continue
		}
		seen[candidate] = struct{}{}
		unique = append(unique, candidate)
	}
	return unique, nil
}

func (s *Store) resolveMeetingIdentity(ctx context.Context, r reboundReader, identity meetingIdentity, budget *int) ([]linkOccurrence, error) {
	rows, err := s.meetingIdentityRows(ctx, r, identity, budget)
	if err != nil {
		return nil, err
	}
	var out []linkOccurrence
	for _, row := range rows {
		ref := OccurrenceRef{MessageID: row.MessageID, Key: identity.OccurrenceKey}
		if row.Event.Kind == "exception" {
			if row.Event.Original != nil && row.Event.Original.Key() != identity.OccurrenceKey {
				continue
			}
			ref.Key = ""
		}
		o, err := resolveMeetingOccurrence(ctx, r, ref, budget)
		if errors.Is(err, sql.ErrNoRows) || errors.Is(err, calrecur.ErrUnsupportedRecurrence) {
			continue
		}
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, nil
}
