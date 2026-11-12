package store_test

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/activity"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestSubsetPreservesFirstContactState(t *testing.T) {
	for _, merge := range []bool{true, false} {
		name := "clear"
		if merge {
			name = "merge packet"
		}
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			path := filepath.Join(t.TempDir(), "source.db")
			st, err := store.OpenContext(ctx, path)
			require.NoError(t, err)
			require.NoError(t, st.InitSchemaContext(ctx))
			source, err := st.GetOrCreateSource("gmail", "owner@example.test")
			require.NoError(t, err)
			conv, err := st.EnsureConversation(ctx, source.ID, "thread", "Thread")
			require.NoError(t, err)
			fixture := &storetest.Fixture{T: t, Store: st, Source: source, ConvID: conv}
			require.NoError(t, st.AddAccountIdentity(source.ID, "owner@example.test", "manual"))
			owner := fixture.EnsureParticipant("owner@example.test", "Owner", "example.test")
			sender := fixture.EnsureParticipant("sam@example.test", "Sam", "example.test")
			target := fixture.EnsureParticipant("alex@example.test", "Alex", "example.test")
			other := fixture.EnsureParticipant("riley@example.test", "Riley", "example.test")
			person, _, err := st.CreatePersonFromParticipant(target)
			require.NoError(t, err)
			_, _, err = st.CreatePersonFromParticipant(sender)
			require.NoError(t, err)
			absorbed, _, err := st.CreatePersonFromParticipant(other)
			require.NoError(t, err)
			projector, err := activity.NewProjector(st, activity.Options{Timezone: "UTC", BatchSize: 10, MaxDirectCounterparts: 25})
			require.NoError(t, err)
			f := &introFixture{f: fixture, projector: projector, owner: owner, sender: sender, target: target, person: person}
			f.message(t, "intro", sender, time.Date(2024, 5, 1, 12, 0, 0, 0, time.UTC), owner, target, other)
			f.run(t)
			var projectedRows int
			err = st.DB().QueryRowContext(ctx, st.Rebind(`SELECT COUNT(*) FROM person_first_contacts WHERE person_id IN (?, ?)`), person.ID, absorbed.ID).Scan(&projectedRows)
			require.NoError(t, err)
			require.Equal(t, 2, projectedRows)
			values := f.values(t, false)
			require.Len(t, values, 1)
			var mergeID int64
			if merge {
				person, err = st.GetPersonContext(ctx, person.ID)
				require.NoError(t, err)
				absorbed, err = st.GetPersonContext(ctx, absorbed.ID)
				require.NoError(t, err)
				result, err := st.MergePersonsContext(ctx, store.PersonMergeRequest{SurvivorID: person.ID, AbsorbedID: absorbed.ID, ExpectedSurvivorRevision: person.Revision, ExpectedAbsorbedRevision: absorbed.Revision, IdempotencyKey: "subset-first-contact", Actor: "test"})
				require.NoError(t, err)
				mergeID = result.Merge.ID
			} else {
				_, err = st.SupersedePersonAttributeValueContext(ctx, store.PersonAttributeSupersedeInput{PersonID: person.ID, DefinitionSlug: store.AttributeSlugIntroducedBy, ExpectedValueID: &values[0].ID})
				require.NoError(t, err)
			}
			require.NoError(t, st.Close())
			dir := filepath.Join(t.TempDir(), "subset")
			_, err = store.CopySubsetWithOptions(ctx, path, dir, 10, store.CopySubsetOptions{IncludeIdentity: true, IncludeProfiles: true, IncludeAttributes: true})
			require.NoError(t, err)
			dst, err := store.OpenContext(ctx, filepath.Join(dir, "msgvault.db"))
			require.NoError(t, err)
			t.Cleanup(func() { assert.NoError(t, dst.Close()) })
			if merge {
				_, err = dst.GetPersonMergeContext(ctx, mergeID)
				require.NoError(t, err)
			} else {
				p, err := activity.NewProjector(dst, activity.Options{Timezone: "UTC", BatchSize: 10, MaxDirectCounterparts: 25})
				require.NoError(t, err)
				_, err = p.RunBackstop(ctx)
				require.NoError(t, err)
				values, err := dst.ListPersonAttributeValuesContext(ctx, person.ID, store.PersonAttributeQuery{DefinitionSlug: store.AttributeSlugIntroducedBy})
				require.NoError(t, err)
				assert.Empty(t, values)
			}
		})
	}
}
