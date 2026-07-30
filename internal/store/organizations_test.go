package store_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestCreateGetAndListOrganizations(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := context.Background()
	st := testutil.NewTestStore(t)

	description := "Synthetic fixture organization."
	domain := "Example.COM"
	created, err := st.CreateOrganizationContext(ctx, store.OrganizationInput{
		Name: "  Example Org  ", Kind: store.OrganizationKindCompany,
		PrimaryDomain: &domain, Description: &description,
	})
	require.NoError(err)
	assert.Positive(created.ID)
	assert.Equal("Example Org", created.Name)
	assert.Equal(store.OrganizationKindCompany, created.Kind)
	require.NotNil(created.PrimaryDomain)
	assert.Equal("example.com", *created.PrimaryDomain)
	assert.Equal(int64(1), created.Revision)
	assert.Nil(created.RetiredAt)
	assert.Nil(created.MergedIntoID)

	got, err := st.GetOrganizationContext(ctx, created.ID)
	require.NoError(err)
	assert.Equal(created.ID, got.ID)
	assert.Equal(created.Revision, got.Revision)

	second, err := st.CreateOrganizationContext(ctx, store.OrganizationInput{
		Name: "Another Org", Kind: store.OrganizationKindNonprofit,
	})
	require.NoError(err)

	listed, err := st.ListOrganizationsContext(ctx, store.OrganizationFilter{Limit: 10})
	require.NoError(err)
	require.Len(listed, 2)
	assert.Equal(second.ID, listed[0].ID)
	assert.Equal(created.ID, listed[1].ID)

	total, err := st.CountOrganizationsContext(ctx, store.OrganizationFilter{})
	require.NoError(err)
	assert.Equal(int64(2), total)

	filtered, err := st.ListOrganizationsContext(ctx, store.OrganizationFilter{
		Query: "exam", Limit: 10,
	})
	require.NoError(err)
	require.Len(filtered, 1)
	assert.Equal(created.ID, filtered[0].ID)
}

func TestCreateOrganizationRejectsInvalidInput(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewTestStore(t)

	tests := []struct {
		name    string
		input   store.OrganizationInput
		wantErr string
	}{
		{
			name: "empty name",
			input: store.OrganizationInput{
				Name: "   ", Kind: store.OrganizationKindCompany,
			},
			wantErr: "name is required",
		},
		{
			name: "unknown kind",
			input: store.OrganizationInput{
				Name: "Example Org", Kind: "conglomerate",
			},
			wantErr: `unknown kind "conglomerate"`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			_, err := st.CreateOrganizationContext(ctx, test.input)
			require.Error(err)
			require.ErrorIs(err, store.ErrOrganizationInvalid)
			assert.ErrorContains(err, test.wantErr)
		})
	}
}

func TestCreateOrganizationDefaultsKindToOther(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)

	created, err := st.CreateOrganizationContext(
		context.Background(), store.OrganizationInput{Name: "Example Org"})
	require.NoError(err)
	assert.Equal(store.OrganizationKindOther, created.Kind)
}

func TestUpdateOrganizationBumpsRevisionAndRejectsStaleWrites(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := context.Background()
	st := testutil.NewTestStore(t)

	created, err := st.CreateOrganizationContext(ctx, store.OrganizationInput{
		Name: "Example Org", Kind: store.OrganizationKindCompany,
	})
	require.NoError(err)

	updated, err := st.UpdateOrganizationContext(ctx, created.ID, created.Revision,
		store.OrganizationInput{Name: "Example Group", Kind: store.OrganizationKindCompany})
	require.NoError(err)
	assert.Equal("Example Group", updated.Name)
	assert.Equal(created.Revision+1, updated.Revision)

	_, err = st.UpdateOrganizationContext(ctx, created.ID, created.Revision,
		store.OrganizationInput{Name: "Stale Write", Kind: store.OrganizationKindCompany})
	require.Error(err)
	require.ErrorIs(err, store.ErrOrganizationRevisionConflict)

	_, err = st.UpdateOrganizationContext(ctx, created.ID+9999, 1,
		store.OrganizationInput{Name: "Missing", Kind: store.OrganizationKindCompany})
	require.Error(err)
	require.ErrorIs(err, store.ErrOrganizationNotFound)
}

