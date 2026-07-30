package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

var (
	ErrInvalidProfilePref   = errors.New("profile value pref must be between 1 and 100")
	ErrProfileValueNotFound = errors.New("profile value not found")
)

// VCardIdentity identifies one property inside one vCard resource.
type VCardIdentity struct {
	Property string   `json:"property,omitempty"`
	Group    *string  `json:"group,omitempty"`
	PropID   *string  `json:"prop_id,omitempty"`
	PID      []string `json:"pid,omitempty"`
	AltID    *string  `json:"altid,omitempty"`
}

// IsZero reports whether no vCard property identity was captured.
func (v VCardIdentity) IsZero() bool {
	return v.Property == "" && v.Group == nil && v.PropID == nil &&
		len(v.PID) == 0 && v.AltID == nil
}

// ValueEnvelope carries ordering, provenance, vCard identity, and history.
type ValueEnvelope struct {
	ID           int64         `json:"id"`
	Pref         *int          `json:"pref,omitempty"`
	Ordinal      int           `json:"ordinal"`
	TypeLabel    *string       `json:"type_label,omitempty"`
	TypeTokens   []string      `json:"type_tokens,omitempty"`
	VCard        VCardIdentity `json:"vcard"`
	Source       Provenance    `json:"source"`
	SourceRef    *string       `json:"source_ref,omitempty"`
	Confidence   *float64      `json:"confidence,omitempty"`
	ActiveFrom   *time.Time    `json:"active_from,omitempty"`
	ActiveUntil  *time.Time    `json:"active_until,omitempty"`
	CreatedAt    time.Time     `json:"created_at"`
	UpdatedAt    time.Time     `json:"updated_at"`
	SupersededAt *time.Time    `json:"superseded_at,omitempty"`
}

// IsCurrent reports whether both world time and transaction time remain open.
func (e ValueEnvelope) IsCurrent() bool {
	return e.ActiveUntil == nil && e.SupersededAt == nil
}

// Validate checks the shared profile-value invariants.
func (e ValueEnvelope) Validate() error {
	if !e.Source.Valid() {
		return ErrInvalidProvenance
	}
	if e.Pref != nil && (*e.Pref < 1 || *e.Pref > 100) {
		return ErrInvalidProfilePref
	}
	if e.Confidence != nil && e.Source.IsDeclared() {
		return ErrConfidenceScope
	}
	if e.Confidence != nil && (*e.Confidence < 0 || *e.Confidence > 1) {
		return ErrConfidenceScope
	}
	if e.ActiveFrom != nil && e.ActiveUntil != nil && e.ActiveUntil.Before(*e.ActiveFrom) {
		return fmt.Errorf("profile value active_until precedes active_from")
	}
	return nil
}

const profileEnvelopeWriteColumns = `pref, ordinal, type_label, type_tokens, ` +
	`vcard_property, vcard_group, vcard_prop_id, vcard_pid, vcard_altid, ` +
	`source, source_ref, confidence, active_from, active_until`

const profileEnvelopeReadColumns = profileEnvelopeWriteColumns +
	`, created_at, updated_at, superseded_at`

func profileEnvelopeArgs(env ValueEnvelope) []any {
	return []any{
		intValue(env.Pref),
		env.Ordinal,
		stringValue(env.TypeLabel),
		joinTypeTokens(env.TypeTokens),
		env.VCard.Property,
		stringValue(env.VCard.Group),
		stringValue(env.VCard.PropID),
		joinTypeTokens(env.VCard.PID),
		stringValue(env.VCard.AltID),
		string(env.Source),
		stringValue(env.SourceRef),
		floatValue(env.Confidence),
		timeValue(env.ActiveFrom),
		timeValue(env.ActiveUntil),
	}
}

func scanProfileEnvelope(row scanner, env *ValueEnvelope) error {
	var (
		pref, ordinal                           sql.NullInt64
		typeLabel, typeTokens                   sql.NullString
		vcardProperty, vcardGroup, vcardPropID  sql.NullString
		vcardPID, vcardAltID, source, sourceRef sql.NullString
		confidence                              sql.NullFloat64
		activeFrom, activeUntil                 sql.NullTime
		createdAt, updatedAt, supersededAt      sql.NullTime
	)
	if err := row.Scan(
		&env.ID,
		&pref, &ordinal, &typeLabel, &typeTokens,
		&vcardProperty, &vcardGroup, &vcardPropID, &vcardPID, &vcardAltID,
		&source, &sourceRef, &confidence, &activeFrom, &activeUntil,
		&createdAt, &updatedAt, &supersededAt,
	); err != nil {
		return err
	}

	env.Pref = nullIntPtr(pref)
	if ordinal.Valid {
		env.Ordinal = int(ordinal.Int64)
	}
	env.TypeLabel = nullStringPtr(typeLabel)
	env.TypeTokens = splitTypeTokens(nullStringPtr(typeTokens))
	env.VCard = VCardIdentity{
		Property: vcardProperty.String,
		Group:    nullStringPtr(vcardGroup),
		PropID:   nullStringPtr(vcardPropID),
		PID:      splitTypeTokens(nullStringPtr(vcardPID)),
		AltID:    nullStringPtr(vcardAltID),
	}
	env.Source = Provenance(source.String)
	env.SourceRef = nullStringPtr(sourceRef)
	env.Confidence = nullFloatPtr(confidence)
	env.ActiveFrom = nullTimePtr(activeFrom)
	env.ActiveUntil = nullTimePtr(activeUntil)
	var err error
	env.CreatedAt, err = requireNullTime(createdAt, "created_at")
	if err != nil {
		return err
	}
	env.UpdatedAt, err = requireNullTime(updatedAt, "updated_at")
	if err != nil {
		return err
	}
	env.SupersededAt = nullTimePtr(supersededAt)
	return nil
}

func joinTypeTokens(tokens []string) *string {
	if len(tokens) == 0 {
		return nil
	}
	joined := strings.Join(tokens, ",")
	return &joined
}

func splitTypeTokens(raw *string) []string {
	if raw == nil || *raw == "" {
		return []string{}
	}
	return strings.Split(*raw, ",")
}

func stringValue(value *string) any {
	if value == nil {
		return nil
	}
	return *value
}

func floatValue(value *float64) any {
	if value == nil {
		return nil
	}
	return *value
}

func timeValue(value *time.Time) any {
	if value == nil {
		return nil
	}
	return *value
}

func nullStringPtr(value sql.NullString) *string {
	if !value.Valid {
		return nil
	}
	return &value.String
}

func nullFloatPtr(value sql.NullFloat64) *float64 {
	if !value.Valid {
		return nil
	}
	return &value.Float64
}

func nullTimePtr(value sql.NullTime) *time.Time {
	if !value.Valid {
		return nil
	}
	return &value.Time
}
