package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

type PersonProfile struct {
	Person        Person               `json:"person"`
	Names         []PersonName         `json:"names"`
	ContactPoints []PersonContactPoint `json:"contact_points"`
	Addresses     []PersonAddress      `json:"addresses"`
	Dates         []PersonDate         `json:"dates"`
	Categories    []PersonCategory     `json:"categories"`
	Media         []PersonMedia        `json:"media"`
}

type PersonProfilePatch struct {
	Names         *PersonNamePatch         `json:"names,omitempty"`
	ContactPoints *PersonContactPointPatch `json:"contact_points,omitempty"`
	Addresses     *PersonAddressPatch      `json:"addresses,omitempty"`
	Dates         *PersonDatePatch         `json:"dates,omitempty"`
	Categories    *PersonCategoryPatch     `json:"categories,omitempty"`
	Media         *PersonMediaPatch        `json:"media,omitempty"`
}

type PersonNamePatch struct {
	Add       []PersonNameInput `json:"add,omitempty"`
	Supersede []int64           `json:"supersede,omitempty"`
}

type PersonContactPointPatch struct {
	Add       []PersonContactPointInput `json:"add,omitempty"`
	Supersede []int64                   `json:"supersede,omitempty"`
}

type PersonAddressPatch struct {
	Add       []PersonAddressInput `json:"add,omitempty"`
	Supersede []int64              `json:"supersede,omitempty"`
}

type PersonDatePatch struct {
	Add       []PersonDateInput `json:"add,omitempty"`
	Supersede []int64           `json:"supersede,omitempty"`
}

type PersonCategoryPatch struct {
	Add       []PersonCategoryInput `json:"add,omitempty"`
	Supersede []int64               `json:"supersede,omitempty"`
}

type PersonMediaPatch struct {
	Add       []PersonMediaInput `json:"add,omitempty"`
	Supersede []int64            `json:"supersede,omitempty"`
}

type PersonProfileHistory struct {
	Person        Person                          `json:"person"`
	Names         []PersonName                    `json:"names"`
	ContactPoints []PersonContactPoint            `json:"contact_points"`
	Addresses     []PersonAddress                 `json:"addresses"`
	Dates         []PersonDate                    `json:"dates"`
	Categories    []PersonCategory                `json:"categories"`
	Media         []PersonMedia                   `json:"media"`
	Observations  []ParticipantContactObservation `json:"observations"`
}

const MaxPersonProfilePatchOperations = 200

var (
	ErrPersonProfilePatchTooLarge = errors.New("person profile patch exceeds the operation limit")
	ErrPersonProfilePatchEmpty    = errors.New("person profile patch contains no operations")
)

func (s *Store) GetPersonProfileContext(
	ctx context.Context, personID int64,
) (*PersonProfile, error) {
	var profile *PersonProfile
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		var err error
		profile, err = s.getPersonProfileTx(ctx, tx, personID, true)
		return err
	})
	return profile, err
}

func (s *Store) ApplyPersonProfilePatchContext(
	ctx context.Context,
	personID, expectedRevision int64,
	patch PersonProfilePatch,
) (*PersonProfile, error) {
	operations := countPersonProfilePatchOperations(patch)
	if operations == 0 {
		return nil, ErrPersonProfilePatchEmpty
	}
	if operations > MaxPersonProfilePatchOperations {
		return nil, ErrPersonProfilePatchTooLarge
	}
	var profile *PersonProfile
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		if err := s.lockIdentityMutationTxContext(ctx, tx); err != nil {
			return err
		}
		var updatedID int64
		err := tx.QueryRowContext(ctx, `UPDATE persons
			SET revision = revision + 1, updated_at = `+s.dialect.Now()+`
			WHERE id = ? AND revision = ? RETURNING id`,
			personID, expectedRevision,
		).Scan(&updatedID)
		if errors.Is(err, sql.ErrNoRows) {
			return s.personCASMissTx(ctx, tx, personID)
		}
		if err != nil {
			return fmt.Errorf("update person profile revision: %w", err)
		}
		if err := s.applyPersonProfilePatchTx(ctx, tx, personID, patch); err != nil {
			return err
		}
		profile, err = s.getPersonProfileTx(ctx, tx, updatedID, true)
		return err
	})
	return profile, err
}

