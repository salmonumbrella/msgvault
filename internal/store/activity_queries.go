package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"
)

// ActivityCounterpart is one participant on a candidate event, resolved to
// the current curated person and to owner status for that event's source.
type ActivityCounterpart struct {
	ParticipantID int64
	PersonID      *int64
	RecipientType string
	IsOwner       bool
	OwnerAddress  string
}

// ActivityQueueObservation is the exact durable queue generation observed
// beside a candidate. Historical scans preserve drained rows too; an absent
// row is distinct from revision zero so Task 4 can reserve it without ABA.
type ActivityQueueObservation struct {
	Exists            bool
	Revision          int64
	ProcessedRevision int64
}

// ActivityCandidate is an immutable projection input. Queue, LastModified, and
// both identity revisions together let Task 4 reject every mutable-input race
// without re-querying classification inputs outside its authoritative write.
type ActivityCandidate struct {
	MessageID               int64
	SourceID                int64
	ConversationID          *int64
	ConversationType        string
	MessageType             string
	SentAt                  *time.Time
	ReceivedAt              *time.Time
	InternalDate            *time.Time
	LastModified            time.Time
	DateOnly                bool
	Eligible                bool
	Queue                   ActivityQueueObservation
	TimezoneTransition      ActivityTimezoneTransition
	IdentityRevision        int64
	AccountIdentityRevision int64
	Counterparts            []ActivityCounterpart
}

type activityMessageMetadata struct {
	AllDay bool   `json:"all_day"`
	Status string `json:"status"`
}

type activityCandidateRow struct {
	candidate           ActivityCandidate
	deletedAt           *time.Time
	deletedFromSourceAt *time.Time
	metadata            sql.NullString
}

const activityCandidateStateCTE = `
	WITH activity_current_state AS (
		SELECT
			COALESCE(MAX(CASE WHEN key = 'identity_revision'
				THEN CAST(value AS BIGINT) END), 0) AS identity_revision,
			COALESCE(MAX(CASE WHEN key = 'account_identity_revision'
				THEN CAST(value AS BIGINT) END), 0) AS account_identity_revision,
			COUNT(CASE WHEN key = 'activity_spine_timezone_active' THEN 1 END)
				AS timezone_active,
			COALESCE(
				MAX(CASE WHEN key = 'activity_spine_timezone_active' THEN value END),
				MAX(CASE WHEN key = 'activity_spine_timezone' THEN value END),
				''
			) AS timezone_target,
			COALESCE(MAX(CASE WHEN key = 'activity_spine_timezone_generation'
				THEN CAST(value AS BIGINT) END), 0) AS timezone_generation
		FROM archive_metadata
		WHERE key IN (
			'identity_revision',
			'account_identity_revision',
			'activity_spine_timezone',
			'activity_spine_timezone_active',
			'activity_spine_timezone_generation'
		)
	)`

const activityCandidateColumns = `
	m.id, m.source_id, m.conversation_id,
	COALESCE(c.conversation_type, ''), COALESCE(m.message_type, ''),
	m.sent_at, m.received_at, m.internal_date, m.last_modified,
	m.deleted_at, m.deleted_from_source_at, m.metadata,
	r.identity_revision, r.account_identity_revision,
	r.timezone_active, r.timezone_target, r.timezone_generation`

// LoadQueuedActivityCandidatesContext loads the next pending queue batch in
// stable message order. Ineligible rows stay in the result as retractions.
func (s *Store) LoadQueuedActivityCandidatesContext(
	ctx context.Context,
	limit int,
) ([]ActivityCandidate, error) {
	if limit <= 0 {
		return []ActivityCandidate{}, nil
	}
	rows, err := s.db.QueryContext(ctx, s.dialect.Rebind(
		activityCandidateStateCTE+`
		SELECT `+activityCandidateColumns+`,
		       1 AS queue_exists, q.revision, q.processed_revision
		FROM activity_projection_queue q
		JOIN messages m ON m.id = q.message_id
		LEFT JOIN conversations c ON c.id = m.conversation_id
		CROSS JOIN activity_current_state r
		WHERE q.revision > q.processed_revision
		ORDER BY m.id
		LIMIT ?
	`), limit)
	if err != nil {
		return nil, fmt.Errorf("load queued activity candidates: %w", err)
	}
	candidates, err := scanActivityCandidateRows(rows)
	if err != nil {
		return nil, err
	}
	return s.attachActivityCounterpartsContext(ctx, candidates)
}

