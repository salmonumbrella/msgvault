package store

import "context"

const migrationRoomRecipientsV1 = "room_recipients_v1"

func (s *Store) repairRoomRecipientsContext(ctx context.Context) error {
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		room := `SELECT id FROM participants WHERE
   lower(trim(email_address)) LIKE '%@resource.calendar.google.com'
   OR lower(trim(email_address)) LIKE '%.resource.calendar.google.com'
   OR lower(trim(email_address)) IN (SELECT lower(trim(email)) FROM calendar_event_attendees WHERE role = 'room')`
		if _, err := tx.ExecContext(ctx, `DELETE FROM message_recipients
   WHERE participant_id IN (`+room+`) AND message_id IN
   (SELECT id FROM messages WHERE message_type IN ('calendar_event','meeting_transcript'))`); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE messages SET sender_id = NULL
   WHERE message_type IN ('calendar_event','meeting_transcript') AND sender_id IN (`+room+`)`); err != nil {
			return err
		}
		return s.markMigrationAppliedContext(ctx, tx, migrationRoomRecipientsV1, 1)
	})
}
