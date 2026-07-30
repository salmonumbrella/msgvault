package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

var (
	ErrOrganizationDuplicateSuggestionNotFound        = errors.New("organization duplicate suggestion not found")
	ErrOrganizationDuplicateSuggestionInvalid         = errors.New("invalid organization duplicate suggestion")
	ErrOrganizationDuplicateSuggestionAlreadyResolved = errors.New(
		"organization duplicate suggestion already resolved")
)

// Duplicate-suggestion criteria. Both are review signals derived from
// normalized values; neither is identity proof.
const (
	OrganizationDuplicateCriterionDomain = "domain"
	OrganizationDuplicateCriterionName   = "name"
)

// Duplicate-suggestion statuses. A rejected row is retained so the same
// inference is not raised again.
const (
	OrganizationDuplicateStatusOpen     = "open"
	OrganizationDuplicateStatusAccepted = "accepted"
	OrganizationDuplicateStatusRejected = "rejected"
)

// Confidence values for the two criteria. A shared registrable domain is
// stronger evidence than a shared normalized name, and neither reaches 1
// because only a user may confirm a duplicate.
const (
	organizationDuplicateDomainConfidence = 0.7
	organizationDuplicateNameConfidence   = 0.4
)

// OrganizationDuplicateSuggestion is one reviewable possible-duplicate pair.
// Accepting it records the user's decision; merging is the separate explicit
// MergeOrganizationsContext call, so no scan can ever combine two
// organizations on its own.
type OrganizationDuplicateSuggestion struct {
	ID              int64      `json:"id"`
	OrganizationAID int64      `json:"organization_a_id"`
	OrganizationBID int64      `json:"organization_b_id"`
	Criterion       string     `json:"criterion"`
	Evidence        string     `json:"evidence"`
	Confidence      float64    `json:"confidence"`
	Status          string     `json:"status"`
	ResolvedAt      *time.Time `json:"resolved_at,omitempty"`
	ResolvedBy      *string    `json:"resolved_by,omitempty"`
	ResolutionNote  *string    `json:"resolution_note,omitempty"`
	Source          Provenance `json:"source"`
	CreatedAt       time.Time  `json:"created_at"`
	UpdatedAt       time.Time  `json:"updated_at"`
}

const organizationDuplicateSuggestionColumns = `
	id, organization_a_id, organization_b_id, criterion, evidence, confidence,
	status, resolved_at, resolved_by, resolution_note, source, created_at, updated_at
`

const organizationDuplicateDomainPairsQuery = `
	WITH live AS (
		SELECT id FROM organizations
		WHERE retired_at IS NULL AND merged_into_id IS NULL
	),
	domains AS (
		SELECT o.id AS organization_id, o.primary_domain AS value
		FROM organizations o
		JOIN live ON live.id = o.id
		WHERE o.primary_domain IS NOT NULL AND LENGTH(o.primary_domain) > 0
		UNION
		SELECT i.organization_id, i.normalized_value
		FROM organization_identifiers i
		JOIN live ON live.id = i.organization_id
		WHERE i.identifier_kind = 'domain'
		  AND i.active_until IS NULL AND i.superseded_at IS NULL
	)
	SELECT a.organization_id, b.organization_id, a.value
	FROM domains a
	JOIN domains b
		ON b.value = a.value AND b.organization_id > a.organization_id
	ORDER BY a.organization_id, b.organization_id, a.value
`

const organizationDuplicateNamePairsQuery = `
	WITH live AS (
		SELECT id FROM organizations
		WHERE retired_at IS NULL AND merged_into_id IS NULL
	),
	names AS (
		SELECT o.id AS organization_id, o.name_normalized AS value
		FROM organizations o
		JOIN live ON live.id = o.id
		WHERE LENGTH(o.name_normalized) > 0
		UNION
		SELECT n.organization_id, n.name_normalized
		FROM organization_names n
		JOIN live ON live.id = n.organization_id
		WHERE n.active_until IS NULL AND n.superseded_at IS NULL
	)
	SELECT a.organization_id, b.organization_id, a.value
	FROM names a
	JOIN names b
		ON b.value = a.value AND b.organization_id > a.organization_id
	ORDER BY a.organization_id, b.organization_id, a.value
`

