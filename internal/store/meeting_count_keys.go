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
	var keys map[int64]string
	err := s.withReadSnapshotContext(ctx, func(tx *loggedTx) error {
		var err error
		keys, err = s.MeetingCountKeysFromSnapshotContext(ctx, tx.Tx)
		return err
	})
	return keys, err
}

// MeetingCountKeysFromSnapshotContext uses the caller's pinned archive snapshot.
// Calendar rows receive keys only when a transcript links to that occurrence.
func (s *Store) MeetingCountKeysFromSnapshotContext(ctx context.Context, reader SQLReader) (map[int64]string, error) {
	keys := make(map[int64]string)
	eventKeys := make(map[int64]string)
	conflictingEvents := make(map[int64]bool)
	err := func() error {
		r := reboundReader{reader: reader, rebind: s.dialect.Rebind}
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
					eventID := occurrence.Ref.MessageID
					if prior, exists := eventKeys[eventID]; exists && prior != keys[id] {
						conflictingEvents[eventID] = true
					}
					eventKeys[eventID] = keys[id]
				}
			}
		}
		return nil
	}()
	for id, key := range eventKeys {
		if !conflictingEvents[id] {
			keys[id] = key
		}
	}
	return keys, err
}
