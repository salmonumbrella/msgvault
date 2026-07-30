package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// EmploymentProjection is the derived current-company view of a person. It is
// computed from the primary current employment on every read and is never
// stored: the roadmap forbids mutable person.company and person.job_title
// copies precisely so these values cannot drift from employment history.
// Empty strings mean the underlying employment left that field unset.
type EmploymentProjection struct {
	PersonID         int64  `json:"person_id"`
	EmploymentID     int64  `json:"employment_id"`
	OrganizationID   int64  `json:"organization_id"`
	OrganizationName string `json:"organization_name"`
	Title            string `json:"title"`
	Role             string `json:"role"`
	Department       string `json:"department"`
}

// PrimaryCurrentEmploymentContext returns the person's derived company and
// title. found is false when the person has no primary current employment,
// which is a normal state and not an error.
func (s *Store) PrimaryCurrentEmploymentContext(
	ctx context.Context, personID int64,
) (EmploymentProjection, bool, error) {
	projection, err := scanEmploymentProjection(s.db.QueryRowContext(ctx, fmt.Sprintf(`
		SELECT e.person_id, e.id, e.organization_id, o.name,
		       COALESCE(e.title, ''), COALESCE(e.role, ''), COALESCE(e.department, '')
		FROM employments e
		JOIN organizations o ON o.id = e.organization_id
		WHERE e.person_id = ? AND %s AND %s
	`, s.dialect.BoolTrueExpr("e.is_primary"), s.dialect.BoolTrueExpr("e.is_current")), personID))
	if errors.Is(err, sql.ErrNoRows) {
		return EmploymentProjection{}, false, nil
	}
	if err != nil {
		return EmploymentProjection{}, false, fmt.Errorf("project primary current employment for person %d: %w", personID, err)
	}
	return projection, true, nil
}

// PrimaryCurrentEmploymentsContext resolves projections for many people in one
// query so a people listing never issues a query per row. People without a
// primary current employment are absent from the map rather than mapped to a
// zero value.
func (s *Store) PrimaryCurrentEmploymentsContext(
	ctx context.Context, personIDs []int64,
) (map[int64]EmploymentProjection, error) {
	projections := make(map[int64]EmploymentProjection)
	ids := make([]int64, 0, len(personIDs))
	for _, personID := range personIDs {
		if personID > 0 {
			ids = append(ids, personID)
		}
	}
	if len(ids) == 0 {
		return projections, nil
	}

	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	args := make([]any, len(ids))
	for i, personID := range ids {
		args[i] = personID
	}
	rows, err := s.db.QueryContext(ctx, fmt.Sprintf(`
		SELECT e.person_id, e.id, e.organization_id, o.name,
		       COALESCE(e.title, ''), COALESCE(e.role, ''), COALESCE(e.department, '')
		FROM employments e
		JOIN organizations o ON o.id = e.organization_id
		WHERE e.person_id IN (%s) AND %s AND %s
	`, placeholders, s.dialect.BoolTrueExpr("e.is_primary"), s.dialect.BoolTrueExpr("e.is_current")), args...)
	if err != nil {
		return nil, fmt.Errorf("query primary current employment projections: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		projection, err := scanEmploymentProjection(rows)
		if err != nil {
			return nil, fmt.Errorf("scan primary current employment projection: %w", err)
		}
		projections[projection.PersonID] = projection
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate primary current employment projections: %w", err)
	}
	return projections, nil
}

func scanEmploymentProjection(row scanner) (EmploymentProjection, error) {
	var projection EmploymentProjection
	err := row.Scan(
		&projection.PersonID,
		&projection.EmploymentID,
		&projection.OrganizationID,
		&projection.OrganizationName,
		&projection.Title,
		&projection.Role,
		&projection.Department,
	)
	if err != nil {
		return EmploymentProjection{}, err
	}
	return projection, nil
}
