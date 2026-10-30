package store

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strings"
	"time"
)

// LineWebhookEvent is sanitized provider evidence and its durable processing state.
// TargetMessageID indexes lifecycle events whose original message may arrive later.
type LineWebhookEvent struct {
	ID              int64
	SourceID        int64
	WebhookEventID  string
	EventType       string
	TargetMessageID string
	RawJSON         string
	State           string
	LastError       string
	EventTimestamp  int64
	Attempts        int
	ReceivedAt      time.Time
	NextAttemptAt   time.Time
}

// LineInboxStatus counts work without reading message content.
type LineInboxStatus struct {
	LastReceivedAt *time.Time `json:"last_received_at,omitempty"`
	Pending        int64      `json:"pending"`
	Failed         int64      `json:"failed"`
}

// EnqueueLineEventsContext acknowledges a batch only after all its events commit.
// The mounted receiver's channel and bot identities are checked under the same
// source lock as receipt so a stale receiver cannot target a reused source ID.
// Compact event IDs remain after payload retention so old delivery cannot replay.
func (s *Store) EnqueueLineEventsContext(ctx context.Context, sourceID int64, channelID, botUserID string, events []LineWebhookEvent) error {
	if err := s.requireSyncSource(sourceID); err != nil {
		return err
	}
	if channelID == "" || botUserID == "" {
		return errors.New("LINE inbox requires a channel and bot identity")
	}
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		// Receipt and retirement serialize on the source row. Recheck lifecycle
		// after taking the lock so a mounted receiver cannot acknowledge work for
		// a source that a concurrent merge has just retired.
		if err := s.lockSyncSourceTx(ctx, tx, sourceID); err != nil {
			return err
		}
		if err := requireSourceWritableWith(boundQuerier{ctx: ctx, q: tx}, sourceID); err != nil {
			return err
		}
		var kind, identifier string
		var raw sql.NullString
		if err := tx.QueryRowContext(ctx, `SELECT source_type,identifier,sync_config FROM sources WHERE id=?`, sourceID).Scan(&kind, &identifier, &raw); err != nil {
			return fmt.Errorf("load LINE inbox source: %w", err)
		}
		if kind != "line" || identifier != channelID {
			return errors.New("LINE inbox source binding changed")
		}
		registeredBotID, err := lineBotUserIDFromConfig(raw)
		if err != nil {
			return err
		}
		if registeredBotID == "" || registeredBotID != botUserID {
			return errors.New("LINE inbox bot binding changed")
		}
		for _, e := range events {
			if e.WebhookEventID == "" || len(e.WebhookEventID) > 200 || strings.HasPrefix(e.WebhookEventID, "_") || e.EventType == "" || e.EventTimestamp <= 0 || !jsontext.Value(e.RawJSON).IsValid() {
				return errors.New("invalid LINE inbox event")
			}
			received := e.ReceivedAt
			if received.IsZero() {
				received = time.Now().UTC()
			}
			_, err := tx.ExecContext(ctx, `INSERT INTO line_webhook_events
    (source_id,webhook_event_id,event_type,target_message_id,event_timestamp,raw_json,state,received_at)
    VALUES (?,?,?,?,?,?,'pending',?) ON CONFLICT(source_id,webhook_event_id) DO NOTHING`, sourceID, e.WebhookEventID, e.EventType, e.TargetMessageID, e.EventTimestamp, e.RawJSON, received)
			if err != nil {
				return fmt.Errorf("enqueue LINE event: %w", err)
			}
		}
		return nil
	})
}

const lineEventColumns = `id,source_id,webhook_event_id,event_type,target_message_id,event_timestamp,raw_json,state,attempts,last_error,received_at,next_attempt_at`

