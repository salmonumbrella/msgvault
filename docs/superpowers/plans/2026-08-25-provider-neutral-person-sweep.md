---
last_edited: 2026-08-25
---

# Provider-neutral Person Sweep Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make people-sweep inference work with direct API keys, stored credentials, OpenAI-compatible aggregators, four structured-inference protocol families, and the existing Codex session while preserving exact consent, verification, and budget boundaries.

**Architecture:** Named provider profiles resolve to one active immutable policy. A protocol-keyed driver registry prepares deterministic bytes and normalizes responses; the runner owns credentials, exact verification and consent checks, local schema validation, and repair decisions, while the worker journals and reserves every provider call. models.dev is an onboarding-only discovery input whose selected values are copied into local configuration.

**Tech Stack:** Go, Cobra, BurntSushi TOML, `net/http`, `google/jsonschema-go`, SQLite, PostgreSQL, testify, models.dev JSON catalog.

**Spec:** `docs/superpowers/specs/2026-08-25-provider-neutral-person-sweep-design.md`

## Global Constraints

- Keep people sweeps disabled by default.
- Do not add provider-name presets or runtime model-name branches.
- `https://models.dev/api.json` is onboarding-only; normal sweeps and existing-profile checks do not fetch it.
- Use `OpenAI File Downloader, XaiImageApiFetch/1.0` for the models.dev request, a bounded body and timeout, and disabled redirects.
- Never put credential values in TOML, policy JSON, database rows, logs, errors, process arguments, or daemon HTTP metadata.
- Remote inference requires HTTPS. Plain HTTP requires loopback; `auth=none` is never allowed off loopback.
- Runtime uses only the active profile's saved protocol, endpoint, model, and capabilities. It never probes or falls back to another provider.
- Validate every result locally against the exact requested JSON Schema.
- Allow at most one repair call, through the same profile, with a separate deterministic request, reservation, journal row, and usage record.
- Preserve the existing Codex executable attestation and packet-only isolation boundary.
- All new and modified Go tests use testify. Run Go tests with `-tags "fts5 sqlite_vec"`.
- Every modified Markdown file starts with `last_edited` frontmatter, bumped only for a meaningful edit.
- Never run `roborev review` unless the user explicitly requests it.

---

### Task 1: Named Provider Configuration and Legacy Loading

**Files:**
- Modify: `internal/peoplesweep/config.go`
- Modify: `internal/peoplesweep/config_test.go`
- Modify: `internal/config/config.go`
- Modify: `internal/config/people_sweep_test.go`
- Modify: call sites under `internal/peoplesweep`, `cmd/msgvault/cmd`, and `internal/api` that read `Config.Provider`

**Interfaces:**
- Produces: `ProviderSelection`, `Protocol`, `OutputMode`, `AuthScheme`, `CredentialSource`, and `Config.ActiveProviderConfig()`.
- Produces: `ProviderProfile` fields used by every later task.
- Preserves: legacy `[people.sweep.provider]` loading as the named `default` profile.

- [ ] **Step 1: Write failing configuration and fingerprint tests**

Add tests named `TestConfigLoadsNamedProviderProfiles`, `TestConfigMigratesLegacyProviderTable`, `TestConfigRejectsMixedProviderShapes`, `TestConfigRejectsMissingActiveProvider`, and `TestProviderFingerprintIncludesProtocolCapabilities`. Pin this shape:

```toml
[people.sweep]
enabled = true
provider = "glm"

[people.sweep.providers.glm]
protocol = "openai_chat"
endpoint = "https://api.z.ai/api/paas/v4"
model = "glm-5.3"
auth = "bearer"
credential = "env"
credential_env = "ZAI_API_KEY"
output_mode = "json_object"
token_limit_parameter = "max_tokens"
reasoning_effort = "max"
retention_posture = "provider-declared"
training_posture = "provider-declared"
allowed_sources = ["conversation_text"]
source_since = "2026-01-01"
```

Assert that `request_timeout` and credential values remain outside the fingerprint, while protocol, endpoint, model, auth scheme, credential source/reference, output mode, token field, reasoning settings, privacy declarations, driver version, renderer policy, and program fingerprint change it.

- [ ] **Step 2: Run the focused tests and confirm the old single-provider model fails**

Run:

```bash
go test -tags "fts5 sqlite_vec" ./internal/peoplesweep ./internal/config -run 'TestConfig(LoadsNamed|MigratesLegacy|RejectsMixed|RejectsMissing)|TestProviderFingerprintIncludesProtocol' -count=1
```

Expected: FAIL because `provider` is currently a `ProviderConfig` table and profiles are not named.

- [ ] **Step 3: Introduce typed protocol and profile configuration**

Implement these public shapes in `internal/peoplesweep/config.go`:

