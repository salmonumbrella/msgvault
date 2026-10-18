package store

import (
	"context"
	"errors"
	"strings"

	"go.kenn.io/msgvault/internal/calevent"
)

// RoomClassifierContext combines provider resource flags with archived room evidence.
func (s *Store) RoomClassifierContext(ctx context.Context) (func(string, bool) bool, error) {
	return roomClassifierContext(ctx, s.meetingReader(ctx))
}

func roomClassifierContext(ctx context.Context, r SQLReader) (func(string, bool) bool, error) {
	rows, err := r.QueryContext(ctx, `SELECT email FROM calendar_event_attendees WHERE role = 'room'`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	rooms := make(map[string]bool)
	for rows.Next() {
		var email string
		if err := rows.Scan(&email); err != nil {
			return nil, err
		}
		rooms[strings.ToLower(strings.TrimSpace(email))] = true
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	return func(email string, resource bool) bool {
		return calevent.IsRoomAttendee(email, resource) || rooms[strings.ToLower(strings.TrimSpace(email))]
	}, nil
}
