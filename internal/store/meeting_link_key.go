package store

import (
	"context"
	"fmt"
	"time"

	"go.kenn.io/msgvault/internal/meetingcontent"
)

type meetingLinkKey struct {
	Kind, Value, CalendarID string
	ScheduledAt             *time.Time
}

func deriveMeetingLinkKey(content meetingcontent.Content) meetingLinkKey {
	evidence := content.LinkEvidence
	key := meetingLinkKey{ScheduledAt: evidence.ScheduledAt}
	switch {
	case evidence.GoogleEventID != "" && evidence.CalendarID != "":
		key.Kind, key.Value, key.CalendarID = "google_event_id", evidence.GoogleEventID, evidence.CalendarID
	case evidence.ICalUID != "":
		key.Kind, key.Value = "ical_uid", evidence.ICalUID
	}
	return key
}

func (s *Store) replaceMeetingLinkKey(ctx context.Context, tx *loggedTx, messageID int64, key meetingLinkKey) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM meeting_link_key WHERE message_id = ?`, messageID); err != nil {
		return fmt.Errorf("clear meeting key: %w", err)
	}
	if key.Value != "" || key.ScheduledAt != nil {
		if _, err := tx.ExecContext(ctx, `INSERT INTO meeting_link_key(message_id,kind,value,calendar_id,scheduled_at) VALUES (?,?,?,?,?)`, messageID, key.Kind, key.Value, key.CalendarID, key.ScheduledAt); err != nil {
			return fmt.Errorf("write meeting key: %w", err)
		}
	}
	if !s.IsPostgreSQL() {
		if _, err := tx.ExecContext(ctx, `INSERT INTO cache_related_change_journal(dataset,message_id) VALUES('meeting_links', ?)`, messageID); err != nil {
			return fmt.Errorf("journal meeting key: %w", err)
		}
	}
	return nil
}