```go
type Protocol string
const (
	ProtocolOpenAIChat       Protocol = "openai_chat"
	ProtocolOpenAIResponses  Protocol = "openai_responses"
	ProtocolAnthropicMessages Protocol = "anthropic_messages"
	ProtocolGoogleGenerateContent Protocol = "google_generate_content"
	ProtocolCodexAppServer   Protocol = "codex_app_server"
)

type ProviderSelection struct {
	Name   string
	legacy *ProviderConfig
}

type Config struct {
	Enabled              bool                      `toml:"enabled"`
	Schedule             string                    `toml:"schedule"`
	WorkBatchSize        int                       `toml:"work_batch_size"`
	ChangeBatchSize      int                       `toml:"change_batch_size"`
	HistoricalMessageCap int                       `toml:"historical_message_cap"`
	ContextPerTarget     int                       `toml:"context_per_target"`
	EvidenceMaxBytes     int                       `toml:"evidence_max_bytes"`
	EvidenceMaxItems     int                       `toml:"evidence_max_items"`
	LeaseDuration        time.Duration             `toml:"lease_duration"`
	BackstopInterval     time.Duration             `toml:"backstop_interval"`
	RetryBase            time.Duration             `toml:"retry_base"`
	RetryMax             time.Duration             `toml:"retry_max"`
	Budgets              BudgetConfig              `toml:"budgets"`
	Provider             ProviderSelection         `toml:"provider"`
	Providers            map[string]ProviderConfig `toml:"providers"`
}

type ProviderConfig struct {
	Protocol            Protocol         `toml:"protocol"`
	Endpoint            string           `toml:"endpoint"`
	Model               string           `toml:"model"`
	Auth                 AuthScheme       `toml:"auth"`
	Credential           CredentialSource `toml:"credential"`
	CredentialEnv        string           `toml:"credential_env"`
	OutputMode           OutputMode       `toml:"output_mode"`
	TokenLimitParameter  string           `toml:"token_limit_parameter"`
	ReasoningEffort      string           `toml:"reasoning_effort"`
	ReasoningMode        string           `toml:"reasoning_mode"`
	DriverVersion        string           `toml:"-"`
	RetentionPosture     string           `toml:"retention_posture"`
	TrainingPosture      string           `toml:"training_posture"`
	AllowedSources       []SourceClass    `toml:"allowed_sources"`
	SourceSince          string           `toml:"source_since"`
	SourceUntil          string           `toml:"source_until"`
	AllowSensitive       bool             `toml:"allow_sensitive"`
	Executable           string           `toml:"executable"`
	ExecutionBoundary    string           `toml:"execution_boundary"`
	RequestTimeout       time.Duration    `toml:"request_timeout"`
}

type ProviderProfile struct {
	Fingerprint           string           `json:"fingerprint"`
	Protocol              Protocol         `json:"protocol"`
	Endpoint              string           `json:"endpoint"`
	Model                 string           `json:"model"`
	Auth                  AuthScheme       `json:"auth"`
	Credential            CredentialSource `json:"credential"`
	CredentialRef         string           `json:"credential_ref"`
	OutputMode            OutputMode       `json:"output_mode"`
	TokenLimitParameter   string           `json:"token_limit_parameter"`
	ReasoningEffort       string           `json:"reasoning_effort"`
	ReasoningMode         string           `json:"reasoning_mode"`
	DriverVersion         string           `json:"driver_version"`
	RetentionPosture      string           `json:"retention_posture"`
	TrainingPosture       string           `json:"training_posture"`
	AllowedSources        []SourceClass    `json:"allowed_sources"`
	SourceSince           string           `json:"source_since"`
	SourceUntil           string           `json:"source_until"`
	AllowSensitive        bool             `json:"allow_sensitive"`
	ExecutionBoundary     string           `json:"execution_boundary"`
	PacketRendererPolicy  string           `json:"packet_renderer_policy"`
	ProgramFingerprint    string           `json:"program_fingerprint"`
	DisclosedPacketFields []string         `json:"disclosed_packet_fields"`
	PolicyJSON            json.RawMessage  `json:"-"`
}

func (c Config) ActiveProviderConfig() (string, ProviderConfig, error)
func (c Config) Profile() (ProviderProfile, error)
```

`ProviderSelection.UnmarshalTOML(any)` accepts either a string or the legacy table map. `ApplyDefaults` turns a legacy table into `provider="default"` plus one `Providers["default"]` entry only when no new provider map exists. `ProviderSelection.MarshalTOML` emits only a quoted profile name. Reject mixed shapes and unknown profile names.

- [ ] **Step 4: Add protocol-specific validation and canonical policy encoding**

Use one validator switch keyed by protocol. Enforce these exact rules:

```go
switch provider.Protocol {
case ProtocolOpenAIChat:
	requireOneOf(provider.TokenLimitParameter, "max_completion_tokens", "max_tokens")
case ProtocolOpenAIResponses, ProtocolAnthropicMessages, ProtocolGoogleGenerateContent:
	requireEmpty(provider.TokenLimitParameter)
case ProtocolCodexAppServer:
	requireCodexIsolationFields(provider)
default:
	return fmt.Errorf("unsupported people inference protocol %q", provider.Protocol)
}
```

Require a saved output mode for every HTTP protocol, validate normalized reasoning values, and retain the current endpoint canonicalization, source-class, date-bound, privacy-posture, and loopback rules. Build `ProviderProfile.PolicyJSON` from the new typed fields and exclude the secret and timeout.

- [ ] **Step 5: Update runtime call sites to resolve the active provider once**

Replace direct field access with:

```go
_, provider, err := config.ActiveProviderConfig()
if err != nil {
	return err
}
```

Pass `provider` to factories and Codex helpers. Do not cache a mutable map entry by pointer.

- [ ] **Step 6: Run package tests and commit**

Run:

```bash
go test -tags "fts5 sqlite_vec" ./internal/peoplesweep ./internal/config ./cmd/msgvault/cmd ./internal/api -count=1
git add internal/peoplesweep internal/config cmd/msgvault/cmd internal/api
git commit -m "feat: add named people inference profiles"
```

Expected: PASS.

### Task 2: Private Credential Store and Resolver

**Files:**
- Create: `internal/peoplesweep/credential_store.go`
- Create: `internal/peoplesweep/credential_store_test.go`
- Modify: `internal/peoplesweep/runner.go`
- Modify: `internal/peoplesweep/runner_test.go`
- Modify: `cmd/msgvault/cmd/serve_people_sweep.go`

**Interfaces:**
- Consumes: `ProviderConfig.Auth`, `ProviderConfig.Credential`, and `ProviderConfig.CredentialEnv` from Task 1.
- Produces: `Credential`, `CredentialStore`, and `CredentialResolver` for drivers and CLI setup.

- [ ] **Step 1: Write failing credential lifecycle and redaction tests**

Cover stored, environment, and none sources; mode `0600`; atomic rotation; concurrent saves; invalid names; malformed JSON; symlinked root, file, and lock rejection; exact deletion; and formatting redaction. Use a canary secret and assert it is absent from `fmt.Sprint`, `%#v`, returned errors, profile JSON, and captured logs.

```go
func TestCredentialNeverFormatsSecret(t *testing.T) {
	credential := peoplesweep.NewCredential(peoplesweep.AuthBearer, "secret-canary")
	assert.NotContains(t, fmt.Sprintf("%v %#v", credential, credential), "secret-canary")
}
```

- [ ] **Step 2: Run the credential tests and confirm the store is missing**

Run:

