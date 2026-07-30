package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestRefreshOrganizationDuplicateSuggestionsFindsSharedDomainsAndNames(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := context.Background()
	st := testutil.NewTestStore(t)

	domain := "example.com"
	first, err := st.CreateOrganizationContext(ctx, store.OrganizationInput{
		Name: "Example Org", PrimaryDomain: &domain,
	})
	require.NoError(err)
	second, err := st.CreateOrganizationContext(ctx, store.OrganizationInput{
		Name: "Example Org Inc", PrimaryDomain: &domain,
	})
	require.NoError(err)
	third, err := st.CreateOrganizationContext(ctx, store.OrganizationInput{
		Name: "  example   org  ",
	})
	require.NoError(err)
	unrelated, err := st.CreateOrganizationContext(ctx, store.OrganizationInput{
		Name: "Unrelated Org",
	})
	require.NoError(err)

	created, err := st.RefreshOrganizationDuplicateSuggestionsContext(ctx)
	require.NoError(err)
	assert.Equal(2, created)

	suggestions, err := st.ListOrganizationDuplicateSuggestionsContext(ctx,
		store.OrganizationDuplicateStatusOpen, 0, 0)
	require.NoError(err)
	require.Len(suggestions, 2)

	byCriterion := map[string]store.OrganizationDuplicateSuggestion{}
	for _, suggestion := range suggestions {
		byCriterion[suggestion.Criterion] = suggestion
	}

	domainSuggestion, ok := byCriterion[store.OrganizationDuplicateCriterionDomain]
	require.True(ok)
	assert.Equal(first.ID, domainSuggestion.OrganizationAID)
	assert.Equal(second.ID, domainSuggestion.OrganizationBID)
	assert.Equal("example.com", domainSuggestion.Evidence)
	assert.Equal(store.OrganizationDuplicateStatusOpen, domainSuggestion.Status)

	nameSuggestion, ok := byCriterion[store.OrganizationDuplicateCriterionName]
	require.True(ok)
	assert.Equal(first.ID, nameSuggestion.OrganizationAID)
	assert.Equal(third.ID, nameSuggestion.OrganizationBID)
	assert.Equal("example org", nameSuggestion.Evidence)

	for _, suggestion := range suggestions {
		assert.NotEqual(unrelated.ID, suggestion.OrganizationAID)
		assert.NotEqual(unrelated.ID, suggestion.OrganizationBID)
	}
}

func TestRefreshOrganizationDuplicateSuggestionsIsIdempotentAndNeverMerges(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := context.Background()
	st := testutil.NewTestStore(t)

	domain := "example.com"
	first, err := st.CreateOrganizationContext(ctx, store.OrganizationInput{
		Name: "Example Org", PrimaryDomain: &domain,
	})
	require.NoError(err)
	second, err := st.CreateOrganizationContext(ctx, store.OrganizationInput{
		Name: "Example Org Inc", PrimaryDomain: &domain,
	})
	require.NoError(err)

	firstRun, err := st.RefreshOrganizationDuplicateSuggestionsContext(ctx)
	require.NoError(err)
	assert.Equal(1, firstRun)

	secondRun, err := st.RefreshOrganizationDuplicateSuggestionsContext(ctx)
	require.NoError(err)
	assert.Equal(0, secondRun, "a repeated scan creates no duplicate suggestion rows")

	stillFirst, err := st.GetOrganizationContext(ctx, first.ID)
	require.NoError(err)
	assert.Nil(stillFirst.MergedIntoID, "a suggestion never merges anything")
	assert.Nil(stillFirst.RetiredAt)
	stillSecond, err := st.GetOrganizationContext(ctx, second.ID)
	require.NoError(err)
	assert.Nil(stillSecond.MergedIntoID)
	assert.Nil(stillSecond.RetiredAt)
	assert.Equal(first.Revision, stillFirst.Revision,
		"a suggestion does not touch the organization revision")
}

