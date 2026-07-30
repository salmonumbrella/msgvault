package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

type PersonContactPoint struct {
	Envelope             ValueEnvelope      `json:"envelope"`
	PersonID             int64              `json:"person_id"`
	AddressKind          ContactAddressKind `json:"address_kind"`
	ServiceSlug          *string            `json:"service_slug,omitempty"`
	ScopeKind            *string            `json:"scope_kind,omitempty"`
	ScopeValue           *string            `json:"scope_value,omitempty"`
	OriginalValue        string             `json:"original_value"`
	NormalizedValue      string             `json:"normalized_value"`
	Normalization        string             `json:"normalization"`
	NormalizationVersion int                `json:"normalization_version"`
	URI                  *string            `json:"uri,omitempty"`
}

type PersonContactPointInput struct {
	AddressKind   ContactAddressKind `json:"address_kind"`
	ServiceSlug   *string            `json:"service_slug,omitempty"`
	ScopeKind     *string            `json:"scope_kind,omitempty"`
	ScopeValue    *string            `json:"scope_value,omitempty"`
	OriginalValue string             `json:"original_value"`
	URI           *string            `json:"uri,omitempty"`
	Envelope      ValueEnvelope      `json:"envelope"`
}

type ContactPointQuery struct {
	AddressKind     ContactAddressKind
	ServiceSlug     *string
	ScopeKind       *string
	ScopeValue      *string
	NormalizedValue string
}

var (
	ErrInvalidContactAddressKind = errors.New("invalid contact address kind")
	ErrContactPointValueMissing  = errors.New("contact point requires a non-empty value")
)

func (s *Store) AddPersonContactPointContext(
	ctx context.Context, personID int64, input PersonContactPointInput,
) (*PersonContactPoint, error) {
	if !input.AddressKind.Valid() {
		return nil, ErrInvalidContactAddressKind
	}
	if err := input.Envelope.Validate(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(input.OriginalValue) == "" {
		return nil, ErrContactPointValueMissing
	}
	service, err := s.resolveOptionalCommunicationServiceContext(ctx, input.ServiceSlug)
	if err != nil {
		return nil, err
	}
	if err := ValidateServiceScope(service, input.ScopeKind, input.ScopeValue); err != nil {
		return nil, err
	}
	normalized, err := NormalizeServiceValue(service, input.AddressKind, input.OriginalValue)
	if err != nil {
		return nil, err
	}
	normalization := fallbackContactNormalization(input.AddressKind)
	normalizationVersion := 1
	var serviceID any
	if service != nil {
		serviceID = service.ID
		normalization = service.Normalization
		normalizationVersion = service.NormalizationVersion
	}

	var result *PersonContactPoint
	err = s.withTxContext(ctx, func(tx *loggedTx) error {
		if err := s.lockIdentityMutationTxContext(ctx, tx); err != nil {
			return err
		}
		var exists int
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM persons WHERE id = ?`, personID,
		).Scan(&exists); err != nil {
			return fmt.Errorf("check person: %w", err)
		}
		if exists == 0 {
			return ErrPersonNotFound
		}
		env := input.Envelope
		if env.Ordinal == 0 {
			if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(ordinal) + 1, 0)
				FROM person_contact_points
				WHERE person_id = ? AND address_kind = ?
				  AND active_until IS NULL AND superseded_at IS NULL`,
				personID, input.AddressKind,
			).Scan(&env.Ordinal); err != nil {
				return fmt.Errorf("choose contact-point ordinal: %w", err)
			}
		}
		args := []any{
			personID, input.AddressKind, serviceID, stringValue(input.ScopeKind),
			stringValue(input.ScopeValue), input.OriginalValue, normalized,
			normalization, normalizationVersion, stringValue(input.URI),
		}
		args = append(args, profileEnvelopeArgs(env)...)
		var id int64
		if err := tx.QueryRowContext(ctx, `INSERT INTO person_contact_points (
			person_id, address_kind, service_id, scope_kind, scope_value,
			original_value, normalized_value, normalization,
			normalization_version, uri, `+profileEnvelopeWriteColumns+`,
			created_at, updated_at
		) VALUES (
			?, ?, ?, ?, ?, ?, ?, ?, ?, ?,
			?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?,
			`+s.dialect.Now()+`, `+s.dialect.Now()+`
		) RETURNING id`, args...).Scan(&id); err != nil {
			return fmt.Errorf(
				"add person contact point property=%q prop_id=%v: %w",
				env.VCard.Property, env.VCard.PropID, err,
			)
		}
		if err := s.bumpPersonRevisionsTx(ctx, tx, personID); err != nil {
			return err
		}
		result, err = getPersonContactPointTx(ctx, tx, personID, id)
		return err
	})
	return result, err
}

