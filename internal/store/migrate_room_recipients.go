package store

import (
	"context"
	"errors"

	"go.kenn.io/msgvault/internal/calevent"
)

const migrationRoomRecipientsV1 = "room_recipients_v1"

func (s *Store) repairRoomRecipientsContext(ctx context.Context) error {
	return s.withTxContext(ctx, func(tx *loggedTx) error {
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
			if calevent.IsRoomAttendee(email, false) {
				rooms = append(rooms, id)
			}
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			return err
		}
		for _, id := range rooms {
			if err := removeRoomParticipantEdges(boundQuerier{ctx: ctx, q: tx}, id); err != nil {
				return err
			}
		}

		// An event's resource flag is local evidence. Repair legacy edges only
		// on that event; its conversation can contain other events, and the same
		// address can be a person in another meeting.
		rows, err = tx.QueryContext(ctx, `SELECT a.message_id, p.id FROM calendar_event_attendees a
		 JOIN participants p ON lower(trim(p.email_address)) = lower(trim(a.email))
		 WHERE a.role = 'room' AND NOT (lower(trim(a.email)) LIKE '%@resource.calendar.google.com'
		   OR lower(trim(a.email)) LIKE '%@%.resource.calendar.google.com')`)
		if err != nil {
			return err
		}
		type eventRoom struct{ messageID, participantID int64 }
		var eventRooms []eventRoom
		for rows.Next() {
			var room eventRoom
			if err := rows.Scan(&room.messageID, &room.participantID); err != nil {
				_ = rows.Close()
				return err
			}
			eventRooms = append(eventRooms, room)
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			return err
		}
		for _, room := range eventRooms {
			if err := removeRoomEventMessageEdges(boundQuerier{ctx: ctx, q: tx}, room.messageID, room.participantID); err != nil {
				return err
			}
		}
		return nil
	})
}
