package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"go.kenn.io/msgvault/internal/calrecur"
)

func readMeetingLinkDecisions(ctx context.Context, r SQLReader, memberID int64) (out []meetingLinkDecision, err error) {
	rows, err := r.QueryContext(ctx, `SELECT target_ical_uid,COALESCE(target_message_id,0),occurrence_key,decision,created_at FROM meeting_link_decisions WHERE member_message_id = ? ORDER BY target_ical_uid,target_message_id,occurrence_key`, memberID)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, rows.Close()) }()
	for rows.Next() {
		var d meetingLinkDecision
		if err := rows.Scan(&d.Identity.ICalUID, &d.Identity.EventMessageID, &d.Identity.OccurrenceKey, &d.Decision, &d.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// LinkMeetingContext replaces automatic evidence with the owner's chosen links.
func (s *Store) LinkMeetingContext(ctx context.Context, memberID int64, occurrence OccurrenceRef) error {
	return s.writeMeetingLinkDecision(ctx, memberID, occurrence, "link")
}

// UnlinkMeetingContext splits an occurrence and copies with the same identity.
func (s *Store) UnlinkMeetingContext(ctx context.Context, memberID int64, occurrence OccurrenceRef) error {
	return s.writeMeetingLinkDecision(ctx, memberID, occurrence, "split")
}

func (s *Store) writeMeetingLinkDecision(ctx context.Context, memberID int64, occurrence OccurrenceRef, decision string) error {
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		first, second := memberID, occurrence.MessageID
		if first > second {
			first, second = second, first
		}
		for _, id := range []int64{first, second} {
			if s.IsPostgreSQL() {
				var found int64
				if err := tx.QueryRowContext(ctx, `SELECT id FROM messages WHERE id = ? FOR NO KEY UPDATE`, id).Scan(&found); err != nil {
					return err
				}
			} else if err := s.lockMeetingEvidenceWith(ctx, tx, id); err != nil {
				return err
			}
		}
		var found int64
		if err := tx.QueryRowContext(ctx, `SELECT id FROM messages WHERE id = ? AND message_type = 'meeting_transcript' AND deleted_at IS NULL`, memberID).Scan(&found); err != nil {
			return err
		}
		target, savedIdentity, err := s.resolveMeetingDecisionTarget(ctx, reboundReader{reader: tx.Tx, rebind: s.dialect.Rebind, dialect: s.dialect}, memberID, occurrence, decision)
		if err != nil {
			return fmt.Errorf("resolve meeting decision target: %w", err)
		}
		createdAt := time.Now().UTC()
		if decision == "split" {
			reader := reboundReader{reader: tx.Tx, rebind: s.dialect.Rebind, dialect: s.dialect}
			budget := calrecur.RequestReadBudget
			identities, err := s.meetingSplitIdentities(ctx, reader, target.Identity, &budget)
			if err != nil {
				return fmt.Errorf("resolve meeting split identities: %w", err)
			}
			if err := s.supersedeMessageMeetingLinks(ctx, tx, memberID, target.Identity, savedIdentity, createdAt); err != nil {
				return err
			}
			seen := make(map[meetingIdentity]struct{}, len(identities))
			for _, identity := range identities {
				if _, ok := seen[identity]; ok {
					continue
				}
				seen[identity] = struct{}{}
				_, err := tx.ExecContext(ctx, meetingDecisionUpsert, memberID, identity.ICalUID, sql.NullInt64{Int64: identity.EventMessageID, Valid: identity.ICalUID == ""}, identity.OccurrenceKey, decision, createdAt)
				if err != nil {
					return err
				}
			}
		} else {
			_, err = tx.ExecContext(ctx, meetingDecisionUpsert, memberID, target.Identity.ICalUID, sql.NullInt64{Int64: target.Identity.EventMessageID, Valid: target.Identity.ICalUID == ""}, target.Identity.OccurrenceKey, decision, createdAt)
			if err != nil {
				return err
			}
		}
		return s.journalMeetingLinks(ctx, tx, memberID)
	})
}

