package peoplesweep

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"time"
)

const capabilityProfileName = "provider-check"

// NegotiatedCapabilities is the exact synthetic representation accepted by a
// selected protocol, endpoint, and model. Callers decide whether to persist
// these values as part of an onboarding transaction.
type NegotiatedCapabilities struct {
	OutputMode          OutputMode
	TokenLimitParameter string
	ReasoningEffort     string
	ReasoningMode       string
	DriverVersion       string
	Response            StructuredResponse
}

// CapabilityChecker owns setup-only synthetic negotiation. It has no archive,
// authority, consent, history, credential-store, or persistence dependency.
type CapabilityChecker struct {
	registry *DriverRegistry
}

// NewCapabilityChecker constructs a caller-invoked setup checker.
func NewCapabilityChecker(registry *DriverRegistry) *CapabilityChecker {
	return &CapabilityChecker{registry: registry}
}

// Negotiate checks one selected protocol, endpoint, model, and credential with
// package-owned synthetic input. It never repairs, falls back, or changes the
// selected provider identity.
func (c *CapabilityChecker) Negotiate(
	ctx context.Context,
	candidate ProviderConfig,
	credential Credential,
) (NegotiatedCapabilities, error) {
	driver, err := c.registry.capabilityDriver(candidate.Protocol)
	if err != nil {
		return NegotiatedCapabilities{}, errors.New("provider capability negotiation is unavailable")
	}
	if err := validateCapabilityReasoning(candidate); err != nil {
		return NegotiatedCapabilities{}, errors.New("provider capability negotiation settings are invalid")
	}

	for _, mode := range capabilityOutputModes(candidate.Protocol) {
		for _, tokenParameter := range capabilityTokenParameters(candidate.Protocol) {
			base, profileErr := capabilityProfile(candidate, mode, tokenParameter, false)
			if profileErr != nil {
				return NegotiatedCapabilities{}, errors.New("provider capability negotiation settings are invalid")
			}
			response, attemptErr := runCapabilityAttempt(ctx, candidate.RequestTimeout,
				driver, base, credential)
			if attemptErr != nil {
				if capabilityMiss(attemptErr) {
					continue
				}
				return NegotiatedCapabilities{}, errors.New("provider capability negotiation failed")
			}

			result := NegotiatedCapabilities{
				OutputMode: mode, TokenLimitParameter: tokenParameter,
				DriverVersion: base.DriverVersion, Response: response,
			}
			if !capabilityReasoningRequested(candidate) {
				result.ReasoningEffort = candidate.ReasoningEffort
				result.ReasoningMode = candidate.ReasoningMode
				return result, nil
			}

			reasoningProfile, profileErr := capabilityProfile(candidate, mode, tokenParameter, true)
			if profileErr != nil {
				return NegotiatedCapabilities{}, errors.New("provider capability negotiation settings are invalid")
			}
			reasoningResponse, reasoningErr := runCapabilityAttempt(ctx, candidate.RequestTimeout,
				driver, reasoningProfile, credential)
			if reasoningErr != nil {
				return NegotiatedCapabilities{}, errors.New("provider capability negotiation rejected requested reasoning settings")
			}
			result.ReasoningEffort = candidate.ReasoningEffort
			result.ReasoningMode = candidate.ReasoningMode
			result.Response = reasoningResponse
			return result, nil
		}
	}
	return NegotiatedCapabilities{}, errors.New("provider capability negotiation found no supported structured output mode")
}

func capabilityOutputModes(protocol Protocol) []OutputMode {
	switch protocol {
	case ProtocolOpenAIChat, ProtocolOpenAIResponses:
		return []OutputMode{OutputModeNativeJSONSchema, OutputModeJSONObject, OutputModePromptJSON}
	case ProtocolAnthropicMessages, ProtocolGoogleGenerateContent:
		return []OutputMode{OutputModeNativeJSONSchema, OutputModePromptJSON}
	default:
		return nil
	}
}

func capabilityTokenParameters(protocol Protocol) []string {
	if protocol == ProtocolOpenAIChat {
		return []string{"max_completion_tokens", "max_tokens"}
	}
	return []string{""}
}

func validateCapabilityReasoning(candidate ProviderConfig) error {
	if err := validateReasoning(candidate); err != nil {
		return err
	}
	switch candidate.Protocol {
	case ProtocolOpenAIChat:
		return nil
	case ProtocolOpenAIResponses:
		if candidate.ReasoningMode == "" || candidate.ReasoningMode == "provider_default" {
			return nil
		}
	case ProtocolAnthropicMessages, ProtocolGoogleGenerateContent:
		if candidate.ReasoningEffort == "" &&
			(candidate.ReasoningMode == "" || candidate.ReasoningMode == "provider_default") {
			return nil
		}
	}
	return errors.New("reasoning settings are not represented by the selected protocol")
}

