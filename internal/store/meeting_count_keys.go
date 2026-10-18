package store

import (
	"context"
	"errors"
	"fmt"

	"go.kenn.io/msgvault/internal/calrecur"
)

// MeetingCountKeysContext resolves linked transcripts in one read snapshot.
// journalSeq bounds the related changes represented by these keys.
func (s *Store) MeetingCountKeysContext(ctx context.Context) (keys map[int64]string, journalSeq int64, err error) {
	keys = make(map[int64]string)
	err = s.withReadSnapshotContext(ctx, func(tx *loggedTx) error {
		if !s.IsPostgreSQL() {
			if err := tx.QueryRowContext(ctx, `SELECT COALESCE((SELECT seq FROM sqlite_sequence WHERE name = 'cache_related_change_journal'), 0)`).Scan(&journalSeq); err != nil {
				return fmt.Errorf("read meeting key boundary: %w", err)
			}
		}
		r := reboundReader{reader: tx.Tx, rebind: s.dialect.Rebind}
		rows, err := r.QueryContext(ctx, `SELECT id FROM messages m
   WHERE message_type = 'meeting_transcript' AND deleted_at IS NULL
   AND (EXISTS (SELECT 1 FROM meeting_link_key k WHERE k.message_id = m.id)
        OR EXISTS (SELECT 1 FROM meeting_link_decisions d WHERE d.member_message_id = m.id))`)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		var ids []int64
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				return errors.Join(err, rows.Close())
			}
			ids = append(ids, id)
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			return err
		}
		for _, id := range ids {
			budget := calrecur.SeriesReadBudget
			occurrences, err := s.meetingMemberOccurrences(ctx, r, id, &budget)
			if errors.Is(err, ErrQueryTooBroad) || errors.Is(err, calrecur.ErrUnsupportedRecurrence) {
				continue
			}
			if err != nil {
				return fmt.Errorf("resolve transcript %d: %w", id, err)
			}
			if len(occurrences) == 0 {
				continue
			}
			identity := occurrences[0].Identity
			shared := true
			for _, occurrence := range occurrences[1:] {
				// Calendar copies without a shared UID keep separate archive identities.
				if occurrence.Identity != identity {
					shared = false
					break
				}
			}
			if shared {
				if identity.ICalUID != "" {
					keys[id] = "ical:" + identity.ICalUID + "|" + identity.OccurrenceKey
				} else {
					keys[id] = fmt.Sprintf("event:%d|%s", identity.EventMessageID, identity.OccurrenceKey)
				}
				for _, occurrence := range occurrences {
					if occurrence.Row.Event.Kind != meetingSeriesKind {
						keys[occurrence.Row.MessageID] = keys[id]
					}
				}
			}
		}

		return nil
	})
	return keys, journalSeq, err
}