```bash
go test -tags "fts5 sqlite_vec" ./internal/peoplesweep -run 'TestCredential' -count=1
```

Expected: FAIL because credential-store types do not exist.

- [ ] **Step 3: Implement the opaque credential and symlink-safe store**

Use the same pointer-backed secret and formatting protections as `internal/discord/token.go`, rooted at `<tokensDir>/people-providers`:

```go
type Credential struct {
	Scheme AuthScheme
	secret *credentialSecret
}

type CredentialStore interface {
	Save(profileName string, credential Credential) error
	Load(profileName string) (Credential, error)
	Delete(profileName string) error
}

func NewFileCredentialStore(tokensDir string) *FileCredentialStore
func (c Credential) Value() string
```

Validate profile names with `^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`; map them to `<tokensDir>/people-providers/<name>.json`; lock the namespace; use mode `0700` for the private directory and `0600` for lock, candidate, and credential files; sync before atomic publication; and reject symlinks at every retained path boundary.

- [ ] **Step 4: Implement one credential resolver**

```go
type CredentialResolver interface {
	Resolve(profileName string, profile ProviderProfile) (Credential, error)
}

func NewCredentialResolver(store CredentialStore, lookup CredentialLookup) CredentialResolver
```

`stored` loads the exact named record, `env` reads only `profile.CredentialRef`, and `none` returns an empty credential only when profile validation already admitted loopback HTTP. Authenticated loopback gateways may resolve stored or environment credentials. Validate that the stored auth scheme equals the fingerprinted profile auth scheme.

- [ ] **Step 5: Wire the resolver into runner construction and commit**

Replace the raw string lookup in `Runner` with `CredentialResolver`. Pass `cfg.TokensDir()` from command construction; keep Codex credentials empty.

```bash
go test -tags "fts5 sqlite_vec" ./internal/peoplesweep ./cmd/msgvault/cmd -run 'Credential|Runner' -count=1
git add internal/peoplesweep cmd/msgvault/cmd/serve_people_sweep.go
git commit -m "feat: store people provider credentials privately"
```

Expected: PASS.

### Task 3: Durable Profile Verification and Schema Migration

**Files:**
- Modify: `internal/store/schema.sql`
- Modify: `internal/store/schema_pg.sql`
- Modify: `internal/store/migrations.go`
- Modify: `internal/store/store.go`
- Create: `internal/store/migrate_person_inference_provider_v2.go`
- Create: `internal/store/person_inference_check.go`
- Create: `internal/store/person_inference_check_test.go`
- Modify: `internal/store/person_inference_consent.go`
- Modify: `internal/store/person_inference_schema_test.go`
- Modify: `internal/peoplesweep/runner.go`
- Modify: `internal/peoplesweep/runner_test.go`

**Interfaces:**
- Consumes: canonical `ProviderProfile` from Task 1.
- Produces: `RecordPersonInferenceCheck`, `GetPersonInferenceCheck`, and `HasSuccessfulPersonInferenceCheck`.
- Produces: `ProviderAuthority`, the runner's combined verification and consent dependency.
- Changes: synthetic checks do not require archive consent; ordinary requests require both a matching successful check and active consent.

- [ ] **Step 1: Write failing SQLite and PostgreSQL-parity store tests**

Pin a verification row to one fingerprint:

```go
type PersonInferenceCheck struct {
	ProfileFingerprint string
	CheckedAt          time.Time
	DriverVersion      string
	OutputMode         peoplesweep.OutputMode
	ProviderRequestID  string
	ModelVersion       string
}
```

Test idempotent replacement for the same fingerprint, rejection of unknown profiles, unsafe metadata rejection, exact-fingerprint lookup, and the fact that a different profile has no successful check.

- [ ] **Step 2: Run the focused schema tests and confirm failure**

```bash
go test -tags "fts5 sqlite_vec" ./internal/store -run 'PersonInference(Check|ConsentSchema|ProviderV2)' -count=1
```

Expected: FAIL because the check table and v2 profile columns do not exist.

- [ ] **Step 3: Extend both canonical schemas and migrate existing branch databases**

Keep the physical `provider_kind`, `api_key_env`, and `allow_anonymous` columns for upgrade compatibility, but treat `provider_kind` as the protocol value. Add non-secret v2 columns for auth, credential source/reference, output mode, token field, reasoning settings, and driver version. Add:

```sql
CREATE TABLE IF NOT EXISTS person_inference_checks (
    profile_fingerprint TEXT PRIMARY KEY REFERENCES person_inference_profiles(fingerprint),
    checked_at TEXT NOT NULL,
    driver_version TEXT NOT NULL,
    output_mode TEXT NOT NULL,
    provider_request_id TEXT NOT NULL DEFAULT '',
    model_version TEXT NOT NULL
);
```

Use `TIMESTAMPTZ` for PostgreSQL. `migrationPersonInferenceProviderV2` adds missing columns with safe legacy defaults and creates the check table. It does not synthesize successful checks or consents.

- [ ] **Step 4: Update immutable profile persistence and verification methods**

Make `EnsurePersonInferenceProfile` write and compare every fingerprinted v2 field plus canonical `policy_json`. Implement:

```go
func (s *Store) RecordPersonInferenceCheck(ctx context.Context, check PersonInferenceCheck) error
func (s *Store) GetPersonInferenceCheck(ctx context.Context, fingerprint string) (*PersonInferenceCheck, error)
func (s *Store) HasSuccessfulPersonInferenceCheck(ctx context.Context, fingerprint string) (bool, error)

type ProviderAuthority interface {
	HasSuccessfulPersonInferenceCheck(context.Context, string) (bool, error)
	HasActivePersonInferenceConsent(context.Context, string) (bool, error)
}
```

An upsert may replace only the check metadata for the same immutable profile fingerprint.

- [ ] **Step 5: Add runner verification gating without gating synthetic probes on consent**

Split runner policy checks explicitly:

```go
if synthetic {
	return r.generateAndValidate(ctx, profile, prepared)
}
if err := r.requireVerifiedAndConsented(ctx, profile.Fingerprint); err != nil {
	return StructuredResponse{}, err
}
return r.generateAndValidate(ctx, profile, prepared)
```