// ScanForActivityProjectionContext provides a bounded, resumable backstop.
// Starting at zero discovers stale revision stamps even when their message IDs
// are below the ordinary forward watermark; afterID advances that independent
// pass without weakening the mismatch predicate.
func (s *Store) ScanForActivityProjectionContext(
	ctx context.Context,
	afterID int64,
	limit int,
) ([]ActivityCandidate, error) {
	if limit <= 0 {
		return []ActivityCandidate{}, nil
	}
	rows, err := s.db.QueryContext(ctx, s.dialect.Rebind(
		activityCandidateStateCTE+`
		SELECT `+activityCandidateColumns+`,
		       CASE WHEN q.message_id IS NULL THEN 0 ELSE 1 END AS queue_exists,
		       COALESCE(q.revision, 0), COALESCE(q.processed_revision, 0)
		FROM messages m
		LEFT JOIN conversations c ON c.id = m.conversation_id
		LEFT JOIN activity_events ae ON ae.message_id = m.id
		LEFT JOIN activity_projection_queue q ON q.message_id = m.id
		CROSS JOIN activity_current_state r
		WHERE m.id > ?
		  AND (
			(
				ae.message_id IS NULL
				AND (
					q.message_id IS NULL
					OR q.revision > q.processed_revision
				)
			)
			OR ae.projected_identity_revision <> r.identity_revision
			OR ae.projected_account_identity_revision <> r.account_identity_revision
		  )
		ORDER BY m.id
		LIMIT ?
	`), afterID, limit)
	if err != nil {
		return nil, fmt.Errorf("scan for activity projection: %w", err)
	}
	candidates, err := scanActivityCandidateRows(rows)
	if err != nil {
		return nil, err
	}
	return s.attachActivityCounterpartsContext(ctx, candidates)
}

// ScanAllActivityCandidatesContext returns every archived message in stable ID
// order. Unlike the optimized scan it intentionally includes current and
// drained/eventless rows for repair and timezone convergence passes.
func (s *Store) ScanAllActivityCandidatesContext(
	ctx context.Context,
	afterID int64,
	limit int,
) ([]ActivityCandidate, error) {
	if limit <= 0 {
		return []ActivityCandidate{}, nil
	}
	rows, err := s.db.QueryContext(ctx, s.dialect.Rebind(
		activityCandidateStateCTE+`
		SELECT `+activityCandidateColumns+`,
		       CASE WHEN q.message_id IS NULL THEN 0 ELSE 1 END AS queue_exists,
		       COALESCE(q.revision, 0), COALESCE(q.processed_revision, 0)
		FROM messages m
		LEFT JOIN conversations c ON c.id = m.conversation_id
		LEFT JOIN activity_projection_queue q ON q.message_id = m.id
		CROSS JOIN activity_current_state r
		WHERE m.id > ?
		ORDER BY m.id
		LIMIT ?
	`), afterID, limit)
	if err != nil {
		return nil, fmt.Errorf("scan all activity candidates: %w", err)
	}
	candidates, err := scanActivityCandidateRows(rows)
	if err != nil {
		return nil, err
	}
	return s.attachActivityCounterpartsContext(ctx, candidates)
}