// A UID split must supersede earlier message-based links for the same
// occurrence. Otherwise a later recurrence edit can make a hidden link
// visible again after the split has moved to the UID identity.
func (s *Store) supersedeMessageMeetingLinks(ctx context.Context, tx *loggedTx, memberID int64, identity meetingIdentity, savedIdentity *meetingIdentity, createdAt time.Time) error {
	reader := reboundReader{reader: tx.Tx, rebind: s.dialect.Rebind, dialect: s.dialect}
	budget := calrecur.RequestReadBudget
	rows, err := s.meetingIdentityRows(ctx, reader, identity, &budget)
	if err != nil {
		return fmt.Errorf("resolve message-based meeting links for split: %w", err)
	}
	messageIDs := make([]int64, 0, len(rows)+1)
	seen := make(map[int64]struct{}, len(rows)+1)
	if savedIdentity != nil && savedIdentity.ICalUID == "" && savedIdentity.EventMessageID != 0 {
		messageIDs = append(messageIDs, savedIdentity.EventMessageID)
		seen[savedIdentity.EventMessageID] = struct{}{}
	}
	for _, row := range rows {
		if _, ok := seen[row.MessageID]; ok {
			continue
		}
		seen[row.MessageID] = struct{}{}
		messageIDs = append(messageIDs, row.MessageID)
	}
	decisions, err := readMeetingLinkDecisions(ctx, reader, memberID)
	if err != nil {
		return fmt.Errorf("read saved meeting links for split: %w", err)
	}
	for _, decision := range decisions {
		if decision.Decision != "link" ||
			decision.Identity.ICalUID != "" ||
			decision.Identity.EventMessageID == 0 ||
			decision.Identity.OccurrenceKey != identity.OccurrenceKey {
			continue
		}
		identities, err := s.meetingSplitIdentities(ctx, reader, decision.Identity, &budget)
		if err != nil {
			return fmt.Errorf("resolve saved message-based meeting link for split: %w", err)
		}
		for _, resolved := range identities {
			if resolved != identity {
				continue
			}
			if _, ok := seen[decision.Identity.EventMessageID]; !ok {
				seen[decision.Identity.EventMessageID] = struct{}{}
				messageIDs = append(messageIDs, decision.Identity.EventMessageID)
			}
			break
		}
	}
	if len(messageIDs) == 0 {
		return nil
	}
	args := make([]any, 0, len(messageIDs)+3)
	args = append(args, createdAt, memberID)
	for _, messageID := range messageIDs {
		args = append(args, messageID)
	}
	args = append(args, identity.OccurrenceKey)
	query := `UPDATE meeting_link_decisions SET decision = 'split', created_at = ? WHERE member_message_id = ? AND target_ical_uid = '' AND target_message_id IN (` + placeholders(len(messageIDs)) + `) AND occurrence_key = ? AND decision = 'link'`
	if _, err := tx.ExecContext(ctx, query, args...); err != nil {
		return fmt.Errorf("supersede message-based meeting links: %w", err)
	}
	return nil
}

func (s *Store) resolveMeetingDecisionTarget(ctx context.Context, r reboundReader, memberID int64, ref OccurrenceRef, decision string) (linkOccurrence, *meetingIdentity, error) {
	budget := calrecur.RequestReadBudget
	target, err := resolveMeetingOccurrence(ctx, r, ref, &budget)
	if decision != "split" || !errors.Is(err, sql.ErrNoRows) && !errors.Is(err, calrecur.ErrUnsupportedRecurrence) && !errors.Is(err, ErrQueryTooBroad) {
		return target, nil, err
	}
	// A series edit can remove an occurrence while its saved link still
	// suppresses automatic matches. Return the matched identity so the write
	// can supersede it even if the current event no longer qualifies by UID.
	rows, err := meetingEventRows(ctx, r, `e.message_id = ?`, []any{ref.MessageID}, &budget)
	if err != nil {
		return linkOccurrence{}, nil, err
	}
	if len(rows) != 1 {
		return linkOccurrence{}, nil, sql.ErrNoRows
	}
	target = occurrenceFromRow(rows[0], ref.Key)
	identities, err := s.meetingSplitIdentities(ctx, r, meetingIdentity{EventMessageID: ref.MessageID, OccurrenceKey: ref.Key}, &budget)
	if err != nil {
		return linkOccurrence{}, nil, err
	}
	for _, identity := range identities {
		var found int
		err := r.QueryRowContext(ctx, `SELECT 1 FROM meeting_link_decisions WHERE member_message_id = ? AND target_ical_uid = ? AND COALESCE(target_message_id,0) = ? AND occurrence_key = ?`, memberID, identity.ICalUID, identity.EventMessageID, identity.OccurrenceKey).Scan(&found)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return linkOccurrence{}, nil, err
		}
		if err == nil {
			// Keep the current event identity for the new split. A saved
			// message-based link may authorize unlinking, but its source can
			// later be removed and cascade that identity away.
			savedIdentity := identity
			return target, &savedIdentity, nil
		}
	}
	return linkOccurrence{}, nil, sql.ErrNoRows
}