func TestRetireAndUnretireOrganizationHidesItFromDefaultListing(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := context.Background()
	st := testutil.NewTestStore(t)

	created, err := st.CreateOrganizationContext(ctx, store.OrganizationInput{
		Name: "Example Org", Kind: store.OrganizationKindCompany,
	})
	require.NoError(err)

	retired, err := st.RetireOrganizationContext(ctx, created.ID, created.Revision)
	require.NoError(err)
	require.NotNil(retired.RetiredAt)
	assert.Equal(created.Revision+1, retired.Revision)

	listed, err := st.ListOrganizationsContext(ctx, store.OrganizationFilter{Limit: 10})
	require.NoError(err)
	assert.Empty(listed)

	withRetired, err := st.ListOrganizationsContext(ctx, store.OrganizationFilter{
		IncludeRetired: true, Limit: 10,
	})
	require.NoError(err)
	require.Len(withRetired, 1)
	assert.Equal(created.ID, withRetired[0].ID)

	revived, err := st.UnretireOrganizationContext(ctx, created.ID, retired.Revision)
	require.NoError(err)
	assert.Nil(revived.RetiredAt)
	assert.Equal(retired.Revision+1, revived.Revision)
}

func TestDeleteOrganizationSucceedsOnlyWithoutEmploymentHistory(t *testing.T) {
	require := require.New(t)
	ctx := context.Background()
	st := testutil.NewTestStore(t)

	created, err := st.CreateOrganizationContext(ctx, store.OrganizationInput{
		Name: "Example Org", Kind: store.OrganizationKindCompany,
	})
	require.NoError(err)
	require.NoError(st.DeleteOrganizationContext(ctx, created.ID, created.Revision))

	_, err = st.GetOrganizationContext(ctx, created.ID)
	require.Error(err)
	require.ErrorIs(err, store.ErrOrganizationNotFound)

	err = st.DeleteOrganizationContext(ctx, created.ID, created.Revision)
	require.Error(err)
	require.ErrorIs(err, store.ErrOrganizationNotFound)
}

func TestNormalizeOrganizationNameAndDomain(t *testing.T) {
	assert := assert.New(t)
	nameTests := []struct{ raw, want string }{
		{raw: "Example Org", want: "example org"},
		{raw: "  Example   Org  ", want: "example org"},
		{raw: "EXAMPLE\tORG", want: "example org"},
		{raw: "", want: ""},
	}
	for _, test := range nameTests {
		assert.Equal(test.want, store.NormalizeOrganizationName(test.raw), "raw %q", test.raw)
	}

	domainTests := []struct{ raw, want string }{
		{raw: "Example.COM", want: "example.com"},
		{raw: "  WWW.Example.com  ", want: "example.com"},
		{raw: "https://example.com/careers", want: "example.com"},
		{raw: "user@example.com", want: "example.com"},
		{raw: "", want: ""},
	}
	for _, test := range domainTests {
		assert.Equal(test.want, store.NormalizeDomain(test.raw), "raw %q", test.raw)
	}
}

func TestMergeOrganizationsRejectsSelfWithoutChangingTheRoot(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := context.Background()
	st := testutil.NewTestStore(t)

	organization, err := st.CreateOrganizationContext(ctx, store.OrganizationInput{
		Name: "Example Org", Kind: store.OrganizationKindCompany,
	})
	require.NoError(err)

	_, err = st.MergeOrganizationsContext(ctx,
		organization.ID, organization.Revision, organization.ID, organization.Revision)
	require.Error(err)
	require.ErrorIs(err, store.ErrOrganizationInvalid)
	require.ErrorContains(err, "cannot merge an organization into itself")

	unchanged, err := st.GetOrganizationContext(ctx, organization.ID)
	require.NoError(err)
	assert.Equal(organization.Revision, unchanged.Revision)
	assert.Nil(unchanged.MergedIntoID)
}

