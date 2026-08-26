---
last_edited: 2026-08-26
---

# Task 9 report: setup-only capability negotiation and models.dev discovery

## Result

- Commit: `080698cefe2e72f608841277deb1359d948b430d`
- Subject: `feat: negotiate provider capabilities at setup`
- Base: `cb9cc3a0bc76603c73a59f36b15b09ad95abc415`

## Files

- `internal/peoplesweep/capability_check.go`
- `internal/peoplesweep/capability_check_test.go`
- `internal/peoplesweep/driver_registry.go`
- `internal/peoplesweep/http_driver.go`
- `internal/peoplesweep/modelsdev.go`
- `internal/peoplesweep/modelsdev_test.go`
- `internal/peoplesweep/provider.go`
- `internal/peoplesweep/testdata/modelsdev/catalog.json`

## Negotiation analysis

- `CapabilityChecker` depends only on the protocol registry. It has no archive, authority, consent, history, credential-store, repair, or persistence dependency.
- Every attempt constructs a fresh sanitized profile for the exact selected protocol, endpoint, and model. Package-owned policy placeholders satisfy profile validation but never enter the wire request.
- The only prompt and schema ask for `{"ok":true}`. Local validation requires that exact single-key object plus safe provider metadata before returning a result.
- OpenAI output attempts are ordered native JSON Schema, JSON object, then prompt JSON. Each mode tries `max_completion_tokens` before `max_tokens`. Anthropic and Google preserve the same output order while omitting the JSON-object representation their registered drivers cannot encode.
- Requested reasoning is tested only after a reasoning-free representation succeeds. Rejection is terminal; the checker never silently drops the requested setting or tries a different representation/provider.
- HTTP 400, 404, and 422 advance only when the selected protocol's bounded structured error envelope carries an exact allowlisted unsupported-representation code. Status alone, free-form messages, generic aggregator errors, wrong endpoint/model, authentication, billing, policy, malformed envelopes, 408, 429, 5xx, transport/timeout, unsafe metadata, and local validation/security failures stop immediately.
- Synthetic output is parsed with duplicate-member detection. Exactly one `ok` member containing boolean `true` is accepted; duplicates in either order, extra members, trailing values, null, and non-boolean values return no response.
- The checker resolves one registered driver before the loop and never changes protocol, endpoint, model, provider, or credential. It has no repair or runtime fallback path.

## Catalog analysis

- `ModelsDevClient.Fetch` is caller-invoked only and always requests `https://models.dev/api.json` with `OpenAI File Downloader, XaiImageApiFetch/1.0`.
- The client constructs a fresh direct transport and never inherits a caller RoundTripper, proxy, cookie jar, TLS client identity, headers, redirect policy, or subsequent mutation. Redirects remain disabled, the total deadline is 15 seconds, the body read is bounded to 8 MiB plus one overflow byte, and every response is bounded-drained/closed.
- The same deadline covers JSON duplicate scanning, provider/model transformation, and deterministic sorting. Context checks abort transformation with the fixed timeout sentinel and return no partial suggestions.
- Requests carry no credential, config, archive, host-identity, query, or body data. Returned errors are fixed sentinels and never include transport text, response bodies, catalog values, or credentials.
- Parsing detects duplicate JSON members and duplicate semantic provider/model IDs, validates key/ID identity, bounds display fields, validates fixed and documented environment-template endpoints, and rejects credentialed, queried, fragmented, unsafe, or remote-plaintext URLs.
- Provider/model/environment output is sorted; environment values are deduplicated. Display names are trimmed before bounded control/format-character validation.
- Protocol candidates come only from exact documented `npm` SDK-shape strings. Provider/model names and IDs never affect mapping. OpenAI's SDK shape remains explicitly ambiguous, and unsupported shapes return an empty candidate list for caller choice.
- Input/output prices use exact rational parsing and checked integer conversion. Positive sub-micro-USD fractions round upward, preventing advisory underestimation; negative, nonnumeric, null-present, and overflowing prices fail closed.
- Catalog suggestions are returned in memory only. No runtime config, stored price, or system state is read or changed. A fetch error remains available to onboarding so custom entry can continue.

