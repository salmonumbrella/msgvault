package store

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPersonNetworkIDValuesCTECastsEveryParameterOnlyIDRowAsBigint(t *testing.T) {
	tests := []struct {
		name    string
		cteName string
		want    string
	}{
		{
			name:    "person frontier",
			cteName: "frontier_people",
			want:    "frontier_people(id) AS (VALUES (CAST(? AS BIGINT)), (CAST(? AS BIGINT)))",
		},
		{
			name:    "organization frontier",
			cteName: "frontier_organizations",
			want:    "frontier_organizations(id) AS (VALUES (CAST(? AS BIGINT)), (CAST(? AS BIGINT)))",
		},
		{
			name:    "seen relationship edges",
			cteName: "admitted_relationships",
			want:    "admitted_relationships(id) AS (VALUES (CAST(? AS BIGINT)), (CAST(? AS BIGINT)))",
		},
		{
			name:    "seen employment edges",
			cteName: "admitted_employments",
			want:    "admitted_employments(id) AS (VALUES (CAST(? AS BIGINT)), (CAST(? AS BIGINT)))",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			query, args := personNetworkIDValuesCTE(test.cteName, []int64{9, 3})

			assert.Equal(t, test.want, query)
			assert.Equal(t, []any{int64(3), int64(9)}, args)
		})
	}
}

func TestPersonNetworkAdjacencyQueriesUseIndexedEdgeOrder(t *testing.T) {
	st, err := OpenForTest(filepath.Join(t.TempDir(), "network-query-plan.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, st.Close()) })
	require.NoError(t, st.InitSchema())

	tests := []struct {
		name        string
		query       string
		args        []any
		wantIndexes []string
	}{
		{
			name:        "current person employments",
			query:       st.personNetworkEmploymentAdjacencyQuery("person_id", false),
			args:        []any{int64(7), 501},
			wantIndexes: []string{"idx_employments_person"},
		},
		{
			name:        "current organization employments",
			query:       st.personNetworkEmploymentAdjacencyQuery("organization_id", false),
			args:        []any{int64(9), 501},
			wantIndexes: []string{"idx_employments_organization_current_edge"},
		},
		{
			name:        "all organization employments",
			query:       st.personNetworkEmploymentAdjacencyQuery("organization_id", true),
			args:        []any{int64(9), 501},
			wantIndexes: []string{"idx_employments_organization"},
		},
		{
			name:  "current relationships in both directions",
			query: personNetworkRelationshipAdjacencyQuery(false),
			args:  []any{int64(11), int64(11), 501},
			wantIndexes: []string{
				"idx_person_relationships_source",
				"idx_person_relationships_target",
			},
		},
		{
			name:  "all relationships in both directions",
			query: personNetworkRelationshipAdjacencyQuery(true),
			args:  []any{int64(13), int64(13), 501},
			wantIndexes: []string{
				"idx_person_relationships_source",
				"idx_person_relationships_target",
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			rows, err := st.db.QueryContext(t.Context(), "EXPLAIN QUERY PLAN "+test.query, test.args...)
			require.NoError(err)
			defer func() { require.NoError(rows.Close()) }()
			details := make([]string, 0)
			for rows.Next() {
				var id, parent, unused int
				var detail string
				require.NoError(rows.Scan(&id, &parent, &unused, &detail))
				details = append(details, detail)
			}
			require.NoError(rows.Err())
			plan := strings.Join(details, "\n")
			assert.NotContains(plan, "USE TEMP B-TREE")
			for _, index := range test.wantIndexes {
				assert.Contains(plan, index)
			}
		})
	}
}