func TestMergedOrganizationRedirectCannotBeRemergedOrDeleted(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := context.Background()
	st := testutil.NewTestStore(t)
	firstSurvivor := mustOrganization(t, st, "Example Org")
	secondSurvivor := mustOrganization(t, st, "Another Org")
	losing := mustOrganization(t, st, "Former Org")

	_, err := st.MergeOrganizationsContext(ctx,
		firstSurvivor.ID, firstSurvivor.Revision, losing.ID, losing.Revision)
	require.NoError(err)
	redirect, err := st.GetOrganizationContext(ctx, losing.ID)
	require.NoError(err)
	require.NotNil(redirect.MergedIntoID)

	_, err = st.MergeOrganizationsContext(ctx,
		secondSurvivor.ID, secondSurvivor.Revision, redirect.ID, redirect.Revision)
	require.Error(err)
	require.ErrorIs(err, store.ErrOrganizationInvalid)

	err = st.DeleteOrganizationContext(ctx, redirect.ID, redirect.Revision)
	require.Error(err)
	require.ErrorIs(err, store.ErrOrganizationInvalid)

	unchanged, err := st.GetOrganizationContext(ctx, redirect.ID)
	require.NoError(err)
	assert.Equal(redirect.Revision, unchanged.Revision)
	require.NotNil(unchanged.MergedIntoID)
	assert.Equal(firstSurvivor.ID, *unchanged.MergedIntoID)
}

func TestMergedOrganizationRedirectRejectsRootAndProfileMutations(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(context.Context, *store.Store, *store.Organization) error
	}{
		{
			name: "root update",
			mutate: func(ctx context.Context, st *store.Store, redirect *store.Organization) error {
				_, err := st.UpdateOrganizationContext(
					ctx, redirect.ID, redirect.Revision,
					store.OrganizationInput{
						Name: "Hidden Rewrite", Kind: store.OrganizationKindCompany,
					})
				return err
			},
		},
		{
			name: "retire",
			mutate: func(ctx context.Context, st *store.Store, redirect *store.Organization) error {
				_, err := st.RetireOrganizationContext(
					ctx, redirect.ID, redirect.Revision)
				return err
			},
		},
		{
			name: "profile replacement",
			mutate: func(ctx context.Context, st *store.Store, redirect *store.Organization) error {
				_, err := st.ReplaceOrganizationProfileContext(
					ctx, redirect.ID, redirect.Revision,
					store.OrganizationProfileInput{})
				return err
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			ctx := context.Background()
			st := testutil.NewTestStore(t)
			survivor := mustOrganization(t, st, "Immutable Survivor")
			losing := mustOrganization(t, st, "Immutable Redirect")
			_, err := st.MergeOrganizationsContext(ctx,
				survivor.ID, survivor.Revision, losing.ID, losing.Revision)
			require.NoError(err)
			redirect, err := st.GetOrganizationContext(ctx, losing.ID)
			require.NoError(err)

			err = test.mutate(ctx, st, redirect)
			require.ErrorIs(err, store.ErrOrganizationInvalid)

			unchanged, err := st.GetOrganizationContext(ctx, redirect.ID)
			require.NoError(err)
			assert.Equal(redirect.Revision, unchanged.Revision)
			assert.Equal(redirect.Name, unchanged.Name)
			assert.Equal(redirect.RetiredAt, unchanged.RetiredAt)
		})
	}
}

func TestOrganizationKindVocabularyIsOpenAtTheDatabaseBoundary(t *testing.T) {
	require := require.New(t)
	ctx := context.Background()
	st := testutil.NewTestStore(t)

	var organizationID int64
	err := st.DB().QueryRowContext(ctx, st.Rebind(`
		INSERT INTO organizations (name, name_normalized, kind)
		VALUES (?, ?, ?)
		RETURNING id
	`), "Open Vocabulary Cooperative", "open vocabulary cooperative",
		"cooperative").Scan(&organizationID)
	require.NoError(err)
	require.Positive(organizationID)
}

func TestOrganizationNameKindVocabularyIsOpenAtTheDatabaseBoundary(t *testing.T) {
	require := require.New(t)
	ctx := context.Background()
	st := testutil.NewTestStore(t)
	organization := mustOrganization(t, st, "Open Vocabulary Names")

	result, err := st.DB().ExecContext(ctx, st.Rebind(`
		INSERT INTO organization_names (
			organization_id, name_kind, original_value, name_normalized, source
		) VALUES (?, ?, ?, ?, ?)
	`), organization.ID, "localized", "Nom localisé", "nom localisé",
		string(store.ProvenanceUser))
	require.NoError(err)
	affected, err := result.RowsAffected()
	require.NoError(err)
	require.Equal(int64(1), affected)
}
