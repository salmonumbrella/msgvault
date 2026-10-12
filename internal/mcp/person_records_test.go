package mcp

import (
	"encoding/json/jsontext"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sync/atomic"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/query/querytest"
)

// Malformed remote responses cannot be produced by an honest Store success.
// Only the HTTP response is synthetic; the typed daemon client, SDK, native
// handler and confirmation challenge are the production implementations.
func personAttributeResponseSession(t *testing.T, preview *atomic.Value, writes *atomic.Int32) *sdkmcp.ClientSession {
	t.Helper()
	assert := assert.New(t)
	require := require.New(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			assert.Equal("/api/v1/people/7/attributes", r.URL.Path)
			w.Header().Set("ETag", `"person-7-r1"`)
			_, _ = io.WriteString(w, `{"person_id":7,"attributes":[]}`)
			return
		}
		assert.Equal("/api/v1/people/7/attributes/synthetic_json", r.URL.Path)
		assert.Equal(`"person-7-r1"`, r.Header.Get("If-Match"))
		if r.URL.Query().Get("dry_run") == "true" {
			body, ok := preview.Load().(string)
			if !assert.True(ok) {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			_, _ = io.WriteString(w, body)
			return
		}
		writes.Add(1)
		_, _ = io.WriteString(w, `{"dry_run":false}`)
	}))
	t.Cleanup(server.Close)
	backend, err := daemonclient.New(daemonclient.Config{URL: server.URL, AllowInsecure: true, HTTPClient: server.Client()})
	require.NoError(err)
	t.Cleanup(func() { assert.NoError(backend.Close()) })
	clientTransport, serverTransport := sdkmcp.NewInMemoryTransports()
	serverSession, err := newMCPServer(ServeOptions{Engine: &querytest.MockEngine{}, PersonRecords: backend, AllowProfileWrites: true}, true).Connect(t.Context(), serverTransport, nil)
	require.NoError(err)
	t.Cleanup(func() { assert.NoError(serverSession.Close()) })
	client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "contact-preview-test", Version: "1.0"}, &sdkmcp.ClientOptions{MultiRoundTrip: &sdkmcp.MultiRoundTripOptions{Disabled: true}})
	session, err := client.Connect(t.Context(), clientTransport, nil)
	require.NoError(err)
	t.Cleanup(func() { assert.NoError(session.Close()) })
	return session
}

func TestPersonAttributePreviewMustBeDryRun(t *testing.T) {
	for _, tool := range []string{"set_person_attribute", "clear_person_attribute"} {
		for _, body := range []string{"null", `{}`, `{"dry_run":false}`, `{"dry_run":"true"}`, `{"dry_run":`} {
			t.Run(tool+"/"+body, func(t *testing.T) {
				assert := assert.New(t)
				require := require.New(t)
				var preview atomic.Value
				preview.Store(body)
				var writes atomic.Int32
				session := personAttributeResponseSession(t, &preview, &writes)
				args := map[string]any{"person_id": 7, "etag": `"person-7-r1"`, "attribute_slug": "synthetic_json", "expected_value_id": 9}
				if tool == "set_person_attribute" {
					args["value"] = map[string]any{"type": "json", "json": map[string]any{"a": 1}}
				}
				result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: tool, Arguments: args})
				require.NoError(err)
				assert.True(result.IsError, "invalid preview must fail closed")
				assert.False(result.NeedsInput(), "invalid preview must not request confirmation")
				assert.Zero(writes.Load(), "invalid preview must not execute a real mutation")
			})
		}
	}
}

