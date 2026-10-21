package daemonclient

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMCPCapabilitiesFailClosed(t *testing.T) {
	tests := []struct {
		name, body string
		status     int
	}{
		{"old daemon", `{"error":"not_found"}`, http.StatusNotFound},
		{"unauthorized", `{"error":"unauthorized"}`, http.StatusUnauthorized},
		{"missing version", `{"routes":[]}`, http.StatusOK},
		{"unknown version", `{"version":2,"routes":[]}`, http.StatusOK},
		{"malformed", `{"version":1`, http.StatusOK},
		{"multiple values", `{"version":1} {"version":1}`, http.StatusOK},
		{"oversized", strings.Repeat(" ", 1<<20) + `{"version":1}`, http.StatusOK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
				_, _ = io.WriteString(w, tt.body)
			}))
			t.Cleanup(server.Close)
			client, err := New(Config{URL: server.URL, APIKey: "synthetic-key", AllowInsecure: true})
			requirements.NoError(err)
			capabilities, err := client.MCPCapabilities(context.Background())
			requirements.Error(err)
			assertions.Nil(capabilities)
		})
	}
}

func TestMCPCapabilitiesRetainsRouteParameters(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	seen := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/health" {
			_, _ = io.WriteString(w, `{"api_schema_version":"3.16.0"}`)
			return
		}
		seen <- r.Header.Get("X-Api-Key")
		_, _ = io.WriteString(w, `{"version":1,"routes":[{"operation_id":"listSourceStatus","method":"GET","path":"/api/v1/sources/status","query_parameters":["source_type"]}]}`)
	}))
	t.Cleanup(server.Close)
	client, err := New(Config{URL: server.URL, APIKey: "synthetic-key", AllowInsecure: true})
	requirements.NoError(err)
	capabilities, err := client.MCPCapabilities(context.Background())
	requirements.NoError(err)
	requirements.Len(capabilities.Routes, 1)
	assertions.Equal("synthetic-key", <-seen)
	assertions.Equal([]string{"source_type"}, capabilities.Routes[0].QueryParameters)
}

func TestMCPCapabilitiesReusesSchemaProbe(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	var healthRequests, capabilityRequests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/health":
			healthRequests.Add(1)
			_, _ = io.WriteString(w, `{"status":"ok","api_schema_version":"3.15.0"}`)
		case "/api/v1/mcp/capabilities":
			capabilityRequest := capabilityRequests.Add(1)
			operations := `"get_state"`
			if capabilityRequest > 1 {
				operations = ""
			}
			_, _ = io.WriteString(w, `{"version":1,"routes":[],"inbox_operations":[`+operations+`]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	client, err := New(Config{URL: server.URL, APIKey: "synthetic-key", AllowInsecure: true})
	requirements.NoError(err)

	for i := range 3 {
		capabilities, err := client.MCPCapabilities(t.Context())
		requirements.NoError(err)
		requirements.NotNil(capabilities)
		if i == 0 {
			assertions.Equal([]string{"get_state"}, capabilities.InboxOperations)
		} else {
			assertions.Empty(capabilities.InboxOperations, "live caller admission is refreshed")
		}
	}
	assertions.Equal(int64(1), healthRequests.Load(), "compatible schema is stable for this client")
	assertions.Equal(int64(3), capabilityRequests.Load(), "caller admission is refreshed for every operation")
}

func TestMCPCapabilitiesOwnerRouteIdentities(t *testing.T) {
	for name, body := range map[string]string{
		"missing operation":   `{"version":1,"routes":[{"method":"GET","path":"/api/v1/sources"}]}`,
		"missing method":      `{"version":1,"routes":[{"operation_id":"getSources","path":"/api/v1/sources"}]}`,
		"unsafe path":         `{"version":1,"routes":[{"operation_id":"getSources","method":"GET","path":"https://example.com"}]}`,
		"duplicate operation": `{"version":1,"routes":[{"operation_id":"getSources","method":"GET","path":"/api/v1/sources"},{"operation_id":"getSources","method":"POST","path":"/api/v1/sources"}]}`,
		"duplicate parameter": `{"version":1,"routes":[{"operation_id":"getSources","method":"GET","path":"/api/v1/sources","query_parameters":["limit","limit"]}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, body) }))
			t.Cleanup(server.Close)
			client, err := New(Config{URL: server.URL, AllowInsecure: true})
			requirements.NoError(err)
			capabilities, err := client.MCPCapabilities(context.Background())
			requirements.Error(err)
			assertions.Nil(capabilities)
		})
	}
}
