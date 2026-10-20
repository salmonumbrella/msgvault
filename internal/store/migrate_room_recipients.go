package store

import (
	"context"
	"fmt"
)

const migrationRoomRecipientsV1 = "room_recipients_v1"

func (s *Store) repairRoomRecipientsContext(ctx context.Context) error {
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		// Provider room addresses apply to calendar events and transcripts. A
		// resource flag applies only to the calendar event that supplied it.
		const roomEdges = `WITH room_edges AS (
   SELECT m.id AS message_id, p.id AS participant_id FROM participants p
   JOIN messages m ON m.message_type IN ('calendar_event','meeting_transcript')
   WHERE lower(trim(p.email_address)) LIKE '%@resource.calendar.google.com'
      OR lower(trim(p.email_address)) LIKE '%@%.resource.calendar.google.com'
   UNION
   SELECT a.message_id, p.id FROM calendar_event_attendees a
   JOIN participants p ON lower(trim(p.email_address)) = lower(trim(a.email))
   WHERE a.role = 'room'
  ) `
		for _, query := range []string{
			roomEdges + `DELETE FROM message_recipients WHERE EXISTS (
    SELECT 1 FROM room_edges r WHERE r.message_id = message_recipients.message_id
     AND r.participant_id = message_recipients.participant_id)`,
			roomEdges + `UPDATE messages SET sender_id = NULL WHERE EXISTS (
    SELECT 1 FROM room_edges r WHERE r.message_id = messages.id AND r.participant_id = messages.sender_id)`,
			// A recurring conversation can also hold events where the flagged address
			// is a person. Keep the roster entry while one of those events uses it.
			roomEdges + `DELETE FROM conversation_participants WHERE EXISTS (
    SELECT 1 FROM room_edges r JOIN messages m ON m.id = r.message_id
    WHERE m.conversation_id = conversation_participants.conversation_id
     AND r.participant_id = conversation_participants.participant_id
   ) AND NOT EXISTS (
    SELECT 1 FROM messages m WHERE m.conversation_id = conversation_participants.conversation_id
     AND (m.sender_id = conversation_participants.participant_id OR EXISTS (
      SELECT 1 FROM message_recipients r WHERE r.message_id = m.id
       AND r.participant_id = conversation_participants.participant_id)))`,
		} {
			if _, err := tx.ExecContext(ctx, query); err != nil {
				return fmt.Errorf("remove room edges: %w", err)
			}
		}
		return nil
	})
}
