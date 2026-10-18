package store

import (
	"context"
	"errors"

	"go.kenn.io/msgvault/internal/calevent"
)

const migrationRoomRecipientsV1 = "room_recipients_v1"

func (s *Store) repairRoomRecipientsContext(ctx context.Context) error {
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		rows, err := tx.QueryContext(ctx, `SELECT p.id, coalesce(p.email_address, ''),
         EXISTS (SELECT 1 FROM calendar_event_attendees a WHERE a.role = 'room' AND lower(trim(a.email)) = lower(trim(p.email_address)))
         FROM participants p`)
		if err != nil {
			return err
		}
		var rooms []int64
		for rows.Next() {
			var id int64
			var email string
			var resource bool
			if err := rows.Scan(&id, &email, &resource); err != nil {
				_ = rows.Close()
				return err
			}
			if calevent.IsRoomAttendee(email, resource) {
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
		return nil
	})
}
