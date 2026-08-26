package peoplesweep

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	capabilityArchiveCanary   = "archive-message-canary-never-send"
	capabilityCredentialValue = "credential-canary-never-report"
	capabilityResponseCanary  = "provider-body-canary-never-report"
)

type capabilityAttempt struct {
	path string
	body map[string]any
}

func TestCapabilityNegotiationUsesFixedOutputAndTokenOrderWithoutArchiveContext(t *testing.T) {
	var mu sync.Mutex
	var attempts []capabilityAttempt
	statuses := []int{http.StatusBadRequest, http.StatusUnprocessableEntity,
		http.StatusNotFound, http.StatusOK}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		encoded, err := json.Marshal(body)
		require.NoError(t, err)
		assert.NotContains(t, string(encoded), capabilityArchiveCanary)
		assert.NotContains(t, string(encoded), capabilityCredentialValue)
		assert.Equal(t, "Bearer "+capabilityCredentialValue, r.Header.Get("Authorization"))

		mu.Lock()
		attempt := len(attempts)
		attempts = append(attempts, capabilityAttempt{path: r.URL.Path, body: body})
		mu.Unlock()
		status := statuses[attempt]
		w.WriteHeader(status)
		if status == http.StatusOK {
			_, _ = w.Write([]byte(`{"model":"synthetic-model-version","choices":[{"message":{"content":"{\"ok\":true}"}}]}`))
			return
		}
		_, _ = w.Write([]byte(capabilityResponseCanary))
	}))
	t.Cleanup(server.Close)

	registry, err := NewDriverRegistry(server.Client(), nil, nil)
	require.NoError(t, err)
	candidate := capabilityTestCandidate(ProtocolOpenAIChat, server.URL)
	candidate.RetentionPosture = capabilityArchiveCanary
	candidate.TrainingPosture = capabilityArchiveCanary
	candidate.SourceSince = capabilityArchiveCanary
	candidate.SourceUntil = capabilityArchiveCanary
	candidate.AllowedSources = []SourceClass{SourceConversationText, SourceMeetingText}
	candidate.AllowSensitive = true

	got, err := NewCapabilityChecker(registry).Negotiate(t.Context(), candidate,
		NewCredential(AuthBearer, capabilityCredentialValue))
	require.NoError(t, err)
	assert.Equal(t, OutputModeJSONObject, got.OutputMode)
	assert.Equal(t, "max_tokens", got.TokenLimitParameter)
	assert.Equal(t, defaultDriverVersion(ProtocolOpenAIChat), got.DriverVersion)
	assert.JSONEq(t, `{"ok":true}`, string(got.Response.Output))

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, attempts, 4)
	for _, attempt := range attempts {
		assert.Equal(t, "/chat/completions", attempt.path)
		assert.Equal(t, "synthetic-model", attempt.body["model"])
		assert.NotContains(t, attempt.body, "max_output_tokens")
	}
	assert.Equal(t, "json_schema", responseFormatType(t, attempts[0].body))
	assert.Contains(t, attempts[0].body, "max_completion_tokens")
	assert.Equal(t, "json_schema", responseFormatType(t, attempts[1].body))
	assert.Contains(t, attempts[1].body, "max_tokens")
	assert.Equal(t, "json_object", responseFormatType(t, attempts[2].body))
	assert.Contains(t, attempts[2].body, "max_completion_tokens")
	assert.Equal(t, "json_object", responseFormatType(t, attempts[3].body))
	assert.Contains(t, attempts[3].body, "max_tokens")
}

func TestCapabilityNegotiationChecksRequestedReasoningSeparately(t *testing.T) {
	for _, test := range []struct {
		name       string
		reasonCode int
		wantErr    bool
	}{
		{name: "accepted", reasonCode: http.StatusOK},
		{name: "rejected", reasonCode: http.StatusBadRequest, wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				call := calls.Add(1)
				var body map[string]any
				require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
				if call == 1 {
					assert.NotContains(t, body, "reasoning_effort")
					assert.NotContains(t, body, "reasoning")
				} else {
					assert.Equal(t, "high", body["reasoning_effort"])
					reasoning, ok := body["reasoning"].(map[string]any)
					require.True(t, ok)
					assert.Equal(t, true, reasoning["enabled"])
				}
				if call == 2 && test.reasonCode != http.StatusOK {
					w.WriteHeader(test.reasonCode)
					_, _ = w.Write([]byte(capabilityResponseCanary))
					return
				}
				_, _ = w.Write([]byte(`{"model":"reasoning-model-version","choices":[{"message":{"content":"{\"ok\":true}"}}]}`))
			}))
			t.Cleanup(server.Close)
			registry, err := NewDriverRegistry(server.Client(), nil, nil)
			require.NoError(t, err)
			candidate := capabilityTestCandidate(ProtocolOpenAIChat, server.URL)
			candidate.ReasoningEffort = "high"
			candidate.ReasoningMode = "enabled"

			got, negotiationErr := NewCapabilityChecker(registry).Negotiate(t.Context(), candidate,
				NewCredential(AuthBearer, capabilityCredentialValue))
			assert.Equal(t, int32(2), calls.Load())
			if test.wantErr {
				require.Error(t, negotiationErr)
				assert.Empty(t, got)
				assert.NotContains(t, negotiationErr.Error(), capabilityResponseCanary)
				assert.NotContains(t, negotiationErr.Error(), capabilityCredentialValue)
				return
			}
			require.NoError(t, negotiationErr)
			assert.Equal(t, "high", got.ReasoningEffort)
			assert.Equal(t, "enabled", got.ReasoningMode)
		})
	}
}

