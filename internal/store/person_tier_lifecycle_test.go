package store_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func tierLifecyclePeople(t *testing.T) (*store.Store, *store.Person, *store.Person) {
	t.Helper()
	require := require.New(t)
	f := storetest.New(t)
	var people []*store.Person
	for _, name := range []string{"survivor", "absorbed"} {
		p := f.EnsureParticipant(name+"-tier@example.test", name, "example.test")
		person, _, err := f.Store.CreatePersonFromParticipant(p)
		require.NoError(err)
		people = append(people, person)
	}
	return f.Store, people[0], people[1]
}

func tierMerge(t *testing.T, st *store.Store, survivor, absorbed *store.Person) *store.PersonMergeResult {
	t.Helper()
	require := require.New(t)
	merged, err := st.MergePersonsContext(t.Context(), store.PersonMergeRequest{
		SurvivorID: survivor.ID, AbsorbedID: absorbed.ID,
		ExpectedSurvivorRevision: survivor.Revision, ExpectedAbsorbedRevision: absorbed.Revision,
		IdempotencyKey: "tier-merge", Actor: "owner",
	})
	require.NoError(err)
	return merged
}

// Dropping an absorbed manual grant during deduplication would let the next
// automated demotion erase the owner's explicit enrollment.
func TestPersonTierMergeManualGrantDominatesAndSplitRestores(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, survivor, absorbed := tierLifecyclePeople(t)
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	for i, person := range []*store.Person{survivor, absorbed} {
		provenance, actor := "auto", "signal-tiers"
		if i == 1 {
			provenance, actor = "manual", "owner"
		}
		_, err := st.DB().ExecContext(t.Context(), st.Rebind(`INSERT INTO person_tracking(person_id,tracked_at,provenance,actor) VALUES(?,?,?,?)`), person.ID, at, provenance, actor)
		require.NoError(err)
		_, err = st.DB().ExecContext(t.Context(), st.Rebind(`INSERT INTO person_brief_enrollments(person_id,enabled_at,provenance,actor) VALUES(?,?,?,?)`), person.ID, at, provenance, actor)
		require.NoError(err)
	}
	merged := tierMerge(t, st, survivor, absorbed)
	for _, table := range []string{"person_tracking", "person_brief_enrollments"} {
		var provenance, actor string
		require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT provenance,actor FROM `+table+` WHERE person_id=?`), survivor.ID).Scan(&provenance, &actor))
		assert.Equal("manual", provenance)
		assert.Equal("owner", actor)
	}
	split, err := st.SplitPersonMergeContext(t.Context(), store.PersonSplitRequest{
		SourcePersonID: merged.Person.ID, MergeID: merged.Merge.ID,
		ParticipantIDs: absorbed.ParticipantIDs, ExpectedSourceRevision: merged.Person.Revision,
		IdempotencyKey: "tier-split", Actor: "owner",
	})
	require.NoError(err)
	require.True(split.ExactReversal)
	for _, table := range []string{"person_tracking", "person_brief_enrollments"} {
		for i, personID := range []int64{split.SourcePerson.ID, split.NewPerson.ID} {
			var provenance, actor string
			require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT provenance,actor FROM `+table+` WHERE person_id=?`), personID).Scan(&provenance, &actor))
			expectedProvenance, expectedActor := "auto", "signal-tiers"
			if i == 1 {
				expectedProvenance, expectedActor = "manual", "owner"
			}
			assert.Equal(expectedProvenance, provenance)
			assert.Equal(expectedActor, actor)
		}
	}
}

