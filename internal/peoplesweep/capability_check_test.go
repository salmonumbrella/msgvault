package peoplesweep

import (
	"context"
	"encoding/json"
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
	capabilityMessageCanary   = "message-fragment-canary-never-report"
	capabilityParamCanary     = "param-fragment-canary-never-report"
	capabilityCodeCanary      = "code-fragment-canary-never-report"
	capabilityAuthCanary      = "auth-fragment-canary-never-report"
	capabilityStatusCanary    = "status-fragment-canary-never-report"
	capabilityDomainCanary    = "domain-fragment-canary-never-report"
	capabilityBodyCanary      = "body-fragment-canary-never-report"
)

type capabilityAttempt struct {
	path string
	body map[string]any
	err  error
}

func TestCapabilityNegotiationUsesFixedOutputAndTokenOrderWithoutArchiveContext(t *testing.T) {
	var mu sync.Mutex
	var attempts []capabilityAttempt
	statuses := []int{http.StatusBadRequest, http.StatusUnprocessableEntity,
		http.StatusNotFound, http.StatusOK}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		assert.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		encoded, err := json.Marshal(body)
		assert.NoError(t, err)
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
				assert.NoError(t, json.NewDecoder(r.Body).Decode(&body))
				if call == 1 {
					assert.NotContains(t, body, "reasoning_effort")
					assert.NotContains(t, body, "reasoning")
				} else {
					assert.Equal(t, "high", body["reasoning_effort"])
					reasoning, ok := body["reasoning"].(map[string]any)
					if assert.True(t, ok) {
						assert.Equal(t, true, reasoning["enabled"])
					}
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
		name            string
		protocol        Protocol
		mode            OutputMode
		tokenParameter  string
		reasoningEffort string
		reasoningMode   string
		parameter       string
		want            bool
	}{
		{name: "chat native field", protocol: ProtocolOpenAIChat, mode: OutputModeNativeJSONSchema, tokenParameter: "max_tokens", parameter: "response_format.json_schema", want: true},
		{name: "chat selected token field", protocol: ProtocolOpenAIChat, mode: OutputModePromptJSON, tokenParameter: "max_completion_tokens", parameter: "max_completion_tokens", want: true},
		{name: "chat other token field", protocol: ProtocolOpenAIChat, mode: OutputModePromptJSON, tokenParameter: "max_completion_tokens", parameter: "max_tokens"},
		{name: "chat wrong mode", protocol: ProtocolOpenAIChat, mode: OutputModeJSONObject, tokenParameter: "max_tokens", parameter: "response_format.json_schema"},
		{name: "chat fake descendant", protocol: ProtocolOpenAIChat, mode: OutputModeJSONObject, tokenParameter: "max_tokens", parameter: "response_format.model"},
		{name: "chat prompt field", protocol: ProtocolOpenAIChat, mode: OutputModePromptJSON, tokenParameter: "max_tokens", parameter: "response_format"},
		{name: "chat effort omitted", protocol: ProtocolOpenAIChat, mode: OutputModePromptJSON, tokenParameter: "max_tokens", parameter: "reasoning_effort"},
		{name: "chat effort emitted", protocol: ProtocolOpenAIChat, mode: OutputModePromptJSON, tokenParameter: "max_tokens", reasoningEffort: "high", parameter: "reasoning_effort", want: true},
		{name: "chat reasoning object emitted", protocol: ProtocolOpenAIChat, mode: OutputModePromptJSON, tokenParameter: "max_tokens", reasoningMode: "enabled", parameter: "reasoning", want: true},
		{name: "chat reasoning enabled emitted", protocol: ProtocolOpenAIChat, mode: OutputModePromptJSON, tokenParameter: "max_tokens", reasoningMode: "disabled", parameter: "reasoning.enabled", want: true},
		{name: "chat reasoning wrong descendant", protocol: ProtocolOpenAIChat, mode: OutputModePromptJSON, tokenParameter: "max_tokens", reasoningMode: "enabled", parameter: "reasoning.effort"},
		{name: "responses reasoning omitted", protocol: ProtocolOpenAIResponses, mode: OutputModeNativeJSONSchema, parameter: "reasoning.effort"},
		{name: "responses reasoning emitted", protocol: ProtocolOpenAIResponses, mode: OutputModeNativeJSONSchema, reasoningEffort: "high", parameter: "reasoning.effort", want: true},
		{name: "responses wrong mode", protocol: ProtocolOpenAIResponses, mode: OutputModeJSONObject, parameter: "text.format.schema"},
		{name: "anthropic fake descendant", protocol: ProtocolAnthropicMessages, mode: OutputModeNativeJSONSchema, parameter: "tools.0"},
		{name: "google casing alias", protocol: ProtocolGoogleGenerateContent, mode: OutputModeNativeJSONSchema, parameter: "generationconfig.responseSchema"},
		{name: "google wrong mode", protocol: ProtocolGoogleGenerateContent, mode: OutputModePromptJSON, parameter: "generationConfig.responseSchema"},
		{name: "leading whitespace alias", protocol: ProtocolAnthropicMessages, mode: OutputModeNativeJSONSchema, parameter: " tools"},
		{name: "trailing whitespace alias", protocol: ProtocolGoogleGenerateContent, mode: OutputModeNativeJSONSchema, parameter: "generationConfig.responseSchema "},
	} {
		t.Run(test.name, func(t *testing.T) {
			candidate := capabilityTestCandidate(test.protocol, "https://example.test")
			candidate.ReasoningEffort = test.reasoningEffort
			candidate.ReasoningMode = test.reasoningMode
			profile, err := capabilityProfile(candidate, test.mode, test.tokenParameter,
				test.reasoningEffort != "" || test.reasoningMode != "")
			require.NoError(t, err)
			assert.Equal(t, test.want, capabilityParameterMatchesProfile(profile, test.parameter))
		})
	}
}