The check request remains package-owned `{"ok":true}` input. An ordinary request must fail before credential resolution when verification is missing.

- [ ] **Step 6: Run store and runner tests and commit**

```bash
go test -tags "fts5 sqlite_vec" ./internal/store ./internal/peoplesweep -run 'PersonInference|Runner' -count=1
git add internal/store internal/peoplesweep/runner.go internal/peoplesweep/runner_test.go
git commit -m "feat: persist exact provider verification"
```

Expected: PASS on SQLite; PostgreSQL parity tests compile and use the same method contract.

### Task 4: Protocol Driver Registry, OpenAI Chat, and Codex Adaptation

**Files:**
- Modify: `internal/peoplesweep/provider.go`
- Replace: `internal/peoplesweep/openai_compatible.go` with `internal/peoplesweep/openai_chat.go`
- Replace: `internal/peoplesweep/openai_compatible_test.go` with `internal/peoplesweep/openai_chat_test.go`
- Modify: `internal/peoplesweep/codex_app_server.go`
- Modify: `internal/peoplesweep/codex_app_server_test.go`
- Replace: `internal/peoplesweep/transport_factory.go` with `internal/peoplesweep/driver_registry.go`
- Replace: `internal/peoplesweep/transport_factory_test.go` with `internal/peoplesweep/driver_registry_test.go`
- Create: `internal/peoplesweep/http_driver.go`
- Create: `internal/peoplesweep/testdata/providers/glm-5.3-chat.json`
- Create: `internal/peoplesweep/testdata/providers/kimi-k3-chat.json`
- Create: `internal/peoplesweep/testdata/providers/openrouter-chat.json`
- Create: `internal/peoplesweep/testdata/providers/venice-chat.json`

**Interfaces:**
- Consumes: typed profiles and opaque credentials.
- Produces: `StructuredDriver`, `DriverResponse`, and `NewDriverRegistry`.
- Preserves: prepared-byte hash verification and Codex isolation.

- [ ] **Step 1: Write failing registry and exact-wire tests**

Define the contract in tests:

```go
type StructuredDriver interface {
	Prepare(ProviderProfile, StructuredRequest) (PreparedStructuredRequest, error)
	GeneratePrepared(context.Context, ProviderProfile, Credential, PreparedStructuredRequest) (DriverResponse, error)
}
```

Table-test native schema, JSON object, and prompt JSON; both OpenAI token fields; bearer auth; reasoning fields; redirects; bounded bodies; safe errors; usage; and model metadata. Load the GLM, Kimi, OpenRouter, and Venice fixtures and assert no provider name appears in the production driver's decision logic.

- [ ] **Step 2: Run the driver tests and confirm the old hard-coded schema request fails**

```bash
go test -tags "fts5 sqlite_vec" ./internal/peoplesweep -run 'OpenAIChat|DriverRegistry|GLM|Kimi|OpenRouter|Venice' -count=1
```

Expected: FAIL because the existing transport always emits `json_schema` plus `max_completion_tokens`.

- [ ] **Step 3: Implement shared HTTP safety and driver response normalization**

```go
type DriverResponse struct {
	CandidateJSON      json.RawMessage
	ProviderRequestID  string
	ProviderVersion    string
	ModelVersion       string
	Usage              TokenUsage
	UsageKnown         bool
}
```

`http_driver.go` owns an HTTP client clone with redirects disabled, credential header application by typed auth scheme, the 1 MiB body limit, safe request-ID selection, `Retry-After`, and response-body disposal. It must never return body text in an error. Drivers perform one attempt only; the worker retains the existing bounded timeout and retry classification for 408, 429, 5xx, and temporary transport failures.

- [ ] **Step 4: Implement OpenAI Chat request variants**

Build exactly one body from saved capabilities:

```go
body := map[string]any{
	"model": profile.Model,
	"messages": messagesFor(request, profile.OutputMode),
	profile.TokenLimitParameter: request.MaxOutputTokens,
}
```

Add `response_format` only for `native_json_schema` or `json_object`; add verified reasoning fields only when configured; decode `choices[0].message.content`; and set `UsageKnown` only when the usage object is present.

- [ ] **Step 5: Register protocols and adapt Codex without weakening isolation**

```go
type DriverRegistry struct {
	drivers map[Protocol]StructuredDriver
}

func NewDriverRegistry(httpClient *http.Client, commands CommandStarter, isolation CodexIsolationGate) (*DriverRegistry, error)
func (r *DriverRegistry) Driver(protocol Protocol, provider ProviderConfig) (StructuredDriver, error)
```

Codex implements the renamed methods, rejects non-empty credentials, and retains its exact packet encoding and attestation checks.

Change runner construction to the signature used by later production wiring:

```go
func NewRunner(config Config, authority ProviderAuthority, registry *DriverRegistry, credentials CredentialResolver) (*Runner, error)
```

`ProviderAuthority` embeds the exact verification and consent checks from Task 3. The runner resolves the active profile and obtains its driver by protocol; callers never select a driver by provider name.

- [ ] **Step 6: Run driver, Codex isolation, and end-to-end tests and commit**

```bash
go test -tags "fts5 sqlite_vec" ./internal/peoplesweep -run 'OpenAIChat|DriverRegistry|Codex|EndToEnd' -count=1
git add internal/peoplesweep
git commit -m "refactor: add people inference driver registry"
```

Expected: PASS.

### Task 5: OpenAI Responses Driver

**Files:**
- Create: `internal/peoplesweep/openai_responses.go`
- Create: `internal/peoplesweep/openai_responses_test.go`
- Modify: `internal/peoplesweep/driver_registry.go`
- Modify: `internal/peoplesweep/driver_registry_test.go`

**Interfaces:**
- Consumes: `StructuredDriver`, shared HTTP safety, and saved output modes.
- Produces: `OpenAIResponsesDriver` registered for `openai_responses`.

- [ ] **Step 1: Write failing exact request and response-block tests**

Cover `POST /responses`, `max_output_tokens`, native `text.format` schema, JSON-object and prompt modes, output text block selection, refusal/missing text, usage presence/absence, request ID, and non-2xx redaction.

- [ ] **Step 2: Run the focused tests and confirm the driver is absent**

