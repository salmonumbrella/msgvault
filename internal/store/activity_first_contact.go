package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

const maxIntroCounterparts = 4

type FirstContactKind string

const (
	FirstContactEmailIntro      FirstContactKind = "email_intro"
	FirstContactOwnerOutreach   FirstContactKind = "owner_outreach"
	FirstContactInboundOutreach FirstContactKind = "inbound_outreach"
	FirstContactGroupShared     FirstContactKind = "group_shared"
	FirstContactMeeting         FirstContactKind = "meeting"
	FirstContactObserved        FirstContactKind = "observed"
)

type FirstContactIntroducer struct {
	ParticipantID int64  `json:"participant_id"`
	DisplayName   string `json:"display_name"`
	PersonID      *int64 `json:"person_id,omitzero" nullable:"false"`
}

// ClassifyFirstContact describes a person's earliest linked event using current identities.
func ClassifyFirstContact(candidate ActivityCandidate, personID int64, maxDirectCounterparts int) (FirstContactKind, *FirstContactIntroducer) {
	if IsMeetingMessageType(candidate.MessageType) {
		return FirstContactMeeting, nil
	}
	counterparts := collapseActivityCounterparts(candidate.Counterparts)
	ownerLinked := activityOwnerLinked(counterparts)
	classification := ClassifyActivityCandidate(candidate, maxDirectCounterparts)
	if classification.Direction == DirectionObserved {
		return FirstContactObserved, nil
	}
	switch candidate.ConversationType {
	case "group_chat", "channel":
		return FirstContactGroupShared, nil
	}
	email := candidate.ConversationType == "email_thread"
	direct := candidate.ConversationType == string(MessageExportConversationDirectChat)
	if !email && !direct {
		return FirstContactObserved, nil
	}
	var sender *ActivityCounterpart
	fromCount := 0
	addressed, introAddressed := false, false
	others := map[audienceKey]struct{}{}
	for i := range counterparts {
		c := &counterparts[i]
		isTarget := c.PersonID != nil && *c.PersonID == personID
		if c.RecipientType == recipientTypeFrom {
			fromCount++
			if candidate.SenderID == nil || *candidate.SenderID == c.ParticipantID {
				sender = c
			}
			continue
		}
		recipient := c.RecipientType == "to" || c.RecipientType == "cc" || c.RecipientType == "bcc" || (direct && c.RecipientType == "member")
		if isTarget && recipient {
			addressed = true
			if c.RecipientType == "to" || c.RecipientType == "cc" {
				introAddressed = true
			}
		}
		if recipient && !isTarget && !ownerLinked(*c) {
			others[activityAudienceKey(*c)] = struct{}{}
		}
	}
	if candidate.SenderID == nil && fromCount != 1 {
		sender = nil
	}
	targetSender := sender != nil && sender.PersonID != nil && *sender.PersonID == personID
	introducer := func(c *ActivityCounterpart) *FirstContactIntroducer {
		return &FirstContactIntroducer{ParticipantID: c.ParticipantID, PersonID: c.PersonID}
	}
	if email && classification.Direction == DirectionInbound && sender != nil && !ownerLinked(*sender) && !targetSender && introAddressed && len(others) <= maxIntroCounterparts {
		return FirstContactEmailIntro, introducer(sender)
	}
	if classification.Direction == DirectionOutbound && addressed && !classification.Broadcast {
		if sender != nil && ownerLinked(*sender) {
			return FirstContactOwnerOutreach, introducer(sender)
		}
		return FirstContactOwnerOutreach, nil
	}
	if classification.Direction == DirectionInbound && targetSender && !classification.Broadcast {
		return FirstContactInboundOutreach, nil
	}
	return FirstContactObserved, nil
}

// earliestFirstContactTx reads the earliest linked event in the current snapshot.
func earliestFirstContactTx(ctx context.Context, tx *loggedTx, personID int64) (ActivityEvent, sql.NullString, error) {
	var event ActivityEvent
	var refKind sql.NullString
	err := tx.QueryRowContext(ctx, `SELECT ae.message_id, ae.occurred_at, ae.conversation_id, ae.ref_kind
 FROM activity_event_persons aep JOIN activity_events ae ON ae.message_id = aep.message_id
 WHERE aep.person_id = ? AND NOT (aep.role = 'member' AND COALESCE(ae.ref_kind, '') <> 'meeting' AND EXISTS (SELECT 1 FROM conversations c WHERE c.id = ae.conversation_id AND c.conversation_type IN ('group_chat', 'channel'))) ORDER BY ae.occurred_at, ae.message_id LIMIT 1`, personID).Scan(&event.MessageID, &event.OccurredAt, &event.ConversationID, &refKind)
	return event, refKind, err
}