func (s *Store) ListPersonContactPointsContext(
	ctx context.Context, personID int64, currentOnly bool,
) ([]PersonContactPoint, error) {
	query := personContactPointSelect + ` WHERE p.person_id = ?`
	if currentOnly {
		query += ` AND p.active_until IS NULL AND p.superseded_at IS NULL`
	}
	query += ` ORDER BY p.address_kind,
		CASE WHEN p.pref IS NULL THEN 1 ELSE 0 END, p.pref, p.ordinal, p.id`
	rows, err := s.db.QueryContext(ctx, query, personID)
	if err != nil {
		return nil, fmt.Errorf("list person contact points: %w", err)
	}
	defer func() { _ = rows.Close() }()
	points := make([]PersonContactPoint, 0)
	for rows.Next() {
		point, err := scanPersonContactPoint(rows)
		if err != nil {
			return nil, fmt.Errorf("scan person contact point: %w", err)
		}
		points = append(points, *point)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list person contact points: %w", err)
	}
	return points, nil
}

func (s *Store) FindPersonContactPointsContext(
	ctx context.Context, query ContactPointQuery,
) ([]PersonContactPoint, error) {
	if !query.AddressKind.Valid() {
		return nil, ErrInvalidContactAddressKind
	}
	service, err := s.resolveOptionalCommunicationServiceContext(ctx, query.ServiceSlug)
	if err != nil {
		return nil, err
	}
	var serviceID any
	if service != nil {
		serviceID = service.ID
	}
	rows, err := s.db.QueryContext(ctx, personContactPointSelect+`
		WHERE p.address_kind = ?
		  AND (p.service_id = ? OR (p.service_id IS NULL AND CAST(? AS BIGINT) IS NULL))
		  AND (p.scope_kind = ? OR (p.scope_kind IS NULL AND CAST(? AS TEXT) IS NULL))
		  AND (p.scope_value = ? OR (p.scope_value IS NULL AND CAST(? AS TEXT) IS NULL))
		  AND p.normalized_value = ?
		  AND p.active_until IS NULL AND p.superseded_at IS NULL
		ORDER BY p.person_id, p.id`,
		query.AddressKind,
		serviceID, serviceID,
		stringValue(query.ScopeKind), stringValue(query.ScopeKind),
		stringValue(query.ScopeValue), stringValue(query.ScopeValue),
		query.NormalizedValue,
	)
	if err != nil {
		return nil, fmt.Errorf("find person contact points: %w", err)
	}
	defer func() { _ = rows.Close() }()
	points := make([]PersonContactPoint, 0)
	for rows.Next() {
		point, err := scanPersonContactPoint(rows)
		if err != nil {
			return nil, fmt.Errorf("scan person contact point: %w", err)
		}
		points = append(points, *point)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("find person contact points: %w", err)
	}
	return points, nil
}

func (s *Store) SupersedePersonContactPointContext(
	ctx context.Context, personID, contactPointID int64, activeUntil *time.Time,
) error {
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		if err := s.lockIdentityMutationTxContext(ctx, tx); err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, `UPDATE person_contact_points
			SET active_until = COALESCE(?, `+s.dialect.Now()+`),
			    superseded_at = `+s.dialect.Now()+`,
			    updated_at = `+s.dialect.Now()+`
			WHERE id = ? AND person_id = ?
			  AND active_until IS NULL AND superseded_at IS NULL`,
			timeValue(activeUntil), contactPointID, personID,
		)
		if err != nil {
			return fmt.Errorf("supersede person contact point: %w", err)
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("check superseded person contact point: %w", err)
		}
		if changed == 0 {
			return ErrProfileValueNotFound
		}
		return s.bumpPersonRevisionsTx(ctx, tx, personID)
	})
}