## Security review

- Real local TLS tests inspect every capability probe for archive canaries and prove the credential appears only in the selected auth header.
- Status/error tests prove provider-body, candidate, archive, and credential canaries do not reach returned errors.
- Attempt tests prove fixed ordering, strict short-circuiting, no protocol/path/model switching, and no I/O for unsupported local reasoning settings.
- Catalog TLS tests prove the fixed host/path, exact User-Agent, empty body/query, absent sensitive headers/cookies, redirect refusal, caller and total deadlines, byte overflow detection, and response closure on success/error boundaries.
- The committed suite has no live-network dependency. A development-only bounded download plus temporary test overlay parsed the current 4.3 MiB catalog as 202 deterministic provider suggestions.
- Symbol review found catalog and checker construction only in their setup files/tests. Runner, worker, and normal check paths have no models.dev reachability.

## RED

Required focused command before production code:

```text
go test -tags "fts5 sqlite_vec" ./internal/peoplesweep -run 'Capability|ModelsDev' -count=1
# go.kenn.io/msgvault/internal/peoplesweep [go.kenn.io/msgvault/internal/peoplesweep.test]
internal/peoplesweep/capability_check_test.go:67:14: undefined: NewCapabilityChecker
internal/peoplesweep/modelsdev_test.go:53:14: undefined: NewModelsDevClient
internal/peoplesweep/modelsdev_test.go:66:18: undefined: ModelSuggestion
FAIL  go.kenn.io/msgvault/internal/peoplesweep [build failed]
```

Current-shape regression before parser hardening:

```text
go test -tags "fts5 sqlite_vec" ./internal/peoplesweep -run TestModelsDevFetchParsesCurrentFixtureDeterministicallyByAPIShape -count=1
--- FAIL: TestModelsDevFetchParsesCurrentFixtureDeterministicallyByAPIShape
    Received unexpected error: models.dev catalog is invalid
FAIL
```

Cookie/template isolation regressions before the final security fix:

```text
go test -tags "fts5 sqlite_vec" ./internal/peoplesweep -run 'TestModelsDevFetchParsesCurrentFixtureDeterministicallyByAPIShape|TestModelsDevFetchRejectsDuplicateAndUnsafeCatalogData/unsafe_base_template_suffix' -count=1
--- FAIL: TestModelsDevFetchParsesCurrentFixtureDeterministicallyByAPIShape
    Should be empty, but was session=catalog-request-canary-never-send
--- FAIL: TestModelsDevFetchRejectsDuplicateAndUnsafeCatalogData/unsafe_base_template_suffix
    An error is expected but got nil.
FAIL
```

Race regression before fixture synchronization:

```text
go test -race -tags "fts5 sqlite_vec" ./internal/peoplesweep -run 'Capability|ModelsDev|DriverRegistry' -count=1
WARNING: DATA RACE
TestCapabilityNegotiationStopsOnNonCapabilityFailuresAndInvalidOutput/transport_timeout
FAIL
```

## GREEN

Required focused command:

```text
go test -tags "fts5 sqlite_vec" ./internal/peoplesweep -run 'Capability|ModelsDev|DriverRegistry' -count=1
ok  go.kenn.io/msgvault/internal/peoplesweep  0.254s
```

Full people-sweep package:

```text
go test -tags "fts5 sqlite_vec" ./internal/peoplesweep -count=1
ok  go.kenn.io/msgvault/internal/peoplesweep  33.409s
```

Race and static gates:

```text
go test -race -tags "fts5 sqlite_vec" ./internal/peoplesweep -run 'Capability|ModelsDev|DriverRegistry' -count=1
ok  go.kenn.io/msgvault/internal/peoplesweep  1.706s

go fmt ./...
go vet -tags "fts5 sqlite_vec" ./...
git diff --check
[exit 0]
```

## Concerns

- models.dev is mutable external input. The parser accepts the current documented shape and fails closed on unknown unsafe representations; Task 10 must keep catalog failure recoverable and preserve the custom setup path.
- Catalog prices are advisory snapshots only. Task 10 must require explicit operator confirmation before copying them into budget configuration and must never refresh saved runtime prices automatically.