func (s *Store) populateFirstContactTx(ctx context.Context, tx *loggedTx, state *ContactState) error {
	event, refKind, kind, introducer, found, err := s.firstContactTx(ctx, tx, state.PersonID)
	if err != nil {
		return err
	}
	if !found {
		return nil
	}
	messageID := event.MessageID
	occurredAt := sql.NullTime{Time: event.OccurredAt, Valid: true}
	state.FirstSeenAt = utcNullTimePointer(occurredAt)
	state.FirstSeenRef = activityStoredRef(refKind, sql.NullInt64{Int64: messageID, Valid: true})
	state.FirstContactKind, state.Introducer = kind, introducer
	if state.Introducer != nil {
		err = tx.QueryRowContext(ctx, `SELECT COALESCE(NULLIF(TRIM(person.display_name), ''), NULLIF(TRIM(`+participantSenderNameSQL+`), ''), NULLIF(TRIM(p.email_address), ''), NULLIF(TRIM(p.phone_number), ''), '') FROM participants p
  LEFT JOIN persons person ON person.id = ?
  LEFT JOIN message_recipients mr ON mr.participant_id = p.id AND mr.message_id = ? AND mr.recipient_type = 'from'
  WHERE p.id = ? ORDER BY mr.id LIMIT 1`, state.Introducer.PersonID, messageID, state.Introducer.ParticipantID).Scan(&state.Introducer.DisplayName)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("load first-contact introducer: %w", err)
		}
	}
	return nil
}

// firstContactTx shares the same evidence and classifier as the read path.
func (s *Store) firstContactTx(ctx context.Context, tx *loggedTx, personID int64) (ActivityEvent, sql.NullString, FirstContactKind, *FirstContactIntroducer, bool, error) {
	event, refKind, err := earliestFirstContactTx(ctx, tx, personID)
	if errors.Is(err, sql.ErrNoRows) {
		return event, refKind, "", nil, false, nil
	}
	if err != nil {
		return event, refKind, "", nil, false, fmt.Errorf("read first contact: %w", err)
	}
	candidates, err := s.loadActivityCandidatesByIDQueryerContext(ctx, tx, []int64{event.MessageID})
	if err != nil {
		return event, refKind, "", nil, false, fmt.Errorf("load first-contact candidate: %w", err)
	}
	if len(candidates) == 0 {
		return event, refKind, "", nil, false, nil
	}
	candidate := candidates[0]
	kind, introducer := ClassifyFirstContact(candidate, personID, candidate.DirectLimitTransition.Target)
	return event, refKind, kind, introducer, true, nil
}

