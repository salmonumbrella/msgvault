package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

func personAttributesPath(personID int64) string {
	return fmt.Sprintf("%s/%d/attributes", personsPath, personID)
}

func newPersonAttributeFixture(t *testing.T) (*Server, int64) {
	t.Helper()
	srv, st := newIdentityLinkTestServer(t)
	participant := st.mustParticipant(t, "alice@example.com", "alice", "example.com")
	person, _, err := st.CreatePersonFromParticipant(participant)
	require.NoError(t, err)
	return srv, person.ID
}

func decodePersonAttributes(
	t *testing.T, body []byte,
) map[string]PersonAttributeGroup {
	t.Helper()
	var response PersonAttributesResponse
	require.NoError(t, json.Unmarshal(body, &response))
	grouped := make(map[string]PersonAttributeGroup, len(response.Attributes))
	for _, group := range response.Attributes {
		grouped[group.Definition.Slug] = group
	}
	return grouped
}

func TestPersonAttributesHTTPListsDefinitionsSetsHistoryAndClears(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	srv, person := newPersonAttributeFixture(t)

	listed := attributeRequest(t, srv, http.MethodGet,
		personAttributesPath(person), nil, "")
	require.Equal(http.StatusOK, listed.Code, listed.Body.String())
	groups := decodePersonAttributes(t, listed.Body.Bytes())
	require.Len(groups, 4)
	assert.Empty(groups[store.AttributeSlugLastContacted].Current)

	path := personAttributesPath(person) + "/" + store.AttributeSlugPrimaryChannel
	first := attributeRequest(t, srv, http.MethodPut, path,
		[]byte(`{"value":{"type":"text","text":"email"},"source":"user"}`), "")
	require.Equal(http.StatusOK, first.Code, first.Body.String())
	var firstWrite store.PersonAttributeWrite
	require.NoError(json.Unmarshal(first.Body.Bytes(), &firstWrite))

	second := attributeRequest(t, srv, http.MethodPut, path,
		[]byte(`{"value":{"type":"text","text":"chat"},"source":"user"}`), "")
	require.Equal(http.StatusOK, second.Code, second.Body.String())
	var secondWrite store.PersonAttributeWrite
	require.NoError(json.Unmarshal(second.Body.Bytes(), &secondWrite))
	require.NotNil(secondWrite.Superseded)
	assert.Equal(firstWrite.Value.ID, secondWrite.Superseded.ID)

	history := attributeRequest(t, srv, http.MethodGet,
		personAttributesPath(person)+"?history=true", nil, "")
	require.Equal(http.StatusOK, history.Code)
	historyGroups := decodePersonAttributes(t, history.Body.Bytes())
	assert.Len(historyGroups[store.AttributeSlugPrimaryChannel].History, 2)

	cleared := attributeRequest(t, srv, http.MethodDelete, path, nil, "")
	require.Equal(http.StatusOK, cleared.Code, cleared.Body.String())
}

func TestPersonAttributesHTTPRejectsDerivedInvalidUnknownAndSupportsDryRun(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	srv, person := newPersonAttributeFixture(t)

	derived := attributeRequest(t, srv, http.MethodPut,
		personAttributesPath(person)+"/"+store.AttributeSlugLastContacted,
		[]byte(`{"value":{"type":"timestamp","timestamp":"2026-07-30T09:00:00Z"},
			"source":"system"}`), "")
	assert.Equal(http.StatusConflict, derived.Code)

	invalid := attributeRequest(t, srv, http.MethodPut,
		personAttributesPath(person)+"/"+store.AttributeSlugPrimaryChannel,
		[]byte(`{"value":{"type":"text","text":"carrier_pigeon"},"source":"user"}`), "")
	assert.Equal(http.StatusBadRequest, invalid.Code)

	unknown := attributeRequest(t, srv, http.MethodPut,
		personAttributesPath(person)+"/no_such_definition",
		[]byte(`{"value":{"type":"text","text":"x"},"source":"user"}`), "")
	assert.Equal(http.StatusNotFound, unknown.Code)

	preview := attributeRequest(t, srv, http.MethodPut,
		personAttributesPath(person)+"/"+store.AttributeSlugPrimaryChannel+"?dry_run=true",
		[]byte(`{"value":{"type":"text","text":"email"},"source":"user"}`), "")
	require.Equal(http.StatusOK, preview.Code, preview.Body.String())
	var write store.PersonAttributeWrite
	require.NoError(json.Unmarshal(preview.Body.Bytes(), &write))
	assert.True(write.DryRun)
	assert.Zero(write.Value.ID)

	missing := attributeRequest(t, srv, http.MethodGet,
		personAttributesPath(999_999), nil, "")
	assert.Equal(http.StatusNotFound, missing.Code)
}