func TestCapabilityRepresentationCodeMustMatchProtocolAndActiveMode(t *testing.T) {
	for _, test := range []struct {
		name     string
		protocol Protocol
		mode     OutputMode
		code     string
		want     bool
	}{
		{name: "chat native schema", protocol: ProtocolOpenAIChat, mode: OutputModeNativeJSONSchema, code: "unsupported_json_schema", want: true},
		{name: "chat JSON object", protocol: ProtocolOpenAIChat, mode: OutputModeJSONObject, code: "unsupported_response_format", want: true},
		{name: "chat prompt", protocol: ProtocolOpenAIChat, mode: OutputModePromptJSON, code: "unsupported_json_schema"},
		{name: "chat casing alias", protocol: ProtocolOpenAIChat, mode: OutputModeNativeJSONSchema, code: "UNSUPPORTED_JSON_SCHEMA"},
		{name: "responses native schema", protocol: ProtocolOpenAIResponses, mode: OutputModeNativeJSONSchema, code: "unsupported_json_schema", want: true},
		{name: "responses prompt", protocol: ProtocolOpenAIResponses, mode: OutputModePromptJSON, code: "unsupported_response_format"},
		{name: "anthropic native schema", protocol: ProtocolAnthropicMessages, mode: OutputModeNativeJSONSchema, code: "unsupported_json_schema", want: true},
		{name: "anthropic wrong protocol", protocol: ProtocolAnthropicMessages, mode: OutputModeNativeJSONSchema, code: "unsupported_response_format"},
		{name: "anthropic prompt", protocol: ProtocolAnthropicMessages, mode: OutputModePromptJSON, code: "unsupported_json_schema"},
		{name: "google native schema", protocol: ProtocolGoogleGenerateContent, mode: OutputModeNativeJSONSchema, code: "UNSUPPORTED_JSON_SCHEMA", want: true},
		{name: "google native response format", protocol: ProtocolGoogleGenerateContent, mode: OutputModeNativeJSONSchema, code: "UNSUPPORTED_RESPONSE_FORMAT", want: true},
		{name: "google prompt", protocol: ProtocolGoogleGenerateContent, mode: OutputModePromptJSON, code: "UNSUPPORTED_RESPONSE_FORMAT"},
		{name: "google casing alias", protocol: ProtocolGoogleGenerateContent, mode: OutputModeNativeJSONSchema, code: "unsupported_json_schema"},
	} {
		t.Run(test.name, func(t *testing.T) {
			candidate := capabilityTestCandidate(test.protocol, "https://example.test")
			profile, err := capabilityProfile(candidate, test.mode,
				map[bool]string{true: "max_tokens"}[test.protocol == ProtocolOpenAIChat], false)
			require.NoError(t, err)
			assert.Equal(t, test.want, capabilityCodeMatchesProfile(profile, test.code, "", false))
		})
	}
}