// LoadActivityCandidatesByIDContext loads exact current tokens for the
// selected existing messages. IDs are deduplicated and returned in stable
// order; absent message IDs are omitted.
func (s *Store) LoadActivityCandidatesByIDContext(
	ctx context.Context,
	messageIDs []int64,
) ([]ActivityCandidate, error) {
	unique := make(map[int64]struct{}, len(messageIDs))
	for _, messageID := range messageIDs {
		if messageID <= 0 {
			return nil, fmt.Errorf("%w: message id must be positive",
				ErrInvalidActivity)
		}
		unique[messageID] = struct{}{}
	}
	if len(unique) == 0 {
		return []ActivityCandidate{}, nil
	}
	sorted := make([]int64, 0, len(unique))
	for messageID := range unique {
		sorted = append(sorted, messageID)
	}
	slices.Sort(sorted)
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(sorted)), ",")
	args := make([]any, len(sorted))
	for index, messageID := range sorted {
		args[index] = messageID
	}
	rows, err := s.db.QueryContext(ctx, s.dialect.Rebind(
		activityCandidateStateCTE+`
		SELECT `+activityCandidateColumns+`,
		       CASE WHEN q.message_id IS NULL THEN 0 ELSE 1 END AS queue_exists,
		       COALESCE(q.revision, 0), COALESCE(q.processed_revision, 0)
		FROM messages m
		LEFT JOIN conversations c ON c.id = m.conversation_id
		LEFT JOIN activity_projection_queue q ON q.message_id = m.id
		CROSS JOIN activity_current_state r
		WHERE m.id IN (`+placeholders+`)
		ORDER BY m.id
	`), args...)
	if err != nil {
		return nil, fmt.Errorf("load exact activity candidates: %w", err)
	}
	candidates, err := scanActivityCandidateRows(rows)
	if err != nil {
		return nil, err
	}
	return s.attachActivityCounterpartsContext(ctx, candidates)
}

func scanActivityCandidateRows(rows rowsScanner) ([]ActivityCandidate, error) {
	defer func() { _ = rows.Close() }()
	var result []ActivityCandidate
	for rows.Next() {
		var row activityCandidateRow
		var sentAt, receivedAt, internalDate, lastModified sql.NullTime
		var queueExists, timezoneActive int
		if err := rows.Scan(
			&row.candidate.MessageID,
			&row.candidate.SourceID,
			&row.candidate.ConversationID,
			&row.candidate.ConversationType,
			&row.candidate.MessageType,
			&sentAt,
			&receivedAt,
			&internalDate,
			&lastModified,
			&row.deletedAt,
			&row.deletedFromSourceAt,
			&row.metadata,
			&row.candidate.IdentityRevision,
			&row.candidate.AccountIdentityRevision,
			&timezoneActive,
			&row.candidate.TimezoneTransition.Target,
			&row.candidate.TimezoneTransition.Generation,
			&queueExists,
			&row.candidate.Queue.Revision,
			&row.candidate.Queue.ProcessedRevision,
		); err != nil {
			return nil, fmt.Errorf("scan activity candidate: %w", err)
		}
		row.candidate.Queue.Exists = queueExists != 0
		row.candidate.TimezoneTransition.Active = timezoneActive != 0
		row.candidate.SentAt = activityTimePointer(sentAt)
		row.candidate.ReceivedAt = activityTimePointer(receivedAt)
		row.candidate.InternalDate = activityTimePointer(internalDate)
		if lastModified.Valid {
			row.candidate.LastModified = lastModified.Time
		}
		applyActivityEligibility(&row)
		result = append(result, row.candidate)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate activity candidates: %w", err)
	}
	return result, nil
}

func activityTimePointer(value sql.NullTime) *time.Time {
	if !value.Valid {
		return nil
	}
	instant := value.Time
	return &instant
}

func applyActivityEligibility(row *activityCandidateRow) {
	var metadata activityMessageMetadata
	if row.metadata.Valid {
		_ = json.Unmarshal([]byte(row.metadata.String), &metadata)
	}
	row.candidate.DateOnly = metadata.AllDay
	hasTimestamp := activityUsableTime(row.candidate.SentAt) ||
		activityUsableTime(row.candidate.ReceivedAt) ||
		activityUsableTime(row.candidate.InternalDate)
	cancelled := row.candidate.MessageType == "calendar_event" &&
		strings.EqualFold(strings.TrimSpace(metadata.Status), "cancelled")
	row.candidate.Eligible = row.deletedAt == nil &&
		row.deletedFromSourceAt == nil &&
		hasTimestamp &&
		!cancelled
}

func activityUsableTime(value *time.Time) bool {
	return value != nil && !value.IsZero()
}