// ReconcileFirstContactsContext processes a bounded set from a reconciled activity epoch.
func (s *Store) ReconcileFirstContactsContext(ctx context.Context, limit int, revisions ContactRevisions) (int, error) {
	if limit <= 0 {
		return 0, nil
	}
	owners, err := s.loopOwnerIdentities(ctx)
	if err != nil {
		return 0, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT person_id FROM person_first_contacts WHERE dirty_at IS NOT NULL ORDER BY person_id LIMIT ?`, limit)
	if err != nil {
		return 0, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return 0, err
	}
	count := 0
	for _, id := range ids {
		cleared, disabled := false, false
		err := s.withTxContext(ctx, func(tx *loggedTx) error {
			if err := s.lockPersonFactAttributeTx(ctx, tx, id, AttributeSlugIntroducedBy); err != nil {
				return err
			}
			if err := s.lockActivityProjectionFreshnessTx(ctx, tx, revisions); err != nil {
				return err
			}
			reconciled, err := s.activityEpochReconciledTx(ctx, tx, revisions)
			if err != nil {
				return err
			}
			if !reconciled {
				return &ErrActivityProjectionStale{Reason: "activity epoch not reconciled"}
			}
			var dirty sql.NullTime
			var written sql.NullInt64
			err = tx.QueryRowContext(ctx, `SELECT dirty_at, written_value_id FROM person_first_contacts WHERE person_id = ?`+s.dialect.SelectForUpdate(), id).Scan(&dirty, &written)
			if errors.Is(err, sql.ErrNoRows) {
				return nil
			}
			if err != nil {
				return err
			}
			if !dirty.Valid {
				return nil
			}
			def, err := s.getAttributeDefinitionBySlugTx(ctx, tx, AttributeObjectPerson, AttributeSlugIntroducedBy)
			if err != nil {
				return err
			}
			if !def.IsActive {
				disabled = true
				return nil
			}
			event, refKind, kind, introducer, found, err := s.firstContactTx(ctx, tx, id)
			if err != nil {
				return err
			}
			var target *int64
			if found && kind == FirstContactEmailIntro && introducer != nil {
				target = introducer.PersonID
			}
			if found && kind == FirstContactOwnerOutreach && len(owners.persons) == 1 {
				for owner := range owners.persons {
					ownerID := owner
					target = &ownerID
				}
			}
			if target != nil && *target == id {
				target = nil
			}
			ref := firstContactRefPrefix + activityStoredRef(refKind, sql.NullInt64{Int64: event.MessageID, Valid: found})
			written, err = s.reconcileFirstContactValueTx(ctx, tx, *def, id, target, ref, written)
			if err != nil {
				return err
			}
			_, err = tx.ExecContext(ctx, `UPDATE person_first_contacts SET dirty_at = NULL, computed_at = CURRENT_TIMESTAMP, written_value_id = ? WHERE person_id = ?`, written, id)
			if err == nil {
				cleared = true
			}
			return err
		})
		if err != nil {
			return count, err
		}
		if disabled {
			return count, nil
		}
		if cleared {
			count++
		}
	}
	return count, nil
}

func (s *Store) reconcileFirstContactValueTx(ctx context.Context, tx *loggedTx, def AttributeDefinition, personID int64, target *int64, ref string, written sql.NullInt64) (sql.NullInt64, error) {
	values, err := s.listPersonAttributeValuesContext(ctx, tx, personID, PersonAttributeQuery{DefinitionSlug: AttributeSlugIntroducedBy, IncludeHistory: true})
	if err != nil {
		return written, err
	}
	current, latest := firstContactValueState(values)
	if current != nil && !isFirstContactValue(current) {
		return sql.NullInt64{}, nil
	}
	if current == nil && latest != nil && (!isFirstContactValue(latest) || (written.Valid && written.Int64 == latest.ID)) {
		return written, nil
	}
	if current != nil && target != nil && current.Value.RecordID != nil && *current.Value.RecordID == *target && current.SourceRef != nil && *current.SourceRef == ref {
		return sql.NullInt64{Int64: current.ID, Valid: true}, nil
	}
	now := time.Now().UTC()
	if current != nil && now.Before(current.ActiveFrom) {
		now = current.ActiveFrom
	}
	if target == nil {
		if current != nil {
			if err := s.retireFirstContactValueTx(ctx, tx, current); err != nil {
				return written, err
			}
		}
		return sql.NullInt64{}, nil
	}
	recordType := "person"
	input := PersonAttributeValueInput{PersonID: personID, DefinitionSlug: AttributeSlugIntroducedBy, Value: AttributeValue{Type: AttributeValueRecordReference, RecordType: &recordType, RecordID: target}, Source: ProvenanceSystem, SourceRef: &ref}
	if current != nil {
		input.ExpectedValueID = &current.ID
	}
	result, err := s.setPersonAttributeValueTx(ctx, tx, def, input, now, now)
	if err != nil {
		return written, err
	}
	return sql.NullInt64{Int64: result.Value.ID, Valid: true}, nil
}

const firstContactRefPrefix = "first-contact:"

func isFirstContactValue(v *PersonAttributeValue) bool {
	return v != nil && v.Source == ProvenanceSystem && v.SourceRef != nil && strings.HasPrefix(*v.SourceRef, firstContactRefPrefix)
}

func firstContactValueState(values []PersonAttributeValue) (current, latest *PersonAttributeValue) {
	for i := range values {
		v := &values[i]
		if latest == nil || v.ID > latest.ID {
			latest = v
		}
		if v.ActiveUntil == nil && v.SupersededAt == nil {
			current = v
		}
	}
	return current, latest
}

func (s *Store) retireFirstContactValueTx(ctx context.Context, tx *loggedTx, v *PersonAttributeValue) error {
	now := time.Now().UTC()
	if now.Before(v.ActiveFrom) {
		now = v.ActiveFrom
	}
	_, err := s.closePersonAttributeValueTx(ctx, tx, v.ID, now, now)
	return err
}