func scanLineEvents(rows *loggedRows) ([]LineWebhookEvent, error) {
	defer func() { _ = rows.Close() }()
	var out []LineWebhookEvent
	for rows.Next() {
		var e LineWebhookEvent
		var next sql.NullTime
		if err := rows.Scan(&e.ID, &e.SourceID, &e.WebhookEventID, &e.EventType, &e.TargetMessageID, &e.EventTimestamp, &e.RawJSON, &e.State, &e.Attempts, &e.LastError, &e.ReceivedAt, &next); err != nil {
			return nil, fmt.Errorf("scan LINE inbox: %w", err)
		}
		if next.Valid {
			e.NextAttemptAt = next.Time
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *Store) DueLineEventsContext(ctx context.Context, sourceID int64, now time.Time, limit int) ([]LineWebhookEvent, error) {
	if limit <= 0 || limit > 1000 {
		limit = 1000
	}
	rows, err := s.db.QueryContext(ctx, s.Rebind(`SELECT `+lineEventColumns+` FROM line_webhook_events
  WHERE source_id=? AND (state='pending' OR (state='retry' AND next_attempt_at<=?))
  ORDER BY event_timestamp,webhook_event_id LIMIT ?`), sourceID, now, limit)
	if err != nil {
		return nil, fmt.Errorf("load due LINE events: %w", err)
	}
	return scanLineEvents(rows)
}

// LineEventStateContext returns the durable processing state of one inbox event.
func (s *Store) LineEventStateContext(ctx context.Context, id int64) (string, error) {
	var state string
	err := s.db.QueryRowContext(ctx, s.Rebind(`SELECT state FROM line_webhook_events WHERE id=?`), id).Scan(&state)
	return state, err
}

// LineMutationsContext also returns expired orphan mutations: a late original
// must still acquire their latest edit and unsend state after polling stops.
func (s *Store) LineMutationsContext(ctx context.Context, sourceID int64, messageID string) ([]LineWebhookEvent, error) {
	rows, err := s.db.QueryContext(ctx, s.Rebind(`SELECT `+lineEventColumns+` FROM line_webhook_events
 WHERE source_id=? AND target_message_id=? AND event_type IN ('messageEdited','unsend')
 AND raw_json<>'{}' ORDER BY event_timestamp,webhook_event_id`), sourceID, messageID)
	if err != nil {
		return nil, fmt.Errorf("load LINE lifecycle evidence: %w", err)
	}
	return scanLineEvents(rows)
}

func (s *Store) FinishLineEventContext(ctx context.Context, id int64, state, lastError string, next time.Time) error {
	switch state {
	case "processed", "ignored", "retry", "failed":
	default:
		return errors.New("invalid LINE event state")
	}
	if len(lastError) > 500 {
		lastError = lastError[:500]
	}
	var nextAt any
	if !next.IsZero() {
		nextAt = next.UTC()
	}
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		var sourceID int64
		if err := tx.QueryRowContext(ctx, `SELECT source_id FROM line_webhook_events WHERE id=?`, id).Scan(&sourceID); err != nil {
			return err
		}
		if err := s.requireSyncSource(sourceID); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `UPDATE line_webhook_events SET state=?,last_error=?,next_attempt_at=?,attempts=attempts+1 WHERE id=?`, state, lastError, nextAt, id)
		return err
	})
}

func (s *Store) PruneLineEventsContext(ctx context.Context, sourceID int64, before time.Time) error {
	if err := s.requireSyncSource(sourceID); err != nil {
		return err
	}
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		_, err := tx.ExecContext(ctx, `UPDATE line_webhook_events SET raw_json='{}',last_error=''
   WHERE source_id=? AND state IN ('processed','ignored') AND event_type NOT IN ('_profile','_membership')
   AND received_at<? AND raw_json<>'{}'`, sourceID, before)
		return err
	})
}

func (s *Store) LineInboxStatusContext(ctx context.Context, sourceID int64) (LineInboxStatus, error) {
	var out LineInboxStatus
	err := s.db.QueryRowContext(ctx, s.Rebind(`SELECT COALESCE(SUM(CASE WHEN state IN ('pending','retry') THEN 1 ELSE 0 END),0),
 COALESCE(SUM(CASE WHEN state='failed' THEN 1 ELSE 0 END),0) FROM line_webhook_events WHERE source_id=? AND event_type NOT IN ('_profile','_membership')`), sourceID).Scan(&out.Pending, &out.Failed)
	if err != nil {
		return out, fmt.Errorf("count LINE inbox: %w", err)
	}
	var received time.Time
	err = s.db.QueryRowContext(ctx, s.Rebind(`SELECT received_at FROM line_webhook_events WHERE source_id=? AND event_type NOT IN ('_profile','_membership') ORDER BY received_at DESC LIMIT 1`), sourceID).Scan(&received)
	if errors.Is(err, sql.ErrNoRows) {
		return out, nil
	}
	if err != nil {
		return out, fmt.Errorf("last LINE receipt: %w", err)
	}
	out.LastReceivedAt = &received
	return out, nil
}

