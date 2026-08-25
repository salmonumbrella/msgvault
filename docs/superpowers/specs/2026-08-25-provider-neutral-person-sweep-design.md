---
last_edited: 2026-08-25
---

# Provider-neutral person sweep

## Summary

The person sweep should accept a user-selected model through a small set of protocol drivers instead of treating OpenAI Chat Completions as the universal wire format. This makes direct API-key providers such as Z.AI GLM, Kimi, OpenRouter, and Venice work without provider-specific presets. It also keeps the existing Codex logged-in-session path and allows local OAuth gateways such as open-agent-api through the protocol they expose.

Provider discovery happens only during `msgvault person provider add`. The command may consult models.dev for a current provider and model list, but it saves a complete local profile after the user confirms it. Sweeps never call models.dev, never change provider automatically, and never send archive data during setup checks.

Msgvault owns consent, privacy policy, deterministic request preparation, budgets, retries, strict local schema validation, and one bounded repair attempt. Drivers only translate the provider-neutral structured request to and from a protocol.

## Goals

- Let a user configure direct API keys, environment-backed keys, loopback services, or an existing logged-in Codex session.
- Support OpenAI Chat Completions, OpenAI Responses, Anthropic Messages, and Google Gemini `generateContent` as first-class protocol families.
- Support OpenAI-compatible aggregators and gateways, including OpenRouter, Venice, and open-agent-api, without special runtime code for each service.
- Support current reasoning models such as GLM 5.3 and Kimi K3 without baking model names into msgvault.
- Keep every archive disclosure behind the existing exact-profile consent boundary.
- Keep runtime deterministic and offline except for the selected inference endpoint.
- Validate every model result locally against msgvault's JSON Schema, even when a provider claims native schema enforcement.

## Non-goals

- Automatic provider fallback, load balancing, price routing, or model substitution.
- A universal proxy embedded in msgvault.
- Importing or reverse-engineering third-party OAuth sessions. The existing Codex app-server integration remains the only native logged-in-session driver.
- Runtime dependency on models.dev or any other provider catalog.
- Automatic protocol detection using real archive data.
- Arbitrary provider request bodies, arbitrary response templates, or an unbounded `extra_body` escape hatch.
- Cloud IAM integrations such as AWS Bedrock, Vertex AI service accounts, or Azure managed identity in this change. They can be added later as drivers or used through a supported gateway.

## Architecture

```text
onboarding only
models.dev or custom entry
          |
          v
user confirmation -> synthetic {"ok": true} negotiation
          |
          v
named local profile + separate credential + verified fingerprint
          |
          v
active profile -> protocol driver -> selected endpoint
          |                                |
          +<- local JSON Schema validation-+
                         |
                         +-- one same-profile repair request if needed
```

There are three boundaries:

1. The runner owns policy: profile resolution, credential access, consent, verification, disclosure checks, budgets, deadlines, retry classification, local schema validation, and repair.
2. A protocol driver owns deterministic wire encoding and safe response normalization for one API family.
3. Onboarding owns mutable discovery data and synthetic capability negotiation. Its output is an explicit runtime profile, not a live catalog reference.

Gateways are not a separate protocol. A gateway profile selects the driver for the API it exposes. For example, open-agent-api uses the OpenAI Chat driver; OpenRouter and Venice also use the OpenAI Chat driver.

## Configuration

Profiles are named so users can retain alternatives, but exactly one profile is active for people sweeps.

```toml
[people.sweep]
provider = "glm"

[people.sweep.providers.glm]
protocol = "openai_chat"
endpoint = "https://api.z.ai/api/paas/v4"
model = "glm-5.3"
auth = "bearer"
credential = "stored"
output_mode = "json_object"
token_limit_parameter = "max_tokens"
reasoning_effort = "max"
request_timeout = "1m"
retention_posture = "provider-declared"
training_posture = "provider-declared"
allowed_sources = ["conversation_text", "meeting_text"]
source_since = "2026-01-01"
allow_sensitive = false
```

The supported protocol identifiers are:

- `openai_chat`
- `openai_responses`
- `anthropic_messages`
- `google_generate_content`
- `codex_app_server`

The supported negotiated output modes are:

- `native_json_schema`
- `json_object`
- `prompt_json`

`token_limit_parameter` is only configurable for OpenAI Chat profiles and is either `max_completion_tokens` or `max_tokens`. The other drivers use their protocol-defined field: `max_output_tokens` for Responses, `max_tokens` for Anthropic, and `generationConfig.maxOutputTokens` for Google. This distinction is required because GLM-compatible endpoints reject the `max_completion_tokens` body emitted by the current PR.