func (s *Store) GetPersonProfileHistoryContext(
	ctx context.Context, personID int64,
) (*PersonProfileHistory, error) {
	var history *PersonProfileHistory
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		profile, err := s.getPersonProfileTx(ctx, tx, personID, false)
		if err != nil {
			return err
		}
		observations, err := s.listObservationsForPersonTx(ctx, tx, personID)
		if err != nil {
			return err
		}
		history = &PersonProfileHistory{
			Person: profile.Person, Names: profile.Names,
			ContactPoints: profile.ContactPoints, Addresses: profile.Addresses,
			Dates: profile.Dates, Categories: profile.Categories,
			Media: profile.Media, Observations: observations,
		}
		return nil
	})
	return history, err
}

func countPersonProfilePatchOperations(patch PersonProfilePatch) int {
	count := 0
	if patch.Names != nil {
		count += len(patch.Names.Add) + len(patch.Names.Supersede)
	}
	if patch.ContactPoints != nil {
		count += len(patch.ContactPoints.Add) + len(patch.ContactPoints.Supersede)
	}
	if patch.Addresses != nil {
		count += len(patch.Addresses.Add) + len(patch.Addresses.Supersede)
	}
	if patch.Dates != nil {
		count += len(patch.Dates.Add) + len(patch.Dates.Supersede)
	}
	if patch.Categories != nil {
		count += len(patch.Categories.Add) + len(patch.Categories.Supersede)
	}
	if patch.Media != nil {
		count += len(patch.Media.Add) + len(patch.Media.Supersede)
	}
	return count
}