func (s *Store) LineBotUserIDContext(ctx context.Context, sourceID int64) (string, error) {
	var raw sql.NullString
	if err := s.db.QueryRowContext(ctx, s.Rebind(`SELECT sync_config FROM sources WHERE id=? AND source_type='line'`), sourceID).Scan(&raw); err != nil {
		return "", fmt.Errorf("load LINE bot identity: %w", err)
	}
	return lineBotUserIDFromConfig(raw)
}

func lineBotUserIDFromConfig(raw sql.NullString) (string, error) {
	if !raw.Valid {
		return "", nil
	}
	var config struct {
		BotUserID string `json:"line_bot_user_id"`
	}
	if err := json.Unmarshal([]byte(raw.String), &config); err != nil {
		return "", fmt.Errorf("decode LINE bot identity: %w", err)
	}
	return config.BotUserID, nil
}

// ValidateLineSourceBindingContext verifies that a processor still refers to
// the channel captured when its receiver and scheduler job were mounted.
func (s *Store) ValidateLineSourceBindingContext(ctx context.Context, sourceID int64, channelID string) (string, error) {
	var kind, identifier string
	var raw sql.NullString
	if err := s.db.QueryRowContext(ctx, s.Rebind(`SELECT source_type,identifier,sync_config FROM sources WHERE id=?`), sourceID).Scan(&kind, &identifier, &raw); err != nil {
		return "", fmt.Errorf("load LINE source binding: %w", err)
	}
	if kind != "line" || channelID == "" || identifier != channelID {
		return "", errors.New("LINE processor source binding changed")
	}
	botID, err := lineBotUserIDFromConfig(raw)
	if err != nil {
		return "", fmt.Errorf("load LINE bot identity: %w", err)
	}
	return botID, nil
}