func (s *Store) attachActivityCounterpartsContext(
	ctx context.Context,
	candidates []ActivityCandidate,
) ([]ActivityCandidate, error) {
	if len(candidates) == 0 {
		return candidates, nil
	}

	byMessageID := make(map[int64]*ActivityCandidate, len(candidates))
	messageIDs := make([]any, 0, len(candidates))
	for index := range candidates {
		byMessageID[candidates[index].MessageID] = &candidates[index]
		messageIDs = append(messageIDs, candidates[index].MessageID)
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(messageIDs)), ",")

	query := `
		WITH direct AS (
			SELECT mr.message_id, mr.participant_id, mr.recipient_type
			FROM message_recipients mr
			WHERE mr.message_id IN (` + placeholders + `)
		),
		members AS (
			SELECT m.id AS message_id, cp.participant_id, 'member' AS recipient_type
			FROM messages m
			JOIN conversation_participants cp
			  ON cp.conversation_id = m.conversation_id
			WHERE m.id IN (` + placeholders + `)
			  AND NOT EXISTS (
				SELECT 1 FROM direct d WHERE d.message_id = m.id
			  )
		),
		counterparts AS (
			SELECT message_id, participant_id, recipient_type FROM direct
			UNION ALL
			SELECT message_id, participant_id, recipient_type FROM members
		)
		SELECT
			cp.message_id,
			cp.participant_id,
			cp.recipient_type,
			pp.person_id,
			CASE
				WHEN COALESCE(m.is_from_me, FALSE) AND cp.recipient_type = 'from' THEN 1
				WHEN EXISTS (
					SELECT 1
					FROM account_identities ai
					WHERE ai.source_id = m.source_id
					  AND (
						(p.email_address IS NOT NULL
						 AND LOWER(p.email_address) = LOWER(ai.address))
						OR EXISTS (
							SELECT 1
							FROM participant_identifiers pi
							WHERE pi.participant_id = p.id
							  AND (
								(pi.identifier_type = 'email'
								 AND LOWER(pi.identifier_value) = LOWER(ai.address))
								OR (pi.identifier_type <> 'email'
								 AND pi.identifier_value = ai.address)
							  )
						)
					  )
				) THEN 1
				ELSE 0
			END AS is_owner,
			COALESCE((
				SELECT MIN(ai.address)
				FROM account_identities ai
				WHERE ai.source_id = m.source_id
				  AND (
					(p.email_address IS NOT NULL
					 AND LOWER(p.email_address) = LOWER(ai.address))
					OR EXISTS (
						SELECT 1
						FROM participant_identifiers pi
						WHERE pi.participant_id = p.id
						  AND (
							(pi.identifier_type = 'email'
							 AND LOWER(pi.identifier_value) = LOWER(ai.address))
							OR (pi.identifier_type <> 'email'
							 AND pi.identifier_value = ai.address)
						  )
					)
				  )
			), '') AS owner_address
		FROM counterparts cp
		JOIN messages m ON m.id = cp.message_id
		JOIN participants p ON p.id = cp.participant_id
		LEFT JOIN person_participants pp ON pp.participant_id = cp.participant_id
		ORDER BY cp.message_id, cp.participant_id, cp.recipient_type`

	args := make([]any, 0, len(messageIDs)*2)
	args = append(args, messageIDs...)
	args = append(args, messageIDs...)
	rows, err := s.db.QueryContext(ctx, s.dialect.Rebind(query), args...)
	if err != nil {
		return nil, fmt.Errorf("load activity counterparts: %w", err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var (
			messageID     int64
			participantID int64
			recipientType string
			personID      *int64
			isOwner       int
			ownerAddress  string
		)
		if err := rows.Scan(
			&messageID,
			&participantID,
			&recipientType,
			&personID,
			&isOwner,
			&ownerAddress,
		); err != nil {
			return nil, fmt.Errorf("scan activity counterpart: %w", err)
		}
		candidate, found := byMessageID[messageID]
		if !found {
			continue
		}
		candidate.Counterparts = append(candidate.Counterparts, ActivityCounterpart{
			ParticipantID: participantID,
			PersonID:      personID,
			RecipientType: recipientType,
			IsOwner:       isOwner != 0,
			OwnerAddress:  ownerAddress,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate activity counterparts: %w", err)
	}
	return candidates, nil
}
