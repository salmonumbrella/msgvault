package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

type PersonCategory struct {
	Envelope        ValueEnvelope `json:"envelope"`
	PersonID        int64         `json:"person_id"`
	OriginalValue   string        `json:"original_value"`
	NormalizedValue string        `json:"normalized_value"`
}

type PersonCategoryInput struct {
	OriginalValue string        `json:"original_value"`
	Envelope      ValueEnvelope `json:"envelope"`
}

var (
	ErrPersonCategoryDuplicate = errors.New("person already has this current category")
	ErrPersonCategoryEmpty     = errors.New("person category must be non-empty")
)

func (s *Store) AddPersonCategoryContext(
	ctx context.Context, personID int64, input PersonCategoryInput,
) (*PersonCategory, error) {
	if err := input.Envelope.Validate(); err != nil {
		return nil, err
	}
	original := strings.TrimSpace(input.OriginalValue)
	if original == "" {
		return nil, ErrPersonCategoryEmpty
	}
	normalized := strings.ToLower(original)
	var result *PersonCategory
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		if err := s.lockIdentityMutationTxContext(ctx, tx); err != nil {
			return err
		}
		if err := ensureProfilePersonTx(ctx, tx, personID); err != nil {
			return err
		}
		var duplicate int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM person_categories
			WHERE person_id = ? AND normalized_value = ?
			  AND active_until IS NULL AND superseded_at IS NULL`,
			personID, normalized,
		).Scan(&duplicate); err != nil {
			return fmt.Errorf("check person category: %w", err)
		}
		if duplicate > 0 {
			return ErrPersonCategoryDuplicate
		}
		env := input.Envelope
		if env.Ordinal == 0 {
			if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(ordinal) + 1, 0)
				FROM person_categories
				WHERE person_id = ?
				  AND active_until IS NULL AND superseded_at IS NULL`,
				personID,
			).Scan(&env.Ordinal); err != nil {
				return fmt.Errorf("choose person category ordinal: %w", err)
			}
		}
		args := []any{personID, original, normalized}
		args = append(args, profileEnvelopeArgs(env)...)
		var id int64
		if err := tx.QueryRowContext(ctx, `INSERT INTO person_categories (
			person_id, original_value, normalized_value, `+profileEnvelopeWriteColumns+`,
			created_at, updated_at
		) VALUES (
			?, ?, ?,
			?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?,
			`+s.dialect.Now()+`, `+s.dialect.Now()+`
		) RETURNING id`, args...).Scan(&id); err != nil {
			return fmt.Errorf("add person category: %w", err)
		}
		if err := s.bumpPersonRevisionsTx(ctx, tx, personID); err != nil {
			return err
		}
		var err error
		result, err = getPersonCategoryTx(ctx, tx, personID, id)
		return err
	})
	return result, err
}

func (s *Store) ListPersonCategoriesContext(
	ctx context.Context, personID int64, currentOnly bool,
) ([]PersonCategory, error) {
	query := personCategorySelect + ` WHERE person_id = ?`
	if currentOnly {
		query += currentProfileValueFilter
	}
	query += ` ORDER BY normalized_value,
		CASE WHEN pref IS NULL THEN 1 ELSE 0 END, pref, ordinal, id`
	rows, err := s.db.QueryContext(ctx, query, personID)
	if err != nil {
		return nil, fmt.Errorf("list person categories: %w", err)
	}
	defer func() { _ = rows.Close() }()
	categories := make([]PersonCategory, 0)
	for rows.Next() {
		category, err := scanPersonCategory(rows)
		if err != nil {
			return nil, fmt.Errorf("scan person category: %w", err)
		}
		categories = append(categories, *category)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list person categories: %w", err)
	}
	return categories, nil
}

func (s *Store) SupersedePersonCategoryContext(
	ctx context.Context, personID, categoryID int64, activeUntil *time.Time,
) error {
	return s.supersedeProfileValueContext(
		ctx, "person_categories", personID, categoryID, activeUntil,
	)
}

const personCategorySelect = `SELECT
	id, person_id, original_value, normalized_value, ` + profileEnvelopeReadColumns + `
	FROM person_categories`

func getPersonCategoryTx(
	ctx context.Context, tx *loggedTx, personID, id int64,
) (*PersonCategory, error) {
	category, err := scanPersonCategory(tx.QueryRowContext(ctx,
		personCategorySelect+` WHERE person_id = ? AND id = ?`, personID, id,
	))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrProfileValueNotFound
	}
	return category, err
}

func scanPersonCategory(row scanner) (*PersonCategory, error) {
	var category PersonCategory
	var env profileEnvelopeScanValues
	dest := []any{
		&category.Envelope.ID, &category.PersonID,
		&category.OriginalValue, &category.NormalizedValue,
	}
	dest = append(dest, env.destinations()...)
	if err := row.Scan(dest...); err != nil {
		return nil, err
	}
	if err := env.apply(&category.Envelope); err != nil {
		return nil, err
	}
	return &category, nil
}
