package store_test

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestFederationLinks(t *testing.T) {
	st := testutil.NewTestStore(t)
	link := store.FederationLink{HubArchiveUID: "hub", HubOrigin: "https://example.com", EnrollmentUID: "enrollment", Member: "alice", PolicyJSON: `{}`, PolicyDigest: "digest"}
	require.NoError(t, st.CreateFederationLinkContext(t.Context(), link))
	links, err := st.ListFederationLinksContext(t.Context())
	require.NoError(t, err)
	require.Len(t, links, 1)
	assert.Equal(t, "active", links[0].State)
	assert.Equal(t, link.HubOrigin, links[0].HubOrigin)
	other := link
	other.HubArchiveUID = "other"
	assert.Error(t, st.CreateFederationLinkContext(t.Context(), other))
	require.NoError(t, st.SetFederationLinkStateContext(t.Context(), link.HubArchiveUID, "identity_changed"))
	links, err = st.ListFederationLinksContext(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "identity_changed", links[0].State)
	require.NoError(t, st.DeleteFederationLinkContext(t.Context(), link.HubArchiveUID))
	links, err = st.ListFederationLinksContext(t.Context())
	require.NoError(t, err)
	assert.Empty(t, links)
}

func TestFederationLinksReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "archive.db")
	st := testutil.NewSQLiteTestStoreAt(t, path)
	link := store.FederationLink{HubArchiveUID: "hub", HubOrigin: "https://example.com", EnrollmentUID: "enrollment", Member: "alice", PolicyJSON: `{}`, PolicyDigest: "digest"}
	require.NoError(t, st.CreateFederationLinkContext(t.Context(), link))
	reopened, err := store.OpenForTest(t.Context(), path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reopened.Close()) })
	require.NoError(t, reopened.InitSchemaUnlessCurrentContext(t.Context()))
	links, err := reopened.ListFederationLinksContext(t.Context())
	require.NoError(t, err)
	require.Len(t, links, 1)
	assert.Equal(t, link.HubOrigin, links[0].HubOrigin)
}
