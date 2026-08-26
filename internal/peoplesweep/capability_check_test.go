package peoplesweep

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
		_, _ = w.Write([]byte(`{"error":{"type":"invalid_request_error","code":"unsupported_parameter","param":"response_format","message":"` + capabilityResponseCanary + `"}}`))
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
		{name: "duplicate output members", status: http.StatusOK,
			response:   `{"model":"synthetic-model-version","choices":[{"message":{"content":"{\"ok\":true,\"ok\":false}"}}]}`,
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

func TestCapabilityNegotiationRejectsAmbiguousSyntheticJSON(t *testing.T) {
	for _, output := range []string{
		`{"ok":true,"ok":false}`, `{"ok":false,"ok":true}`, `{"ok":true,"extra":true}`,
		`{"ok":true} {"ok":true}`, `{"ok":null}`, `{"ok":"true"}`,
	} {
		t.Run(output, func(t *testing.T) {
			got, err := validateCapabilityResponse(DriverResponse{
				CandidateJSON: []byte(output), ModelVersion: "synthetic-model-version",
			})
			require.Error(t, err)
			assert.Empty(t, got)
			assert.NotContains(t, err.Error(), output)
		})
	}
}

func TestCapabilityErrorClassificationRequiresProtocolSpecificStructuredCode(t *testing.T) {
	for _, test := range []struct {
		name     string
		protocol Protocol
		body     string
		want     ProviderCapabilityError
	}{
		{name: "openai chat", protocol: ProtocolOpenAIChat, body: `{"error":{"type":"invalid_request_error","code":"unsupported_parameter","param":"response_format","message":"secret"}}`, want: ProviderCapabilityUnsupportedRepresentation},
		{name: "openai responses", protocol: ProtocolOpenAIResponses, body: `{"error":{"type":"invalid_request_error","code":"unsupported_value","param":"text.format","message":"secret"}}`, want: ProviderCapabilityUnsupportedRepresentation},
		{name: "anthropic", protocol: ProtocolAnthropicMessages, body: `{"type":"error","error":{"type":"invalid_request_error","code":"unsupported_parameter","param":"tools","message":"secret"}}`, want: ProviderCapabilityUnsupportedRepresentation},
		{name: "google", protocol: ProtocolGoogleGenerateContent, body: `{"error":{"code":400,"status":"INVALID_ARGUMENT","message":"secret","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"UNSUPPORTED_PARAMETER","domain":"generativelanguage.googleapis.com","metadata":{"parameter":"generationConfig.responseSchema"}}]}}`, want: ProviderCapabilityUnsupportedRepresentation},
		{name: "openai generic", protocol: ProtocolOpenAIChat, body: `{"error":{"type":"invalid_request_error","message":"unsupported parameter secret"}}`},
		{name: "openai wrong model parameter", protocol: ProtocolOpenAIChat, body: `{"error":{"type":"invalid_request_error","code":"unsupported_value","param":"model","message":"secret"}}`},
		{name: "openai unknown parameter", protocol: ProtocolOpenAIChat, body: `{"error":{"type":"invalid_request_error","code":"unsupported_parameter","param":"unknown","message":"secret"}}`},
		{name: "anthropic generic", protocol: ProtocolAnthropicMessages, body: `{"type":"error","error":{"type":"invalid_request_error","message":"unsupported parameter secret"}}`},
		{name: "google generic", protocol: ProtocolGoogleGenerateContent, body: `{"error":{"code":400,"status":"INVALID_ARGUMENT","message":"unsupported parameter secret"}}`},
		{name: "malformed", protocol: ProtocolOpenAIChat, body: `{"error":`},
	} {
		t.Run(test.name, func(t *testing.T) {
			tokenParameter := ""
			if test.protocol == ProtocolOpenAIChat {
				tokenParameter = "max_tokens"
			}
			profile, err := capabilityProfile(capabilityTestCandidate(test.protocol, "https://example.test"), OutputModeNativeJSONSchema, tokenParameter, false)
			require.NoError(t, err)
			assert.Equal(t, test.want, classifyProviderCapabilityError(profile, []byte(test.body)))
		})
	}
}

func TestCapabilityParameterMustBeExactlyEmittedByAttempt(t *testing.T) {
	for _, test := range []struct {
		protocol  Protocol
		mode      OutputMode
		reasoning string
		parameter string
		want      bool
	}{
		{ProtocolOpenAIChat, OutputModeNativeJSONSchema, "", "response_format.json_schema", true},
		{ProtocolOpenAIChat, OutputModeJSONObject, "", "response_format.json_schema", false},
		{ProtocolOpenAIChat, OutputModeJSONObject, "", "response_format.model", false},
		{ProtocolOpenAIChat, OutputModePromptJSON, "", "response_format", false},
		{ProtocolOpenAIResponses, OutputModeNativeJSONSchema, "", "reasoning.effort", false},
		{ProtocolOpenAIResponses, OutputModeNativeJSONSchema, "high", "reasoning.effort", true},
		{ProtocolOpenAIResponses, OutputModeJSONObject, "", "text.format.schema", false},
		{ProtocolAnthropicMessages, OutputModeNativeJSONSchema, "", "tools.0", false},
		{ProtocolGoogleGenerateContent, OutputModeNativeJSONSchema, "", "generationconfig.responseSchema", false},
		{ProtocolGoogleGenerateContent, OutputModePromptJSON, "", "generationConfig.responseSchema", false},
	} {
		candidate := capabilityTestCandidate(test.protocol, "https://example.test")
		candidate.ReasoningEffort = test.reasoning
		profile, err := capabilityProfile(candidate, test.mode, map[bool]string{true: "max_tokens"}[test.protocol == ProtocolOpenAIChat], test.reasoning != "")
		require.NoError(t, err)
		assert.Equal(t, test.want, capabilityParameterMatchesProfile(profile, test.parameter))
	}
}

func TestCapabilityMissRequiresAllowedStatusAndClassification(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusRequestTimeout,
		http.StatusConflict, http.StatusTooManyRequests, http.StatusInternalServerError} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			assert.False(t, capabilityMiss(&ProviderError{StatusCode: status, Capability: ProviderCapabilityUnsupportedRepresentation}))
		})
	}
	for _, status := range []int{http.StatusBadRequest, http.StatusNotFound, http.StatusUnprocessableEntity} {
		assert.True(t, capabilityMiss(&ProviderError{StatusCode: status, Capability: ProviderCapabilityUnsupportedRepresentation}))
	}
}