func TestCapabilityRepresentationCodesRequireAbsentParameter(t *testing.T) {
	for _, test := range []struct {
		name           string
		protocol       Protocol
		mode           OutputMode
		tokenParameter string
		code           string
		parameter      string
	}{
		{name: "chat native schema", protocol: ProtocolOpenAIChat, mode: OutputModeNativeJSONSchema,
			tokenParameter: "max_tokens", code: "unsupported_json_schema", parameter: "response_format"},
		{name: "chat JSON object", protocol: ProtocolOpenAIChat, mode: OutputModeJSONObject,
			tokenParameter: "max_tokens", code: "unsupported_response_format", parameter: "response_format"},
		{name: "responses native schema", protocol: ProtocolOpenAIResponses, mode: OutputModeNativeJSONSchema,
			code: "unsupported_json_schema", parameter: "text.format"},
		{name: "responses JSON object", protocol: ProtocolOpenAIResponses, mode: OutputModeJSONObject,
			code: "unsupported_response_format", parameter: "text.format"},
		{name: "anthropic native schema", protocol: ProtocolAnthropicMessages, mode: OutputModeNativeJSONSchema,
			code: "unsupported_json_schema", parameter: "tools"},
		{name: "google native schema", protocol: ProtocolGoogleGenerateContent, mode: OutputModeNativeJSONSchema,
			code: "UNSUPPORTED_JSON_SCHEMA", parameter: "generationConfig.responseSchema"},
		{name: "google native response format", protocol: ProtocolGoogleGenerateContent, mode: OutputModeNativeJSONSchema,
			code: "UNSUPPORTED_RESPONSE_FORMAT", parameter: "generationConfig.responseMimeType"},
	} {
		t.Run(test.name, func(t *testing.T) {
			profile, err := capabilityProfile(capabilityTestCandidate(test.protocol, "https://example.test"),
				test.mode, test.tokenParameter, false)
			require.NoError(t, err)

			assert.True(t, capabilityCodeMatchesProfile(profile, test.code, "", false))
			assert.False(t, capabilityCodeMatchesProfile(profile, test.code, test.parameter, true))
		})
	}
}