func TestRejectedOrganizationDuplicateSuggestionsAreRetainedAndNotReopened(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := context.Background()
	st := testutil.NewTestStore(t)

	domain := "example.com"
	_, err := st.CreateOrganizationContext(ctx, store.OrganizationInput{
		Name: "Example Org", PrimaryDomain: &domain,
	})
	require.NoError(err)
	_, err = st.CreateOrganizationContext(ctx, store.OrganizationInput{
		Name: "Example Org Inc", PrimaryDomain: &domain,
	})
	require.NoError(err)

	_, err = st.RefreshOrganizationDuplicateSuggestionsContext(ctx)
	require.NoError(err)
	open, err := st.ListOrganizationDuplicateSuggestionsContext(ctx,
		store.OrganizationDuplicateStatusOpen, 0, 0)
	require.NoError(err)
	require.Len(open, 1)

	note := "Separate legal entities that share a marketing domain."
	rejected, err := st.ResolveOrganizationDuplicateSuggestionContext(ctx,
		open[0].ID, store.OrganizationDuplicateStatusRejected, &note)
	require.NoError(err)
	assert.Equal(store.OrganizationDuplicateStatusRejected, rejected.Status)
	require.NotNil(rejected.ResolvedAt)
	require.NotNil(rejected.ResolutionNote)
	assert.Equal(note, *rejected.ResolutionNote)

	rerun, err := st.RefreshOrganizationDuplicateSuggestionsContext(ctx)
	require.NoError(err)
	assert.Equal(0, rerun, "the same low-quality inference is not repeated")

	stillRejected, err := st.ListOrganizationDuplicateSuggestionsContext(ctx,
		store.OrganizationDuplicateStatusRejected, 0, 0)
	require.NoError(err)
	require.Len(stillRejected, 1)
	assert.Equal(open[0].ID, stillRejected[0].ID)

	remainingOpen, err := st.ListOrganizationDuplicateSuggestionsContext(ctx,
		store.OrganizationDuplicateStatusOpen, 0, 0)
	require.NoError(err)
	assert.Empty(remainingOpen)
}

func TestRefreshOrganizationDuplicateSuggestionsSkipsRetiredAndMergedOrganizations(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := context.Background()
	st := testutil.NewTestStore(t)

	domain := "example.com"
	first, err := st.CreateOrganizationContext(ctx, store.OrganizationInput{
		Name: "Example Org", PrimaryDomain: &domain,
	})
	require.NoError(err)
	second, err := st.CreateOrganizationContext(ctx, store.OrganizationInput{
		Name: "Example Org Inc", PrimaryDomain: &domain,
	})
	require.NoError(err)

	_, err = st.RetireOrganizationContext(ctx, second.ID, second.Revision)
	require.NoError(err)
	merged, err := st.CreateOrganizationContext(ctx, store.OrganizationInput{
		Name: "Merged Example Org", PrimaryDomain: &domain,
	})
	require.NoError(err)
	_, err = st.MergeOrganizationsContext(ctx, first.ID, first.Revision, merged.ID, merged.Revision)
	require.NoError(err)
	merged, err = st.GetOrganizationContext(ctx, merged.ID)
	require.NoError(err)
	require.NotNil(merged.MergedIntoID)
	_, err = st.DB().ExecContext(ctx,
		st.Rebind(`UPDATE organizations SET retired_at = NULL WHERE id = ?`), merged.ID)
	require.NoError(err)

	created, err := st.RefreshOrganizationDuplicateSuggestionsContext(ctx)
	require.NoError(err)
	assert.Equal(0, created, "retired organizations are not duplicate candidates")

	_, err = st.GetOrganizationContext(ctx, first.ID)
	require.NoError(err)
}