func capabilityReasoningRequested(candidate ProviderConfig) bool {
	return candidate.ReasoningEffort != "" ||
		(candidate.ReasoningMode != "" && candidate.ReasoningMode != "provider_default")
}

func capabilityProfile(
	candidate ProviderConfig,
	mode OutputMode,
	tokenParameter string,
	includeReasoning bool,
) (ProviderProfile, error) {
	credentialSource := CredentialStored
	if candidate.Auth == AuthNone {
		credentialSource = CredentialNone
	}
	provider := ProviderConfig{
		Protocol: candidate.Protocol, Endpoint: candidate.Endpoint, Model: candidate.Model,
		Auth: candidate.Auth, Credential: credentialSource,
		OutputMode: mode, TokenLimitParameter: tokenParameter,
		DriverVersion:    defaultDriverVersion(candidate.Protocol),
		RetentionPosture: "synthetic-check-only", TrainingPosture: "synthetic-check-only",
		AllowedSources: []SourceClass{SourceConversationText}, SourceSince: "1970-01-01",
		RequestTimeout: candidate.RequestTimeout,
	}
	if includeReasoning || !capabilityReasoningRequested(candidate) {
		provider.ReasoningEffort = candidate.ReasoningEffort
		provider.ReasoningMode = candidate.ReasoningMode
	}
	config := Config{
		Enabled: true, Provider: ProviderSelection{Name: capabilityProfileName},
		Providers: map[string]ProviderConfig{capabilityProfileName: provider},
	}
	config.ApplyDefaults()
	return config.Profile()
}

func runCapabilityAttempt(
	ctx context.Context,
	timeout time.Duration,
	driver StructuredDriver,
	profile ProviderProfile,
	credential Credential,
) (StructuredResponse, error) {
	request := capabilitySyntheticRequest()
	prepared, err := driver.Prepare(profile, request)
	if err != nil {
		return StructuredResponse{}, err
	}
	if timeout <= 0 {
		timeout = time.Minute
	}
	attemptCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	driverResponse, err := driver.GeneratePrepared(attemptCtx, profile, credential, prepared)
	if err != nil {
		return StructuredResponse{}, err
	}
	return validateCapabilityResponse(driverResponse)
}

func capabilitySyntheticRequest() StructuredRequest {
	return StructuredRequest{
		ProgramID: "provider-check", ProgramVersion: "1",
		InputText:  "Return an object with ok set to true.",
		SchemaName: "provider_check", JSONSchema: slices.Clone(syntheticCheckSchema),
		MaxOutputTokens: 16,
	}
}

func validateCapabilityResponse(driverResponse DriverResponse) (StructuredResponse, error) {
	response := structuredResponseFromDriver(driverResponse)
	if !safeProviderMetadata(response.ProviderVersion) ||
		!safeProviderMetadata(response.ModelVersion) ||
		(response.ProviderRequestID != "" && !safeProviderMetadata(response.ProviderRequestID)) {
		return StructuredResponse{}, errors.New("provider capability response metadata is invalid")
	}
	output, valid := decodeUniqueCapabilityObject(response.Output)
	if !valid || len(output) != 1 {
		return StructuredResponse{}, errors.New("provider capability response is invalid")
	}
	var okValue bool
	if err := json.Unmarshal(output["ok"], &okValue); err != nil || !okValue {
		return StructuredResponse{}, errors.New("provider capability response is invalid")
	}
	response.Output = append(json.RawMessage(nil), response.Output...)
	return response, nil
}

func decodeUniqueCapabilityObject(raw []byte) (map[string]json.RawMessage, bool) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	start, err := decoder.Token()
	if err != nil || start != json.Delim('{') {
		return nil, false
	}
	result := make(map[string]json.RawMessage)
	for decoder.More() {
		token, tokenErr := decoder.Token()
		key, valid := token.(string)
		if tokenErr != nil || !valid {
			return nil, false
		}
		if _, duplicate := result[key]; duplicate {
			return nil, false
		}
		var value json.RawMessage
		if decoder.Decode(&value) != nil {
			return nil, false
		}
		result[key] = value
	}
	end, err := decoder.Token()
	if err != nil || end != json.Delim('}') {
		return nil, false
	}
	var trailing any
	return result, errors.Is(decoder.Decode(&trailing), io.EOF)
}

func capabilityMiss(err error) bool {
	var providerErr *ProviderError
	if !errors.As(err, &providerErr) {
		return false
	}
	return providerErr.Capability == ProviderCapabilityUnsupportedRepresentation
}