func TestCapabilityDriversRejectParameterizedRepresentationCodesForEveryActiveMode(t *testing.T) {
	for _, test := range []struct {
		name           string
		protocol       Protocol
		auth           AuthScheme
		mode           OutputMode
		tokenParameter string
		path           string
		code           string
		parameter      string
		errorBody      string
	}{
		{name: "openai chat native schema", protocol: ProtocolOpenAIChat, auth: AuthBearer,
			mode: OutputModeNativeJSONSchema, tokenParameter: "max_tokens", path: "/chat/completions",
			code: "unsupported_json_schema", parameter: "response_format",
			errorBody: `{"error":{"type":"invalid_request_error","code":"unsupported_json_schema","param":"response_format","message":"` + capabilityMessageCanary + `"}}`},
		{name: "openai chat JSON object", protocol: ProtocolOpenAIChat, auth: AuthBearer,
			mode: OutputModeJSONObject, tokenParameter: "max_tokens", path: "/chat/completions",
			code: "unsupported_response_format", parameter: "response_format",
			errorBody: `{"error":{"type":"invalid_request_error","code":"unsupported_response_format","param":"response_format","message":"` + capabilityMessageCanary + `"}}`},
		{name: "openai responses native schema", protocol: ProtocolOpenAIResponses, auth: AuthBearer,
			mode: OutputModeNativeJSONSchema, path: "/responses", code: "unsupported_json_schema", parameter: "text.format",
			errorBody: `{"error":{"type":"invalid_request_error","code":"unsupported_json_schema","param":"text.format","message":"` + capabilityMessageCanary + `"}}`},
		{name: "openai responses JSON object", protocol: ProtocolOpenAIResponses, auth: AuthBearer,
			mode: OutputModeJSONObject, path: "/responses", code: "unsupported_response_format", parameter: "text.format",
			errorBody: `{"error":{"type":"invalid_request_error","code":"unsupported_response_format","param":"text.format","message":"` + capabilityMessageCanary + `"}}`},
		{name: "anthropic native schema", protocol: ProtocolAnthropicMessages, auth: AuthXAPIKey,
			mode: OutputModeNativeJSONSchema, path: "/v1/messages", code: "unsupported_json_schema", parameter: "tools",
			errorBody: `{"type":"error","error":{"type":"invalid_request_error","code":"unsupported_json_schema","param":"tools","message":"` + capabilityMessageCanary + `"}}`},
		{name: "google native schema", protocol: ProtocolGoogleGenerateContent, auth: AuthGoogleAPIKey,
			mode: OutputModeNativeJSONSchema, path: "/models/synthetic-model:generateContent",
			code: "UNSUPPORTED_JSON_SCHEMA", parameter: "generationConfig.responseSchema",
			errorBody: `{"error":{"code":400,"status":"INVALID_ARGUMENT","message":"` + capabilityMessageCanary + `","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"UNSUPPORTED_JSON_SCHEMA","domain":"generativelanguage.googleapis.com","metadata":{"parameter":"generationConfig.responseSchema"}}]}}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				assert.Equal(t, test.path, r.URL.Path)
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(test.errorBody))
			}))
			t.Cleanup(server.Close)
			registry, err := NewDriverRegistry(server.Client(), nil, nil)
			require.NoError(t, err)
			candidate := capabilityTestCandidate(test.protocol, server.URL)
			candidate.Auth = test.auth
			profile, err := capabilityProfile(candidate, test.mode, test.tokenParameter, false)
			require.NoError(t, err)
			driver, err := registry.capabilityDriver(test.protocol)
			require.NoError(t, err)
			prepared, err := driver.Prepare(profile, capabilitySyntheticRequest())
			require.NoError(t, err)

			_, callErr := driver.GeneratePrepared(t.Context(), profile,
				NewCredential(test.auth, capabilityCredentialValue), prepared)
			require.Error(t, callErr)
			var providerErr *ProviderError
			require.ErrorAs(t, callErr, &providerErr)
			assert.Empty(t, providerErr.Capability)
			assert.Equal(t, int32(1), calls.Load())
			for _, fragment := range []string{test.code, test.parameter, capabilityMessageCanary,
				capabilityCredentialValue, test.errorBody} {
				assert.NotContains(t, callErr.Error(), fragment)
			}
		})
	}
}

func TestCapabilityNegotiationStopsOnParameterizedRepresentationCodeForEveryProtocol(t *testing.T) {
	for _, test := range []struct {
		name      string
		protocol  Protocol
		auth      AuthScheme
		path      string
		code      string
		parameter string
		errorBody string
	}{
		{name: "openai chat", protocol: ProtocolOpenAIChat, auth: AuthBearer, path: "/chat/completions",
			code: "unsupported_json_schema", parameter: "response_format",
			errorBody: `{"error":{"type":"invalid_request_error","code":"unsupported_json_schema","param":"response_format","message":"` + capabilityMessageCanary + `"}}`},
		{name: "openai responses", protocol: ProtocolOpenAIResponses, auth: AuthBearer, path: "/responses",
			code: "unsupported_json_schema", parameter: "text.format",
			errorBody: `{"error":{"type":"invalid_request_error","code":"unsupported_json_schema","param":"text.format","message":"` + capabilityMessageCanary + `"}}`},
		{name: "anthropic", protocol: ProtocolAnthropicMessages, auth: AuthXAPIKey, path: "/v1/messages",
			code: "unsupported_json_schema", parameter: "tools",
			errorBody: `{"type":"error","error":{"type":"invalid_request_error","code":"unsupported_json_schema","param":"tools","message":"` + capabilityMessageCanary + `"}}`},
		{name: "google", protocol: ProtocolGoogleGenerateContent, auth: AuthGoogleAPIKey,
			path: "/models/synthetic-model:generateContent", code: "UNSUPPORTED_JSON_SCHEMA",
			parameter: "generationConfig.responseSchema",
			errorBody: `{"error":{"code":400,"status":"INVALID_ARGUMENT","message":"` + capabilityMessageCanary + `","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"UNSUPPORTED_JSON_SCHEMA","domain":"generativelanguage.googleapis.com","metadata":{"parameter":"generationConfig.responseSchema"}}]}}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				assert.Equal(t, test.path, r.URL.Path)
				var body map[string]any
				assert.NoError(t, json.NewDecoder(r.Body).Decode(&body))
				if test.protocol == ProtocolGoogleGenerateContent {
					assert.NotContains(t, body, "model")
				} else {
					assert.Equal(t, "synthetic-model", body["model"])
				}
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(test.errorBody))
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
			for _, fragment := range []string{test.code, test.parameter, capabilityMessageCanary,
				capabilityCredentialValue, test.errorBody} {
				assert.NotContains(t, negotiationErr.Error(), fragment)
			}
		})
	}
}