func TestCapabilityNegotiationStopsAfterUnclassified400404And422(t *testing.T) {
	for _, test := range []struct {
		name   string
		status int
		body   string
	}{
		{name: "generic aggregator", status: http.StatusBadRequest, body: `{"error":{"type":"invalid_request_error","message":"unsupported parameter ` + capabilityResponseCanary + `"}}`},
		{name: "wrong endpoint", status: http.StatusNotFound, body: `{"error":{"type":"not_found_error","code":"endpoint_not_found"}}`},
		{name: "wrong model", status: http.StatusNotFound, body: `{"error":{"type":"invalid_request_error","code":"model_not_found"}}`},
		{name: "authentication", status: http.StatusUnprocessableEntity, body: `{"error":{"type":"authentication_error","code":"invalid_api_key"}}`},
		{name: "billing", status: http.StatusBadRequest, body: `{"error":{"type":"billing_error","code":"insufficient_quota"}}`},
		{name: "policy", status: http.StatusUnprocessableEntity, body: `{"error":{"type":"policy_error","code":"content_policy_violation"}}`},
		{name: "malformed", status: http.StatusBadRequest, body: `{"error":`},
	} {
		t.Run(test.name, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				w.WriteHeader(test.status)
				_, _ = w.Write([]byte(test.body))
			}))
			t.Cleanup(server.Close)
			registry, err := NewDriverRegistry(server.Client(), nil, nil)
			require.NoError(t, err)

			got, negotiationErr := NewCapabilityChecker(registry).Negotiate(t.Context(),
				capabilityTestCandidate(ProtocolOpenAIChat, server.URL),
				NewCredential(AuthBearer, capabilityCredentialValue))
			require.Error(t, negotiationErr)
			assert.Empty(t, got)
			assert.Equal(t, int32(1), calls.Load())
			assert.NotContains(t, negotiationErr.Error(), capabilityResponseCanary)
		})
	}
}