```bash
go test -tags "fts5 sqlite_vec" ./internal/peoplesweep -run 'OpenAIResponses' -count=1
```

Expected: FAIL because `OpenAIResponsesDriver` is undefined.

- [ ] **Step 3: Implement the Responses request and envelope parser**

Use typed request structs with these stable fields:

```go
type responsesRequest struct {
	Model           string         `json:"model"`
	Input           []responseItem `json:"input"`
	Text            *responseText  `json:"text,omitempty"`
	MaxOutputTokens int            `json:"max_output_tokens"`
	Reasoning       *responseReasoning `json:"reasoning,omitempty"`
}
```

Iterate documented output items and content blocks, accepting one non-empty `output_text` candidate. Reject multiple conflicting candidates.

- [ ] **Step 4: Register, test, and commit**

```bash
go test -tags "fts5 sqlite_vec" ./internal/peoplesweep -run 'OpenAIResponses|DriverRegistry' -count=1
git add internal/peoplesweep/openai_responses.go internal/peoplesweep/openai_responses_test.go internal/peoplesweep/driver_registry.go internal/peoplesweep/driver_registry_test.go
git commit -m "feat: add OpenAI Responses inference driver"
```

Expected: PASS.

### Task 6: Anthropic Messages Driver

**Files:**
- Create: `internal/peoplesweep/anthropic_messages.go`
- Create: `internal/peoplesweep/anthropic_messages_test.go`
- Modify: `internal/peoplesweep/driver_registry.go`
- Modify: `internal/peoplesweep/driver_registry_test.go`

**Interfaces:**
- Produces: `AnthropicMessagesDriver` registered for `anthropic_messages`.
- Uses: `x_api_key`, `anthropic-version`, `max_tokens`, and a verified schema/tool or prompt output mode.

- [ ] **Step 1: Write failing Messages protocol tests**

Cover exact `x-api-key`, default `anthropic-version: 2023-06-01`, `max_tokens`, schema/tool extraction, prompt JSON extraction, Kimi K3 Anthropic-compatible fixture, content-block ambiguity, usage, safe IDs, and body-redacted failures.

- [ ] **Step 2: Run the focused tests and confirm failure**

```bash
go test -tags "fts5 sqlite_vec" ./internal/peoplesweep -run 'AnthropicMessages|KimiAnthropic' -count=1
```

Expected: FAIL because the driver is absent.

- [ ] **Step 3: Implement deterministic Messages encoding and block extraction**

```go
type anthropicRequest struct {
	Model     string             `json:"model"`
	System    string             `json:"system"`
	Messages  []anthropicMessage `json:"messages"`
	MaxTokens int                `json:"max_tokens"`
	Tools     []anthropicTool    `json:"tools,omitempty"`
	ToolChoice any               `json:"tool_choice,omitempty"`
}
```

For native schema mode, use one forced tool named from the sanitized schema name and return its `tool_use.input`. For prompt JSON, accept one text block. JSON-object mode is not admitted unless onboarding verifies a documented protocol representation.

- [ ] **Step 4: Register, test, and commit**

```bash
go test -tags "fts5 sqlite_vec" ./internal/peoplesweep -run 'AnthropicMessages|KimiAnthropic|DriverRegistry' -count=1
git add internal/peoplesweep/anthropic_messages.go internal/peoplesweep/anthropic_messages_test.go internal/peoplesweep/driver_registry.go internal/peoplesweep/driver_registry_test.go
git commit -m "feat: add Anthropic Messages inference driver"
```

Expected: PASS.

### Task 7: Google Gemini generateContent Driver

**Files:**
- Create: `internal/peoplesweep/google_generate_content.go`
- Create: `internal/peoplesweep/google_generate_content_test.go`
- Modify: `internal/peoplesweep/driver_registry.go`
- Modify: `internal/peoplesweep/driver_registry_test.go`

**Interfaces:**
- Produces: `GoogleGenerateContentDriver` registered for `google_generate_content`.
- Uses: `x-goog-api-key`, `generationConfig.maxOutputTokens`, and verified response schema fields.

- [ ] **Step 1: Write failing Gemini protocol tests**

Cover the model path, URL escaping, API-key header, native `responseMimeType` plus `responseSchema`, prompt JSON, candidate parts, blocked/empty candidates, usage metadata, response limits, safe IDs, and redacted failures.

- [ ] **Step 2: Run the focused tests and confirm failure**

```bash
go test -tags "fts5 sqlite_vec" ./internal/peoplesweep -run 'GoogleGenerateContent' -count=1
```

Expected: FAIL because the driver is absent.

- [ ] **Step 3: Implement deterministic generateContent encoding**

```go
type googleGenerationConfig struct {
	MaxOutputTokens int             `json:"maxOutputTokens"`
	ResponseMIMEType string          `json:"responseMimeType,omitempty"`
	ResponseSchema  json.RawMessage `json:"responseSchema,omitempty"`
}
```

Build `<endpoint>/models/<url.PathEscape(model)>:generateContent`; reject endpoints containing query or fragment before joining; collect one candidate's text parts in order; and normalize `promptTokenCount` and `candidatesTokenCount` only when both exist.

- [ ] **Step 4: Register, test, and commit**

```bash
go test -tags "fts5 sqlite_vec" ./internal/peoplesweep -run 'GoogleGenerateContent|DriverRegistry' -count=1
git add internal/peoplesweep/google_generate_content.go internal/peoplesweep/google_generate_content_test.go internal/peoplesweep/driver_registry.go internal/peoplesweep/driver_registry_test.go
git commit -m "feat: add Gemini generateContent inference driver"
```

Expected: PASS.

### Task 8: Local Validation Repair with Per-call Budget Accounting

**Files:**
- Modify: `internal/peoplesweep/provider.go`
- Modify: `internal/peoplesweep/runner.go`
- Modify: `internal/peoplesweep/runner_test.go`
- Modify: `internal/peoplesweep/worker.go`
- Modify: `internal/peoplesweep/worker_test.go`
- Modify: `internal/peoplesweep/types.go`
- Modify: `internal/store/schema.sql`
- Modify: `internal/store/schema_pg.sql`
- Create: `internal/store/migrate_person_sweep_calls_v2.go`
- Modify: `internal/store/migrations.go`
- Modify: `internal/store/store.go`
- Modify: `internal/store/person_sweep_budget.go`
- Modify: `internal/store/person_sweep_budget_test.go`
- Modify: `internal/store/person_sweep_commit.go`
- Modify: `internal/store/person_sweep_commit_test.go`
- Modify: `internal/store/person_sweep_history_test.go`

