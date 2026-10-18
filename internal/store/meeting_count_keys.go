package store

import (
	"context"
	"errors"
	"fmt"

	"go.kenn.io/msgvault/internal/calrecur"
)

// MeetingCountKeysContext resolves transcripts with one calendar occurrence identity.
// Transcripts without an unambiguous link retain their individual message counts.
func (s *Store) MeetingCountKeysContext(ctx context.Context) (map[int64]string, error) {
	keys := make(map[int64]string)
	err := s.withReadSnapshotContext(ctx, func(tx *loggedTx) error {
		r := reboundReader{reader: tx.Tx, rebind: s.dialect.Rebind}
		rows, err := r.QueryContext(ctx, `SELECT id FROM messages WHERE message_type = 'meeting_transcript' AND deleted_at IS NULL`)
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
			}
		}
		return nil
	})
	return keys, err
}
