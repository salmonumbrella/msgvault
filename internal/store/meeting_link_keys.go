package store

import (
	"context"
	"fmt"
	"time"

	"go.kenn.io/msgvault/internal/calrecur"
	"go.kenn.io/msgvault/internal/meetingcontent"
)

type meetingLinkKey struct {
	Kind, Value, CalendarID, Base, InstanceKey string
	ScheduledAt                                *time.Time
}

func deriveMeetingLinkKeys(content meetingcontent.Content) []meetingLinkKey {
	evidence := content.LinkEvidence
	key := meetingLinkKey{ScheduledAt: evidence.ScheduledAt}
	switch {
	case evidence.GoogleEventID != "" && evidence.CalendarID != "":
		key.Kind, key.Value, key.CalendarID = "google_event_id", evidence.GoogleEventID, evidence.CalendarID
		if base, id, ok := calrecur.ParseInstanceID(key.Value); ok {
			key.Base, key.InstanceKey = base, id.Key()
		}
	case evidence.ICalUID != "":
		key.Kind, key.Value = "ical_uid", evidence.ICalUID
	default:
		return nil
	}
	return []meetingLinkKey{key}
}

func replaceMeetingLinkKeys(ctx context.Context, tx *loggedTx, messageID int64, keys []meetingLinkKey) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM meeting_link_keys WHERE message_id = ?`, messageID); err != nil {
		return fmt.Errorf("clear meeting keys: %w", err)
	}
	for ordinal, key := range keys {
		if _, err := tx.ExecContext(ctx, `INSERT INTO meeting_link_keys(message_id,ordinal,kind,value,calendar_id,base,instance_key,scheduled_at) VALUES (?,?,?,?,?,?,?,?)`, messageID, ordinal, key.Kind, key.Value, key.CalendarID, key.Base, key.InstanceKey, key.ScheduledAt); err != nil {
			return fmt.Errorf("write meeting key: %w", err)
		}
	}
	return nil
}