// RefreshOrganizationDuplicateSuggestionsContext scans live organizations for
// shared normalized domains and shared normalized names and records any pair
// it has not recorded before. It returns the number of new suggestion rows. It
// never merges, retires, or revises an organization.
func (s *Store) RefreshOrganizationDuplicateSuggestionsContext(ctx context.Context) (int, error) {
	created := 0
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		var err error
		created, err = refreshOrganizationDuplicateSuggestionsTx(ctx, tx, s.dialect,
			organizationDuplicateDomainPairsQuery, OrganizationDuplicateCriterionDomain,
			organizationDuplicateDomainConfidence)
		if err != nil {
			return err
		}
		nameCreated, err := refreshOrganizationDuplicateSuggestionsTx(ctx, tx, s.dialect,
			organizationDuplicateNamePairsQuery, OrganizationDuplicateCriterionName,
			organizationDuplicateNameConfidence)
		if err != nil {
			return err
		}
		created += nameCreated
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("refresh organization duplicate suggestions: %w", err)
	}
	return created, nil
}

func refreshOrganizationDuplicateSuggestionsTx(
	ctx context.Context, tx *loggedTx, dialect Dialect, query, criterion string, confidence float64,
) (int, error) {
	type pair struct {
		aID, bID int64
		evidence string
	}
	rows, err := tx.QueryContext(ctx, query)
	if err != nil {
		return 0, fmt.Errorf("find %s duplicate pairs: %w", criterion, err)
	}
	pairs := make([]pair, 0)
	for rows.Next() {
		var candidate pair
		if err := rows.Scan(&candidate.aID, &candidate.bID, &candidate.evidence); err != nil {
			_ = rows.Close()
			return 0, fmt.Errorf("scan %s duplicate pair: %w", criterion, err)
		}
		pairs = append(pairs, candidate)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return 0, fmt.Errorf("iterate %s duplicate pairs: %w", criterion, err)
	}
	if err := rows.Close(); err != nil {
		return 0, fmt.Errorf("close %s duplicate pairs: %w", criterion, err)
	}

	insert := dialect.InsertOrIgnore(fmt.Sprintf(`
		INSERT OR IGNORE INTO organization_duplicate_suggestions (
			organization_a_id, organization_b_id, criterion, evidence, confidence,
			status, source, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, %s, %s)
	`, dialect.Now(), dialect.Now()))
	created := 0
	for _, candidate := range pairs {
		result, err := tx.ExecContext(ctx, insert,
			candidate.aID, candidate.bID, criterion, candidate.evidence, confidence,
			OrganizationDuplicateStatusOpen, ProvenanceSystem)
		if err != nil {
			return 0, fmt.Errorf("insert %s duplicate suggestion: %w", criterion, err)
		}
		count, err := result.RowsAffected()
		if err != nil {
			return 0, fmt.Errorf("count inserted %s duplicate suggestions: %w", criterion, err)
		}
		created += int(count)
	}
	return created, nil
}