func (s *Store) SetLineBotContext(ctx context.Context, sourceID int64, botID string) error {
	if botID == "" {
		return errors.New("LINE bot identity required")
	}
	return s.withAttributionTxContext(ctx, attributionLock{Sources: []int64{sourceID}}, func(tx *loggedTx) error {
		var raw sql.NullString
		if err := tx.QueryRowContext(ctx, `SELECT sync_config FROM sources WHERE id=? AND source_type='line'`, sourceID).Scan(&raw); err != nil {
			return err
		}
		config := map[string]jsontext.Value{}
		if raw.Valid && raw.String != "" {
			if err := json.Unmarshal([]byte(raw.String), &config); err != nil {
				return err
			}
		}
		if config == nil {
			config = map[string]jsontext.Value{}
		}
		encoded, err := json.Marshal(botID)
		if err != nil {
			return err
		}
		config["line_bot_user_id"] = encoded
		encoded, err = json.Marshal(config)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE sources SET sync_config=`+s.dialect.JSONBindExpr()+` WHERE id=?`, string(encoded), sourceID)
		return err
	})
}

// SetLineMembershipContext preserves both transition clocks in a compact inbox
// record, while projecting the existing left_at=NULL convention for active users.
func (s *Store) SetLineMembershipContext(ctx context.Context, conversationID, participantID int64, joined bool, at time.Time) error {
	if at.IsZero() {
		return errors.New("LINE membership timestamp required")
	}
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		if err := s.requireSyncConversationSourceTx(tx, conversationID); err != nil {
			return err
		}
		var sourceID int64
		if err := tx.QueryRowContext(ctx, `SELECT source_id FROM conversations WHERE id=?`, conversationID).Scan(&sourceID); err != nil {
			return err
		}
		key := fmt.Sprintf("_membership:%d:%d", conversationID, participantID)
		var clocks struct {
			Joined int64 `json:"joined"`
			Left   int64 `json:"left"`
		}
		var raw string
		err := tx.QueryRowContext(ctx, `SELECT raw_json FROM line_webhook_events WHERE source_id=? AND webhook_event_id=?`, sourceID, key).Scan(&raw)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err == nil {
			if err = json.Unmarshal([]byte(raw), &clocks); err != nil {
				return err
			}
		}
		timestamp := at.UnixMilli()
		if joined {
			clocks.Joined = max(clocks.Joined, timestamp)
		} else {
			clocks.Left = max(clocks.Left, timestamp)
		}
		encoded, err := json.Marshal(clocks)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO line_webhook_events(source_id,webhook_event_id,event_type,event_timestamp,raw_json,state)
  VALUES (?,?,'_membership',?,?,'processed') ON CONFLICT(source_id,webhook_event_id) DO UPDATE SET event_timestamp=excluded.event_timestamp,raw_json=excluded.raw_json`, sourceID, key, max(clocks.Joined, clocks.Left), string(encoded))
		if err != nil {
			return err
		}
		var join, leave any
		if clocks.Joined > 0 {
			join = time.UnixMilli(clocks.Joined).UTC()
		}
		// Equal-time leave wins, making ties deterministic across delivery order.
		if clocks.Left >= clocks.Joined && clocks.Left > 0 {
			leave = time.UnixMilli(clocks.Left).UTC()
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO conversation_participants(conversation_id,participant_id,role,joined_at,left_at)
  VALUES (?,?,'member',?,?) ON CONFLICT(conversation_id,participant_id) DO UPDATE SET joined_at=excluded.joined_at,left_at=excluded.left_at`, conversationID, participantID, join, leave)
		return err
	})
}

// ResolveLineQuotesForConversationContext settles unresolved quotes in one
// LINE conversation after the inbox pass touches that conversation.
func (s *Store) ResolveLineQuotesForConversationContext(ctx context.Context, sourceID int64, sourceConversationID string) error {
	if sourceConversationID == "" {
		return errors.New("LINE quote resolution requires a conversation ID")
	}
	quoted := `json_extract(messages.metadata,'$.quotedMessageId')`
	if s.IsPostgreSQL() {
		quoted = `messages.metadata->>'quotedMessageId'`
	}
	target := `SELECT parent.id FROM messages parent WHERE parent.source_id=? AND parent.source_message_id=` + quoted + ` AND parent.conversation_id=messages.conversation_id AND parent.id<>messages.id`
	return s.withSyncSourceWriteContext(ctx, sourceID, func(q querier) error {
		_, err := q.Exec(`UPDATE messages SET reply_to_message_id=(`+target+`)
 WHERE source_id=? AND message_type='line' AND reply_to_message_id IS NULL
 AND EXISTS (`+target+`)
		AND messages.conversation_id=(SELECT id FROM conversations WHERE source_id=? AND source_conversation_id=?)`, sourceID, sourceID, sourceID, sourceID, sourceConversationID)
		return err
	})
}

// migrateLineServiceScope widens only the untouched original system seed.
// An operator's policy remains authoritative; registration checks compatibility.
func (s *Store) migrateLineServiceScope(ctx context.Context) error {
	return s.runOnceMigration(ctx, "line_channel_scope_v1", 1, false, func(ctx context.Context) error {
		return s.withTxContext(ctx, func(tx *loggedTx) error {
			_, err := tx.ExecContext(ctx, `UPDATE communication_services SET scope_policy='optional',default_scope_kind='line_channel'
   WHERE slug='line' AND is_system=TRUE AND is_active=TRUE AND display_label='LINE'
   AND scope_policy='none' AND default_scope_kind IS NULL AND normalization='lower' AND normalization_version=1
   AND uri_scheme IS NULL AND profile_url_template IS NULL AND updated_at=created_at`)
			return err
		})
	})
}

// LineProfileContext returns durable profile presentation, separate from opaque
// contact identity evidence. Profile rows are excluded from inbox work/status.
func (s *Store) LineProfileContext(ctx context.Context, sourceID int64, userID string) (string, time.Time, error) {
	var raw string
	var at int64
	err := s.db.QueryRowContext(ctx, s.Rebind(`SELECT raw_json,event_timestamp FROM line_webhook_events WHERE source_id=? AND webhook_event_id=? AND event_type='_profile'`), sourceID, "_profile:"+userID).Scan(&raw, &at)
	return raw, time.UnixMilli(at).UTC(), err
}

func (s *Store) CacheLineProfileContext(ctx context.Context, sourceID int64, userID, raw string, at time.Time) error {
	if userID == "" || !jsontext.Value(raw).IsValid() {
		return errors.New("invalid LINE profile cache")
	}
	if err := s.requireSyncSource(sourceID); err != nil {
		return err
	}
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO line_webhook_events(source_id,webhook_event_id,event_type,event_timestamp,raw_json,state,received_at)
   VALUES (?,?,'_profile',?,?,'processed',?) ON CONFLICT(source_id,webhook_event_id) DO UPDATE SET raw_json=excluded.raw_json,event_timestamp=excluded.event_timestamp`, sourceID, "_profile:"+userID, at.UnixMilli(), raw, at.UTC())
		return err
	})
}