## Review fix: 2026-08-26

- Code commit: `a69b1d86286dc07e0ec3b96fa3488212b8c760fd` (`fix: harden provider capability negotiation`).
- Transport isolation now uses a fresh direct `http.Transport`; the only TLS fixture seam accepts a dial function, root pool, and server name, and cannot inject headers, proxy credentials, cookies, or a client certificate.
- Every registered HTTP protocol now derives the same safe `unsupported_representation` classification only from protocol-specific structured fields and exact bounded codes. Provider errors expose no response text. TLS tests prove two attempts for classified errors in all four families and one attempt for generic/wrong-endpoint/wrong-model/auth/billing/policy/malformed 400/404/422 failures.
- Duplicate-aware synthetic-output validation returns an empty result for ambiguous JSON, including through the real driver/TLS path.
- Catalog parsing now receives the fetch context throughout object scans and provider/model loops, checks before and after sorting, and has a deterministic cancellation hook regression proving timeout after body read returns the fixed error with no partial suggestions.
- The tautological custom-candidate assertion was removed. Production behavior is covered by stable catalog error/no-partial-result assertions; custom-setup continuation remains Task 10 scope.

Review RED commands:

```text
go test -tags "fts5 sqlite_vec" ./internal/peoplesweep -run 'TestCapability(ErrorClassification|NegotiationRejectsAmbiguous|NegotiationStopsAfterGeneric)' -count=1
internal/peoplesweep/capability_check_test.go:223:5: unknown field Output in struct literal of type DriverResponse
internal/peoplesweep/capability_check_test.go:237:12: undefined: ProviderCapabilityError
internal/peoplesweep/capability_check_test.go:239:163: undefined: ProviderCapabilityUnsupportedRepresentation
internal/peoplesweep/capability_check_test.go:240:169: undefined: ProviderCapabilityUnsupportedRepresentation
internal/peoplesweep/capability_check_test.go:241:183: undefined: ProviderCapabilityUnsupportedRepresentation
internal/peoplesweep/capability_check_test.go:242: undefined: ProviderCapabilityUnsupportedRepresentation
internal/peoplesweep/capability_check_test.go:249:31: undefined: classifyProviderCapabilityError
FAIL  go.kenn.io/msgvault/internal/peoplesweep [build failed]

go test -tags "fts5 sqlite_vec" ./internal/peoplesweep -run 'Test(NewModelsDevClient|ModelsDevFetchCancelsDuring|ModelsDevFetchParsesCurrent)' -count=1
internal/peoplesweep/modelsdev_test.go:60:9: client.Jar undefined (type *ModelsDevClient has no field or method Jar)
internal/peoplesweep/modelsdev_test.go:259:18: undefined: modelsDevHooks
internal/peoplesweep/modelsdev_test.go:262:26: undefined: ErrModelsDevTimeout
internal/peoplesweep/modelsdev_test.go:276:9: undefined: newModelsDevClientForTest
FAIL  go.kenn.io/msgvault/internal/peoplesweep [build failed]
```

Review GREEN commands:

```text
go test -tags "fts5 sqlite_vec" ./internal/peoplesweep -run 'Test(OpenAIChat|OpenAIResponses|AnthropicMessages|GoogleGenerateContent|DriverRegistry|Capability|ModelsDev)' -count=1
ok  go.kenn.io/msgvault/internal/peoplesweep  9.460s

go test -tags "fts5 sqlite_vec" ./internal/peoplesweep -count=1
ok  go.kenn.io/msgvault/internal/peoplesweep  33.101s

go test -race -tags "fts5 sqlite_vec" ./internal/peoplesweep -count=1
ok  go.kenn.io/msgvault/internal/peoplesweep  41.330s

go fmt ./...
go vet -tags "fts5 sqlite_vec" ./internal/peoplesweep/...
git diff --check
[exit 0]
```