// ListOrganizationDuplicateSuggestionsContext returns suggestions filtered by
// status. An empty status returns every status. Limit is clamped to
// MaxOrganizationPageSize.
func (s *Store) ListOrganizationDuplicateSuggestionsContext(
	ctx context.Context, status string, limit, offset int,
) ([]OrganizationDuplicateSuggestion, error) {
	if err := validateOrganizationDuplicateSuggestionStatus(status, true); err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = DefaultOrganizationPageSize
	} else if limit > MaxOrganizationPageSize {
		limit = MaxOrganizationPageSize
	}
	offset = max(offset, 0)
	args := []any{limit, offset}
	where := ""
	if status != "" {
		where = " WHERE status = ?"
		args = append([]any{status}, args...)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+organizationDuplicateSuggestionColumns+
		` FROM organization_duplicate_suggestions`+where+` ORDER BY id LIMIT ? OFFSET ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("list organization duplicate suggestions: %w", err)
	}
	defer func() { _ = rows.Close() }()

	suggestions := make([]OrganizationDuplicateSuggestion, 0)
	for rows.Next() {
		suggestion, err := scanOrganizationDuplicateSuggestion(rows)
		if err != nil {
			return nil, fmt.Errorf("scan organization duplicate suggestion: %w", err)
		}
		suggestions = append(suggestions, *suggestion)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate organization duplicate suggestions: %w", err)
	}
	return suggestions, nil
}

// ResolveOrganizationDuplicateSuggestionContext records the user's decision.
// status must be accepted or rejected; resolving back to open is rejected so
// an audit trail cannot be erased.
func (s *Store) ResolveOrganizationDuplicateSuggestionContext(
	ctx context.Context, id int64, status string, note *string,
) (*OrganizationDuplicateSuggestion, error) {
	if status == OrganizationDuplicateStatusOpen {
		return nil, fmt.Errorf("%w: cannot resolve a suggestion back to open",
			ErrOrganizationDuplicateSuggestionInvalid)
	}
	if err := validateOrganizationDuplicateSuggestionStatus(status, false); err != nil {
		return nil, err
	}
	if note != nil {
		trimmed := strings.TrimSpace(*note)
		if trimmed == "" {
			note = nil
		} else {
			note = &trimmed
		}
	}
	suggestion, err := scanOrganizationDuplicateSuggestion(s.db.QueryRowContext(ctx, fmt.Sprintf(`
		UPDATE organization_duplicate_suggestions
		SET status = ?, resolution_note = ?, resolved_at = %s, updated_at = %s
		WHERE id = ? AND status = ?
		RETURNING %s
	`, s.dialect.Now(), s.dialect.Now(), organizationDuplicateSuggestionColumns),
		status, note, id, OrganizationDuplicateStatusOpen))
	if errors.Is(err, sql.ErrNoRows) {
		var currentStatus string
		statusErr := s.db.QueryRowContext(ctx, `
			SELECT status FROM organization_duplicate_suggestions WHERE id = ?
		`, id).Scan(&currentStatus)
		if errors.Is(statusErr, sql.ErrNoRows) {
			return nil, ErrOrganizationDuplicateSuggestionNotFound
		}
		if statusErr != nil {
			return nil, fmt.Errorf(
				"check organization duplicate suggestion %d resolution: %w", id, statusErr)
		}
		return nil, fmt.Errorf("%w: current status is %s",
			ErrOrganizationDuplicateSuggestionAlreadyResolved, currentStatus)
	}
	if err != nil {
		return nil, fmt.Errorf("resolve organization duplicate suggestion %d: %w", id, err)
	}
	return suggestion, nil
}

func validateOrganizationDuplicateSuggestionStatus(status string, allowEmpty bool) error {
	if status == "" && allowEmpty {
		return nil
	}
	if status != OrganizationDuplicateStatusOpen &&
		status != OrganizationDuplicateStatusAccepted &&
		status != OrganizationDuplicateStatusRejected {
		return fmt.Errorf("%w: unknown status %q", ErrOrganizationDuplicateSuggestionInvalid, status)
	}
	return nil
}

func scanOrganizationDuplicateSuggestion(row scanner) (*OrganizationDuplicateSuggestion, error) {
	var (
		suggestion                 OrganizationDuplicateSuggestion
		resolvedAt                 sql.NullTime
		resolvedBy, resolutionNote sql.NullString
	)
	if err := row.Scan(
		&suggestion.ID, &suggestion.OrganizationAID, &suggestion.OrganizationBID,
		&suggestion.Criterion, &suggestion.Evidence, &suggestion.Confidence,
		&suggestion.Status, &resolvedAt, &resolvedBy, &resolutionNote,
		&suggestion.Source, &suggestion.CreatedAt, &suggestion.UpdatedAt,
	); err != nil {
		return nil, err
	}
	suggestion.ResolvedAt = nullTimePtr(resolvedAt)
	suggestion.ResolvedBy = nullStringPtr(resolvedBy)
	suggestion.ResolutionNote = nullStringPtr(resolutionNote)
	return &suggestion, nil
}
