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
- `internal/peoplesweep/modelsdev.go`
- `internal/peoplesweep/modelsdev_test.go`
- `internal/peoplesweep/testdata/modelsdev/catalog.json`

## Negotiation analysis

- `CapabilityChecker` depends only on the protocol registry. It has no archive, authority, consent, history, credential-store, repair, or persistence dependency.
- Every attempt constructs a fresh sanitized profile for the exact selected protocol, endpoint, and model. Package-owned policy placeholders satisfy profile validation but never enter the wire request.
- The only prompt and schema ask for `{"ok":true}`. Local validation requires that exact single-key object plus safe provider metadata before returning a result.
- OpenAI output attempts are ordered native JSON Schema, JSON object, then prompt JSON. Each mode tries `max_completion_tokens` before `max_tokens`. Anthropic and Google preserve the same output order while omitting the JSON-object representation their registered drivers cannot encode.
- Requested reasoning is tested only after a reasoning-free representation succeeds. Rejection is terminal; the checker never silently drops the requested setting or tries a different representation/provider.
- Only HTTP 400, 404, and 422 advance to another capability representation. Authentication failures, 408, 429, 5xx, transport/timeout failures, unsafe metadata, invalid output, and local validation/security failures stop immediately.
- The checker resolves one registered driver before the loop and never changes protocol, endpoint, model, provider, or credential. It has no repair or runtime fallback path.

## Catalog analysis

- `ModelsDevClient.Fetch` is caller-invoked only and always requests `https://models.dev/api.json` with `OpenAI File Downloader, XaiImageApiFetch/1.0`.
- The isolated client removes cookie-jar state, disables redirects, enforces a total 15-second timeout, reads at most 8 MiB plus one overflow byte, and always bounded-drains/closes the response.
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
