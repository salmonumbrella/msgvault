package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

type PersonNameKind string

const (
	PersonNameFormatted  PersonNameKind = "formatted"
	PersonNameStructured PersonNameKind = "structured"
	PersonNameNickname   PersonNameKind = "nickname"
	PersonNamePhonetic   PersonNameKind = "phonetic"
	PersonNameSort       PersonNameKind = "sort"
)

func (k PersonNameKind) Valid() bool {
	switch k {
	case PersonNameFormatted, PersonNameStructured, PersonNameNickname,
		PersonNamePhonetic, PersonNameSort:
		return true
	default:
		return false
	}
}

type PersonName struct {
	Envelope          ValueEnvelope  `json:"envelope"`
	PersonID          int64          `json:"person_id"`
	NameKind          PersonNameKind `json:"name_kind"`
	Formatted         *string        `json:"formatted,omitempty"`
	FamilyName        *string        `json:"family_name,omitempty"`
	GivenName         *string        `json:"given_name,omitempty"`
	AdditionalNames   *string        `json:"additional_names,omitempty"`
	HonorificPrefixes *string        `json:"honorific_prefixes,omitempty"`
	HonorificSuffixes *string        `json:"honorific_suffixes,omitempty"`
	SecondarySurname  *string        `json:"secondary_surname,omitempty"`
	Generation        *string        `json:"generation,omitempty"`
	Language          *string        `json:"language,omitempty"`
	Script            *string        `json:"script,omitempty"`
	PhoneticSystem    *string        `json:"phonetic_system,omitempty"`
	PhoneticScript    *string        `json:"phonetic_script,omitempty"`
	SortAs            *string        `json:"sort_as,omitempty"`
	IsDerived         bool           `json:"is_derived"`
	OriginalValue     string         `json:"original_value"`
}

type PersonNameInput struct {
	NameKind          PersonNameKind
	Formatted         *string
	FamilyName        *string
	GivenName         *string
	AdditionalNames   *string
	HonorificPrefixes *string
	HonorificSuffixes *string
	SecondarySurname  *string
	Generation        *string
	Language          *string
	Script            *string
	PhoneticSystem    *string
	PhoneticScript    *string
	SortAs            *string
	IsDerived         bool
	OriginalValue     string
	Envelope          ValueEnvelope
}

var (
	ErrInvalidPersonNameKind  = errors.New("invalid person name kind")
	ErrPersonNameValueMissing = errors.New("person name requires at least one non-empty component")
)

func (s *Store) AddPersonNameContext(ctx context.Context, personID int64, input PersonNameInput) (*PersonName, error) {
	var result *PersonName
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		if err := s.lockIdentityMutationTxContext(ctx, tx); err != nil {
			return err
		}
		if err := ensureProfilePersonTx(ctx, tx, personID); err != nil {
			return err
		}
		var err error
		result, err = s.addPersonNameTx(ctx, tx, personID, input)
		if err != nil {
			return err
		}
		if err := s.bumpPersonRevisionsTx(ctx, tx, personID); err != nil {
			return err
		}
		return nil
	})
	return result, err
}

func (s *Store) ListPersonNamesContext(ctx context.Context, personID int64, currentOnly bool) ([]PersonName, error) {
	query := personNameSelect + ` WHERE person_id = ?`
	if currentOnly {
		query += ` AND active_until IS NULL AND superseded_at IS NULL`
	}
	query += ` ORDER BY name_kind, CASE WHEN pref IS NULL THEN 1 ELSE 0 END, pref, ordinal, id`
	rows, err := s.db.QueryContext(ctx, query, personID)
	if err != nil {
		return nil, fmt.Errorf("list person names: %w", err)
	}
	defer rows.Close()
	names := make([]PersonName, 0)
	for rows.Next() {
		name, err := scanPersonName(rows)
		if err != nil {
			return nil, fmt.Errorf("scan person name: %w", err)
		}
		names = append(names, *name)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list person names: %w", err)
	}
	return names, nil
}

func (s *Store) SupersedePersonNameContext(ctx context.Context, personID, nameID int64, activeUntil *time.Time) error {
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		if err := s.lockIdentityMutationTxContext(ctx, tx); err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, `UPDATE person_names
			SET active_until = COALESCE(?, `+s.dialect.Now()+`),
			    superseded_at = `+s.dialect.Now()+`,
			    updated_at = `+s.dialect.Now()+`
			WHERE id = ? AND person_id = ?
			  AND active_until IS NULL AND superseded_at IS NULL`,
			timeValue(activeUntil), nameID, personID,
		)
		if err != nil {
			return fmt.Errorf("supersede person name: %w", err)
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("check superseded person name: %w", err)
		}
		if changed == 0 {
			return ErrProfileValueNotFound
		}
		return s.bumpPersonRevisionsTx(ctx, tx, personID)
	})
}

