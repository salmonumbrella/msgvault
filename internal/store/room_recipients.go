package store

import (
	"context"
	"strings"

	"go.kenn.io/msgvault/internal/calevent"
)

// IsRoomAddressContext preserves contacts with ordinary email or chat history.
// Provider resource domains always identify rooms.
func (s *Store) IsRoomAddressContext(ctx context.Context, email string, resource bool) (bool, error) {
	return isRoomAddressWith(boundQuerier{ctx: ctx, q: s.db}, email, resource)
}

func isRoomAddressWith(q querier, email string, resource bool) (bool, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	if calevent.IsRoomAttendee(email, false) {
		return true, nil
	}
	if !resource {
		return false, nil
	}
	var history bool
	err := q.QueryRow(`SELECT EXISTS (
 SELECT 1 FROM participants p WHERE lower(trim(p.email_address)) = ? AND (
 EXISTS (SELECT 1 FROM messages m WHERE m.sender_id = p.id AND coalesce(m.message_type, '') NOT IN ('calendar_event','meeting_transcript')) OR
 EXISTS (SELECT 1 FROM message_recipients r JOIN messages m ON m.id = r.message_id WHERE r.participant_id = p.id AND coalesce(m.message_type, '') NOT IN ('calendar_event','meeting_transcript')) OR
 EXISTS (SELECT 1 FROM conversation_participants cp JOIN messages m ON m.conversation_id = cp.conversation_id WHERE cp.participant_id = p.id AND coalesce(m.message_type, '') NOT IN ('calendar_event','meeting_transcript'))
 ))`, email).Scan(&history)
	return !history, err
}

func removeRoomParticipantEdges(q querier, id int64) error {
	for _, query := range []string{
		`DELETE FROM message_recipients WHERE participant_id = ? AND message_id IN
   (SELECT id FROM messages WHERE message_type IN ('calendar_event','meeting_transcript'))`,
		`DELETE FROM conversation_participants WHERE participant_id = ? AND conversation_id IN
   (SELECT conversation_id FROM messages WHERE message_type IN ('calendar_event','meeting_transcript'))`,
		`UPDATE messages SET sender_id = NULL WHERE message_type IN ('calendar_event','meeting_transcript') AND sender_id = ?`,
	} {
		if _, err := q.Exec(query, id); err != nil {
			return err
		}
	}
	return nil
}

func removeRoomEventMessageEdges(q querier, messageID, participantID int64) error {
	var email string
	if err := q.QueryRow(`SELECT coalesce(email_address, '') FROM participants WHERE id = ?`, participantID).Scan(&email); err != nil {
		return err
	}
	room, err := isRoomAddressWith(q, email, true)
	if err != nil || !room {
		return err
	}
	if _, err := q.Exec(`DELETE FROM message_recipients WHERE message_id = ? AND participant_id = ?`, messageID, participantID); err != nil {
		return err
	}
	if _, err := q.Exec(`UPDATE messages SET sender_id = NULL WHERE id = ? AND sender_id = ?`, messageID, participantID); err != nil {
		return err
	}
	_, err = q.Exec(`DELETE FROM conversation_participants
         WHERE conversation_id = (SELECT conversation_id FROM messages WHERE id = ?)
           AND participant_id = ?
           AND NOT EXISTS (
             SELECT 1 FROM messages m
             WHERE m.conversation_id = (SELECT conversation_id FROM messages WHERE id = ?)
               AND (m.sender_id = ? OR EXISTS (
                 SELECT 1 FROM message_recipients r WHERE r.message_id = m.id AND r.participant_id = ?
               ))
           )`, messageID, participantID, messageID, participantID, participantID)
	return err
}