**Interfaces:**
- Consumes: candidate JSON from every protocol driver.
- Produces: `ValidationFailure`, `PrepareRepair`, and call-addressed budget reservations.
- Preserves: no unreserved provider call and no repair to a different profile.

- [ ] **Step 1: Write failing repair and budget-journal tests**

Pin these outcomes: valid output makes one call; JSON Schema failure and `ParseExtraction` semantic failure each prepare a repair containing bounded validation errors; repair is reserved after its deterministic bytes exist; budget denial prevents the repair network call; repair uses the same profile; a second invalid output stops; missing usage reconciles conservatively; and history has call ordinals `0` and `1` with purposes `primary` and `repair`.

```go
type ProviderCallCoordinate struct {
	BatchOrdinal int
	CallOrdinal  int
	Purpose      string
}
```

- [ ] **Step 2: Run runner, worker, and budget tests and confirm failure**

```bash
go test -tags "fts5 sqlite_vec" ./internal/peoplesweep ./internal/store -run 'Repair|CallOrdinal|MissingUsage|PersonSweepBudget' -count=1
```

Expected: FAIL because reservations are keyed only by batch ordinal and runner returns validation errors directly.

- [ ] **Step 3: Separate candidate generation from local validation**

Add:

```go
type ValidationFailure struct {
	Candidate json.RawMessage
	Errors    []string
}

func (r *Runner) Validate(request StructuredRequest, response DriverResponse) (StructuredResponse, *ValidationFailure, error)
func (r *Runner) PrepareRepair(request StructuredRequest, failure ValidationFailure) (PreparedStructuredRequest, error)
```

`ValidationFailure` implements `error`. `RunPreparedStructured` returns the safe normalized response plus that typed error when local validation fails, allowing the worker to account for the completed call before deciding whether to repair. Bound candidate bytes at 1 MiB and validation messages at 32 entries, 256 bytes each. The repair instruction contains the original request, candidate, and errors but no newly retrieved archive context. Mark repair packets so another repair cannot be prepared from them.

- [ ] **Step 4: Migrate budget rows from batch keys to call keys**

Add `call_ordinal INTEGER NOT NULL DEFAULT 0` and `purpose TEXT NOT NULL DEFAULT 'primary'`; rebuild the SQLite table to use primary key `(attempt_id, batch_ordinal, call_ordinal)`; alter the PostgreSQL primary key in the same one-time migration; and update all selectors, locks, reservation IDs, and replay checks to include the call ordinal.

```go
type BudgetReservationRequest struct {
	RunID                 string
	AttemptID             string
	BatchOrdinal          int
	CallOrdinal           int
	Purpose               string
	PersonID              int64
	ProviderFingerprint   string
	UTCDate               string
	InputHash             string
	ItemCount             int
	EstimatedRequests     int
	EstimatedInputTokens  int64
	EstimatedOutputTokens int64
	EstimatedCostMicroUSD int64
	Budget                BudgetConfig
}
```

Accept only `(0,"primary")` and `(1,"repair")`. Finalization requires one primary call for every durable batch and permits at most one matching repair call.

- [ ] **Step 5: Orchestrate repair in the worker after the primary result**

Keep all primary requests prepared and reserved before the first network call. Treat either the runner's typed JSON Schema failure or a `ParseExtraction` failure as the one repair opportunity; convert parser errors to one bounded validation message while retaining the same candidate JSON. On validation failure:

```go
repair, err := w.Runner.PrepareRepair(batch.Request, failure)
estimate := EstimateWireTokenReservation(repair.WireRequest(), batch.Request.MaxOutputTokens)
reservation := w.Store.ReservePersonSweepBudget(ctx, repairReservation(batch, repair, estimate))
// Mark started immediately before RunPreparedStructured.
```

Append both reservations and completed usage entries. If reservation fails, finalize with the budget error without making a repair call. Never change the active profile or driver.

- [ ] **Step 6: Reconcile unknown usage against the reservation**

When `UsageKnown=false`, store the normalized safe request ID but charge the full reserved token and cost values. When usage is known, retain the existing `max(reserved, reported)` under-reporting protection.

- [ ] **Step 7: Run repair, budget, history, and parity tests and commit**

```bash
go test -tags "fts5 sqlite_vec" ./internal/peoplesweep ./internal/store -run 'Repair|CallOrdinal|PersonSweepBudget|PersonSweepHistory|PersonSweepCommit|Parity' -count=1
git add internal/peoplesweep internal/store
git commit -m "feat: budget one same-provider repair call"
```

Expected: PASS.

### Task 9: Capability Negotiation and models.dev Discovery

**Files:**
- Create: `internal/peoplesweep/capability_check.go`
- Create: `internal/peoplesweep/capability_check_test.go`
- Create: `internal/peoplesweep/modelsdev.go`
- Create: `internal/peoplesweep/modelsdev_test.go`
- Create: `internal/peoplesweep/testdata/modelsdev/catalog.json`
- Modify: `internal/peoplesweep/driver_registry.go`

**Interfaces:**
- Produces: `CapabilityChecker.Negotiate`, `ModelsDevClient.Fetch`, and provider/model suggestions.
- Consumes: protocol drivers and synthetic `{"ok":true}` request.
- Does not persist or contact the catalog outside caller-controlled onboarding.

- [ ] **Step 1: Write failing negotiation and catalog tests**

Test native schema, then JSON object, then prompt JSON ordering; OpenAI token-field negotiation; reasoning acceptance/rejection; no archive strings in any probe; no provider switching; catalog size/timeout/redirect limits; provider/model extraction; ambiguous protocol mapping; and catalog failure that leaves custom setup available.

- [ ] **Step 2: Run the focused tests and confirm failure**