func TestCapabilityNegotiationStopsOnNonCapabilityFailuresAndInvalidOutput(t *testing.T) {
	for _, test := range []struct {
		name       string
		status     int
		response   string
		wait       bool
		credential Credential
	}{
		{name: "authentication", status: http.StatusUnauthorized, response: capabilityResponseCanary,
			credential: NewCredential(AuthBearer, capabilityCredentialValue)},
		{name: "request timeout", status: http.StatusRequestTimeout, response: capabilityResponseCanary,
			credential: NewCredential(AuthBearer, capabilityCredentialValue)},
		{name: "rate limit", status: http.StatusTooManyRequests, response: capabilityResponseCanary,
			credential: NewCredential(AuthBearer, capabilityCredentialValue)},
		{name: "provider failure", status: http.StatusInternalServerError, response: capabilityResponseCanary,
			credential: NewCredential(AuthBearer, capabilityCredentialValue)},
		{name: "unsafe response", status: http.StatusOK,
			response:   `{"model":"unsafe model version","choices":[{"message":{"content":"{\"ok\":true}"}}]}`,
			credential: NewCredential(AuthBearer, capabilityCredentialValue)},
		{name: "locally invalid output", status: http.StatusOK,
			response:   `{"model":"synthetic-model-version","choices":[{"message":{"content":"{\"ok\":false}"}}]}`,
			credential: NewCredential(AuthBearer, capabilityCredentialValue)},
		{name: "transport timeout", wait: true,
			credential: NewCredential(AuthBearer, capabilityCredentialValue)},
		{name: "credential validation", status: http.StatusOK,
			response:   `{"model":"synthetic-model-version","choices":[{"message":{"content":"{\"ok\":true}"}}]}`,
			credential: NewCredential(AuthXAPIKey, capabilityCredentialValue)},
	} {
		t.Run(test.name, func(t *testing.T) {
			var calls atomic.Int32
			release := make(chan struct{})
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if test.wait {
					select {
					case <-r.Context().Done():
					case <-release:
					}
					return
				}
				w.WriteHeader(test.status)
				_, _ = w.Write([]byte(test.response))
			}))
			t.Cleanup(server.Close)
			registry, err := NewDriverRegistry(server.Client(), nil, nil)
			require.NoError(t, err)
			candidate := capabilityTestCandidate(ProtocolOpenAIChat, server.URL)
			ctx := t.Context()
			if test.wait {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, 25*time.Millisecond)
				defer cancel()
			}

			got, negotiationErr := NewCapabilityChecker(registry).Negotiate(ctx, candidate, test.credential)
			if test.wait {
				close(release)
			}
			require.Error(t, negotiationErr)
			assert.Empty(t, got)
			assert.LessOrEqual(t, calls.Load(), int32(1))
			assert.NotContains(t, negotiationErr.Error(), capabilityResponseCanary)
			assert.NotContains(t, negotiationErr.Error(), capabilityCredentialValue)
			assert.NotContains(t, negotiationErr.Error(), capabilityArchiveCanary)
		})
	}
}

func TestCapabilityNegotiationNeverSwitchesProtocolEndpointOrModel(t *testing.T) {
	var paths []string
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		var body map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		assert.Equal(t, "synthetic-model", body["model"])
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(capabilityResponseCanary))
	}))
	t.Cleanup(server.Close)
	registry, err := NewDriverRegistry(server.Client(), nil, nil)
	require.NoError(t, err)

	got, err := NewCapabilityChecker(registry).Negotiate(t.Context(),
		capabilityTestCandidate(ProtocolOpenAIChat, server.URL),
		NewCredential(AuthBearer, capabilityCredentialValue))
	require.Error(t, err)
	assert.Empty(t, got)
	assert.Len(t, paths, 6)
	for _, path := range paths {
		assert.Equal(t, "/chat/completions", path)
	}
}

func TestCapabilityNegotiationRejectsUnsupportedReasoningBeforeIO(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
	t.Cleanup(server.Close)
	registry, err := NewDriverRegistry(server.Client(), nil, nil)
	require.NoError(t, err)
	candidate := capabilityTestCandidate(ProtocolAnthropicMessages, server.URL)
	candidate.Auth = AuthXAPIKey
	candidate.ReasoningEffort = "high"

	_, err = NewCapabilityChecker(registry).Negotiate(t.Context(), candidate,
		NewCredential(AuthXAPIKey, capabilityCredentialValue))
	require.Error(t, err)
	assert.Equal(t, int32(0), calls.Load())
	assert.NotContains(t, err.Error(), capabilityCredentialValue)
}

func TestCapabilityNegotiationRejectsMissingRegistryWithoutPanic(t *testing.T) {
	_, err := NewCapabilityChecker(nil).Negotiate(t.Context(),
		capabilityTestCandidate(ProtocolOpenAIChat, "https://example.test/v1"),
		NewCredential(AuthBearer, capabilityCredentialValue))
	require.Error(t, err)
	assert.False(t, errors.Is(err, context.Canceled))
}

func capabilityTestCandidate(protocol Protocol, endpoint string) ProviderConfig {
	return ProviderConfig{
		Protocol: protocol, Endpoint: endpoint, Model: "synthetic-model",
		Auth: AuthBearer, Credential: CredentialStored,
		RequestTimeout: 5 * time.Second,
	}
}

func responseFormatType(t *testing.T, body map[string]any) string {
	t.Helper()
	format, ok := body["response_format"].(map[string]any)
	require.True(t, ok)
	typeName, ok := format["type"].(string)
	require.True(t, ok)
	return typeName
}
