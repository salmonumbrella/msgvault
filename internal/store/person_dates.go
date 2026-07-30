package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

type PersonDateKind string

const (
	PersonDateBirthday    PersonDateKind = "birthday"
	PersonDateAnniversary PersonDateKind = "anniversary"
	PersonDateDeath       PersonDateKind = "death"
	PersonDateCustom      PersonDateKind = "custom"
)

func (k PersonDateKind) Valid() bool {
	switch k {
	case PersonDateBirthday, PersonDateAnniversary, PersonDateDeath, PersonDateCustom:
		return true
	default:
		return false
	}
}

type PersonDate struct {
	Envelope      ValueEnvelope  `json:"envelope"`
	PersonID      int64          `json:"person_id"`
	DateKind      PersonDateKind `json:"date_kind"`
	Label         *string        `json:"label,omitempty"`
	Date          PartialDate    `json:"date"`
	DateText      *string        `json:"date_text,omitempty"`
	CalendarScale *string        `json:"calendar_scale,omitempty"`
	OriginalValue string         `json:"original_value"`
}

type PersonDateInput struct {
	DateKind      PersonDateKind `json:"date_kind"`
	Label         *string        `json:"label,omitempty"`
	Date          PartialDate    `json:"date"`
	DateText      *string        `json:"date_text,omitempty"`
	CalendarScale *string        `json:"calendar_scale,omitempty"`
	OriginalValue string         `json:"original_value"`
	Envelope      ValueEnvelope  `json:"envelope"`
}

var (
	ErrInvalidPersonDateKind  = errors.New("invalid person date kind")
	ErrPersonDateValueMissing = errors.New("person date requires a partial date or date text")
)

func (s *Store) AddPersonDateContext(
	ctx context.Context, personID int64, input PersonDateInput,
) (*PersonDate, error) {
	if !input.DateKind.Valid() {
		return nil, ErrInvalidPersonDateKind
	}
	if err := input.Envelope.Validate(); err != nil {
		return nil, err
	}
	if input.Date.IsZero() && (input.DateText == nil || strings.TrimSpace(*input.DateText) == "") {
		return nil, ErrPersonDateValueMissing
	}
	if !input.Date.IsZero() {
		if err := input.Date.Validate(); err != nil {
			return nil, err
		}
	}
	original := strings.TrimSpace(input.OriginalValue)
	if original == "" {
		if !input.Date.IsZero() {
			original = input.Date.String()
		} else {
			original = strings.TrimSpace(*input.DateText)
		}
	}
	var result *PersonDate
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		if err := s.lockIdentityMutationTxContext(ctx, tx); err != nil {
			return err
		}
		if err := ensureProfilePersonTx(ctx, tx, personID); err != nil {
			return err
		}
		env := input.Envelope
		if env.Ordinal == 0 {
			var err error
			env.Ordinal, err = nextProfileOrdinalTx(
				ctx, tx, "person_dates", "date_kind", personID, input.DateKind,
			)
			if err != nil {
				return err
			}
		}
		args := []any{personID, input.DateKind, stringValue(input.Label)}
		args = append(args, PartialDateArgs(input.Date)...)
		args = append(args, stringValue(input.DateText), stringValue(input.CalendarScale), original)
		args = append(args, profileEnvelopeArgs(env)...)
		var id int64
		if err := tx.QueryRowContext(ctx, `INSERT INTO person_dates (
			person_id, date_kind, label, date_year, date_month, date_day,
			date_text, calendar_scale, original_value, `+profileEnvelopeWriteColumns+`,
			created_at, updated_at
		) VALUES (
			?, ?, ?, ?, ?, ?, ?, ?, ?,
			?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?,
			`+s.dialect.Now()+`, `+s.dialect.Now()+`
		) RETURNING id`, args...).Scan(&id); err != nil {
			return fmt.Errorf("add person date: %w", err)
		}
		if err := s.bumpPersonRevisionsTx(ctx, tx, personID); err != nil {
			return err
		}
		var err error
		result, err = getPersonDateTx(ctx, tx, personID, id)
		return err
	})
	return result, err
}

func (s *Store) ListPersonDatesContext(
	ctx context.Context, personID int64, currentOnly bool,
) ([]PersonDate, error) {
	query := personDateSelect + ` WHERE person_id = ?`
	if currentOnly {
		query += currentProfileValueFilter
	}
	query += ` ORDER BY date_kind,
		CASE WHEN pref IS NULL THEN 1 ELSE 0 END, pref, ordinal, id`
	rows, err := s.db.QueryContext(ctx, query, personID)
	if err != nil {
		return nil, fmt.Errorf("list person dates: %w", err)
	}
	defer func() { _ = rows.Close() }()
	dates := make([]PersonDate, 0)
	for rows.Next() {
		date, err := scanPersonDate(rows)
		if err != nil {
			return nil, fmt.Errorf("scan person date: %w", err)
		}
		dates = append(dates, *date)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list person dates: %w", err)
	}
	return dates, nil
}

func (s *Store) SupersedePersonDateContext(
	ctx context.Context, personID, dateID int64, activeUntil *time.Time,
) error {
	return s.supersedeProfileValueContext(ctx, "person_dates", personID, dateID, activeUntil)
}

const personDateSelect = `SELECT
	id, person_id, date_kind, label, date_year, date_month, date_day,
	date_text, calendar_scale, original_value, ` + profileEnvelopeReadColumns + `
	FROM person_dates`

func getPersonDateTx(
	ctx context.Context, tx *loggedTx, personID, id int64,
) (*PersonDate, error) {
	date, err := scanPersonDate(tx.QueryRowContext(ctx,
		personDateSelect+` WHERE person_id = ? AND id = ?`, personID, id,
	))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrProfileValueNotFound
	}
	return date, err
}

func scanPersonDate(row scanner) (*PersonDate, error) {
	var date PersonDate
	var label, dateText, calendarScale sql.NullString
	var year, month, day sql.NullInt64
	var env profileEnvelopeScanValues
	dest := []any{
		&date.Envelope.ID, &date.PersonID, &date.DateKind, &label,
		&year, &month, &day, &dateText, &calendarScale, &date.OriginalValue,
	}
	dest = append(dest, env.destinations()...)
	if err := row.Scan(dest...); err != nil {
		return nil, err
	}
	date.Label = nullStringPtr(label)
	date.Date = ScanPartialDate(year, month, day)
	date.DateText = nullStringPtr(dateText)
	date.CalendarScale = nullStringPtr(calendarScale)
	if err := env.apply(&date.Envelope); err != nil {
		return nil, err
	}
	return &date, nil
}