func TestRefreshOrganizationDuplicateSuggestionsUsesAliasesAndIdentifiers(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := context.Background()
	st := testutil.NewTestStore(t)

	first, err := st.CreateOrganizationContext(ctx, store.OrganizationInput{Name: "Example Org"})
	require.NoError(err)
	second, err := st.CreateOrganizationContext(ctx, store.OrganizationInput{Name: "Second Org"})
	require.NoError(err)

	_, err = st.ReplaceOrganizationProfileContext(ctx, first.ID, first.Revision,
		store.OrganizationProfileInput{
			Identifiers: []store.OrganizationIdentifierInput{{
				IdentifierKind: store.OrganizationIdentifierKindDomain,
				Value:          "shared.example",
				Envelope:       store.ValueEnvelope{Source: store.ProvenanceUser},
			}},
		})
	require.NoError(err)
	refreshedSecond, err := st.GetOrganizationContext(ctx, second.ID)
	require.NoError(err)
	_, err = st.ReplaceOrganizationProfileContext(ctx, second.ID, refreshedSecond.Revision,
		store.OrganizationProfileInput{
			Names: []store.OrganizationNameInput{{
				Name:     "Example Org",
				NameKind: store.OrganizationNameKindAlias,
				Envelope: store.ValueEnvelope{Source: store.ProvenanceUser},
			}},
			Identifiers: []store.OrganizationIdentifierInput{{
				IdentifierKind: store.OrganizationIdentifierKindDomain,
				Value:          "shared.example",
				Envelope:       store.ValueEnvelope{Source: store.ProvenanceUser},
			}},
		})
	require.NoError(err)

	created, err := st.RefreshOrganizationDuplicateSuggestionsContext(ctx)
	require.NoError(err)
	assert.Equal(2, created,
		"an alias name and a child-table domain both produce suggestions")
}

func TestResolveOrganizationDuplicateSuggestionValidatesInput(t *testing.T) {
	require := require.New(t)
	ctx := context.Background()
	st := testutil.NewTestStore(t)

	_, err := st.ResolveOrganizationDuplicateSuggestionContext(ctx, 9999,
		store.OrganizationDuplicateStatusAccepted, nil)
	require.Error(err)
	require.ErrorIs(err, store.ErrOrganizationDuplicateSuggestionNotFound)

	domain := "example.com"
	_, err = st.CreateOrganizationContext(ctx, store.OrganizationInput{
		Name: "Example Org", PrimaryDomain: &domain,
	})
	require.NoError(err)
	_, err = st.CreateOrganizationContext(ctx, store.OrganizationInput{
		Name: "Example Org Inc", PrimaryDomain: &domain,
	})
	require.NoError(err)
	_, err = st.RefreshOrganizationDuplicateSuggestionsContext(ctx)
	require.NoError(err)
	open, err := st.ListOrganizationDuplicateSuggestionsContext(ctx,
		store.OrganizationDuplicateStatusOpen, 0, 0)
	require.NoError(err)
	require.Len(open, 1)

	_, err = st.ResolveOrganizationDuplicateSuggestionContext(ctx, open[0].ID, "maybe", nil)
	require.Error(err)
	require.ErrorIs(err, store.ErrOrganizationDuplicateSuggestionInvalid)
	require.ErrorContains(err, `unknown status "maybe"`)

	_, err = st.ResolveOrganizationDuplicateSuggestionContext(ctx, open[0].ID,
		store.OrganizationDuplicateStatusOpen, nil)
	require.Error(err)
	require.ErrorIs(err, store.ErrOrganizationDuplicateSuggestionInvalid)
	require.ErrorContains(err, "cannot resolve a suggestion back to open")
}