func firstNonBlankNameComponent(input PersonNameInput) string {
	for _, value := range []*string{
		input.Formatted, input.FamilyName, input.GivenName, input.AdditionalNames,
		input.HonorificPrefixes, input.HonorificSuffixes, input.SecondarySurname,
		input.Generation, input.SortAs,
	} {
		if value != nil && strings.TrimSpace(*value) != "" {
			return strings.TrimSpace(*value)
		}
	}
	return ""
}

const personNameSelect = `SELECT
	id, person_id, name_kind, formatted, family_name, given_name,
	additional_names, honorific_prefixes, honorific_suffixes,
	secondary_surname, generation, language, script, phonetic_system,
	phonetic_script, sort_as, is_derived, original_value,
	pref, ordinal, type_label, type_tokens, vcard_property, vcard_group,
	vcard_prop_id, vcard_pid, vcard_altid, source, source_ref, confidence,
	active_from, active_until, created_at, updated_at, superseded_at
	FROM person_names`

func getPersonNameTx(ctx context.Context, tx *loggedTx, personID, id int64) (*PersonName, error) {
	name, err := scanPersonName(tx.QueryRowContext(ctx,
		personNameSelect+` WHERE person_id = ? AND id = ?`, personID, id,
	))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrProfileValueNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get person name: %w", err)
	}
	return name, nil
}

func scanPersonName(row scanner) (*PersonName, error) {
	var name PersonName
	var formatted, family, given, additional, prefixes, suffixes sql.NullString
	var secondary, generation, language, script, phoneticSystem sql.NullString
	var phoneticScript, sortAs sql.NullString
	var pref, ordinal sql.NullInt64
	var typeLabel, typeTokens, property, group, propID, pid, altID sql.NullString
	var source, sourceRef sql.NullString
	var confidence sql.NullFloat64
	var activeFrom, activeUntil, createdAt, updatedAt, supersededAt sql.NullTime
	if err := row.Scan(
		&name.Envelope.ID, &name.PersonID, &name.NameKind,
		&formatted, &family, &given, &additional, &prefixes, &suffixes,
		&secondary, &generation, &language, &script, &phoneticSystem,
		&phoneticScript, &sortAs, &name.IsDerived, &name.OriginalValue,
		&pref, &ordinal, &typeLabel, &typeTokens, &property, &group,
		&propID, &pid, &altID, &source, &sourceRef, &confidence,
		&activeFrom, &activeUntil, &createdAt, &updatedAt, &supersededAt,
	); err != nil {
		return nil, err
	}
	name.Formatted = nullStringPtr(formatted)
	name.FamilyName = nullStringPtr(family)
	name.GivenName = nullStringPtr(given)
	name.AdditionalNames = nullStringPtr(additional)
	name.HonorificPrefixes = nullStringPtr(prefixes)
	name.HonorificSuffixes = nullStringPtr(suffixes)
	name.SecondarySurname = nullStringPtr(secondary)
	name.Generation = nullStringPtr(generation)
	name.Language = nullStringPtr(language)
	name.Script = nullStringPtr(script)
	name.PhoneticSystem = nullStringPtr(phoneticSystem)
	name.PhoneticScript = nullStringPtr(phoneticScript)
	name.SortAs = nullStringPtr(sortAs)
	name.Envelope.Pref = nullIntPtr(pref)
	if ordinal.Valid {
		name.Envelope.Ordinal = int(ordinal.Int64)
	}
	name.Envelope.TypeLabel = nullStringPtr(typeLabel)
	name.Envelope.TypeTokens = splitTypeTokens(nullStringPtr(typeTokens))
	name.Envelope.VCard = VCardIdentity{
		Property: property.String, Group: nullStringPtr(group),
		PropID: nullStringPtr(propID), PID: splitTypeTokens(nullStringPtr(pid)),
		AltID: nullStringPtr(altID),
	}
	name.Envelope.Source = Provenance(source.String)
	name.Envelope.SourceRef = nullStringPtr(sourceRef)
	name.Envelope.Confidence = nullFloatPtr(confidence)
	name.Envelope.ActiveFrom = nullTimePtr(activeFrom)
	name.Envelope.ActiveUntil = nullTimePtr(activeUntil)
	var err error
	name.Envelope.CreatedAt, err = requireNullTime(createdAt, "created_at")
	if err != nil {
		return nil, err
	}
	name.Envelope.UpdatedAt, err = requireNullTime(updatedAt, "updated_at")
	if err != nil {
		return nil, err
	}
	name.Envelope.SupersededAt = nullTimePtr(supersededAt)
	return &name, nil
}
