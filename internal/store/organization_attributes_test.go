package store_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func organizationTextDefinition(slug string) store.AttributeDefinitionInput {
	input := personTextDefinition(slug)
	input.UniversalID = "test-org-" + slug
	input.ObjectType = store.AttributeObjectOrganization
	return input
}

func textAttributeValue(value string) store.AttributeValue {
	return store.AttributeValue{Type: store.AttributeValueText, Text: &value}
}

func mustOrganizationAttributeDefinition(
	t *testing.T, st *store.Store, slug string,
) *store.AttributeDefinition {
	t.Helper()
	definition, err := st.CreateAttributeDefinitionContext(
		context.Background(), organizationTextDefinition(slug))
	require.NoError(t, err)
	return definition
}

func mustAttributeOrganization(t *testing.T, st *store.Store) *store.Organization {
	t.Helper()
	organization, err := st.CreateOrganizationContext(
		context.Background(), store.OrganizationInput{Name: "Example Org"})
	require.NoError(t, err)
	return organization
}

func organizationAttributeTestBind(st *store.Store) string {
	if st.IsPostgreSQL() {
		return "$1"
	}
	return "?"
}

func TestOrganizationAttributeSetSupersedeAndHistory(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := context.Background()
	st := testutil.NewTestStore(t)
	organization := mustAttributeOrganization(t, st)
	mustOrganizationAttributeDefinition(t, st, "industry_focus")
	firstAt := time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)
	secondAt := firstAt.Add(24 * time.Hour)

	first, err := st.SetOrganizationAttributeValueContext(
		ctx, store.OrganizationAttributeValueInput{
			OrganizationID: organization.ID,
			DefinitionSlug: "industry_focus",
			Value:          textAttributeValue("archival software"),
			ActiveFrom:     &firstAt,
			Source:         store.ProvenanceUser,
		})
	require.NoError(err)

	second, err := st.SetOrganizationAttributeValueContext(
		ctx, store.OrganizationAttributeValueInput{
			OrganizationID:  organization.ID,
			DefinitionSlug:  "industry_focus",
			Value:           textAttributeValue("information retrieval"),
			ActiveFrom:      &secondAt,
			Source:          store.ProvenanceUser,
			ExpectedValueID: &first.Value.ID,
		})
	require.NoError(err)
	require.NotNil(second.Superseded)
	require.NotNil(second.Superseded.ActiveUntil)
	require.NotNil(second.Superseded.SupersededAt)
	assert.Equal(secondAt, *second.Superseded.ActiveUntil)
	assert.True(
		second.Superseded.SupersededAt.After(*second.Superseded.ActiveUntil),
		"transaction time must not be replaced with a backdated validity boundary",
	)

	history, err := st.ListOrganizationAttributeValuesContext(
		ctx, organization.ID,
		store.OrganizationAttributeQuery{
			DefinitionSlug: "industry_focus",
			IncludeHistory: true,
		})
	require.NoError(err)
	require.Len(history, 2)
	assert.Equal(second.Value.ID, history[0].ID)
	assert.Equal(first.Value.ID, history[1].ID)

	closed, err := st.SupersedeOrganizationAttributeValueContext(
		ctx, store.OrganizationAttributeSupersedeInput{
			OrganizationID:  organization.ID,
			DefinitionSlug:  "industry_focus",
			ExpectedValueID: &second.Value.ID,
		})
	require.NoError(err)
	require.NotNil(closed.Superseded)

	current, err := st.ListOrganizationAttributeValuesContext(
		ctx, organization.ID, store.OrganizationAttributeQuery{})
	require.NoError(err)
	assert.Empty(current)
}

func TestOrganizationAttributeRejectsWrongScopeAndInvalidConfidence(t *testing.T) {
	require := require.New(t)
	ctx := context.Background()
	st := testutil.NewTestStore(t)
	organization := mustAttributeOrganization(t, st)

	_, err := st.CreateAttributeDefinitionContext(
		ctx, personTextDefinition("person_only"))
	require.NoError(err)
	_, err = st.SetOrganizationAttributeValueContext(
		ctx, store.OrganizationAttributeValueInput{
			OrganizationID: organization.ID,
			DefinitionSlug: "person_only",
			Value:          textAttributeValue("not allowed"),
			Source:         store.ProvenanceUser,
		})
	require.ErrorIs(err, store.ErrAttributeObjectTypeMismatch)

	mustOrganizationAttributeDefinition(t, st, "declared_fact")
	confidence := 0.8
	_, err = st.SetOrganizationAttributeValueContext(
		ctx, store.OrganizationAttributeValueInput{
			OrganizationID: organization.ID,
			DefinitionSlug: "declared_fact",
			Value:          textAttributeValue("not allowed"),
			Source:         store.ProvenanceUser,
			Confidence:     &confidence,
		})
	require.ErrorIs(err, store.ErrAttributeValueInvalid)
}