`reasoning_effort` is an optional normalized value: `low`, `medium`, `high`, or `max`. A driver sends it only when the onboarding check proves that the chosen endpoint and model accept it. `reasoning_mode` may be `provider_default`, `enabled`, or `disabled` where the protocol has a distinct thinking switch. Unsupported values fail setup instead of being silently discarded.

Msgvault does not include provider presets. The selected endpoint, protocol, model, auth scheme, output mode, token field, reasoning settings, request timeout, and privacy declarations are all persisted explicitly. Provider names from models.dev are discovery labels only.

## Credentials

Credentials never appear in TOML, policy JSON, logs, command-line arguments, provider errors, or consent history.

Profiles support three credential sources:

- `stored`: the interactive command reads the key without echo and writes it below the configured msgvault tokens directory as `people-providers/<profile>.json`.
- `env`: the profile stores an environment variable name and resolves its value only in the inference subprocess boundary.
- `none`: allowed only for a loopback HTTP endpoint.

Stored credential files use mode `0600`, an atomic create-or-replace sequence, a private parent directory, and the repository's symlink-safe path rules. The JSON records the auth scheme and secret, but never provider policy or archive data. Removing a profile removes its dedicated credential only after resolving and validating the exact target path.

The initial auth schemes are `bearer`, `x_api_key`, `google_api_key`, and `none`. They map to `Authorization: Bearer`, `x-api-key`, `x-goog-api-key`, and no header. A later custom-header mode must receive its own safety review; onboarding does not accept arbitrary headers in this change.

Codex app-server does not use this credential store. It continues to rely on the separately attested local Codex session and executable boundary.

## Onboarding and models.dev

`msgvault person provider add [name]` is the only path that consults models.dev. It fetches the fixed HTTPS catalog endpoint with a bounded response size and timeout, disabled redirects, and the user agent `OpenAI File Downloader, XaiImageApiFetch/1.0`. It does not send a credential, existing configuration, host identifier, or archive content. The catalog response is not persisted.

The catalog provides discovery hints: provider name, API base URL, expected environment names, model identifiers, and capabilities such as reasoning and structured output. A small protocol-family mapper interprets catalog SDK shapes; this maps API shapes, not provider identities. Ambiguous or unsupported entries require the user to choose a protocol.

The user sees and confirms the final endpoint, protocol, model, auth scheme, credential source, output behavior, and privacy declarations before anything is saved. A `--custom` path accepts those fields directly when the catalog is unavailable, incomplete, or wrong. Catalog prices may be shown as a setup hint but do not become runtime truth unless the user explicitly saves budget prices.

Adding a profile then runs a synthetic check. The check contains only an instruction and schema for returning `{"ok": true}`. It tries, in order:

1. Native JSON Schema.
2. JSON-object mode.
3. Prompt-enforced JSON.

For OpenAI Chat it also negotiates the token-limit field. Optional reasoning settings are checked separately so a server cannot appear compatible only because it ignored them. Failed probes do not broaden the request or try another provider. The successful output mode and token behavior are saved in the profile; runtime never probes again.

The check stores the profile fingerprint, check time, safe request ID, reported model version, negotiated output mode, and driver version in the database. A sweep requires a successful check for its exact fingerprint as well as active consent. Editing any fingerprinted setting makes both the old check and old consent inapplicable.

Catalog failure affects assisted onboarding only. Existing profiles and sweeps continue unchanged.

## Driver contract

The current `StructuredRequest` remains the provider-neutral input. Each driver implements a narrow contract equivalent to:

```go
type StructuredDriver interface {
	Prepare(profile ProviderProfile, request StructuredRequest) (PreparedStructuredRequest, error)
	GeneratePrepared(ctx context.Context, profile ProviderProfile, credential Credential, prepared PreparedStructuredRequest) (DriverResponse, error)
}
```

`Prepare` must produce the exact deterministic request bytes before the runner reserves a budget. `GeneratePrepared` verifies that the bytes still match, performs one network or app-server call, and returns candidate JSON plus safe metadata and normalized token usage. It does not retry, repair, validate the business schema, load credentials, change models, or choose providers.

The driver registry is keyed by protocol, not provider name. The existing Codex transport becomes the `codex_app_server` driver rather than a special branch in callers.

### OpenAI Chat Completions

- Sends `POST <endpoint>/chat/completions`.
- Parses `choices[0].message.content` and Chat Completions usage fields.
- Encodes `response_format.type=json_schema`, `response_format.type=json_object`, or a prompt-only contract according to the saved output mode.
- Uses the saved `max_completion_tokens` or `max_tokens` field.
- Carries normalized reasoning options only when verified.

