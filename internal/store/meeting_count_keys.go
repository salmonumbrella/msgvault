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
// meeting targets. Series masters have no count key. journalSeq is the SQLite
// journal high-water mark in the same read snapshot, or zero on PostgreSQL.
func (s *Store) MeetingCountKeysContext(ctx context.Context) (keys map[int64]string, journalSeq int64, err error) {
	ctx, release, err := s.BeginReadSnapshotContext(ctx)
	if err != nil {
		return nil, 0, err
	}
	defer release()
	r := s.meetingReader(ctx)
	if !s.IsPostgreSQL() {
		err = r.QueryRowContext(ctx, `SELECT coalesce((SELECT seq FROM sqlite_sequence WHERE name='cache_related_change_journal'), 0)`).Scan(&journalSeq)
		if err != nil {
			return nil, 0, fmt.Errorf("read meeting journal sequence: %w", err)
		}
	}
	rows, err := r.QueryContext(ctx, `SELECT m.id FROM messages m
 WHERE m.message_type='meeting_transcript' AND m.deleted_at IS NULL
 AND (EXISTS (SELECT 1 FROM meeting_link_key k WHERE k.message_id=m.id AND k.value<>'')
 OR EXISTS (SELECT 1 FROM meeting_link_decisions d WHERE d.member_message_id=m.id))`)
	if err != nil {
		return nil, 0, fmt.Errorf("read meeting count members: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var members []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, 0, err
		}
		members = append(members, id)
	}
	err = rows.Err()
	closeErr := rows.Close()
	if err != nil {
		return nil, 0, err
	}
	if closeErr != nil {
		return nil, 0, closeErr
	}
	keys = make(map[int64]string)
	seen := make(map[meetingIdentity]bool)
	for _, id := range members {
		budget := calrecur.RequestReadBudget
		occurrences, err := s.meetingMemberOccurrences(ctx, r, id, &budget)
		if unresolvedMeetingCount(err) {
			continue
		}
		if err != nil {
			return nil, 0, fmt.Errorf("resolve meeting count member %d: %w", id, err)
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
			return nil, 0, fmt.Errorf("resolve meeting count copies: %w", err)
		}
		for _, copyID := range copies {
			keys[copyID] = identity.countKey()
		}
	}
	return keys, journalSeq, nil
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
	var copies []int64
	for _, row := range rows {
		if row.Event.Kind == meetingSeriesKind {
			continue
		}
		occurrence, err := resolveMeetingOccurrence(ctx, r, OccurrenceRef{MessageID: row.MessageID}, &budget)
		if err != nil {
			return nil, err
		}
		if occurrence.Identity == identity {
			copies = append(copies, row.MessageID)
		}
	}
	return copies, nil
}