```bash
go test -tags "fts5 sqlite_vec" ./internal/peoplesweep -run 'Capability|ModelsDev' -count=1
```

Expected: FAIL because negotiation and catalog clients are absent.

- [ ] **Step 3: Implement synthetic capability negotiation**

```go
type NegotiatedCapabilities struct {
	OutputMode          OutputMode
	TokenLimitParameter string
	ReasoningEffort     string
	ReasoningMode       string
	DriverVersion       string
	Response            StructuredResponse
}

func (c *CapabilityChecker) Negotiate(ctx context.Context, candidate ProviderConfig, credential Credential) (NegotiatedCapabilities, error)
```

Each attempt constructs a fresh candidate profile and calls only the selected protocol driver with package-owned synthetic input. Treat capability-shaped 400/404/422 failures as a reason to try the next saved representation; stop immediately on auth, rate-limit, 5xx, timeout, or unsafe response errors.

- [ ] **Step 4: Implement the bounded models.dev client and protocol-family mapper**

```go
const modelsDevURL = "https://models.dev/api.json"

type ProviderSuggestion struct {
	ID, Name, Endpoint string
	EnvironmentNames  []string
	Models             []ModelSuggestion
	ProtocolCandidates []Protocol
}

type ModelSuggestion struct {
	ID, Name string
	Reasoning, StructuredOutput bool
	InputCostMicroUSDPerMillionTokens  *int64
	OutputCostMicroUSDPerMillionTokens *int64
}

func (c *ModelsDevClient) Fetch(ctx context.Context) ([]ProviderSuggestion, error)
```

Limit the body to 8 MiB, timeout at 15 seconds, disable redirects, set the required user agent, reject duplicate provider/model IDs, and sort suggestions deterministically. Map documented SDK/API shapes to protocol families; never map a provider brand directly to a protocol. Convert catalog prices to checked integer micro-USD suggestions; do not persist the catalog response or update saved prices later.

- [ ] **Step 5: Run tests and commit**

```bash
go test -tags "fts5 sqlite_vec" ./internal/peoplesweep -run 'Capability|ModelsDev|DriverRegistry' -count=1
git add internal/peoplesweep/capability_check.go internal/peoplesweep/capability_check_test.go internal/peoplesweep/modelsdev.go internal/peoplesweep/modelsdev_test.go internal/peoplesweep/testdata/modelsdev internal/peoplesweep/driver_registry.go
git commit -m "feat: negotiate provider capabilities at setup"
```

Expected: PASS without live network access.

### Task 10: Provider Profile Management CLI

**Files:**
- Modify: `cmd/msgvault/cmd/person_provider.go`
- Modify: `cmd/msgvault/cmd/person_provider_test.go`
- Modify: `cmd/msgvault/cmd/person_provider_daemon_test.go`
- Modify: `cmd/msgvault/cmd/person_provider_routing_test.go`
- Create: `cmd/msgvault/cmd/person_provider_setup.go`
- Create: `cmd/msgvault/cmd/person_provider_setup_test.go`
- Modify: `internal/api/cli_handlers.go`
- Modify: `internal/api/cli_allowlist_person_provider_test.go`
- Modify: `internal/config/edit.go`
- Modify: `internal/config/edit_test.go`

**Interfaces:**
- Consumes: catalog suggestions, capability negotiation, credential store, verification store, and named config profiles.
- Produces: `add`, `list`, `use`, `check`, and `remove` commands while preserving `status`, `consent`, `revoke`, `history`, `login`, and `models`.

- [ ] **Step 1: Write failing command and routing tests**

Cover catalog-assisted add, `--custom`, `--credential-env NAME`, `--api-key-stdin`, masked interactive input, rejection of any API key process-argument flag, one active profile, list without secrets, profile-name arguments on status/consent/revoke/history, explicit acceptance of catalog price suggestions, rejection that leaves budget prices unchanged, check persistence, refusal to select an unverified profile, safe removal, config conflict, and Codex-only login/models. Assert daemon metadata never contains the key.

- [ ] **Step 2: Run the focused command tests and confirm failure**

```bash
go test -tags "fts5 sqlite_vec" ./cmd/msgvault/cmd ./internal/api ./internal/config -run 'PersonProvider|CLIRunCommandAllowed|ConfigEdit' -count=1
```

Expected: FAIL because the management commands and nested TOML edits do not exist.

- [ ] **Step 3: Add safe config map edits**

Extend targeted config edits with exact table insertion/removal operations rather than rewriting unrelated user configuration:

```go
type TableEdit struct {
	Path   []string
	Values map[string]any
	Remove bool
}

func EditConfigTables(path, ifMatch string, edits []TableEdit) (ConfigFile, error)
```

Use the existing retained-file, ownership, ETag, validation, atomic replacement, rollback, and recovery-file machinery. Reject ambiguous duplicate tables and mixed legacy/new provider shapes.

- [ ] **Step 4: Implement local profile setup orchestration**

`person_provider_setup.go` should expose a testable dependency bundle for stdin, terminal detection, catalog, config edits, credentials, checks, and store access. The add sequence is fixed, and `add` refuses to overwrite an existing profile name:

```text
discover or collect explicit fields
show final endpoint/protocol/model/auth/privacy values
read credential locally
run synthetic negotiation locally in memory
write credential
atomically add profile and active selector
invoke the normal check command using the saved profile
```

When a daemon owns the database, the final check is proxied without a secret and reloads the provider configuration from disk before resolving the named stored credential. Without a daemon, it opens the writable store directly. On failure after credential publication, restore the ETag-guarded configuration snapshot and delete only the newly created credential. Never grant consent automatically.

- [ ] **Step 5: Implement list, use, check, and remove**

`use` requires an exact successful check before changing the selector. `check [name]` resolves that profile, uses its saved capabilities without models.dev, runs the synthetic request, and records the exact fingerprint result. `remove` revokes active consent, retires audit state without deleting historical rows, deletes only the exact stored credential, and refuses active removal unless sweeps are disabled or another profile is selected.

- [ ] **Step 6: Restrict daemon routing and secret handling**