This driver covers direct OpenAI-compatible endpoints and aggregators such as Z.AI GLM, Kimi's OpenAI surface, OpenRouter, Venice, and open-agent-api. Compatibility is a property of the saved profile and successful synthetic check, not of the brand name. OpenRouter and Venice native schema support is model-dependent, so onboarding may settle on JSON-object or prompt mode for a selected model.

### OpenAI Responses

- Sends `POST <endpoint>/responses`.
- Uses Responses structured text format fields and `max_output_tokens`.
- Extracts candidate text from the documented output content blocks rather than assuming Chat Completions choices.
- Normalizes Responses usage and request metadata.

### Anthropic Messages

- Sends `POST <endpoint>/v1/messages`.
- Uses `x-api-key`, the required Anthropic version header, and `max_tokens`.
- Uses the protocol's verified structured-output or tool schema shape when available; otherwise it uses the saved prompt-JSON mode.
- Extracts candidate JSON from the matching content or tool block and normalizes Messages usage.

This driver also allows Kimi through its Anthropic-compatible surface when the user chooses that protocol.

### Google Gemini generateContent

- Sends the selected model's `generateContent` request to the configured API base.
- Uses `x-goog-api-key` and `generationConfig.maxOutputTokens`.
- Uses `responseMimeType` and `responseSchema` when the synthetic check verifies them; otherwise it uses prompt JSON.
- Extracts candidate text from Gemini candidates and normalizes usage metadata.

### Codex app-server

The existing attested, packet-only Codex integration remains intact. Its device login and model-list commands remain scoped to Codex profiles. It joins the driver registry but does not inherit HTTP credential or onboarding behavior that would weaken its execution boundary.

## Validation and repair

The runner parses one JSON value and validates it locally against the exact msgvault schema for every output mode. Native provider enforcement is defense in depth, not the trust boundary.

If parsing or validation fails, the runner may make exactly one repair request to the same profile, endpoint, and model. The repair input contains the original structured request, the invalid candidate capped to the existing response limit, and bounded local validation errors. It contains no additional archive context. The repair request is separately prepared, reserved, journaled, charged against request and token limits, and subject to the same deadline and consent fingerprint.

If the repair budget is unavailable or the repaired result is invalid, the work item fails with `invalid_structured_output`. Msgvault never sends the request to another provider as recovery.

## Consent, privacy, and fingerprints

The exact profile fingerprint continues to gate all archive egress. It includes:

- protocol, canonical endpoint, model, auth scheme, and credential source reference but never the secret value;
- output mode, token-limit behavior, reasoning settings, and driver version;
- retention and training postures, allowed source classes and date bounds, and sensitive-data permission;
- execution boundary where applicable;
- packet renderer policy, disclosed packet fields, and program fingerprint.

Changing any of these fields requires a new synthetic check and explicit consent. Rotating the secret value without changing its source reference does not require renewed consent because it does not change where or how archive content is disclosed.

Remote endpoints require HTTPS. Plain HTTP is allowed only on loopback with `auth=none`. Redirects remain disabled so consent to one endpoint cannot be redirected to another host. Response bodies, archive prompts, credentials, and arbitrary provider headers are discarded from errors and logs. Safe request IDs, status codes, retry delays, token usage, and reported model versions may be recorded after character and size validation.

For aggregators, the consent screen states that the selected endpoint may route requests to upstream model operators under that service's own policy. Msgvault still sends only to the configured endpoint and does not perform its own provider fallback.

## Budgets and failures

Request, input-token, output-token, and daily/run/person limits remain authoritative. A repair consumes another request and its own token reservation. Provider-reported usage is normalized when present and validated as non-negative. Missing usage is recorded as unknown and conservatively reconciled against the reservation; it must not turn into zero-cost accounting.

Estimated cost caps remain operator-configured. models.dev prices can seed the onboarding prompt, but catalog changes never alter a saved cost policy. Subscription, local, and gateway profiles can rely on request and token caps with estimated cost disabled.

Failures are classified consistently across drivers:

- configuration, credential, failed-check, and consent errors stop immediately;
- HTTP 408, 429, and 5xx responses retain the existing bounded retry policy and safe `Retry-After` handling;
- other HTTP 4xx responses are terminal;
- timeouts and temporary transport failures are retryable within the existing bounds;
- malformed envelopes, oversized bodies, missing candidate content, unsafe metadata, and schema failures are structured-output errors;
- unsupported protocol features found during onboarding are capability errors and do not reach a sweep.

Drivers never include provider response bodies in returned errors.

## CLI behavior