func (s *Store) applyPersonProfilePatchTx(
	ctx context.Context,
	tx *loggedTx,
	personID int64,
	patch PersonProfilePatch,
) error {
	if patch.Names != nil {
		for _, id := range patch.Names.Supersede {
			if err := s.supersedePersonNameTx(ctx, tx, personID, id, nil); err != nil {
				return err
			}
		}
		for _, input := range patch.Names.Add {
			if _, err := s.addPersonNameTx(ctx, tx, personID, input); err != nil {
				return err
			}
		}
	}
	if patch.ContactPoints != nil {
		for _, id := range patch.ContactPoints.Supersede {
			if err := s.supersedePersonContactPointTx(ctx, tx, personID, id, nil); err != nil {
				return err
			}
		}
		for _, input := range patch.ContactPoints.Add {
			if _, err := s.addPersonContactPointTx(ctx, tx, personID, input); err != nil {
				return err
			}
		}
	}
	if patch.Addresses != nil {
		for _, id := range patch.Addresses.Supersede {
			if err := s.supersedePersonAddressTx(ctx, tx, personID, id, nil); err != nil {
				return err
			}
		}
		for _, input := range patch.Addresses.Add {
			if _, err := s.addPersonAddressTx(ctx, tx, personID, input); err != nil {
				return err
			}
		}
	}
	if patch.Dates != nil {
		for _, id := range patch.Dates.Supersede {
			if err := s.supersedePersonDateTx(ctx, tx, personID, id, nil); err != nil {
				return err
			}
		}
		for _, input := range patch.Dates.Add {
			if _, err := s.addPersonDateTx(ctx, tx, personID, input); err != nil {
				return err
			}
		}
	}
	if patch.Categories != nil {
		for _, id := range patch.Categories.Supersede {
			if err := s.supersedePersonCategoryTx(ctx, tx, personID, id, nil); err != nil {
				return err
			}
		}
		for _, input := range patch.Categories.Add {
			if _, err := s.addPersonCategoryTx(ctx, tx, personID, input); err != nil {
				return err
			}
		}
	}
	if patch.Media != nil {
		for _, id := range patch.Media.Supersede {
			if err := s.supersedePersonMediaTx(ctx, tx, personID, id, nil); err != nil {
				return err
			}
		}
		for _, input := range patch.Media.Add {
			if _, err := s.addPersonMediaTx(ctx, tx, personID, input); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Store) supersedeProfileValueTx(
	ctx context.Context,
	tx *loggedTx,
	table string,
	personID, valueID int64,
	activeUntil *time.Time,
) error {
	query := fmt.Sprintf(`UPDATE %s
		SET active_until = COALESCE(?, %s), superseded_at = %s, updated_at = %s
		WHERE id = ? AND person_id = ?
		  AND active_until IS NULL AND superseded_at IS NULL`,
		table, s.dialect.Now(), s.dialect.Now(), s.dialect.Now(),
	)
	result, err := tx.ExecContext(ctx, query, timeValue(activeUntil), valueID, personID)
	if err != nil {
		return fmt.Errorf("supersede %s value: %w", table, err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed == 0 {
		return ErrProfileValueNotFound
	}
	return nil
}

func (s *Store) supersedePersonNameTx(
	ctx context.Context, tx *loggedTx, personID, nameID int64, activeUntil *time.Time,
) error {
	return s.supersedeProfileValueTx(
		ctx, tx, "person_names", personID, nameID, activeUntil,
	)
}

func (s *Store) supersedePersonContactPointTx(
	ctx context.Context,
	tx *loggedTx,
	personID, contactPointID int64,
	activeUntil *time.Time,
) error {
	return s.supersedeProfileValueTx(
		ctx, tx, "person_contact_points", personID, contactPointID, activeUntil,
	)
}

func (s *Store) supersedePersonAddressTx(
	ctx context.Context, tx *loggedTx, personID, addressID int64, activeUntil *time.Time,
) error {
	return s.supersedeProfileValueTx(
		ctx, tx, "person_addresses", personID, addressID, activeUntil,
	)
}

func (s *Store) supersedePersonDateTx(
	ctx context.Context, tx *loggedTx, personID, dateID int64, activeUntil *time.Time,
) error {
	return s.supersedeProfileValueTx(
		ctx, tx, "person_dates", personID, dateID, activeUntil,
	)
}

func (s *Store) supersedePersonCategoryTx(
	ctx context.Context, tx *loggedTx, personID, categoryID int64, activeUntil *time.Time,
) error {
	return s.supersedeProfileValueTx(
		ctx, tx, "person_categories", personID, categoryID, activeUntil,
	)
}

func (s *Store) supersedePersonMediaTx(
	ctx context.Context, tx *loggedTx, personID, mediaID int64, activeUntil *time.Time,
) error {
	return s.supersedeProfileValueTx(
		ctx, tx, "person_media", personID, mediaID, activeUntil,
	)
}

func (s *Store) getPersonProfileTx(
	ctx context.Context, tx *loggedTx, personID int64, currentOnly bool,
) (*PersonProfile, error) {
	person, err := s.getPersonTx(ctx, tx, personID)
	if err != nil {
		return nil, err
	}
	profile := &PersonProfile{Person: *person}
	if profile.Names, err = s.listPersonNamesTx(ctx, tx, personID, currentOnly); err != nil {
		return nil, err
	}
	if profile.ContactPoints, err = s.listPersonContactPointsTx(ctx, tx, personID, currentOnly); err != nil {
		return nil, err
	}
	if profile.Addresses, err = s.listPersonAddressesTx(ctx, tx, personID, currentOnly); err != nil {
		return nil, err
	}
	if profile.Dates, err = s.listPersonDatesTx(ctx, tx, personID, currentOnly); err != nil {
		return nil, err
	}
	if profile.Categories, err = s.listPersonCategoriesTx(ctx, tx, personID, currentOnly); err != nil {
		return nil, err
	}
	if profile.Media, err = s.listPersonMediaTx(ctx, tx, personID, currentOnly); err != nil {
		return nil, err
	}
	return profile, nil
}

func queryProfileRowsTx[T any](
	ctx context.Context,
	tx *loggedTx,
	query string,
	scan func(scanner) (*T, error),
	args ...any,
) ([]T, error) {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	values := make([]T, 0)
	for rows.Next() {
		value, err := scan(rows)
		if err != nil {
			return nil, err
		}
		values = append(values, *value)
	}
	return values, rows.Err()
}

func (s *Store) listPersonNamesTx(
	ctx context.Context, tx *loggedTx, personID int64, currentOnly bool,
) ([]PersonName, error) {
	query := personNameSelect + ` WHERE person_id = ?`
	if currentOnly {
		query += currentProfileValueFilter
	}
	query += ` ORDER BY name_kind,
		CASE WHEN pref IS NULL THEN 1 ELSE 0 END, pref, ordinal, id`
	return queryProfileRowsTx(ctx, tx, query, scanPersonName, personID)
}

func (s *Store) listPersonContactPointsTx(
	ctx context.Context, tx *loggedTx, personID int64, currentOnly bool,
) ([]PersonContactPoint, error) {
	query := personContactPointSelect + ` WHERE p.person_id = ?`
	if currentOnly {
		query += ` AND p.active_until IS NULL AND p.superseded_at IS NULL`
	}
	query += ` ORDER BY p.address_kind,
		CASE WHEN p.pref IS NULL THEN 1 ELSE 0 END, p.pref, p.ordinal, p.id`
	return queryProfileRowsTx(ctx, tx, query, scanPersonContactPoint, personID)
}

func (s *Store) listPersonAddressesTx(
	ctx context.Context, tx *loggedTx, personID int64, currentOnly bool,
) ([]PersonAddress, error) {
	query := personAddressSelect + ` WHERE person_id = ?`
	if currentOnly {
		query += currentProfileValueFilter
	}
	query += ` ORDER BY address_kind,
		CASE WHEN pref IS NULL THEN 1 ELSE 0 END, pref, ordinal, id`
	return queryProfileRowsTx(ctx, tx, query, scanPersonAddress, personID)
}

func (s *Store) listPersonDatesTx(
	ctx context.Context, tx *loggedTx, personID int64, currentOnly bool,
) ([]PersonDate, error) {
	query := personDateSelect + ` WHERE person_id = ?`
	if currentOnly {
		query += currentProfileValueFilter
	}
	query += ` ORDER BY date_kind,
		CASE WHEN pref IS NULL THEN 1 ELSE 0 END, pref, ordinal, id`
	return queryProfileRowsTx(ctx, tx, query, scanPersonDate, personID)
}

func (s *Store) listPersonCategoriesTx(
	ctx context.Context, tx *loggedTx, personID int64, currentOnly bool,
) ([]PersonCategory, error) {
	query := personCategorySelect + ` WHERE person_id = ?`
	if currentOnly {
		query += currentProfileValueFilter
	}
	query += ` ORDER BY normalized_value,
		CASE WHEN pref IS NULL THEN 1 ELSE 0 END, pref, ordinal, id`
	return queryProfileRowsTx(ctx, tx, query, scanPersonCategory, personID)
}

func (s *Store) listPersonMediaTx(
	ctx context.Context, tx *loggedTx, personID int64, currentOnly bool,
) ([]PersonMedia, error) {
	query := personMediaSelect + ` WHERE person_id = ?`
	if currentOnly {
		query += currentProfileValueFilter
	}
	query += ` ORDER BY media_kind,
		CASE WHEN pref IS NULL THEN 1 ELSE 0 END, pref, ordinal, id`
	return queryProfileRowsTx(ctx, tx, query, scanPersonMedia, personID)
}

func (s *Store) listObservationsForPersonTx(
	ctx context.Context, tx *loggedTx, personID int64,
) ([]ParticipantContactObservation, error) {
	query := participantObservationSelect + `
		WHERE EXISTS (
			SELECT 1 FROM person_participants pp
			WHERE pp.participant_id = o.participant_id AND pp.person_id = ?
		)
		ORDER BY o.participant_id, o.id`
	return queryProfileRowsTx(ctx, tx, query, scanParticipantObservation, personID)
}

func (s *Store) addPersonNameTx(
	ctx context.Context, tx *loggedTx, personID int64, input PersonNameInput,
) (*PersonName, error) {
	if !input.NameKind.Valid() {
		return nil, ErrInvalidPersonNameKind
	}
	if err := input.Envelope.Validate(); err != nil {
		return nil, err
	}
	original := strings.TrimSpace(input.OriginalValue)
	if original == "" {
		original = firstNonBlankNameComponent(input)
	}
	if original == "" {
		return nil, ErrPersonNameValueMissing
	}
	env := input.Envelope
	if env.Ordinal == 0 {
		var err error
		env.Ordinal, err = nextProfileOrdinalTx(
			ctx, tx, "person_names", "name_kind", personID, input.NameKind,
		)
		if err != nil {
			return nil, err
		}
	}
	args := []any{
		personID, input.NameKind, stringValue(input.Formatted),
		stringValue(input.FamilyName), stringValue(input.GivenName),
		stringValue(input.AdditionalNames), stringValue(input.HonorificPrefixes),
		stringValue(input.HonorificSuffixes), stringValue(input.SecondarySurname),
		stringValue(input.Generation), stringValue(input.Language),
		stringValue(input.Script), stringValue(input.PhoneticSystem),
		stringValue(input.PhoneticScript), stringValue(input.SortAs),
		input.IsDerived, original,
	}
	args = append(args, profileEnvelopeArgs(env)...)
	var id int64
	if err := tx.QueryRowContext(ctx, `INSERT INTO person_names (
		person_id, name_kind, formatted, family_name, given_name,
		additional_names, honorific_prefixes, honorific_suffixes,
		secondary_surname, generation, language, script, phonetic_system,
		phonetic_script, sort_as, is_derived, original_value, `+
		profileEnvelopeWriteColumns+`, created_at, updated_at
	) VALUES (
		?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?,
		?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?,
		`+s.dialect.Now()+`, `+s.dialect.Now()+`
	) RETURNING id`, args...).Scan(&id); err != nil {
		return nil, fmt.Errorf("add person name: %w", err)
	}
	return getPersonNameTx(ctx, tx, personID, id)
}

func (s *Store) addPersonContactPointTx(
	ctx context.Context,
	tx *loggedTx,
	personID int64,
	input PersonContactPointInput,
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
	service, err := resolveCommunicationServiceTx(ctx, tx, input.ServiceSlug)
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
	normalization, version := fallbackContactNormalization(input.AddressKind), 1
	var serviceID any
	if service != nil {
		serviceID, normalization, version = service.ID, service.Normalization, service.NormalizationVersion
	}
	env := input.Envelope
	if env.Ordinal == 0 {
		env.Ordinal, err = nextProfileOrdinalTx(
			ctx, tx, "person_contact_points", "address_kind", personID, input.AddressKind,
		)
		if err != nil {
			return nil, err
		}
	}
	args := []any{
		personID, input.AddressKind, serviceID, stringValue(input.ScopeKind),
		stringValue(input.ScopeValue), input.OriginalValue, normalized,
		normalization, version, stringValue(input.URI),
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
		return nil, fmt.Errorf("add person contact point: %w", err)
	}
	return getPersonContactPointTx(ctx, tx, personID, id)
}

func (s *Store) addPersonAddressTx(
	ctx context.Context, tx *loggedTx, personID int64, input PersonAddressInput,
) (*PersonAddress, error) {
	if !input.AddressKind.Valid() {
		return nil, ErrInvalidPersonAddressKind
	}
	if err := input.Envelope.Validate(); err != nil {
		return nil, err
	}
	if !personAddressHasValue(input) {
		return nil, ErrPersonAddressValueMissing
	}
	original := strings.TrimSpace(input.OriginalValue)
	if original == "" {
		original = strings.Join([]string{
			derefString(input.PostOfficeBox), derefString(input.ExtendedAddress),
			derefString(input.StreetAddress), derefString(input.Locality),
			derefString(input.Region), derefString(input.PostalCode),
			derefString(input.CountryName),
		}, ";")
	}
	env := input.Envelope
	var err error
	if env.Ordinal == 0 {
		env.Ordinal, err = nextProfileOrdinalTx(
			ctx, tx, "person_addresses", "address_kind", personID, input.AddressKind,
		)
		if err != nil {
			return nil, err
		}
	}
	args := []any{
		personID, input.AddressKind, stringValue(input.PostOfficeBox),
		stringValue(input.ExtendedAddress), stringValue(input.StreetAddress),
		stringValue(input.Locality), stringValue(input.Region),
		stringValue(input.PostalCode), stringValue(input.CountryName),
		stringValue(input.ExtendedComponents), stringValue(input.FreeText),
		stringValue(input.Label), stringValue(input.GeoURI),
		stringValue(input.Timezone), stringValue(input.CountryCode),
		stringValue(input.PlaceURI), original,
	}
	args = append(args, profileEnvelopeArgs(env)...)
	var id int64
	if err := tx.QueryRowContext(ctx, `INSERT INTO person_addresses (
		person_id, address_kind, post_office_box, extended_address,
		street_address, locality, region, postal_code, country_name,
		extended_components, free_text, label, geo_uri, timezone,
		country_code, place_uri, original_value, `+profileEnvelopeWriteColumns+`,
		created_at, updated_at
	) VALUES (
		?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?,
		?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?,
		`+s.dialect.Now()+`, `+s.dialect.Now()+`
	) RETURNING id`, args...).Scan(&id); err != nil {
		return nil, fmt.Errorf("add person address: %w", err)
	}
	return getPersonAddressTx(ctx, tx, personID, id)
}

func (s *Store) addPersonDateTx(
	ctx context.Context, tx *loggedTx, personID int64, input PersonDateInput,
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
		if input.Date.IsZero() {
			original = strings.TrimSpace(*input.DateText)
		} else {
			original = input.Date.String()
		}
	}
	env := input.Envelope
	var err error
	if env.Ordinal == 0 {
		env.Ordinal, err = nextProfileOrdinalTx(
			ctx, tx, "person_dates", "date_kind", personID, input.DateKind,
		)
		if err != nil {
			return nil, err
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
		return nil, fmt.Errorf("add person date: %w", err)
	}
	return getPersonDateTx(ctx, tx, personID, id)
}

func (s *Store) addPersonCategoryTx(
	ctx context.Context, tx *loggedTx, personID int64, input PersonCategoryInput,
) (*PersonCategory, error) {
	if err := input.Envelope.Validate(); err != nil {
		return nil, err
	}
	original := strings.TrimSpace(input.OriginalValue)
	if original == "" {
		return nil, ErrPersonCategoryEmpty
	}
	normalized := strings.ToLower(original)
	var duplicate int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM person_categories
		WHERE person_id = ? AND normalized_value = ?
		  AND active_until IS NULL AND superseded_at IS NULL`,
		personID, normalized,
	).Scan(&duplicate); err != nil {
		return nil, err
	}
	if duplicate > 0 {
		return nil, ErrPersonCategoryDuplicate
	}
	env := input.Envelope
	if env.Ordinal == 0 {
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(ordinal) + 1, 0)
			FROM person_categories WHERE person_id = ?
			  AND active_until IS NULL AND superseded_at IS NULL`,
			personID,
		).Scan(&env.Ordinal); err != nil {
			return nil, err
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
		return nil, fmt.Errorf("add person category: %w", err)
	}
	return getPersonCategoryTx(ctx, tx, personID, id)
}

func (s *Store) addPersonMediaTx(
	ctx context.Context, tx *loggedTx, personID int64, input PersonMediaInput,
) (*PersonMedia, error) {
	if !input.MediaKind.Valid() {
		return nil, ErrInvalidPersonMediaKind
	}
	if err := input.Envelope.Validate(); err != nil {
		return nil, err
	}
	hasURI := input.URI != nil && strings.TrimSpace(*input.URI) != ""
	if len(input.Data) == 0 && !hasURI {
		return nil, ErrPersonMediaEmpty
	}
	if len(input.Data) > MaxPersonMediaBytes {
		return nil, ErrPersonMediaTooLarge
	}
	original := input.OriginalValue
	if strings.TrimSpace(original) == "" && hasURI {
		original = strings.TrimSpace(*input.URI)
	}
	var data, byteSize, contentHash any
	if len(input.Data) > 0 {
		digest := sha256.Sum256(input.Data)
		data, byteSize = input.Data, int64(len(input.Data))
		contentHash = hex.EncodeToString(digest[:])
	}
	env := input.Envelope
	var err error
	if env.Ordinal == 0 {
		env.Ordinal, err = nextProfileOrdinalTx(
			ctx, tx, "person_media", "media_kind", personID, input.MediaKind,
		)
		if err != nil {
			return nil, err
		}
	}
	args := []any{
		personID, input.MediaKind, stringValue(input.MediaType),
		stringValue(input.URI), data, byteSize, contentHash, original,
	}
	args = append(args, profileEnvelopeArgs(env)...)
	var id int64
	if err := tx.QueryRowContext(ctx, `INSERT INTO person_media (
		person_id, media_kind, media_type, uri, data, byte_size,
		content_hash, original_value, `+profileEnvelopeWriteColumns+`,
		created_at, updated_at
	) VALUES (
		?, ?, ?, ?, ?, ?, ?, ?,
		?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?,
		`+s.dialect.Now()+`, `+s.dialect.Now()+`
	) RETURNING id`, args...).Scan(&id); err != nil {
		return nil, fmt.Errorf("add person media: %w", err)
	}
	return getPersonMediaTx(ctx, tx, personID, id)
}

func resolveCommunicationServiceTx(
	ctx context.Context, tx *loggedTx, slug *string,
) (*CommunicationService, error) {
	if slug == nil || strings.TrimSpace(*slug) == "" {
		return nil, nil //nolint:nilnil // No slug means the optional service is absent.
	}
	lookup := strings.ToLower(strings.TrimSpace(*slug))
	service, err := scanCommunicationService(tx.QueryRowContext(ctx, serviceSelect+`
		WHERE slug = ? OR id = (
			SELECT service_id FROM communication_service_aliases WHERE alias = ?
		)
		ORDER BY CASE WHEN slug = ? THEN 0 ELSE 1 END
		LIMIT 1`, lookup, lookup, lookup))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrServiceNotFound
	}
	if err != nil {
		return nil, err
	}
	service.Aliases, err = loadServiceAliasesTx(ctx, tx, service.ID)
	return service, err
}
