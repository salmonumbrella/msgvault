package store

import (
	"context"
	"errors"
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
		if err := q.QueryRow(`SELECT EXISTS (SELECT 1 FROM calendar_event_attendees WHERE role = 'room' AND lower(trim(email)) = ?)`, email).Scan(&resource); err != nil {
			return false, err
		}
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

// RoomAddressesContext reads room evidence once for an import.
func (s *Store) RoomAddressesContext(ctx context.Context) (map[string]bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT email FROM calendar_event_attendees WHERE role = 'room'`)
	if err != nil {
		return nil, err
	}
	rooms := make(map[string]bool)
	for rows.Next() {
		var email string
		if err := rows.Scan(&email); err != nil {
			return nil, errors.Join(err, rows.Close())
		}
		rooms[strings.ToLower(strings.TrimSpace(email))] = true
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	for email := range rooms {
		room, err := s.IsRoomAddressContext(ctx, email, true)
		if err != nil {
			return nil, err
		}
		rooms[email] = room
	}
	return rooms, nil
}

func removeRoomParticipantEdges(q querier, id int64) error {
	var email string
	if err := q.QueryRow(`SELECT coalesce(email_address, '') FROM participants WHERE id = ?`, id).Scan(&email); err != nil {
		return err
	}
	room, err := isRoomAddressWith(q, email, true)
	if err != nil || !room {
		return err
	}
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

func removeRoomAddressEdges(ctx context.Context, tx *loggedTx, email string) error {
	email = strings.ToLower(strings.TrimSpace(email))
	if email == "" {
		return nil
	}
	rows, err := tx.QueryContext(ctx, `SELECT id FROM participants WHERE lower(trim(email_address)) = ?`, email)
	if err != nil {
		return err
	}
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
		if err := removeRoomParticipantEdges(boundQuerier{ctx: ctx, q: tx}, id); err != nil {
			return err
		}
	}
	return nil
}