// When a calendar event gains an iCalendar UID, retain any decisions that
// were saved against its temporary message identity under the now-stable UID
// identity. Keep the original rows so their message lifetime remains intact.
func (s *Store) upgradeUIDlessMeetingDecisions(ctx context.Context, tx *loggedTx, messageID int64) error {
	var uid sql.NullString
	query := `SELECT ` + calendarUIDExpression(s.dialect, "m.metadata") + ` FROM messages m WHERE m.id = ? AND m.message_type = 'calendar_event'`
	if err := tx.QueryRowContext(ctx, query, messageID).Scan(&uid); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return fmt.Errorf("read calendar UID for meeting decision upgrade: %w", err)
	}
	if !uid.Valid || uid.String == "" {
		return nil
	}

	rows, err := tx.QueryContext(ctx, `SELECT member_message_id,occurrence_key,decision,created_at FROM meeting_link_decisions WHERE target_message_id = ? AND target_ical_uid = ''`, messageID)
	if err != nil {
		return fmt.Errorf("read UID-less meeting decisions: %w", err)
	}
	type decision struct {
		member    int64
		key       string
		kind      string
		createdAt time.Time
	}
	var decisions []decision
	for rows.Next() {
		var d decision
		if err := rows.Scan(&d.member, &d.key, &d.kind, &d.createdAt); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan UID-less meeting decision: %w", err)
		}
		decisions = append(decisions, d)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return fmt.Errorf("read UID-less meeting decisions: %w", err)
	}

	upsert := meetingDecisionUpsert + ` WHERE meeting_link_decisions.created_at < excluded.created_at OR (meeting_link_decisions.created_at = excluded.created_at AND excluded.decision = 'split')`
	for _, d := range decisions {
		if _, err := tx.ExecContext(ctx, upsert, d.member, uid.String, sql.NullInt64{}, d.key, d.kind, d.createdAt); err != nil {
			return fmt.Errorf("upgrade meeting decision to calendar UID: %w", err)
		}
		if err := s.journalMeetingLinks(ctx, tx, d.member); err != nil {
			return err
		}
	}
	return nil
}

// Retain the original decisions for undo; purge removes only that hidden copy.
func (s *Store) copyMeetingLinkDecisionsTx(ctx context.Context, tx *loggedTx, from, to int64) error {
	if from == to {
		return nil
	}
	rows, err := tx.QueryContext(ctx, `SELECT member_message_id,target_ical_uid,target_message_id,occurrence_key,decision,created_at FROM meeting_link_decisions WHERE member_message_id = ? OR target_message_id = ?`, from, from)
	if err != nil {
		return err
	}
	type copyDecision struct {
		member             int64
		target             sql.NullInt64
		uid, key, decision string
		created            time.Time
	}
	var decisions []copyDecision
	for rows.Next() {
		var d copyDecision
		if err := rows.Scan(&d.member, &d.uid, &d.target, &d.key, &d.decision, &d.created); err != nil {
			_ = rows.Close()
			return err
		}
		decisions = append(decisions, d)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	for _, d := range decisions {
		if d.member == from {
			d.member = to
		}
		if d.target.Valid && d.target.Int64 == from {
			d.target.Int64 = to
		}
		_, err := tx.ExecContext(ctx, meetingDecisionUpsert+` WHERE meeting_link_decisions.created_at < excluded.created_at OR (meeting_link_decisions.created_at = excluded.created_at AND excluded.decision = 'split')`, d.member, d.uid, d.target, d.key, d.decision, d.created)
		if err != nil {
			return fmt.Errorf("copy meeting decision: %w", err)
		}
		if err := s.journalMeetingLinks(ctx, tx, d.member); err != nil {
			return err
		}
	}
	return nil
}

const meetingDecisionUpsert = `INSERT INTO meeting_link_decisions(member_message_id,target_ical_uid,target_message_id,occurrence_key,decision,created_at) VALUES (?,?,?,?,?,?) ON CONFLICT(member_message_id,target_ical_uid,(COALESCE(target_message_id,0)),occurrence_key) DO UPDATE SET decision = excluded.decision,created_at = excluded.created_at`

func (s *Store) journalMeetingLinks(ctx context.Context, tx *loggedTx, memberID int64) error {
	if s.IsPostgreSQL() {
		return nil
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO cache_related_change_journal(dataset,message_id) VALUES('meeting_links', ?)`, memberID)
	return err
}

// journalCalendarMeetingLinks invalidates cached count keys after calendar
// projection changes. Calendar evidence can alter automatic links for any
// transcript key and can make a previously stored decision stop resolving.
// Message ID zero marks the complete meeting-key dataset dirty.
func (s *Store) journalCalendarMeetingLinks(ctx context.Context, tx *loggedTx) error {
	return s.journalMeetingLinks(ctx, tx, 0)
}