func fallbackContactNormalization(kind ContactAddressKind) string {
	switch kind {
	case ContactAddressEmail:
		return NormalizationEmail
	case ContactAddressPhone:
		return NormalizationPhoneE164
	case ContactAddressLanguage:
		return NormalizationLower
	default:
		return NormalizationNone
	}
}

func (s *Store) resolveOptionalCommunicationServiceContext(
	ctx context.Context, slug *string,
) (*CommunicationService, error) {
	if slug == nil || strings.TrimSpace(*slug) == "" {
		return nil, nil //nolint:nilnil // No slug means the optional service is absent.
	}
	return s.ResolveCommunicationServiceContext(ctx, *slug)
}

const personContactPointSelect = `SELECT
	p.id, p.person_id, p.address_kind, cs.slug, p.scope_kind, p.scope_value,
	p.original_value, p.normalized_value, p.normalization,
	p.normalization_version, p.uri,
	p.pref, p.ordinal, p.type_label, p.type_tokens, p.vcard_property,
	p.vcard_group, p.vcard_prop_id, p.vcard_pid, p.vcard_altid, p.source,
	p.source_ref, p.confidence, p.active_from, p.active_until,
	p.created_at, p.updated_at, p.superseded_at
	FROM person_contact_points p
	LEFT JOIN communication_services cs ON cs.id = p.service_id`

func getPersonContactPointTx(
	ctx context.Context, tx *loggedTx, personID, id int64,
) (*PersonContactPoint, error) {
	point, err := scanPersonContactPoint(tx.QueryRowContext(ctx,
		personContactPointSelect+` WHERE p.person_id = ? AND p.id = ?`,
		personID, id,
	))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrProfileValueNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get person contact point: %w", err)
	}
	return point, nil
}

func scanPersonContactPoint(row scanner) (*PersonContactPoint, error) {
	var point PersonContactPoint
	var serviceSlug, scopeKind, scopeValue, uri sql.NullString
	var pref, ordinal sql.NullInt64
	var typeLabel, typeTokens, property, group, propID, pid, altID sql.NullString
	var source, sourceRef sql.NullString
	var confidence sql.NullFloat64
	var activeFrom, activeUntil, createdAt, updatedAt, supersededAt sql.NullTime
	if err := row.Scan(
		&point.Envelope.ID, &point.PersonID, &point.AddressKind, &serviceSlug,
		&scopeKind, &scopeValue, &point.OriginalValue, &point.NormalizedValue,
		&point.Normalization, &point.NormalizationVersion, &uri,
		&pref, &ordinal, &typeLabel, &typeTokens, &property, &group,
		&propID, &pid, &altID, &source, &sourceRef, &confidence,
		&activeFrom, &activeUntil, &createdAt, &updatedAt, &supersededAt,
	); err != nil {
		return nil, err
	}
	point.ServiceSlug = nullStringPtr(serviceSlug)
	point.ScopeKind = nullStringPtr(scopeKind)
	point.ScopeValue = nullStringPtr(scopeValue)
	point.URI = nullStringPtr(uri)
	point.Envelope.Pref = nullIntPtr(pref)
	if ordinal.Valid {
		point.Envelope.Ordinal = int(ordinal.Int64)
	}
	point.Envelope.TypeLabel = nullStringPtr(typeLabel)
	point.Envelope.TypeTokens = splitTypeTokens(nullStringPtr(typeTokens))
	point.Envelope.VCard = VCardIdentity{
		Property: property.String,
		Group:    nullStringPtr(group),
		PropID:   nullStringPtr(propID),
		PID:      splitTypeTokens(nullStringPtr(pid)),
		AltID:    nullStringPtr(altID),
	}
	point.Envelope.Source = Provenance(source.String)
	point.Envelope.SourceRef = nullStringPtr(sourceRef)
	point.Envelope.Confidence = nullFloatPtr(confidence)
	point.Envelope.ActiveFrom = nullTimePtr(activeFrom)
	point.Envelope.ActiveUntil = nullTimePtr(activeUntil)
	var err error
	point.Envelope.CreatedAt, err = requireNullTime(createdAt, "created_at")
	if err != nil {
		return nil, err
	}
	point.Envelope.UpdatedAt, err = requireNullTime(updatedAt, "updated_at")
	if err != nil {
		return nil, err
	}
	point.Envelope.SupersededAt = nullTimePtr(supersededAt)
	return &point, nil
}