func TestResolveOrganizationDuplicateSuggestionIsOneWay(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := context.Background()
	st := testutil.NewTestStore(t)

	domain := "one-way.example"
	_, err := st.CreateOrganizationContext(ctx, store.OrganizationInput{
		Name: "One Way A", PrimaryDomain: &domain,
	})
	require.NoError(err)
	_, err = st.CreateOrganizationContext(ctx, store.OrganizationInput{
		Name: "One Way B", PrimaryDomain: &domain,
	})
	require.NoError(err)
	_, err = st.RefreshOrganizationDuplicateSuggestionsContext(ctx)
	require.NoError(err)
	open, err := st.ListOrganizationDuplicateSuggestionsContext(
		ctx, store.OrganizationDuplicateStatusOpen, 0, 0)
	require.NoError(err)
	require.Len(open, 1)

	firstNote := "first decision"
	first, err := st.ResolveOrganizationDuplicateSuggestionContext(
		ctx, open[0].ID, store.OrganizationDuplicateStatusAccepted, &firstNote)
	require.NoError(err)
	assert.Equal(store.OrganizationDuplicateStatusAccepted, first.Status)
	assert.Nil(first.ResolvedBy)

	secondNote := "later overwrite"
	_, err = st.ResolveOrganizationDuplicateSuggestionContext(
		ctx, open[0].ID, store.OrganizationDuplicateStatusRejected, &secondNote)
	require.ErrorIs(err, store.ErrOrganizationDuplicateSuggestionAlreadyResolved)

	all, err := st.ListOrganizationDuplicateSuggestionsContext(ctx, "", 0, 0)
	require.NoError(err)
	require.Len(all, 1)
	assert.Equal(store.OrganizationDuplicateStatusAccepted, all[0].Status)
	require.NotNil(all[0].ResolutionNote)
	assert.Equal(firstNote, *all[0].ResolutionNote)
	assert.Nil(all[0].ResolvedBy)
}

func TestConcurrentOrganizationDuplicateSuggestionResolutionHasOneWinner(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := context.Background()
	st := testutil.NewTestStore(t)

	domain := "concurrent-resolution.example"
	_, err := st.CreateOrganizationContext(ctx, store.OrganizationInput{
		Name: "Concurrent Resolution A", PrimaryDomain: &domain,
	})
	require.NoError(err)
	_, err = st.CreateOrganizationContext(ctx, store.OrganizationInput{
		Name: "Concurrent Resolution B", PrimaryDomain: &domain,
	})
	require.NoError(err)
	_, err = st.RefreshOrganizationDuplicateSuggestionsContext(ctx)
	require.NoError(err)
	open, err := st.ListOrganizationDuplicateSuggestionsContext(
		ctx, store.OrganizationDuplicateStatusOpen, 0, 0)
	require.NoError(err)
	require.Len(open, 1)

	type result struct {
		status string
		err    error
	}
	start := make(chan struct{})
	results := make(chan result, 2)
	for _, status := range []string{
		store.OrganizationDuplicateStatusAccepted,
		store.OrganizationDuplicateStatusRejected,
	} {
		go func(status string) {
			<-start
			_, resolveErr := st.ResolveOrganizationDuplicateSuggestionContext(
				ctx, open[0].ID, status, nil)
			results <- result{status: status, err: resolveErr}
		}(status)
	}
	close(start)

	var winner string
	conflicts := 0
	for range 2 {
		got := <-results
		switch {
		case got.err == nil:
			require.Empty(winner, "only one resolution may succeed")
			winner = got.status
		case errors.Is(got.err, store.ErrOrganizationDuplicateSuggestionAlreadyResolved):
			conflicts++
		default:
			require.NoError(got.err)
		}
	}
	require.NotEmpty(winner)
	assert.Equal(1, conflicts)

	all, err := st.ListOrganizationDuplicateSuggestionsContext(ctx, "", 0, 0)
	require.NoError(err)
	require.Len(all, 1)
	assert.Equal(winner, all[0].Status)
}

func TestPostgreSQLOrganizationDuplicateConfidenceUsesDoublePrecision(t *testing.T) {
	require := require.New(t)
	ctx := context.Background()
	st := testutil.NewTestStore(t)
	if !st.IsPostgreSQL() {
		t.Skip("PostgreSQL runtime metadata is required")
	}

	var dataType string
	err := st.DB().QueryRowContext(ctx, `
		SELECT data_type
		FROM information_schema.columns
		WHERE table_schema = current_schema()
		  AND table_name = 'organization_duplicate_suggestions'
		  AND column_name = 'confidence'
	`).Scan(&dataType)
	require.NoError(err)
	require.Equal("double precision", dataType)
}