func TestPersonTierMergeUnionsOptOutsAndReconcilesAutomaticGrants(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, survivor, absorbed := tierLifecyclePeople(t)
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	_, err := st.DB().ExecContext(t.Context(), st.Rebind(`INSERT INTO person_tracking(person_id,tracked_at,provenance,actor) VALUES(?,?,'auto','signal-tiers')`), survivor.ID, at)
	require.NoError(err)
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`INSERT INTO person_brief_enrollments(person_id,enabled_at,provenance,actor) VALUES(?,?,'auto','signal-tiers')`), survivor.ID, at)
	require.NoError(err)
	for _, lane := range []string{"tracking", "briefs", "enrichment"} {
		_, err = st.DB().ExecContext(t.Context(), st.Rebind(`INSERT INTO person_tier_opt_outs(person_id,lane,actor) VALUES(?,?,'owner')`), absorbed.ID, lane)
		require.NoError(err)
	}
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`INSERT INTO person_tier_opt_outs(person_id,lane,actor) VALUES(?,'enrichment','survivor-owner')`), survivor.ID)
	require.NoError(err)
	merged := tierMerge(t, st, survivor, absorbed)
	tracking, err := st.GetPersonTrackingContext(t.Context(), survivor.ID)
	require.NoError(err)
	assert.False(tracking.Tracked)
	brief, err := st.GetPersonBriefEnrollmentContext(t.Context(), survivor.ID)
	require.NoError(err)
	assert.False(brief.Enrolled)
	var count int
	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT COUNT(*) FROM person_tier_opt_outs WHERE person_id=?`), survivor.ID).Scan(&count))
	assert.Equal(3, count)
	var actor string
	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT actor FROM person_tier_opt_outs WHERE person_id=? AND lane='enrichment'`), survivor.ID).Scan(&actor))
	assert.Equal("survivor-owner", actor)
	split, err := st.SplitPersonMergeContext(t.Context(), store.PersonSplitRequest{
		SourcePersonID: merged.Person.ID, MergeID: merged.Merge.ID, ParticipantIDs: absorbed.ParticipantIDs,
		ExpectedSourceRevision: merged.Person.Revision, IdempotencyKey: "tier-opt-out-split", Actor: "owner",
	})
	require.NoError(err)
	assert.True(split.ExactReversal)
	tracking, err = st.GetPersonTrackingContext(t.Context(), split.SourcePerson.ID)
	require.NoError(err)
	assert.True(tracking.Tracked, "exact reversal restores the auto grant removed by merged opt-outs")
	for i, personID := range []int64{split.SourcePerson.ID, split.NewPerson.ID} {
		require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT COUNT(*) FROM person_tier_opt_outs WHERE person_id=?`), personID).Scan(&count))
		want := 1
		if i == 1 {
			want = 3
		}
		assert.Equal(want, count)
	}
}

func TestPersonTierMergeMovesAuditHistoryAndKeepsSurvivorState(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, survivor, absorbed := tierLifecyclePeople(t)
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	for _, person := range []*store.Person{survivor, absorbed} {
		_, err := st.DB().ExecContext(t.Context(), st.Rebind(`INSERT INTO person_tier_state(person_id,tier,tier_since,rule,config_fingerprint,rule_version,enrichment_eligible,lanes_json,as_of_local_date,run_id,run_started_at,applied_at) VALUES(?,1,'2026-10-01','tier1_intensity','config',1,TRUE,'["tracking","briefs","enrichment"]','2026-10-01','run',?,?)`), person.ID, at, at)
		require.NoError(err)
		_, err = st.DB().ExecContext(t.Context(), st.Rebind(`INSERT INTO person_tier_observations(person_id,computed_tier,rule,signals_json,shadow_tier,shadow_since,rule_version,config_fingerprint,as_of_local_date,activity_ready,evaluated_at) VALUES(?,1,'tier1_intensity','{}',1,'2026-10-01',1,'config','2026-10-01',TRUE,?)`), person.ID, at)
		require.NoError(err)
	}
	_, err := st.DB().ExecContext(t.Context(), st.Rebind(`INSERT INTO person_tier_events(person_id,from_tier,to_tier,reason,mode,lanes_json,signals_json,rule,rule_version,actor,run_id) VALUES(?,3,1,'promotion','apply','{}','{}','tier1_intensity',1,'owner','run')`), absorbed.ID)
	require.NoError(err)
	tierMerge(t, st, survivor, absorbed)
	for _, table := range []string{"person_tier_state", "person_tier_observations"} {
		var count int
		require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT COUNT(*) FROM `+table+` WHERE person_id=?`), survivor.ID).Scan(&count))
		assert.Equal(1, count)
	}
	var personID, mergedFrom int64
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT person_id,merged_from_person_id FROM person_tier_events`).Scan(&personID, &mergedFrom))
	assert.Equal(survivor.ID, personID)
	assert.Equal(absorbed.ID, mergedFrom)
}

func TestPersonTierChainedMergeOptOutSplitRestoresGrants(t *testing.T) {
	for _, lane := range []string{"tracking", "briefs"} {
		for _, order := range []string{"outer_first", "inner_first"} {
			t.Run(lane+"/"+order, func(t *testing.T) {
				assert := assert.New(t)
				require := require.New(t)
				st, first, second := tierLifecyclePeople(t)
				participantID, err := st.EnsureParticipant("chain-opt-out@example.test", "Opt-out Person", "example.test")
				require.NoError(err)
				third, _, err := st.CreatePersonFromParticipant(participantID)
				require.NoError(err)
				at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
				for _, table := range []struct{ name, date string }{
					{"person_tracking", "tracked_at"}, {"person_brief_enrollments", "enabled_at"},
				} {
					_, err = st.DB().ExecContext(t.Context(), st.Rebind(`INSERT INTO `+table.name+`(person_id,`+table.date+`,provenance,actor) VALUES(?,?,'auto','signal-tiers')`), second.ID, at)
					require.NoError(err)
				}
				require.NoError(st.SetPersonTierOptOutContext(t.Context(), third.ID, lane, true, "operator", "synthetic preference"))
				firstMerge := tierMerge(t, st, first, second)
				third, err = st.GetPersonContext(t.Context(), third.ID)
				require.NoError(err)
				secondMerge, err := st.MergePersonsContext(t.Context(), store.PersonMergeRequest{
					SurvivorID: third.ID, AbsorbedID: firstMerge.Person.ID,
					ExpectedSurvivorRevision: third.Revision, ExpectedAbsorbedRevision: firstMerge.Person.Revision,
					IdempotencyKey: "tier-chain-second-merge", Actor: "operator",
				})
				require.NoError(err)
				var restoredSecond store.Person
				var untrackedIDs []int64
				if order == "inner_first" {
					split, err := st.SplitPersonMergeContext(t.Context(), store.PersonSplitRequest{
						SourcePersonID: secondMerge.Person.ID, MergeID: firstMerge.Merge.ID,
						ParticipantIDs: second.ParticipantIDs, ExpectedSourceRevision: secondMerge.Person.Revision,
						IdempotencyKey: "tier-chain-inner-split", Actor: "operator",
					})
					require.NoError(err)
					require.True(split.ExactReversal)
					restoredSecond = split.NewPerson
					untrackedIDs = []int64{split.SourcePerson.ID}
				} else {
					secondSplit, err := st.SplitPersonMergeContext(t.Context(), store.PersonSplitRequest{
						SourcePersonID: secondMerge.Person.ID, MergeID: secondMerge.Merge.ID,
						ParticipantIDs: firstMerge.Person.ParticipantIDs, ExpectedSourceRevision: secondMerge.Person.Revision,
						IdempotencyKey: "tier-chain-second-split", Actor: "operator",
					})
					require.NoError(err)
					require.True(secondSplit.ExactReversal)
					restoredFirst, err := st.GetPersonContext(t.Context(), secondSplit.NewPerson.ID)
					require.NoError(err)
					firstSplit, err := st.SplitPersonMergeContext(t.Context(), store.PersonSplitRequest{
						SourcePersonID: restoredFirst.ID, MergeID: firstMerge.Merge.ID,
						ParticipantIDs: second.ParticipantIDs, ExpectedSourceRevision: restoredFirst.Revision,
						IdempotencyKey: "tier-chain-first-split", Actor: "operator",
					})
					require.NoError(err)
					require.True(firstSplit.ExactReversal)
					restoredSecond = firstSplit.NewPerson
					untrackedIDs = []int64{firstSplit.SourcePerson.ID, secondSplit.SourcePerson.ID}
				}
				for _, table := range []struct{ name, date string }{
					{"person_tracking", "tracked_at"}, {"person_brief_enrollments", "enabled_at"},
				} {
					for _, personID := range untrackedIDs {
						var count int
						require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT COUNT(*) FROM `+table.name+` WHERE person_id=?`), personID).Scan(&count))
						assert.Zero(count, "originally untracked people must not gain grants")
					}
					var provenance, actor string
					var gotAt time.Time
					require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT provenance,actor,`+table.date+` FROM `+table.name+` WHERE person_id=?`), restoredSecond.ID).Scan(&provenance, &actor, &gotAt))
					assert.Equal("auto", provenance)
					assert.Equal("signal-tiers", actor)
					assert.Equal(at, gotAt.UTC())
				}
			})
		}
	}
}

func TestPersonTierMergeOnlyJournalsGrantsRemovedByCurrentReconciliation(t *testing.T) {
	for _, tc := range []struct {
		name                       string
		untrackBeforeFollowupMerge bool
		wantBriefRemoval           bool
	}{
		{name: "earlier untrack is not attributed to later merge", untrackBeforeFollowupMerge: true},
		{name: "current opt-out removal is journaled", wantBriefRemoval: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			st, survivor, absorbed := tierLifecyclePeople(t)
			at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
			presenceTables := []struct{ name, timestamp string }{
				{"person_tracking", "tracked_at"}, {"person_brief_enrollments", "enabled_at"},
			}
			// Absorbed-origin rows stay in the first journal; unchanged survivor
			// rows are pruned when the first merge closes its journal.
			for _, table := range presenceTables {
				_, err := st.DB().ExecContext(t.Context(), st.Rebind(`INSERT INTO `+table.name+`(person_id,`+table.timestamp+`,provenance,actor) VALUES(?,?,'auto','signal-tiers')`), absorbed.ID, at)
				require.NoError(err)
			}
			first := tierMerge(t, st, survivor, absorbed)

			originalActions := make(map[string]string, len(presenceTables))
			for _, table := range presenceTables {
				var action string
				require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT action FROM person_merge_rows WHERE merge_id=? AND table_name=?`), first.Merge.ID, table.name).Scan(&action))
				require.NotEqual("deleted_snapshot", action)
				originalActions[table.name] = action
			}
			if tc.untrackBeforeFollowupMerge {
				_, err := st.SetPersonTrackingActorContext(t.Context(), first.Person.ID, false, "owner")
				require.NoError(err)
			}
			firstPerson, err := st.GetPersonContext(t.Context(), first.Person.ID)
			require.NoError(err)

			optOutPersonID, err := st.EnsureParticipant("opt-out-tier@example.test", "Opt Out", "example.test")
			require.NoError(err)
			optOutPerson, _, err := st.CreatePersonFromParticipant(optOutPersonID)
			require.NoError(err)
			require.NoError(st.SetPersonTierOptOutContext(t.Context(), optOutPerson.ID, "briefs", true, "owner", "synthetic preference"))

			second, err := st.MergePersonsContext(t.Context(), store.PersonMergeRequest{
				SurvivorID: firstPerson.ID, AbsorbedID: optOutPerson.ID,
				ExpectedSurvivorRevision: firstPerson.Revision, ExpectedAbsorbedRevision: optOutPerson.Revision,
				IdempotencyKey: "tier-followup-merge", Actor: "owner",
			})
			require.NoError(err)
			for _, table := range presenceTables {
				var gotAction string
				require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT action FROM person_merge_rows WHERE merge_id=? AND table_name=?`), first.Merge.ID, table.name).Scan(&gotAction))
				wantAction := originalActions[table.name]
				if tc.wantBriefRemoval && table.name == "person_brief_enrollments" {
					wantAction = "deleted_snapshot"
				}
				assert.Equal(wantAction, gotAction, "only a grant removed by the current reconciliation gets a deleted snapshot")

				var secondDeletedSnapshots int
				require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT COUNT(*) FROM person_merge_rows
					WHERE merge_id=? AND table_name=? AND action='deleted_snapshot'`), second.Merge.ID, table.name).Scan(&secondDeletedSnapshots))
				wantSecondDeletedSnapshots := 0
				if tc.wantBriefRemoval && table.name == "person_brief_enrollments" {
					wantSecondDeletedSnapshots = 1
				}
				assert.Equal(wantSecondDeletedSnapshots, secondDeletedSnapshots,
					"the current merge journals only the grant removed by its reconciliation")
			}
		})
	}
}