Allowlist only the exact provider verbs. `list`, `status`, `consent`, `revoke`, `history`, and `check` may proxy. Secret-bearing `add` stays in the local CLI process and must not serialize the key into the daemon request. A proxied check reloads the named profile from disk; stored credentials are read from the shared tokens directory, while environment-backed checks forward only that profile's exact configured variable.

- [ ] **Step 7: Run command, config, and API tests and commit**

```bash
go test -tags "fts5 sqlite_vec" ./cmd/msgvault/cmd ./internal/api ./internal/config -run 'PersonProvider|CLIRunCommandAllowed|ConfigEdit' -count=1
git add cmd/msgvault/cmd internal/api internal/config
git commit -m "feat: manage named people providers"
```

Expected: PASS.

### Task 11: Production Wiring, Documentation, and End-to-end Verification

**Files:**
- Modify: `cmd/msgvault/cmd/serve_people_sweep.go`
- Modify: `cmd/msgvault/cmd/serve_people_sweep_test.go`
- Modify: `cmd/msgvault/cmd/person_sweep.go`
- Modify: `cmd/msgvault/cmd/person_sweep_test.go`
- Modify: `internal/peoplesweep/openai_end_to_end_test.go`
- Create: `internal/peoplesweep/provider_modes_end_to_end_test.go`
- Modify: `docs/configuration.md`
- Modify: `docs/usage/people.md`
- Modify: `README.md`

**Interfaces:**
- Consumes: all prior tasks.
- Produces: one production construction path and operator documentation.

- [ ] **Step 1: Write failing production-wiring and end-to-end tests**

Exercise one native-schema OpenAI Chat profile and one prompt-JSON profile through config loading, credential resolution, exact check, explicit consent, driver registry, worker preparation, budget reservation, local validation, and commit. Add a repair case and assert it stays on the same test server. Add construction tests for all five protocols and stored/env/none credential sources.

- [ ] **Step 2: Run the focused end-to-end tests and confirm missing wiring**

```bash
go test -tags "fts5 sqlite_vec" ./internal/peoplesweep ./cmd/msgvault/cmd -run 'ProviderModesEndToEnd|OpenAIEndToEnd|ProductionPersonSweep' -count=1
```

Expected: FAIL until production constructors use the registry, credential resolver, and verification gate.

- [ ] **Step 3: Consolidate production construction**

Create one helper used by scheduled and manual sweeps:

```go
func newProductionStructuredRunner(cfg *config.Config, st *store.Store) (*peoplesweep.Runner, error) {
	registry := peoplesweep.NewDriverRegistry(http.DefaultClient, peoplesweep.NewCodexCommandStarter(), peoplesweep.NewReleasedCodexIsolationGate())
	credentials := peoplesweep.NewCredentialResolver(peoplesweep.NewFileCredentialStore(cfg.TokensDir()), os.LookupEnv)
	return peoplesweep.NewRunner(cfg.People.Sweep, st, registry, credentials)
}
```

Resolve the active profile once per worker run. Keep manual and scheduled behavior identical.

- [ ] **Step 4: Document setup and provider examples**

Add frontmatter where absent, then document:

- models.dev is contacted only by interactive `provider add`;
- custom setup works offline;
- credentials are stored separately or read from an environment variable;
- consent is a separate explicit command after verification;
- GLM 5.3, Kimi K3, OpenRouter, Venice, open-agent-api, Gemini, Anthropic, OpenAI Responses, and Codex examples are protocol profiles, not presets;
- OpenRouter and Venice may route to upstream operators;
- subscription and logged-in endpoints must be used according to provider terms;
- msgvault never switches providers automatically.
- live credential checks are optional developer verification and never CI requirements.

- [ ] **Step 5: Run focused tests, formatting, lint, and the full suite**

```bash
gofmt -w internal/peoplesweep internal/store internal/config cmd/msgvault/cmd internal/api
go test -tags "fts5 sqlite_vec" ./internal/peoplesweep ./internal/store ./internal/config ./cmd/msgvault/cmd ./internal/api -count=1
make lint
make test
git diff --check
```

Expected: every command exits 0.

- [ ] **Step 6: Run the private-data and credential-canary scrub**

```bash
rg -n -i 'secret-canary|api[_ -]?key[=:][[:space:]]*[A-Za-z0-9_-]{16,}|authorization:[[:space:]]*bearer[[:space:]]+[A-Za-z0-9_-]+' --glob '!**/*_test.go' --glob '!docs/superpowers/**' .
git diff --check
git status --short
```

Expected: no credential-like production or documentation matches; only intended source changes are present.

- [ ] **Step 7: Commit the integrated feature documentation and wiring**

```bash
git add cmd/msgvault/cmd internal/peoplesweep internal/store internal/config internal/api README.md docs/configuration.md docs/usage/people.md
git commit -m "feat: enable provider-neutral people sweeps"
```

Expected: commit succeeds with a clean worktree.

### Task 12: Final Branch Verification and PR Update

**Files:**
- Verify only; modify files only if a preceding verification exposes a defect.

**Interfaces:**
- Consumes: the complete implementation and all prior commits.
- Produces: a verified PR branch ready for review.

- [ ] **Step 1: Re-read the design and inspect the complete diff**

```bash
git diff origin/pr-685...HEAD --stat
git diff origin/pr-685...HEAD -- . ':(exclude)docs/superpowers/plans/2026-08-25-provider-neutral-person-sweep.md'
```

Check every design section against an implemented test and confirm there are no provider-name runtime branches, runtime catalog calls, secret-bearing arguments, or cross-provider recovery paths.

- [ ] **Step 2: Run the final required verification**

```bash
make lint
make test
git diff --check origin/pr-685...HEAD
git status --short
```

Expected: lint and all tagged tests pass, diff check is clean, and the worktree has no uncommitted files.

- [ ] **Step 3: Push the reviewed commit range to the PR branch**

```bash
git push fork HEAD:feat/person-self-healing-sweep
```

Expected: the remote PR head advances without a force push.

- [ ] **Step 4: Verify the remote PR head and checks**

```bash
gh pr view 685 --repo kenn-io/msgvault --json headRefOid,url,statusCheckRollup
```

Expected: `headRefOid` equals local `HEAD`; report CI as pending until GitHub completes it and do not claim remote checks passed early.