func TestCapabilityNegotiationRetriesClassifiedErrorsForEachProtocolFamily(t *testing.T) {
	for _, test := range []struct {
		name        string
		protocol    Protocol
		auth        AuthScheme
		errorBody   string
		successBody string
	}{
		{name: "openai chat", protocol: ProtocolOpenAIChat, auth: AuthBearer,
			errorBody:   `{"error":{"type":"invalid_request_error","code":"unsupported_parameter","param":"response_format"}}`,
			successBody: `{"model":"synthetic-model-version","choices":[{"message":{"content":"{\"ok\":true}"}}]}`},
		{name: "openai responses", protocol: ProtocolOpenAIResponses, auth: AuthBearer,
			errorBody:   `{"error":{"type":"invalid_request_error","code":"unsupported_value","param":"text.format"}}`,
			successBody: `{"model":"synthetic-model-version","output":[{"type":"message","content":[{"type":"output_text","text":"{\"ok\":true}"}]}]}`},
		{name: "anthropic", protocol: ProtocolAnthropicMessages, auth: AuthXAPIKey,
			errorBody:   `{"type":"error","error":{"type":"invalid_request_error","code":"unsupported_json_schema"}}`,
			successBody: `{"id":"msg_safe","type":"message","role":"assistant","model":"synthetic-model-version","content":[{"type":"text","text":"{\"ok\":true}"}],"stop_reason":"end_turn"}`},
		{name: "google", protocol: ProtocolGoogleGenerateContent, auth: AuthGoogleAPIKey,
			errorBody:   `{"error":{"code":400,"status":"INVALID_ARGUMENT","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"UNSUPPORTED_RESPONSE_FORMAT","domain":"generativelanguage.googleapis.com"}]}}`,
			successBody: `{"candidates":[{"content":{"role":"model","parts":[{"text":"{\"ok\":true}"}]},"finishReason":"STOP"}],"modelVersion":"synthetic-model-version"}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if calls.Add(1) == 1 {
					w.WriteHeader(http.StatusBadRequest)
					_, _ = w.Write([]byte(test.errorBody))
					return
				}
				_, _ = w.Write([]byte(test.successBody))
			}))
			t.Cleanup(server.Close)
			registry, err := NewDriverRegistry(server.Client(), nil, nil)
			require.NoError(t, err)
			candidate := capabilityTestCandidate(test.protocol, server.URL)
			candidate.Auth = test.auth

			got, negotiationErr := NewCapabilityChecker(registry).Negotiate(t.Context(), candidate,
				NewCredential(test.auth, capabilityCredentialValue))
			require.NoError(t, negotiationErr)
			assert.Equal(t, int32(2), calls.Load())
			assert.JSONEq(t, `{"ok":true}`, string(got.Response.Output))
		})
	}
}

func TestCapabilityNegotiationStopsAfterUnclassifiedErrorForEveryProtocol(t *testing.T) {
	for _, test := range []struct {
		protocol Protocol
		auth     AuthScheme
		bodies   []string
	}{
		{ProtocolOpenAIChat, AuthBearer, []string{
			`{"error":{"type":"invalid_request_error","message":"unsupported parameter"}}`,
			`{"error":{"type":"not_found_error","code":"endpoint_not_found"}}`,
			`{"error":{"type":"invalid_request_error","code":"unsupported_value","param":"model"}}`,
			`{"error":{"type":"authentication_error","code":"invalid_api_key"}}`,
			`{"error":{"type":"billing_error","code":"insufficient_quota"}}`,
			`{"error":{"type":"policy_error","code":"content_policy_violation"}}`,
			`{"error":{"type":"invalid_request_error","code":"unsupported_parameter"}}`,
			`{"error":{"type":"invalid_request_error","code":"unsupported_parameter","param":"response_format.model"}}`,
			`{"error":{"type":"invalid_request_error","code":"unsupported_parameter","param":"Response_Format"}}`,
			`{"error":{"type":"invalid_request_error","code":"unsupported_parameter","param":"response_format","param":"model"}}`, `{"error":`,
		}},
		{ProtocolOpenAIResponses, AuthBearer, []string{
			`{"error":{"type":"invalid_request_error","message":"unsupported parameter"}}`,
			`{"error":{"type":"not_found_error","code":"endpoint_not_found"}}`,
			`{"error":{"type":"invalid_request_error","code":"unsupported_parameter","param":"model"}}`,
			`{"error":{"type":"authentication_error","code":"invalid_api_key"}}`,
			`{"error":{"type":"billing_error","code":"insufficient_quota"}}`,
			`{"error":{"type":"policy_error","code":"content_policy_violation"}}`,
			`{"error":{"type":"invalid_request_error","code":"unsupported_parameter"}}`,
			`{"error":{"type":"invalid_request_error","code":"unsupported_parameter","param":"reasoning.effort"}}`,
			`{"error":{"type":"invalid_request_error","code":"unsupported_parameter","param":"text.format.model"}}`,
			`{"error":{"type":"invalid_request_error","code":"unsupported_parameter","param":"Text.Format"}}`, `{"error":`,
		}},
		{ProtocolAnthropicMessages, AuthXAPIKey, []string{
			`{"type":"error","error":{"type":"invalid_request_error","message":"unsupported parameter"}}`,
			`{"type":"error","error":{"type":"not_found_error","code":"endpoint_not_found"}}`,
			`{"type":"error","error":{"type":"invalid_request_error","code":"model_not_found"}}`,
			`{"type":"error","error":{"type":"authentication_error","code":"invalid_api_key"}}`,
			`{"type":"error","error":{"type":"billing_error","code":"insufficient_quota"}}`,
			`{"type":"error","error":{"type":"permission_error","code":"policy_violation"}}`,
			`{"type":"error","error":{"type":"invalid_request_error","code":"unsupported_parameter"}}`,
			`{"type":"error","error":{"type":"invalid_request_error","code":"unsupported_parameter","param":"tools.0"}}`,
			`{"type":"error","error":{"type":"invalid_request_error","code":"unsupported_parameter","param":"Tools"}}`,
			`{"type":"error","error":{"type":"invalid_request_error","code":"unsupported_parameter","param":"tools","param":"model"}}`, `{"error":`,
		}},
		{ProtocolGoogleGenerateContent, AuthGoogleAPIKey, []string{
			`{"error":{"code":400,"status":"INVALID_ARGUMENT","message":"unsupported response format"}}`,
			`{"error":{"code":400,"status":"NOT_FOUND","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"ENDPOINT_NOT_FOUND","domain":"generativelanguage.googleapis.com"}]}}`,
			`{"error":{"code":400,"status":"INVALID_ARGUMENT","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"MODEL_NOT_FOUND","domain":"generativelanguage.googleapis.com"}]}}`,
			`{"error":{"code":400,"status":"UNAUTHENTICATED"}}`,
			`{"error":{"code":400,"status":"RESOURCE_EXHAUSTED"}}`,
			`{"error":{"code":400,"status":"PERMISSION_DENIED"}}`,
			`{"error":{"code":400,"status":"INVALID_ARGUMENT","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"UNSUPPORTED_PARAMETER","domain":"generativelanguage.googleapis.com"}]}}`,
			`{"error":{"code":400,"status":"INVALID_ARGUMENT","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"UNSUPPORTED_PARAMETER","domain":"generativelanguage.googleapis.com","metadata":{"parameter":"generationConfig.responseSchema.model"}}]}}`,
			`{"error":{"code":400,"status":"INVALID_ARGUMENT","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"UNSUPPORTED_PARAMETER","domain":"generativelanguage.googleapis.com","metadata":{"parameter":"generationconfig.responseSchema"}}]}}`,
			`{"error":{"code":400,"status":"INVALID_ARGUMENT","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"UNSUPPORTED_PARAMETER","domain":"generativelanguage.googleapis.com","metadata":{"parameter":"generationConfig.responseSchema","parameter":"model"}}]}}`, `{"error":`,
		}},
	} {
		t.Run(string(test.protocol), func(t *testing.T) {
			for index, body := range test.bodies {
				t.Run(fmt.Sprintf("case-%d", index), func(t *testing.T) {
					var calls atomic.Int32
					server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
						calls.Add(1)
						w.WriteHeader(http.StatusBadRequest)
						_, _ = w.Write([]byte(body))
					}))
					t.Cleanup(server.Close)
					registry, err := NewDriverRegistry(server.Client(), nil, nil)
					require.NoError(t, err)
					candidate := capabilityTestCandidate(test.protocol, server.URL)
					candidate.Auth = test.auth
					got, negotiationErr := NewCapabilityChecker(registry).Negotiate(t.Context(), candidate,
						NewCredential(test.auth, capabilityCredentialValue))
					require.Error(t, negotiationErr)
					assert.Empty(t, got)
					assert.Equal(t, int32(1), calls.Load())
					for _, fragment := range []string{"unsupported", "parameter", "model", "endpoint", "billing", "policy", capabilityResponseCanary, capabilityCredentialValue} {
						assert.NotContains(t, negotiationErr.Error(), fragment)
					}
				})
			}
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
		_, _ = w.Write([]byte(`{"error":{"type":"invalid_request_error","code":"unsupported_json_schema"}}`))
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