func TestOrganizationAttributeRequiresAnExistingOrganization(t *testing.T) {
	require := require.New(t)
	ctx := context.Background()
	st := testutil.NewTestStore(t)
	mustOrganizationAttributeDefinition(t, st, "industry_focus")

	_, err := st.SetOrganizationAttributeValueContext(
		ctx, store.OrganizationAttributeValueInput{
			OrganizationID: 999_999,
			DefinitionSlug: "industry_focus",
			Value:          textAttributeValue("archival software"),
			Source:         store.ProvenanceUser,
		})
	require.ErrorIs(err, store.ErrOrganizationNotFound)
}

func TestOrganizationAttributeDryRunCASAndMultiOrdinal(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := context.Background()
	st := testutil.NewTestStore(t)
	organization := mustAttributeOrganization(t, st)
	input := organizationTextDefinition("specialties")
	input.Cardinality = store.AttributeCardinalityMulti
	_, err := st.CreateAttributeDefinitionContext(ctx, input)
	require.NoError(err)
	confidence := 0.7

	first, err := st.SetOrganizationAttributeValueContext(
		ctx, store.OrganizationAttributeValueInput{
			OrganizationID: organization.ID,
			DefinitionSlug: "specialties",
			Value:          textAttributeValue("archival software"),
			Source:         store.ProvenanceExtraction,
			Confidence:     &confidence,
		})
	require.NoError(err)
	second, err := st.SetOrganizationAttributeValueContext(
		ctx, store.OrganizationAttributeValueInput{
			OrganizationID: organization.ID,
			DefinitionSlug: "specialties",
			Value:          textAttributeValue("information retrieval"),
			Source:         store.ProvenanceSystem,
		})
	require.NoError(err)
	assert.Equal(int64(0), first.Value.Ordinal)
	assert.Equal(int64(1), second.Value.Ordinal)

	staleID := first.Value.ID + second.Value.ID + 1
	_, err = st.SetOrganizationAttributeValueContext(
		ctx, store.OrganizationAttributeValueInput{
			OrganizationID:  organization.ID,
			DefinitionSlug:  "specialties",
			Ordinal:         new(int64(0)),
			Value:           textAttributeValue("stale"),
			Source:          store.ProvenanceUser,
			ExpectedValueID: &staleID,
		})
	require.ErrorIs(err, store.ErrAttributeValueConflict)

	preview, err := st.SetOrganizationAttributeValueContext(
		ctx, store.OrganizationAttributeValueInput{
			OrganizationID: organization.ID,
			DefinitionSlug: "specialties",
			Ordinal:        new(int64(0)),
			Value:          textAttributeValue("preview"),
			Source:         store.ProvenanceUser,
			DryRun:         true,
		})
	require.NoError(err)
	assert.True(preview.DryRun)
	assert.Zero(preview.Value.ID)

	current, err := st.ListOrganizationAttributeValuesContext(
		ctx, organization.ID, store.OrganizationAttributeQuery{})
	require.NoError(err)
	require.Len(current, 2)
	assert.Equal(first.Value.ID, current[0].ID)
	assert.Equal(second.Value.ID, current[1].ID)
}

func TestOrganizationAttributeRetractionIsNotCurrentAndCanBeReplaced(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := context.Background()
	st := testutil.NewTestStore(t)
	organization := mustAttributeOrganization(t, st)
	mustOrganizationAttributeDefinition(t, st, "industry_focus")

	retracted, err := st.SetOrganizationAttributeValueContext(
		ctx, store.OrganizationAttributeValueInput{
			OrganizationID: organization.ID,
			DefinitionSlug: "industry_focus",
			Value:          textAttributeValue("withdrawn"),
			Source:         store.ProvenanceExtraction,
		})
	require.NoError(err)
	_, err = st.DB().ExecContext(ctx, fmt.Sprintf(`
		UPDATE organization_attribute_values
		SET superseded_at = CURRENT_TIMESTAMP
		WHERE id = %s
	`, organizationAttributeTestBind(st)), retracted.Value.ID)
	require.NoError(err)

	current, err := st.ListOrganizationAttributeValuesContext(
		ctx, organization.ID, store.OrganizationAttributeQuery{})
	require.NoError(err)
	assert.Empty(current)

	replacement, err := st.SetOrganizationAttributeValueContext(
		ctx, store.OrganizationAttributeValueInput{
			OrganizationID: organization.ID,
			DefinitionSlug: "industry_focus",
			Value:          textAttributeValue("current"),
			Source:         store.ProvenanceUser,
		})
	require.NoError(err)
	assert.Equal(int64(0), replacement.Value.Ordinal)

	history, err := st.ListOrganizationAttributeValuesContext(
		ctx, organization.ID,
		store.OrganizationAttributeQuery{
			DefinitionSlug: "industry_focus",
			IncludeHistory: true,
		})
	require.NoError(err)
	require.Len(history, 2)
	assert.Equal(replacement.Value.ID, history[0].ID)
	assert.Equal(retracted.Value.ID, history[1].ID)
	assert.Nil(history[1].ActiveUntil)
	require.NotNil(history[1].SupersededAt)
}

