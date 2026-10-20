package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"go.kenn.io/msgvault/internal/calrecur"
)

func (i meetingIdentity) countKey() string {
	if i.ICalUID != "" {
		return "uid:" + i.ICalUID + "/" + i.OccurrenceKey
	}
	return fmt.Sprintf("evt:%d/%s", i.EventMessageID, i.OccurrenceKey)
}

// MeetingCountKeysContext maps linked transcripts and calendar copies to exact
// meeting targets in one read snapshot. Series masters have no count key.
func (s *Store) MeetingCountKeysContext(ctx context.Context) (map[int64]string, error) {
	ctx, release, err := s.BeginReadSnapshotContext(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	r := s.meetingReader(ctx)

	rows, err := r.QueryContext(ctx, `SELECT m.id FROM messages m
 WHERE m.message_type='meeting_transcript' AND m.deleted_at IS NULL
 AND (EXISTS (SELECT 1 FROM meeting_link_key k WHERE k.message_id=m.id AND k.value<>'')
 OR EXISTS (SELECT 1 FROM meeting_link_decisions d WHERE d.member_message_id=m.id))`)
	if err != nil {
		return nil, fmt.Errorf("read meeting count members: %w", err)
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
	err = rows.Err()
	closeErr := rows.Close()
	if err != nil {
		return nil, err
	}
	if closeErr != nil {
		return nil, closeErr
	}
	keys := make(map[int64]string)
	seen := make(map[meetingIdentity]bool)
	for _, id := range members {
		budget := calrecur.RequestReadBudget
		occurrences, err := s.meetingMemberOccurrences(ctx, r, id, &budget)
		if unresolvedMeetingCount(err) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("resolve meeting count member %d: %w", id, err)
		}
		if len(occurrences) != 1 {
			continue
		}
		identity := occurrences[0].Identity
		keys[id] = identity.countKey()
		if seen[identity] {
			continue
		}
		seen[identity] = true
		copies, err := s.meetingCountCopies(ctx, r, identity)
		if unresolvedMeetingCount(err) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("resolve meeting count copies: %w", err)
		}
		for _, copyID := range copies {
			keys[copyID] = identity.countKey()
		}
	}
	return keys, nil
}

func unresolvedMeetingCount(err error) bool {
	return errors.Is(err, ErrQueryTooBroad) || errors.Is(err, calrecur.ErrUnsupportedRecurrence) || errors.Is(err, sql.ErrNoRows)
}

func (s *Store) meetingCountCopies(ctx context.Context, r reboundReader, identity meetingIdentity) ([]int64, error) {
	budget := calrecur.RequestReadBudget
	rows, err := s.meetingIdentityRows(ctx, r, identity, &budget)
	if err != nil {
		return nil, err
	}
	byID := make(map[int64]bool, len(rows))
	related := make([]CalendarEventRow, 0, len(rows))
	for _, row := range rows {
		byID[row.MessageID] = true
		related = append(related, row)
	}
	if identity.OccurrenceKey != "" {
		_, occurrenceID, ok := calrecur.ParseInstanceID("event_" + identity.OccurrenceKey)
		if ok {
			column, value := "e.original_start_at", any(occurrenceID.Instant)
			if occurrenceID.AllDay {
				column, value = "e.original_start_date", occurrenceID.Date
			}
			seenSeries := make(map[string]bool)
			for _, row := range rows {
				if row.Event.Kind != meetingSeriesKind && row.Event.Kind != "single" {
					continue
				}
				seriesKey := fmt.Sprintf("%d\x00%s", row.SourceID, row.Event.ProviderEventID)
				if seenSeries[seriesKey] {
					continue
				}
				seenSeries[seriesKey] = true
				exceptions, err := meetingEventRows(ctx, r, `e.kind = 'exception' AND m.source_id = ? AND e.series_provider_id = ? AND `+column+` = ?`, []any{row.SourceID, row.Event.ProviderEventID, value}, &budget)
				if err != nil {
					return nil, err
				}
				for _, exception := range exceptions {
					if byID[exception.MessageID] {
						continue
					}
					byID[exception.MessageID] = true
					related = append(related, exception)
				}
			}
		}
	}
	var copies []int64
	for _, row := range related {
		if row.Event.Kind == meetingSeriesKind {
			continue
		}
		occurrence, err := resolveMeetingOccurrence(ctx, r, OccurrenceRef{MessageID: row.MessageID}, &budget)
		if errors.Is(err, sql.ErrNoRows) || errors.Is(err, calrecur.ErrUnsupportedRecurrence) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if occurrence.Identity == identity {
			copies = append(copies, row.MessageID)
		}
	}
	return copies, nil
}
