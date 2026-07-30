package store_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestOrganizationChildTablesMirrorPersonChildTables(t *testing.T) {
	st := testutil.NewTestStore(t)
	pairs := []struct {
		person       string
		organization string
	}{
		{person: "person_names", organization: "organization_names"},
		{person: "person_addresses", organization: "organization_addresses"},
		{person: "person_contact_points", organization: "organization_contact_points"},
		{person: "person_media", organization: "organization_media"},
		{person: "person_categories", organization: "organization_categories"},
		{
			person:       "person_attribute_values",
			organization: "organization_attribute_values",
		},
	}
	for _, pair := range pairs {
		t.Run(pair.organization, func(t *testing.T) {
			assert := assert.New(t)
			personColumns := tableColumns(t, st, pair.person)
			organizationColumns := tableColumns(t, st, pair.organization)
			assert.Contains(organizationColumns, "organization_id")
			assert.NotContains(organizationColumns, "person_id")
			for _, column := range personColumns {
				if column != "person_id" {
					assert.Contains(organizationColumns, column)
				}
			}
		})
	}
}

func TestPersonsHasNoDenormalizedCompanyOrTitle(t *testing.T) {
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	columns := tableColumns(t, st, "persons")
	for _, forbidden := range []string{
		"company", "company_id", "organization_id", "job_title", "title", "role", "department",
	} {
		assert.NotContains(columns, forbidden)
	}
}

var organizationProfileEnvelopeColumnNames = []string{
	"pref", "ordinal", "type_label", "type_tokens",
	"vcard_property", "vcard_group", "vcard_prop_id", "vcard_pid", "vcard_altid",
	"source", "source_ref", "confidence",
	"active_from", "active_until", "created_at", "updated_at", "superseded_at",
}

func TestOrganizationChildTablesCarryTheFullEnvelope(t *testing.T) {
	st := testutil.NewTestStore(t)
	for _, table := range []string{
		"organization_names", "organization_identifiers", "organization_addresses",
		"organization_contact_points", "organization_media", "organization_categories",
	} {
		t.Run(table, func(t *testing.T) {
			assert := assert.New(t)
			columns := tableColumns(t, st, table)
			for _, column := range organizationProfileEnvelopeColumnNames {
				assert.Contains(columns, column)
			}
		})
	}
}

func tableColumns(t *testing.T, st *store.Store, table string) []string {
	t.Helper()
	rows, err := st.DB().QueryContext(context.Background(),
		"SELECT * FROM "+table+" WHERE 1 = 0")
	require.NoError(t, err, "describe %s", table)
	defer func() { _ = rows.Close() }()
	columns, err := rows.Columns()
	require.NoError(t, err, "columns of %s", table)
	require.NoError(t, rows.Err(), "iterate %s", table)
	return columns
}