func (s *Store) LineMessageDeletedContext(ctx context.Context, messageID int64) (bool, error) {
	var deleted bool
	err := s.db.QueryRowContext(ctx, s.Rebind(`SELECT deleted_from_source_at IS NOT NULL FROM messages WHERE id=?`), messageID).Scan(&deleted)
	return deleted, err
}

// LineMessageContext resolves identity and chat scope together without loading
// bodies. MessageMetadataBatch intentionally leaves SourceConversationID unset.
func (s *Store) LineMessageContext(ctx context.Context, sourceID int64, messageID string) (MessageMetadataRecord, error) {
	var out MessageMetadataRecord
	err := s.db.QueryRowContext(ctx, s.Rebind(`SELECT m.id,m.metadata,c.source_conversation_id,
 COALESCE((SELECT display_name FROM message_recipients mr WHERE mr.message_id=m.id AND mr.recipient_type='from' ORDER BY mr.id LIMIT 1),'')
 FROM messages m JOIN conversations c ON c.id=m.conversation_id WHERE m.source_id=? AND m.source_message_id=?`), sourceID, messageID).Scan(&out.ID, &out.Metadata, &out.SourceConversationID, &out.SenderDisplayName)
	return out, err
}

// UpdateLineMessageSenderDisplayNameContext fills an uncached LINE sender
// snapshot and its FTS document without replacing edits or message contents.
func (s *Store) UpdateLineMessageSenderDisplayNameContext(ctx context.Context, sourceID, messageID int64, name string) error {
	if sourceID <= 0 || messageID <= 0 {
		return errors.New("invalid LINE message identity")
	}
	if name == "" {
		return nil
	}
	return s.withMessageAttributionTxContext(ctx, messageID, func(tx *loggedTx) error {
		q := boundQuerier{ctx: ctx, q: tx}
		if err := s.lockMessageForRecipientWrite(q, messageID); err != nil {
			return err
		}
		if err := s.requireSyncMessageSourceTx(q, messageID); err != nil {
			return err
		}
		var actualSourceID int64
		var messageType string
		if err := q.QueryRow(`SELECT source_id,message_type FROM messages WHERE id=?`, messageID).Scan(&actualSourceID, &messageType); err != nil {
			return fmt.Errorf("read LINE message %d: %w", messageID, err)
		}
		if actualSourceID != sourceID || messageType != "line" {
			return errors.New("LINE message does not belong to the requested source")
		}
		result, err := tx.ExecContext(ctx, `UPDATE message_recipients SET display_name=?
 WHERE message_id=? AND recipient_type='from' AND COALESCE(display_name,'')=''`, name, messageID)
		if err != nil {
			return fmt.Errorf("update LINE sender snapshot: %w", err)
		}
		changed, err := result.RowsAffected()
		if err != nil || changed == 0 || !s.fts5Available {
			return err
		}
		var subject sql.NullString
		if err := q.QueryRow(`SELECT subject FROM messages WHERE id=?`, messageID).Scan(&subject); err != nil {
			return fmt.Errorf("read LINE message %d subject: %w", messageID, err)
		}
		var body sql.NullString
		err = q.QueryRow(`SELECT body_text FROM message_bodies WHERE message_id=?`, messageID).Scan(&body)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("read LINE message %d body: %w", messageID, err)
		}
		_, err = s.upsertFTSOrUnsearchable(q, FTSDoc{MessageID: messageID, Subject: subject.String, Body: body.String, FromAddr: name})
		if err != nil {
			return fmt.Errorf("update LINE sender search: %w", err)
		}
		return nil
	})
}

// UpdateLineParticipantNameContext refreshes observed presentation only while
// the participant still uses this source's previously observed name. A chosen
// label (or another source's presentation) remains untouched.
func (s *Store) UpdateLineParticipantNameContext(ctx context.Context, participantID int64, previousName, name string) error {
	if name == "" {
		return nil
	}
	return s.withAttributionTxContext(ctx, attributionLock{Exclusive: true}, func(tx *loggedTx) error {
		result, err := tx.ExecContext(ctx, `UPDATE participants SET display_name=?,updated_at=`+s.dialect.Now()+`
   WHERE id=? AND (COALESCE(display_name,'')='' OR display_name=?) AND COALESCE(display_name,'')<>?`, name, participantID, previousName, name)
		if err != nil {
			return err
		}
		changed, err := s.bumpParticipantDisplayNameRevisionIfChangedContext(ctx, tx, result)
		if err != nil || !changed {
			return err
		}
		return s.invalidateParticipantPersonEnrichmentTx(ctx, tx, participantID)
	})
}