func TestPersonAttributeConfirmationBindsStablePreview(t *testing.T) {
	const first = `{"person_id":7,"etag":"\"person-7-r1\"","attribute_slug":"synthetic_json","expected_value_id":9,"value":{"type":"json","json":{"b":2,"a":1}}}`
	const reordered = `{"value":{"json":{"a":1,"b":2},"type":"json"},"expected_value_id":9,"attribute_slug":"synthetic_json","etag":"\"person-7-r1\"","person_id":7}`
	hashPattern := regexp.MustCompile(`Preview SHA256: ([a-f0-9]{64})`)
	for _, tool := range []string{"set_person_attribute", "clear_person_attribute"} {
		for _, change := range []string{"clock_and_raw_order", "normalized_value", "resolved_slot"} {
			t.Run(tool+"/"+change, func(t *testing.T) {
				assert := assert.New(t)
				require := require.New(t)
				field := "value"
				firstArgs, secondArgs := first, reordered
				if tool == "clear_person_attribute" {
					field = "superseded"
					firstArgs = `{"person_id":7,"etag":"\"person-7-r1\"","attribute_slug":"synthetic_json","expected_value_id":9}`
					secondArgs = `{"expected_value_id":9,"attribute_slug":"synthetic_json","etag":"\"person-7-r1\"","person_id":7}`
				}
				response := func(id, ordinal int, value, clock string) string {
					return fmt.Sprintf(`{"dry_run":true,%q:{"id":%d,"person_id":7,"definition_id":2,"definition_slug":"synthetic_json","ordinal":%d,"value":{"type":"json","json":%s},"active_from":%q,"created_at":%q,"source":"manual"}}`, field, id, ordinal, value, clock, clock)
				}
				var preview atomic.Value
				preview.Store(response(9, 3, `{"b":2,"a":1}`, "2026-01-01T00:00:00Z"))
				var writes atomic.Int32
				session := personAttributeResponseSession(t, &preview, &writes)
				pending, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: tool, Arguments: jsontext.Value(firstArgs)})
				require.NoError(err)
				require.True(pending.NeedsInput())
				prompt, ok := pending.InputRequests["confirm"].(*sdkmcp.ElicitParams)
				require.True(ok)
				assert.Contains(prompt.Message, `"ordinal":3`, "disclose the resolved native slot")
				assert.NotContains(prompt.Message, "2026-01-01", "trial clocks do not bind confirmation")
				ordinal, value := 3, `{"a":1,"b":2}`
				switch change {
				case "normalized_value":
					value = `{"a":1,"b":4}`
				case "resolved_slot":
					ordinal = 4
				}
				preview.Store(response(99, ordinal, value, "2026-01-02T00:00:00Z"))
				again, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: tool, Arguments: jsontext.Value(secondArgs)})
				require.NoError(err)
				require.True(again.NeedsInput())
				second, ok := again.InputRequests["confirm"].(*sdkmcp.ElicitParams)
				require.True(ok)
				firstHash := hashPattern.FindStringSubmatch(prompt.Message)
				secondHash := hashPattern.FindStringSubmatch(second.Message)
				require.Len(firstHash, 2)
				require.Len(secondHash, 2)
				if change == "clock_and_raw_order" {
					assert.Equal(prompt.Message, second.Message, "equivalent raw JSON and changed trial clocks/IDs must preserve disclosure and hash")
					assert.Equal(firstHash[1], secondHash[1])
				} else {
					assert.NotEqual(prompt.Message, second.Message, "changed normalized value or resolved slot must change disclosure and hash")
					assert.NotEqual(firstHash[1], secondHash[1])
				}
				result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: tool, Arguments: jsontext.Value(secondArgs), RequestState: pending.RequestState, InputResponses: sdkmcp.InputResponseMap{"confirm": &sdkmcp.ElicitResult{Action: "accept", Content: map[string]any{"confirm": true}}}})
				require.NoError(err)
				if change == "clock_and_raw_order" {
					assert.False(result.IsError, "one valid preview and matching confirmation permit one real write")
					assert.Equal(int32(1), writes.Load())
				} else {
					assert.True(result.IsError, "confirmation must reject changed native preview semantics")
					assert.Zero(writes.Load())
				}
			})
		}
	}
}

func TestPersonAttributeRejectsUnsafeRawNumbers(t *testing.T) {
	for _, tc := range []struct {
		name, personID, expectedID, ordinal, value string
	}{
		{"fractional person ID", "7.0000000000000001", "9", "3", `{"type":"json","json":{"a":1}}`},
		{"fractional value ID", "7", "9.0000000000000001", "3", `{"type":"json","json":{"a":1}}`},
		{"fractional ordinal", "7", "9", "3.0000000000000001", `{"type":"json","json":{"a":1}}`},
		{"oversized JSON number", "7", "9", "3", `{"type":"json","json":{"nested":9007199254740991.1}}`},
		{"negative oversized JSON number", "7", "9", "3", `{"type":"json","json":{"nested":-9007199254740991.1}}`},
		{"fractional integer value", "7", "9", "3", `{"type":"integer","integer":1.0000000000000001}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			var preview atomic.Value
			preview.Store(`{"dry_run":true,"value":{"id":9,"person_id":7,"definition_id":2,"definition_slug":"synthetic_json","ordinal":3,"value":{"type":"json","json":{"a":1}},"active_from":"2026-01-01T00:00:00Z","created_at":"2026-01-01T00:00:00Z","source":"manual"}}`)
			var writes atomic.Int32
			session := personAttributeResponseSession(t, &preview, &writes)
			args := fmt.Sprintf(`{"person_id":%s,"etag":"\"person-7-r1\"","attribute_slug":"synthetic_json","expected_value_id":%s,"ordinal":%s,"value":%s}`, tc.personID, tc.expectedID, tc.ordinal, tc.value)
			result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "set_person_attribute", Arguments: jsontext.Value(args)})
			require.NoError(err)
			assert.True(result.IsError, "raw numeric values must be validated before SDK float conversion can round them")
			assert.False(result.NeedsInput())
			assert.Zero(writes.Load())
		})
	}
}
