package store

import (
	"context"
	"errors"
)

const migrationRoomRecipientsV1 = "room_recipients_v1"

func (s *Store) repairRoomRecipientsContext(ctx context.Context) error {
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		isRoom, err := roomClassifierContext(ctx, tx.Tx)
		if err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, `SELECT id, coalesce(email_address, '') FROM participants`)
		if err != nil {
			return err
		}
		var rooms []int64
		for rows.Next() {
			var id int64
			var email string
			if err := rows.Scan(&id, &email); err != nil {
				_ = rows.Close()
				return err
			}
			if isRoom(email, false) {
				rooms = append(rooms, id)
			}
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			return err
		}
		for _, id := range rooms {
			if _, err := tx.ExecContext(ctx, `DELETE FROM message_recipients
   WHERE participant_id = ? AND message_id IN
   (SELECT id FROM messages WHERE message_type IN ('calendar_event','meeting_transcript'))`, id); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE messages SET sender_id = NULL
   WHERE message_type IN ('calendar_event','meeting_transcript') AND sender_id = ?`, id); err != nil {
				return err
			}
		}
		return s.markMigrationAppliedContext(ctx, tx, migrationRoomRecipientsV1, 1)
	})
}
