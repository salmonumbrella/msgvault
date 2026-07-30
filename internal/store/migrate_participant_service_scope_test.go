package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestServiceScopeBackfillClassifiesLegacyIdentifiers(t *testing.T) {
	if IsPostgresURL(os.Getenv("MSGVAULT_TEST_DB")) {
		t.Skip("SQLite file-path migration test")
	}
	require := require.New(t)
	assert := assert.New(t)
	st, err := OpenForTest(filepath.Join(t.TempDir(), "legacy.db"))
	require.NoError(err)
	t.Cleanup(func() { _ = st.Close() })
	require.NoError(st.InitSchema())
	for _, identifier := range []struct{ kind, value string }{
		{"email", "alice@example.com"},
		{"phone", "+12025550123"},
		{"imessage", "+12025550124"},
		{"matrix", "@alice:example.org"},
		{"example-unknown", "alice"},
	} {
		_, err := st.EnsureParticipantByIdentifier(
			identifier.kind, identifier.value, "Alice Example",
		)
		require.NoError(err)
	}
	_, err = st.db.Exec(`UPDATE participant_identifiers
		SET service_id = NULL, scope_kind = NULL, scope_value = NULL`)
	require.NoError(err)
	_, err = st.db.Exec(
		`DELETE FROM applied_migrations WHERE name = ?`, migrationParticipantServiceScope,
	)
	require.NoError(err)
	require.NoError(st.InitSchema())
	classified, err := st.classifiedIdentifierServiceSlugs(context.Background())
	require.NoError(err)
	assert.Equal("imessage", classified["imessage:+12025550124"])
	assert.Equal("matrix", classified["matrix:@alice:example.org"])
	assert.Empty(classified["email:alice@example.com"])
	assert.Empty(classified["example-unknown:alice"])
	applied, err := st.IsMigrationApplied(migrationParticipantServiceScope)
	require.NoError(err)
	assert.True(applied)
}