func TestCapabilityDriversRejectRepresentationCodesForPromptOnlyAttempts(t *testing.T) {
	for _, test := range []struct {
		name      string
		protocol  Protocol
		auth      AuthScheme
		path      string
		errorBody string
	}{
		{name: "openai chat", protocol: ProtocolOpenAIChat, auth: AuthBearer, path: "/chat/completions",
			errorBody: `{"error":{"type":"invalid_request_error","code":"unsupported_json_schema"}}`},
		{name: "openai responses", protocol: ProtocolOpenAIResponses, auth: AuthBearer, path: "/responses",
			errorBody: `{"error":{"type":"invalid_request_error","code":"unsupported_response_format"}}`},
		{name: "anthropic", protocol: ProtocolAnthropicMessages, auth: AuthXAPIKey, path: "/v1/messages",
			errorBody: `{"type":"error","error":{"type":"invalid_request_error","code":"unsupported_json_schema"}}`},
		{name: "google", protocol: ProtocolGoogleGenerateContent, auth: AuthGoogleAPIKey, path: "/models/synthetic-model:generateContent",
			errorBody: `{"error":{"code":400,"status":"INVALID_ARGUMENT","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"UNSUPPORTED_RESPONSE_FORMAT","domain":"generativelanguage.googleapis.com"}]}}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			var calls atomic.Int32
			attempts := make(chan capabilityAttempt, 1)
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				var body map[string]any
				decodeErr := json.NewDecoder(r.Body).Decode(&body)
				attempts <- capabilityAttempt{path: r.URL.Path, body: body, err: decodeErr}
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(test.errorBody))
			}))
			t.Cleanup(server.Close)
			registry, err := NewDriverRegistry(server.Client(), nil, nil)
			require.NoError(t, err)
			candidate := capabilityTestCandidate(test.protocol, server.URL)
			candidate.Auth = test.auth
			profile, err := capabilityProfile(candidate, OutputModePromptJSON,
				map[bool]string{true: "max_tokens"}[test.protocol == ProtocolOpenAIChat], false)
			require.NoError(t, err)
			driver, err := registry.capabilityDriver(test.protocol)
			require.NoError(t, err)
			prepared, err := driver.Prepare(profile, capabilitySyntheticRequest())
			require.NoError(t, err)

			_, callErr := driver.GeneratePrepared(t.Context(), profile,
				NewCredential(test.auth, capabilityCredentialValue), prepared)
			require.Error(t, callErr)
			var providerErr *ProviderError
			require.ErrorAs(t, callErr, &providerErr)
			assert.Empty(t, providerErr.Capability)
			assert.Equal(t, int32(1), calls.Load())
			attempt := <-attempts
			require.NoError(t, attempt.err)
			assert.Equal(t, test.path, attempt.path)
			if test.protocol == ProtocolGoogleGenerateContent {
				assert.NotContains(t, attempt.body, "model")
			} else {
				assert.Equal(t, "synthetic-model", attempt.body["model"])
			}
		})
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
			errorBody:   `{"type":"error","error":{"type":"invalid_request_error","code":"unsupported_parameter","param":"tools"}}`,
			successBody: `{"id":"msg_safe","type":"message","role":"assistant","model":"synthetic-model-version","content":[{"type":"text","text":"{\"ok\":true}"}],"stop_reason":"end_turn"}`},
		{name: "google", protocol: ProtocolGoogleGenerateContent, auth: AuthGoogleAPIKey,
			errorBody:   `{"error":{"code":400,"status":"INVALID_ARGUMENT","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"UNSUPPORTED_PARAMETER","domain":"generativelanguage.googleapis.com","metadata":{"parameter":"generationConfig.responseSchema"}}]}}`,
			successBody: `{"candidates":[{"content":{"role":"model","parts":[{"text":"{\"ok\":true}"}]},"finishReason":"STOP"}],"modelVersion":"synthetic-model-version"}`},
		{name: "openai chat parameterless representation", protocol: ProtocolOpenAIChat, auth: AuthBearer,
			errorBody:   `{"error":{"type":"invalid_request_error","code":"unsupported_json_schema"}}`,
			successBody: `{"model":"synthetic-model-version","choices":[{"message":{"content":"{\"ok\":true}"}}]}`},
		{name: "openai responses parameterless representation", protocol: ProtocolOpenAIResponses, auth: AuthBearer,
			errorBody:   `{"error":{"type":"invalid_request_error","code":"unsupported_json_schema"}}`,
			successBody: `{"model":"synthetic-model-version","output":[{"type":"message","content":[{"type":"output_text","text":"{\"ok\":true}"}]}]}`},
		{name: "anthropic parameterless representation", protocol: ProtocolAnthropicMessages, auth: AuthXAPIKey,
			errorBody:   `{"type":"error","error":{"type":"invalid_request_error","code":"unsupported_json_schema"}}`,
			successBody: `{"id":"msg_safe","type":"message","role":"assistant","model":"synthetic-model-version","content":[{"type":"text","text":"{\"ok\":true}"}],"stop_reason":"end_turn"}`},
		{name: "google parameterless representation", protocol: ProtocolGoogleGenerateContent, auth: AuthGoogleAPIKey,
			errorBody:   `{"error":{"code":400,"status":"INVALID_ARGUMENT","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"UNSUPPORTED_JSON_SCHEMA","domain":"generativelanguage.googleapis.com"}]}}`,
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
		path     string
		bodies   []string
	}{
		{ProtocolOpenAIChat, AuthBearer, "/chat/completions", []string{
			`{"error":{"type":"invalid_request_error","message":"unsupported parameter"}}`,
			`{"error":{"type":"not_found_error","code":"endpoint_not_found"}}`,
			`{"error":{"type":"invalid_request_error","code":"unsupported_value","param":"model"}}`,
			`{"error":{"type":"authentication_error","code":"invalid_api_key"}}`,
			`{"error":{"type":"billing_error","code":"insufficient_quota"}}`,
			`{"error":{"type":"policy_error","code":"content_policy_violation"}}`,
			`{"error":{"type":"invalid_request_error","code":"unsupported_parameter"}}`,
			`{"error":{"type":"invalid_request_error","code":"unsupported_parameter","param":"response_format.model"}}`,
			`{"error":{"type":"invalid_request_error","code":"unsupported_parameter","param":"Response_Format"}}`,
			`{"error":{"type":"invalid_request_error","code":"unsupported_parameter","param":" response_format"}}`,
			`{"error":{"type":"invalid_request_error","code":"UNSUPPORTED_JSON_SCHEMA"}}`,
			`{"error":{"type":"invalid_request_error","code":{"value":"unsupported_json_schema"}}}`,
			`{"error":{"type":"invalid_request_error","code":"unsupported_json_schema","param":{"value":"response_format"}}}`,
			`{"error":{"type":"invalid_request_error","code":"unsupported_json_schema","param":""}}`,
			`{"error":{"type":"invalid_request_error","code":"unsupported_json_schema","code":"` + capabilityCodeCanary + `"}}`,
			`{"error":{"type":"invalid_request_error","code":"unsupported_parameter","param":"response_format","param":"model"}}`,
			`{"error":{"type":"invalid_request_error","code":"` + capabilityCodeCanary + `","param":"` + capabilityParamCanary + `","message":"` + capabilityMessageCanary + `","auth":"` + capabilityAuthCanary + `","status":"` + capabilityStatusCanary + `","domain":"` + capabilityDomainCanary + `","body":"` + capabilityBodyCanary + `"}}`, `{"error":`,
		}},
		{ProtocolOpenAIResponses, AuthBearer, "/responses", []string{
			`{"error":{"type":"invalid_request_error","message":"unsupported parameter"}}`,
			`{"error":{"type":"not_found_error","code":"endpoint_not_found"}}`,
			`{"error":{"type":"invalid_request_error","code":"unsupported_parameter","param":"model"}}`,
			`{"error":{"type":"authentication_error","code":"invalid_api_key"}}`,
			`{"error":{"type":"billing_error","code":"insufficient_quota"}}`,
			`{"error":{"type":"policy_error","code":"content_policy_violation"}}`,
			`{"error":{"type":"invalid_request_error","code":"unsupported_parameter"}}`,
			`{"error":{"type":"invalid_request_error","code":"unsupported_parameter","param":"reasoning.effort"}}`,
			`{"error":{"type":"invalid_request_error","code":"unsupported_parameter","param":"text.format.model"}}`,
			`{"error":{"type":"invalid_request_error","code":"unsupported_parameter","param":"Text.Format"}}`,
			`{"error":{"type":"invalid_request_error","code":"unsupported_parameter","param":" text.format"}}`,
			`{"error":{"type":"invalid_request_error","code":"UNSUPPORTED_JSON_SCHEMA"}}`,
			`{"error":{"type":"invalid_request_error","code":{"value":"unsupported_json_schema"}}}`,
			`{"error":{"type":"invalid_request_error","code":"unsupported_json_schema","param":{"value":"text.format"}}}`,
			`{"error":{"type":"invalid_request_error","code":"unsupported_json_schema","param":""}}`,
			`{"error":{"type":"invalid_request_error","code":"unsupported_json_schema","code":"` + capabilityCodeCanary + `"}}`,
			`{"error":{"type":"invalid_request_error","code":"unsupported_parameter","param":"text.format","param":"` + capabilityParamCanary + `"}}`,
			`{"error":{"type":"invalid_request_error","code":"` + capabilityCodeCanary + `","param":"` + capabilityParamCanary + `","message":"` + capabilityMessageCanary + `","auth":"` + capabilityAuthCanary + `","status":"` + capabilityStatusCanary + `","domain":"` + capabilityDomainCanary + `","body":"` + capabilityBodyCanary + `"}}`, `{"error":`,
		}},
		{ProtocolAnthropicMessages, AuthXAPIKey, "/v1/messages", []string{
			`{"type":"error","error":{"type":"invalid_request_error","message":"unsupported parameter"}}`,
			`{"type":"error","error":{"type":"not_found_error","code":"endpoint_not_found"}}`,
			`{"type":"error","error":{"type":"invalid_request_error","code":"model_not_found"}}`,
			`{"type":"error","error":{"type":"authentication_error","code":"invalid_api_key"}}`,
			`{"type":"error","error":{"type":"billing_error","code":"insufficient_quota"}}`,
			`{"type":"error","error":{"type":"permission_error","code":"policy_violation"}}`,
			`{"type":"error","error":{"type":"invalid_request_error","code":"unsupported_parameter"}}`,
			`{"type":"error","error":{"type":"invalid_request_error","code":"unsupported_parameter","param":"tools.0"}}`,
			`{"type":"error","error":{"type":"invalid_request_error","code":"unsupported_parameter","param":"Tools"}}`,
			`{"type":"error","error":{"type":"invalid_request_error","code":"unsupported_parameter","param":" tools"}}`,
			`{"type":"error","error":{"type":"invalid_request_error","code":"UNSUPPORTED_JSON_SCHEMA"}}`,
			`{"type":"error","error":{"type":"invalid_request_error","code":"unsupported_response_format"}}`,
			`{"type":"error","error":{"type":"invalid_request_error","code":{"value":"unsupported_json_schema"}}}`,
			`{"type":"error","error":{"type":"invalid_request_error","code":"unsupported_json_schema","param":{"value":"tools"}}}`,
			`{"type":"error","error":{"type":"invalid_request_error","code":"unsupported_json_schema","param":""}}`,
			`{"type":"error","error":{"type":"invalid_request_error","code":"unsupported_json_schema","code":"` + capabilityCodeCanary + `"}}`,
			`{"type":"error","error":{"type":"invalid_request_error","code":"unsupported_parameter","param":"tools","param":"model"}}`,
			`{"type":"error","error":{"type":"invalid_request_error","code":"` + capabilityCodeCanary + `","param":"` + capabilityParamCanary + `","message":"` + capabilityMessageCanary + `","auth":"` + capabilityAuthCanary + `","status":"` + capabilityStatusCanary + `","domain":"` + capabilityDomainCanary + `","body":"` + capabilityBodyCanary + `"}}`, `{"error":`,
		}},
		{ProtocolGoogleGenerateContent, AuthGoogleAPIKey, "/models/synthetic-model:generateContent", []string{
			`{"error":{"code":400,"status":"INVALID_ARGUMENT","message":"unsupported response format"}}`,
			`{"error":{"code":400,"status":"NOT_FOUND","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"ENDPOINT_NOT_FOUND","domain":"generativelanguage.googleapis.com"}]}}`,
			`{"error":{"code":400,"status":"INVALID_ARGUMENT","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"MODEL_NOT_FOUND","domain":"generativelanguage.googleapis.com"}]}}`,
			`{"error":{"code":400,"status":"UNAUTHENTICATED"}}`,
			`{"error":{"code":400,"status":"RESOURCE_EXHAUSTED"}}`,
			`{"error":{"code":400,"status":"PERMISSION_DENIED"}}`,
			`{"error":{"code":400,"status":"INVALID_ARGUMENT","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"UNSUPPORTED_PARAMETER","domain":"generativelanguage.googleapis.com"}]}}`,
			`{"error":{"code":400,"status":"INVALID_ARGUMENT","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"UNSUPPORTED_PARAMETER","domain":"generativelanguage.googleapis.com","metadata":{"parameter":"generationConfig.responseSchema.model"}}]}}`,
			`{"error":{"code":400,"status":"INVALID_ARGUMENT","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"UNSUPPORTED_PARAMETER","domain":"generativelanguage.googleapis.com","metadata":{"parameter":"generationconfig.responseSchema"}}]}}`,
			`{"error":{"code":400,"status":"INVALID_ARGUMENT","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"UNSUPPORTED_PARAMETER","domain":"generativelanguage.googleapis.com","metadata":{"parameter":" generationConfig.responseSchema"}}]}}`,
			`{"error":{"code":400,"status":"INVALID_ARGUMENT","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"unsupported_json_schema","domain":"generativelanguage.googleapis.com"}]}}`,
			`{"error":{"code":400,"status":"INVALID_ARGUMENT","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":{"value":"UNSUPPORTED_JSON_SCHEMA"},"domain":"generativelanguage.googleapis.com"}]}}`,
			`{"error":{"code":400,"status":"INVALID_ARGUMENT","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"UNSUPPORTED_JSON_SCHEMA","domain":"generativelanguage.googleapis.com","metadata":{"parameter":{"value":"generationConfig.responseSchema"}}}]}}`,
			`{"error":{"code":400,"status":"INVALID_ARGUMENT","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"UNSUPPORTED_JSON_SCHEMA","domain":"generativelanguage.googleapis.com","metadata":{"parameter":""}}]}}`,
			`{"error":{"code":400,"status":"INVALID_ARGUMENT","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"UNSUPPORTED_JSON_SCHEMA","reason":"` + capabilityCodeCanary + `","domain":"generativelanguage.googleapis.com"}]}}`,
			`{"error":{"code":400,"status":"INVALID_ARGUMENT","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"UNSUPPORTED_PARAMETER","domain":"generativelanguage.googleapis.com","metadata":{"parameter":"generationConfig.responseSchema","parameter":"model"}}]}}`,
			`{"error":{"code":400,"status":"INVALID_ARGUMENT","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"UNSUPPORTED_JSON_SCHEMA","domain":"generativelanguage.googleapis.com"},{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"MODEL_NOT_FOUND","domain":"generativelanguage.googleapis.com"}]}}`,
			`{"error":{"code":400,"status":"INVALID_ARGUMENT","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"UNSUPPORTED_JSON_SCHEMA","domain":"generativelanguage.googleapis.com"},{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"UNSUPPORTED_JSON_SCHEMA","domain":"generativelanguage.googleapis.com"}]}}`,
			`{"error":{"code":400,"status":"` + capabilityStatusCanary + `","message":"` + capabilityMessageCanary + `","auth":"` + capabilityAuthCanary + `","body":"` + capabilityBodyCanary + `","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"` + capabilityCodeCanary + `","domain":"` + capabilityDomainCanary + `","metadata":{"parameter":"` + capabilityParamCanary + `"}}]}}`, `{"error":`,
		}},
	} {
		t.Run(string(test.protocol), func(t *testing.T) {
			for index, body := range test.bodies {
				t.Run(fmt.Sprintf("case-%d", index), func(t *testing.T) {
					var calls atomic.Int32
					attempts := make(chan capabilityAttempt, 1)
					server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						calls.Add(1)
						var requestBody map[string]any
						decodeErr := json.NewDecoder(r.Body).Decode(&requestBody)
						attempts <- capabilityAttempt{path: r.URL.Path, body: requestBody, err: decodeErr}
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
					attempt := <-attempts
					require.NoError(t, attempt.err)
					assert.Equal(t, test.path, attempt.path)
					if test.protocol == ProtocolGoogleGenerateContent {
						assert.NotContains(t, attempt.body, "model")
					} else {
						assert.Equal(t, "synthetic-model", attempt.body["model"])
					}
					for _, fragment := range []string{
						"unsupported", "parameter", "model", "endpoint", "billing", "policy",
						capabilityResponseCanary, capabilityCredentialValue, capabilityMessageCanary,
						capabilityParamCanary, capabilityCodeCanary, capabilityAuthCanary,
						capabilityStatusCanary, capabilityDomainCanary, capabilityBodyCanary,
					} {
						assert.NotContains(t, negotiationErr.Error(), fragment)
					}
				})
			}
		})
	}
}

func TestCapabilityNegotiationNeverSwitchesProtocolEndpointOrModel(t *testing.T) {
	attempts := make(chan capabilityAttempt, 6)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		decodeErr := json.NewDecoder(r.Body).Decode(&body)
		attempts <- capabilityAttempt{path: r.URL.Path, body: body, err: decodeErr}
		parameter := "max_completion_tokens"
		if _, present := body["max_tokens"]; present {
			parameter = "max_tokens"
		}
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"error":{"type":"invalid_request_error","code":"unsupported_parameter","param":"` + parameter + `"}}`))
	}))
	t.Cleanup(server.Close)
	registry, err := NewDriverRegistry(server.Client(), nil, nil)
	require.NoError(t, err)

	got, err := NewCapabilityChecker(registry).Negotiate(t.Context(),
		capabilityTestCandidate(ProtocolOpenAIChat, server.URL),
		NewCredential(AuthBearer, capabilityCredentialValue))
	require.Error(t, err)
	assert.Empty(t, got)
	for range 6 {
		attempt := <-attempts
		require.NoError(t, attempt.err)
		assert.Equal(t, "/chat/completions", attempt.path)
		assert.Equal(t, "synthetic-model", attempt.body["model"])
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
	assert.NotErrorIs(t, err, context.Canceled)
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