The provider commands become:

- `msgvault person provider add [name]` for interactive catalog-assisted setup and synthetic verification.
- `msgvault person provider add [name] --custom` for explicit setup without the catalog.
- `msgvault person provider list` to show named profiles, the active marker, verification state, and consent state without secrets.
- `msgvault person provider use <name>` to make one verified profile active.
- `msgvault person provider check [name]` to rerun the synthetic check and save the result for the exact fingerprint.
- `msgvault person provider remove <name>` to revoke its active consent, retire the profile record, remove its stored credential, and refuse removal when active unless another profile is selected or sweeps are disabled.

`status`, `consent`, `revoke`, and `history` keep their exact-fingerprint semantics and accept an optional profile name. `login` and `models` remain available only for `codex_app_server` profiles. Non-interactive setup accepts an environment-variable reference or an API key on stdin; it never accepts a secret in a process argument.

The daemon CLI allowlist is extended only for these exact subcommands and flags. Secret-bearing setup executes at the trusted local CLI boundary and does not forward a raw key through daemon HTTP metadata.

## Migration

The PR's legacy `[people.sweep.provider]` table loads as a named `default` profile only when the new `people.sweep.provider = "name"` selector and `people.sweep.providers` tables are absent. A document that mixes old and new shapes is rejected rather than merged. Any command that rewrites provider configuration emits only the new form.

The legacy `api_key_env` becomes `credential="env"` plus its exact environment name. The existing OpenAI-compatible transport becomes `protocol="openai_chat"`, `output_mode="native_json_schema"`, and `token_limit_parameter="max_completion_tokens"`. Codex fields map to the Codex driver unchanged. The normalized new fingerprint requires a fresh synthetic check and consent because the transport contract has changed.

No automatic migration reads or copies a credential value.

## Test strategy

All new and changed Go tests use testify and run with `-tags "fts5 sqlite_vec"`.

Driver contract tests use real `httptest` servers and exact request/response fixtures. They cover all three output modes, both OpenAI Chat token fields, reasoning settings, auth headers, redirects, response limits, usage normalization, request IDs, and safe errors. GLM 5.3 and Kimi K3 fixtures prove the compatibility gaps that motivated this design. OpenRouter and Venice fixtures prove model-dependent schema fallback without provider-specific branches.

Onboarding tests cover models.dev parsing, protocol-family mapping, user-confirmed snapshots, catalog failure with custom fallback, the three-step synthetic probe, token-field negotiation, reasoning rejection, and the guarantee that synthetic checks contain no archive material. A local catalog fixture is used in CI; live catalog behavior is not a test dependency.

Credential tests cover mode `0600`, atomic replacement, symlink rejection, missing and malformed files, environment lookup, redaction, per-profile removal, and the absence of secret values from config, policy JSON, logs, errors, and database records.

Runner and store tests cover named-profile selection, exact check and consent fingerprints, deterministic bytes before reservation, one separately budgeted same-profile repair, no provider fallback, missing usage reconciliation, and invalidation after profile edits.

End-to-end tests exercise one native-schema driver and one prompt-JSON driver through the production runner, validator, budget journal, and consent store. Migration tests load both legacy provider kinds and reject mixed old/new configuration. Existing Codex isolation integration tests remain mandatory.

Live credential checks are optional developer commands and never run in CI.

## Rollout

Implementation should keep each commit reviewable:

1. Introduce profile collections, credential storage, verification state, and legacy loading without changing the active transport behavior.
2. Refactor the current transports behind the protocol registry.
3. Add local validation and the one-repair runner path.
4. Add OpenAI Responses, Anthropic Messages, and Google generateContent drivers.
5. Add catalog-assisted and custom onboarding, profile management commands, and documentation.
6. Update fixtures, migration coverage, privacy disclosures, and operator examples for GLM, Kimi, OpenRouter, Venice, open-agent-api, and Codex.

The feature remains disabled by default. Existing configured users must complete the new synthetic check and grant consent to the normalized fingerprint before a sweep resumes.

## References

- [models.dev catalog and schema](https://github.com/anomalyco/models.dev)
- [open-agent-api](https://github.com/teslashibe/open-agent-api)
- [Z.AI Chat Completions API](https://docs.z.ai/api-reference/llm/chat-completion)
- [GLM 5.3 announcement](https://z.ai/blog/glm-5.3)
- [Kimi Code API](https://www.kimi.com/code/docs/en/)
- [OpenRouter structured outputs](https://openrouter.ai/docs/guides/features/structured-outputs)
- [Venice structured responses](https://docs.venice.ai/guides/features/structured-responses)