func TestOrganizationDeleteCascadesAttributeValues(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := context.Background()
	st := testutil.NewTestStore(t)
	organization := mustAttributeOrganization(t, st)
	mustOrganizationAttributeDefinition(t, st, "industry_focus")

	_, err := st.SetOrganizationAttributeValueContext(
		ctx, store.OrganizationAttributeValueInput{
			OrganizationID: organization.ID,
			DefinitionSlug: "industry_focus",
			Value:          textAttributeValue("archival software"),
			Source:         store.ProvenanceUser,
		})
	require.NoError(err)
	require.NoError(st.DeleteOrganizationContext(
		ctx, organization.ID, organization.Revision))

	var count int
	require.NoError(st.DB().QueryRowContext(ctx, fmt.Sprintf(`
		SELECT COUNT(*) FROM organization_attribute_values
		WHERE organization_id = %s
	`, organizationAttributeTestBind(st)), organization.ID).Scan(&count))
	assert.Zero(count)
}

func TestDeleteAttributeDefinitionRejectsDefinitionsWithOnlyOrganizationValues(
	t *testing.T,
) {
	require := require.New(t)
	ctx := context.Background()
	st := testutil.NewTestStore(t)
	organization := mustAttributeOrganization(t, st)
	definition := mustOrganizationAttributeDefinition(t, st, "industry_focus")
	_, err := st.SetOrganizationAttributeValueContext(
		ctx, store.OrganizationAttributeValueInput{
			OrganizationID: organization.ID,
			DefinitionSlug: "industry_focus",
			Value:          textAttributeValue("archival software"),
			Source:         store.ProvenanceUser,
		})
	require.NoError(err)

	err = st.DeleteAttributeDefinitionContext(
		ctx, definition.ID, definition.Revision)
	require.ErrorIs(err, store.ErrAttributeDefinitionHasValues)
}

func TestOrganizationAttributeWriteUsesTransactionalDefinitionState(t *testing.T) {
	require := require.New(t)
	ctx := context.Background()
	st := testutil.NewTestStore(t)
	if !st.IsPostgreSQL() {
		t.Skip("PostgreSQL row locks are required for this race regression")
	}
	organization := mustAttributeOrganization(t, st)
	definition := mustOrganizationAttributeDefinition(t, st, "industry_focus")

	definitionUpdate, err := st.DB().BeginTx(ctx, nil)
	require.NoError(err)
	t.Cleanup(func() { _ = definitionUpdate.Rollback() })
	_, err = definitionUpdate.ExecContext(ctx, `
		UPDATE attribute_definitions
		SET is_active = FALSE, revision = revision + 1
		WHERE id = $1
	`, definition.ID)
	require.NoError(err)

	writeDone := make(chan error, 1)
	go func() {
		_, err := st.SetOrganizationAttributeValueContext(
			ctx, store.OrganizationAttributeValueInput{
				OrganizationID: organization.ID,
				DefinitionSlug: definition.Slug,
				Value:          textAttributeValue("archival software"),
				Source:         store.ProvenanceUser,
			})
		writeDone <- err
	}()

	select {
	case err := <-writeDone:
		require.ErrorIs(err, store.ErrAttributeDefinitionInactive,
			"an attribute write must not use stale definition state")
		require.NoError(definitionUpdate.Commit())
		return
	case <-time.After(200 * time.Millisecond):
	}
	require.NoError(definitionUpdate.Commit())

	select {
	case err := <-writeDone:
		require.ErrorIs(err, store.ErrAttributeDefinitionInactive)
	case <-time.After(5 * time.Second):
		require.FailNow("attribute write did not finish after definition update committed")
	}
}
