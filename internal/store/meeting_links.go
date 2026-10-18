package store

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
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

func (id meetingIdentity) encode() string {
	data, _ := json.Marshal([3]any{id.ICalUID, id.EventMessageID, id.OccurrenceKey})
	return string(data)
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
	rows, err := queryCalendarEventRows(ctx, r, predicate, args, 201)
	if err != nil {
		return nil, err
	}
	if len(rows) > 200 {
		return nil, ErrQueryTooBroad
	}
	if err := spendMeetingBudget(ctx, budget, len(rows)); err != nil {
		return nil, err
	}
	return rows, nil
}

func occurrenceFromRow(row CalendarEventRow, key string) linkOccurrence {
	id := meetingIdentity{ICalUID: row.Event.ICalUID, OccurrenceKey: key}
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
	if reason != "" {
		if *budget <= 0 || reason == calrecur.ErrBudgetExceeded.Error() {
			return nil, calendarRecurrenceBudgetError()
		}
		return nil, fmt.Errorf("%w: %s", calrecur.ErrUnsupportedRecurrence, reason)
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
	return out, nil
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
			row, ref = masters[0], OccurrenceRef{MessageID: masters[0].MessageID, Key: row.Event.Original.Key()}
		} else {
			o := occurrenceFromRow(row, row.Event.Original.Key())
			o.Ref.Key = ""
			return o, nil
		}
	}
	if ref.Key == "" {
		if row.Event.Kind == meetingSeriesKind {
			return linkOccurrence{}, sql.ErrNoRows
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
		if x.Event.Start != nil {
			at = *x.Event.Start
		}
	}
	occurrences, err := expandMeetingSeries(ctx, r, row, at.Add(-calendarCandidateMargin), at.Add(calendarCandidateMargin), budget)
	if err != nil {
		return linkOccurrence{}, err
	}
	for _, o := range occurrences {
		if o.Ref.Key == ref.Key {
			return o, nil
		}
	}
	return linkOccurrence{}, sql.ErrNoRows
}

func memberLinkKeys(ctx context.Context, r SQLReader, id int64) (keys []meetingLinkKey, err error) {
	rows, err := r.QueryContext(ctx, `SELECT kind,value,calendar_id,scheduled_at FROM meeting_link_keys WHERE message_id = ?`, id)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, rows.Close()) }()
	for rows.Next() {
		var key meetingLinkKey
		var at sql.NullTime
		if err := rows.Scan(&key.Kind, &key.Value, &key.CalendarID, &at); err != nil {
			return nil, err
		}
		if at.Valid {
			key.ScheduledAt = &at.Time
		}
		keys = append(keys, key)
	}
	return keys, rows.Err()
}

func automaticMeetingOccurrences(ctx context.Context, r SQLReader, key meetingLinkKey, budget *int) ([]linkOccurrence, error) {
	predicate, args := `e.ical_uid = ?`, []any{key.Value}
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
			if decodeCalendarSourceConfig(config).CalendarID == key.CalendarID {
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
			if errors.Is(err, calrecur.ErrUnsupportedRecurrence) {
				continue
			}
			if err != nil {
				return nil, err
			}
		} else {
			o, err := resolveMeetingOccurrence(ctx, r, OccurrenceRef{MessageID: row.MessageID}, budget)
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

func meetingMemberOccurrences(ctx context.Context, r SQLReader, memberID int64, budget *int) ([]linkOccurrence, error) {
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
	keys, err := memberLinkKeys(ctx, r, memberID)
	if err != nil {
		return nil, err
	}
	for _, key := range keys {
		matches, err := automaticMeetingOccurrences(ctx, r, key, budget)
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

// ExportLogicalMeetingKeysContext resolves owner-only transcript identities.
// The caller keeps a pinned reader from this archive alive for the whole call.
// Each of at most 100 members gets its own recurrence budget.
// A failed member keeps its msg:<id> key. Breadth limits are expected fallbacks;
// other member errors accompany the completed map.
func (s *Store) ExportLogicalMeetingKeysContext(ctx context.Context, reader SQLReader, ids []int64) (map[int64]string, error) {
	if len(ids) > 100 {
		return nil, errors.New("export meeting keys: batch exceeds 100 messages")
	}
	out := make(map[int64]string, len(ids))
	if len(ids) == 0 {
		return out, ctx.Err()
	}
	r := reboundReader{reader: reader, rebind: s.dialect.Rebind}
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	rows, err := r.QueryContext(ctx, `SELECT id FROM messages WHERE message_type = 'meeting_transcript' AND deleted_at IS NULL AND id IN (`+placeholders(len(ids))+`) ORDER BY id`, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var members []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		members = append(members, id)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	var memberErrors error
	for _, id := range members {
		budget := calrecur.RequestReadBudget
		occurrences, err := meetingMemberOccurrences(ctx, r, id, &budget)
		key := fmt.Sprintf("msg:%d", id)
		if err != nil {
			out[id] = key
			if ctx.Err() != nil {
				return out, errors.Join(memberErrors, ctx.Err())
			}
			if !errors.Is(err, ErrQueryTooBroad) {
				memberErrors = errors.Join(memberErrors, fmt.Errorf("resolve meeting key for message %d: %w", id, err))
			}
			continue
		}
		if len(occurrences) > 0 {
			identity := occurrences[0].Identity
			same := true
			for _, o := range occurrences[1:] {
				if o.Identity != identity {
					same = false
				}
			}
			if same {
				key = identity.encode()
			}
		}
		out[id] = key
	}
	return out, errors.Join(memberErrors, ctx.Err())
}

func (s *Store) meetingReader(ctx context.Context) SQLReader {
	return reboundReader{reader: ReadDBContext(ctx, s.db.DB), rebind: s.dialect.Rebind}
}