Open concern: structured capability codes vary across compatible aggregators. Unknown or merely message-shaped errors intentionally fail closed so onboarding can fall back to explicit manual/custom selection without changing state.

## Review fix round 2: 2026-08-26

- Code commit: `e30ad5b52062ba201e85007618394c31e77cc8ff` (`fix: scope capability negotiation retries`).
- `capabilityMiss` now requires both an exact 400/404/422 status and the bounded unsupported-representation classification. Forged classifications on 401, 403, 408, 409, 429, and 500 stop.
- Generic `unsupported_parameter` and `unsupported_value` codes now require a bounded structured `param` that matches a field present in the active protocol profile: its structured-output field, token-limit field, or represented reasoning field. `model`, endpoint, auth, billing, policy, unknown parameters, and free-form messages never classify.
- Real TLS tests cover generic, wrong-endpoint, wrong-model, auth, billing, policy, and malformed failures for Chat, Responses, Anthropic, and Gemini. Every case returns a redacted error after exactly one request with no protocol switching.

Round-2 RED:

```text
go test -tags "fts5 sqlite_vec" ./internal/peoplesweep -run 'TestCapability(MissRequires|ErrorClassification|NegotiationStopsAfterUnclassifiedErrorForEvery)' -count=1
internal/peoplesweep/capability_check_test.go:256:63: cannot use profile (variable of struct type ProviderProfile) as Protocol value in argument to classifyProviderCapabilityError
FAIL  go.kenn.io/msgvault/internal/peoplesweep [build failed]
```

Round-2 GREEN:

```text
go test -tags "fts5 sqlite_vec" ./internal/peoplesweep -run 'Test(OpenAIChat|OpenAIResponses|AnthropicMessages|GoogleGenerateContent|DriverRegistry|Capability|ModelsDev)' -count=1
ok  go.kenn.io/msgvault/internal/peoplesweep  7.876s

go test -tags "fts5 sqlite_vec" ./internal/peoplesweep -count=1
ok  go.kenn.io/msgvault/internal/peoplesweep  31.521s

go test -race -tags "fts5 sqlite_vec" ./internal/peoplesweep -count=1
ok  go.kenn.io/msgvault/internal/peoplesweep  50.622s

go fmt ./...
go vet -tags "fts5 sqlite_vec" ./internal/peoplesweep/...
git diff --check
[exit 0]
```

Open concern unchanged: compatible providers may omit structured parameter metadata. Those errors deliberately fail closed and require explicit manual/custom selection.

## Review fix round 3: 2026-08-26

- Code commit: `852ec784541873f7b78fe0f691beb1a7aefe11e9` (`fix: match capability errors to attempted fields`).
- Generic unsupported codes in every protocol family now require a bounded structured parameter. Anthropic reads `error.param`; Gemini reads duplicate-safe `ErrorInfo.metadata.parameter`. Parameterless generic codes stop; only exact representation-specific codes may omit a parameter.
- Parameter matching is an exact per-attempt allowlist. Native-schema, JSON-object, and prompt modes expose different fields; reasoning fields require reasoning to be emitted; arbitrary descendants and case/nesting variants are rejected.
- Four-family TLS negatives cover parameterless/model/endpoint/auth/billing/policy, wrong-mode and absent-reasoning fields, fake descendants/casing, duplicate keys/params, and malformed envelopes. Each stops after one call and errors omit individual message/param/code/body/credential fragments.

Round-3 RED:

```text
go test -tags "fts5 sqlite_vec" ./internal/peoplesweep -run 'TestCapability(ErrorClassification|NegotiationRetriesClassified)' -count=1
--- FAIL: TestCapabilityErrorClassificationRequiresProtocolSpecificStructuredCode/anthropic
    expected: "unsupported_representation" actual: ""
--- FAIL: TestCapabilityErrorClassificationRequiresProtocolSpecificStructuredCode/google
    expected: "unsupported_representation" actual: ""
FAIL
```

Round-3 GREEN:

```text
go test -tags "fts5 sqlite_vec" ./internal/peoplesweep -run 'Test(OpenAIChat|OpenAIResponses|AnthropicMessages|GoogleGenerateContent|DriverRegistry|Capability|ModelsDev)' -count=1
ok  go.kenn.io/msgvault/internal/peoplesweep  19.355s
go test -tags "fts5 sqlite_vec" ./internal/peoplesweep -count=1
ok  go.kenn.io/msgvault/internal/peoplesweep  49.093s
go test -race -tags "fts5 sqlite_vec" ./internal/peoplesweep -count=1
ok  go.kenn.io/msgvault/internal/peoplesweep  50.946s
go fmt ./...
go vet -tags "fts5 sqlite_vec" ./internal/peoplesweep/...
git diff --check
[exit 0]
```

Open concern unchanged: compatible implementations lacking exact structured parameter metadata fail closed to manual/custom selection.

## Review fix round 4: 2026-08-26

- Code commit: `27b2688a0244d63ff9f1f1c03f6f523bb0f40864` (`fix: fail closed on ambiguous capability errors`).
- Parameterless representation codes now use exact case-sensitive protocol and active-output-mode allowlists. Prompt-only attempts, wrong-family spellings, casing aliases, and representation codes paired with present empty, nested, or unrelated parameters stop.
- Generic parameter matching no longer trims input. It matches only exact capability fields emitted by the current encoder, including the selected Chat token field and emitted `reasoning_effort`, `reasoning`, and `reasoning.enabled` fields.
- Gemini scans all 32 bounded details before classifying. It requires exactly one duplicate-safe same-domain `ErrorInfo`; conflicting or repeated details and malformed or duplicate metadata fail closed.
- Four-family real TLS regressions prove exact one-call stops with the selected path/model, no protocol switching, exact generic-parameter retries, duplicate code/parameter rejection, and individual message/parameter/code/auth/status/domain/body/credential redaction.

Round-4 RED:

```text
go test -tags "fts5 sqlite_vec" ./internal/peoplesweep -run 'TestCapability(Parameter|Representation|DriversReject|NegotiationRetries|NegotiationStopsAfterUnclassifiedErrorForEvery)' -count=1
--- FAIL: TestCapabilityParameterMustBeExactlyEmittedByAttempt
--- FAIL: TestCapabilityRepresentationCodeMustMatchProtocolAndActiveMode
--- FAIL: TestCapabilityDriversRejectRepresentationCodesForPromptOnlyAttempts
--- FAIL: TestCapabilityNegotiationStopsAfterUnclassifiedErrorForEveryProtocol
FAIL  go.kenn.io/msgvault/internal/peoplesweep  0.303s

go test -tags "fts5 sqlite_vec" ./internal/peoplesweep -run 'TestCapabilityNegotiationStopsAfterUnclassifiedErrorForEveryProtocol' -count=1
--- FAIL: TestCapabilityNegotiationStopsAfterUnclassifiedErrorForEveryProtocol
    --- FAIL: TestCapabilityNegotiationStopsAfterUnclassifiedErrorForEveryProtocol/openai_chat/case-12
    --- FAIL: TestCapabilityNegotiationStopsAfterUnclassifiedErrorForEveryProtocol/openai_chat/case-13
    --- FAIL: TestCapabilityNegotiationStopsAfterUnclassifiedErrorForEveryProtocol/openai_responses/case-13
    --- FAIL: TestCapabilityNegotiationStopsAfterUnclassifiedErrorForEveryProtocol/openai_responses/case-14
    --- FAIL: TestCapabilityNegotiationStopsAfterUnclassifiedErrorForEveryProtocol/anthropic_messages/case-13
    --- FAIL: TestCapabilityNegotiationStopsAfterUnclassifiedErrorForEveryProtocol/anthropic_messages/case-14
    --- FAIL: TestCapabilityNegotiationStopsAfterUnclassifiedErrorForEveryProtocol/google_generate_content/case-12
    --- FAIL: TestCapabilityNegotiationStopsAfterUnclassifiedErrorForEveryProtocol/google_generate_content/case-13
FAIL  go.kenn.io/msgvault/internal/peoplesweep  0.526s
```

Round-4 GREEN:

```text
go test -tags "fts5 sqlite_vec" ./internal/peoplesweep -run 'Test(OpenAIChat|OpenAIResponses|AnthropicMessages|GoogleGenerateContent|DriverRegistry|Capability|ModelsDev)' -count=1
ok  go.kenn.io/msgvault/internal/peoplesweep  12.229s

go test -tags "fts5 sqlite_vec" ./internal/peoplesweep -count=1
ok  go.kenn.io/msgvault/internal/peoplesweep  43.055s

go test -race -tags "fts5 sqlite_vec" ./internal/peoplesweep -count=1
ok  go.kenn.io/msgvault/internal/peoplesweep  49.966s

go fmt ./...
go vet -tags "fts5 sqlite_vec" ./...
git diff --check
[exit 0]
```

Open concern unchanged: compatible implementations that do not return the exact structured code and parameter contract fail closed to manual/custom selection.

## Review fix round 5: 2026-08-26

- Code commit: `42305c5b1f9eac5c15f3521ec7ba0c7670f5800f` (`fix: require absent representation error parameters`).
- Representation-specific error codes now classify only when the protocol-specific structured parameter field is completely absent. Any present value, including an exact active representation field, stops negotiation; generic unsupported codes keep their exact emitted-field matching.
- Unit coverage spans every active representation-code protocol/mode pair. Real TLS coverage spans both OpenAI representation modes plus Anthropic and Gemini native schema, and negotiation-level coverage proves one call, no representation switch, fixed path/model, and code/parameter/message/body/credential redaction for all four protocol families.
- Parameterless representation-code TLS cases still retry once and accept the next successful synthetic response for every protocol family.

Round-5 RED:

```text
go test -tags "fts5 sqlite_vec" ./internal/peoplesweep -run 'TestCapability(RepresentationCodesRequireAbsentParameter|DriversRejectParameterizedRepresentationCodesForEveryActiveMode|NegotiationStopsOnParameterizedRepresentationCodeForEveryProtocol|NegotiationRetriesClassifiedErrorsForEachProtocolFamily)' -count=1
--- FAIL: TestCapabilityRepresentationCodesRequireAbsentParameter
    --- FAIL: chat_native_schema
    --- FAIL: chat_JSON_object
    --- FAIL: responses_native_schema
    --- FAIL: responses_JSON_object
    --- FAIL: anthropic_native_schema
    --- FAIL: google_native_schema
    --- FAIL: google_native_response_format
--- FAIL: TestCapabilityDriversRejectParameterizedRepresentationCodesForEveryActiveMode
    --- FAIL: openai_chat_native_schema
    --- FAIL: openai_chat_JSON_object
    --- FAIL: openai_responses_native_schema
    --- FAIL: openai_responses_JSON_object
    --- FAIL: anthropic_native_schema
    --- FAIL: google_native_schema
--- FAIL: TestCapabilityNegotiationStopsOnParameterizedRepresentationCodeForEveryProtocol
    --- FAIL: openai_chat (expected one call, got three)
    --- FAIL: openai_responses (expected one call, got two)
    --- FAIL: anthropic (expected one call, got two)
    --- FAIL: google (expected one call, got two)
FAIL
```

Round-5 GREEN:

```text
go test -tags "fts5 sqlite_vec" ./internal/peoplesweep -run 'Test(OpenAIChat|OpenAIResponses|AnthropicMessages|GoogleGenerateContent|DriverRegistry|Capability|ModelsDev)' -count=1
ok  go.kenn.io/msgvault/internal/peoplesweep  6.579s

go test -tags "fts5 sqlite_vec" ./internal/peoplesweep -count=1
ok  go.kenn.io/msgvault/internal/peoplesweep  28.402s

go test -race -tags "fts5 sqlite_vec" ./internal/peoplesweep -count=1
ok  go.kenn.io/msgvault/internal/peoplesweep  40.653s

go fmt ./...
go vet -tags "fts5 sqlite_vec" ./...
git diff --check
[exit 0]
```

Open concern unchanged: compatible implementations that do not return the exact structured code and parameter contract fail closed to manual/custom selection.
