package store_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

func TestPersonTierExactSplitKeepsProvenanceOnlyManualReplacement(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, survivor, absorbed := tierLifecyclePeople(t)
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	_, err := st.DB().ExecContext(t.Context(), st.Rebind(`INSERT INTO person_tracking(person_id,tracked_at,provenance,actor) VALUES(?,?,'auto','signal-tiers')`), absorbed.ID, at)
	require.NoError(err)
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`INSERT INTO person_brief_enrollments(person_id,enabled_at,provenance,actor) VALUES(?,?,'auto','signal-tiers')`), absorbed.ID, at)
	require.NoError(err)
	merged := tierMerge(t, st, survivor, absorbed)
	// Using the same actor makes provenance the only changed grant column.
	_, err = st.SetPersonBriefEnrollmentContext(t.Context(), survivor.ID, true, "signal-tiers", false)
	require.NoError(err)
	split, err := st.SplitPersonMergeContext(t.Context(), store.PersonSplitRequest{
		SourcePersonID: merged.Person.ID, MergeID: merged.Merge.ID, ParticipantIDs: absorbed.ParticipantIDs,
		ExpectedSourceRevision: merged.Person.Revision, IdempotencyKey: "tier-replacement-split", Actor: "owner",
	})
	require.NoError(err)
	assert.True(split.ExactReversal)
	for _, table := range []string{"person_tracking", "person_brief_enrollments"} {
		for i, personID := range []int64{split.SourcePerson.ID, split.NewPerson.ID} {
			var provenance string
			require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT provenance FROM `+table+` WHERE person_id=?`), personID).Scan(&provenance))
			expected := "manual"
			if i == 1 {
				expected = "auto"
			}
			assert.Equal(expected, provenance)
		}
	}
}

func TestPersonTierSplitLegacyMergeJournalUsesMigrationDefaults(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, survivor, absorbed := tierLifecyclePeople(t)
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	_, err := st.DB().ExecContext(t.Context(), st.Rebind(`INSERT INTO person_tracking(person_id,tracked_at) VALUES(?,?)`), absorbed.ID, at)
	require.NoError(err)
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`INSERT INTO person_brief_enrollments(person_id,enabled_at,actor) VALUES(?,?,?)`), absorbed.ID, at, "legacy-owner")
	require.NoError(err)
	merged := tierMerge(t, st, survivor, absorbed)

	for _, journal := range []struct {
		table          string
		missingColumns []string
	}{
		{table: "person_tracking", missingColumns: []string{"provenance", "actor"}},
		{table: "person_brief_enrollments", missingColumns: []string{"provenance"}},
	} {
		var encoded string
		require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT post_merge_row_json FROM person_merge_rows WHERE merge_id=? AND table_name=?`), merged.Merge.ID, journal.table).Scan(&encoded))
		var snapshot map[string]json.RawMessage
		require.NoError(json.Unmarshal([]byte(encoded), &snapshot))
		var columns []map[string]json.RawMessage
		require.NoError(json.Unmarshal(snapshot["columns"], &columns))
		kept := make([]map[string]json.RawMessage, 0, len(columns))
		for _, column := range columns {
			var name string
			require.NoError(json.Unmarshal(column["name"], &name))
			missing := false
			for _, missingColumn := range journal.missingColumns {
				if name == missingColumn {
					missing = true
					break
				}
			}
			if !missing {
				kept = append(kept, column)
			}
		}
		snapshot["columns"], err = json.Marshal(kept)
		require.NoError(err)
		encodedBytes, err := json.Marshal(snapshot)
		require.NoError(err)
		_, err = st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE person_merge_rows SET post_merge_row_json=? WHERE merge_id=? AND table_name=?`), string(encodedBytes), merged.Merge.ID, journal.table)
		require.NoError(err)
	}

	split, err := st.SplitPersonMergeContext(t.Context(), store.PersonSplitRequest{
		SourcePersonID: merged.Person.ID, MergeID: merged.Merge.ID, ParticipantIDs: absorbed.ParticipantIDs,
		ExpectedSourceRevision: merged.Person.Revision, IdempotencyKey: "legacy-tier-split", Actor: "owner",
	})
	require.NoError(err)
	require.True(split.ExactReversal)
	for _, table := range []string{"person_tracking", "person_brief_enrollments"} {
		for _, person := range []store.Person{split.SourcePerson, split.NewPerson} {
			var count int
			require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT COUNT(*) FROM `+table+` WHERE person_id=?`), person.ID).Scan(&count))
			if person.ID == split.SourcePerson.ID {
				assert.Zero(count, "an unchanged legacy grant must move back to its original person")
			} else {
				assert.Equal(1, count, "the absorbed person must retain its original grant")
			}
		}
	}
}
