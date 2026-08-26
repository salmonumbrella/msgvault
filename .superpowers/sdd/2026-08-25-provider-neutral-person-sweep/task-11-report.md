---
last_edited: "2026-08-26"
---

# Task 11 report: production wiring, documentation, and end-to-end verification

## Status

Implemented and committed the load-bearing credential deletion guard before the
ordinary Task 11 work. Production manual and scheduled sweeps now share one
full-config construction path. Real focused end-to-end tests cover config
loading, env and stored credential resolution, exact check, explicit consent,
protocol registry selection, worker preparation, budget reservation, local
validation, one same-session repair, and commit. Operator docs cover the full
requested provider and privacy surface.

Task commits:

- `46334706` — `fix: bind provider credential deletion guard`
- `fc5bc03e` — `feat: wire provider-neutral people sweeps`
- `6ee76482` — `docs: explain people sweep provider profiles`

Base: `733e49ddd4315160fb1d97305d2676134a942520`.

## Changed files

Credential deletion guard and removal recovery:

- `internal/peoplesweep/credential_store.go`
- `internal/peoplesweep/credential_store_unix.go`
- `internal/peoplesweep/credential_store_unsupported.go`
- `internal/peoplesweep/credential_store_test.go`
- `internal/peoplesweep/credential_store_security_test.go`
- `internal/peoplesweep/credential_store_unsupported_test.go`
- `cmd/msgvault/cmd/person_provider.go`
- `cmd/msgvault/cmd/person_provider_setup.go`
- `cmd/msgvault/cmd/person_provider_routing_test.go`

Production construction and E2E:

- `cmd/msgvault/cmd/serve_people_sweep.go`
- `cmd/msgvault/cmd/serve_people_sweep_test.go`
- `cmd/msgvault/cmd/person_sweep.go`
- `cmd/msgvault/cmd/serve.go`
- `internal/peoplesweep/worker.go`
- `internal/peoplesweep/openai_end_to_end_test.go`
- `internal/peoplesweep/provider_modes_end_to_end_test.go`

Operator documentation:

- `README.md`
- `docs/configuration.md`
- `docs/usage/people.md`

## Credential deletion guard

`PreflightDelete` now returns an opaque `CredentialDeleteGuard`, and `Delete`
requires and consumes that guard. The Unix guard pins safe descriptors and
identity metadata for the tokens root, credential namespace, lock marker, and
target. It never retains credential bytes. The namespace lock remains held for
the guard lifetime.

At guarded deletion, every live path/object is reopened and checked against
the exact pinned identity. A valid replacement of the tokens root, namespace,
lock marker, or credential target fails closed and leaves both the pinned old
credential and the replacement untouched. The guard is single-use, explicitly
closable, sealed to the package, rejected by JSON/text serialization, rendered
only as an opaque value, and unusable with another store or profile. Reuse,
close-before-use, wrong store/profile, and unsupported platforms all fail
closed. Existing `Save` and create-only `SaveNew` bootstrap behavior is
preserved.

Production profile removal acquires the guard before daemon routing/revoke,
closes it on every return path, and consumes it only after the ETag config edit
and consent revoke. If guarded deletion fails, removal restores the exact prior
config bytes. Consent deliberately remains revoked and the returned error says
so. This is not atomic across daemon consent revoke, config publication, and
credential deletion; the rollback behavior is explicit.

Deterministic race coverage:

| Replacement after preflight | Guard result | Original target | Replacement target |
|---|---|---|---|
| Tokens root | Reject | Untouched | Untouched |
| Credential namespace | Reject | Untouched | Untouched |
| Lock marker | Reject | Untouched | Untouched |
| Credential target | Reject | Untouched | Untouched |
| Production remove target | Reject and restore exact config | Untouched | Untouched |

## Production construction analysis

`newProductionStructuredRunner(cfg, st)` is the sole runtime constructor. It
creates the protocol-keyed registry with `http.DefaultClient`, the Codex
command starter, and released-isolation gate; creates the credential resolver
from `cfg.TokensDir()` and `os.LookupEnv`; then constructs the runner from the
selected people-sweep config and store authority.

`newProductionPersonSweepWorker` calls that helper. Scheduled sweeps construct
their worker from the full daemon config on each run. Manual sweeps take the
command's already-read people-sweep config, place it in a copy of the full
config, and call the same worker constructor. There is no provider-name branch
and no models.dev reference in runtime construction.

`Worker.Run` resolves the active profile, catalog, and run timestamp once, then
passes those pinned values to every claimed person. The public single-person
entry point still resolves its own values for direct callers. A structured
execution session pins its profile, provider configuration, credential, driver,
and endpoint; its one repair call uses the same session and therefore cannot
resolve a different credential, server, model, or profile.

## Protocol, credential, and E2E matrix

| Protocol | Credential source exercised | Production construction | End-to-end boundary |
|---|---|---|---|
| `openai_chat` | `env` | Success | Config-loaded native schema and prompt JSON; exact HTTP wire; reservation; validation; commit; prompt semantic failure repaired once on the same server |
| `openai_responses` | `stored` | Success after loading the profile-specific file credential | Registry selection, preparation, exact check, consent, stored resolution |
| `anthropic_messages` | `env` | Success | Registry selection, preparation, exact check, consent, env resolution |
| `google_generate_content` | `env` | Success | Registry selection, preparation, exact check, consent, env resolution |
| `codex_app_server` | `none` | Fails closed at the production unreleased-isolation gate | No HTTP credential store access and no process launch |

The OpenAI E2E server also verifies that the exact sent wire hash already has a
durable running budget reservation. The repair request has a distinct reserved
wire hash, reaches the same test server, and the successful claim is committed
to the real store with cursor advancement.

## RED and GREEN evidence

Credential guard RED:

```text
$ go test -tags "fts5 sqlite_vec" ./internal/peoplesweep -run 'CredentialDeleteGuard' -count=1
# go.kenn.io/msgvault/internal/peoplesweep [...]
assignment mismatch / too many arguments against the old error-only PreflightDelete and independent Delete API
FAIL
```

Production removal RED:

```text
$ go test -tags "fts5 sqlite_vec" ./cmd/msgvault/cmd -run 'PersonProviderDaemonRemoveValidReplacementRace' -count=1
# go.kenn.io/msgvault/cmd/msgvault/cmd [...]
compile failure against the old independent deletion API
FAIL
```

Credential guard GREEN:

```text
$ go test -tags "fts5 sqlite_vec" ./internal/peoplesweep -run 'CredentialDeleteGuard' -count=1
ok  go.kenn.io/msgvault/internal/peoplesweep  1.244s

$ go test -tags "fts5 sqlite_vec" ./cmd/msgvault/cmd -run 'PersonProviderDaemonRemoveValidReplacementRace' -count=1
ok  go.kenn.io/msgvault/cmd/msgvault/cmd  0.524s

$ go test -tags "fts5 sqlite_vec" ./internal/peoplesweep -run 'Credential(Store|DeleteGuard)' -count=1 -timeout 3m
ok  go.kenn.io/msgvault/internal/peoplesweep  11.290s

$ go test -tags "fts5 sqlite_vec" ./cmd/msgvault/cmd -run 'PersonProvider.*(Remove|Add|Setup)|Credential' -count=1 -timeout 3m
ok  go.kenn.io/msgvault/cmd/msgvault/cmd  47.187s
```

Production constructor RED:

```text
$ go test -tags "fts5 sqlite_vec" ./cmd/msgvault/cmd -run 'ProductionPersonSweepConstructsEveryHTTPProtocolAndCredentialSource' -count=1
cmd/msgvault/cmd/serve_people_sweep_test.go:131:19: undefined: newProductionStructuredRunner
FAIL
```

One-profile-per-run RED:

```text
$ go test -tags "fts5 sqlite_vec" ./internal/peoplesweep -run 'OpenAIEndToEndNativeSchema' -count=1
expected: 1
actual  : 2
one worker run must resolve one active profile and catalog
FAIL
```

Integrated GREEN:

```text
$ go test -tags "fts5 sqlite_vec" ./internal/peoplesweep ./cmd/msgvault/cmd -run 'ProviderModesEndToEnd|OpenAIEndToEnd|ProductionPersonSweep' -count=1
ok  go.kenn.io/msgvault/internal/peoplesweep  2.136s
ok  go.kenn.io/msgvault/cmd/msgvault/cmd  2.534s

$ go test -tags "fts5 sqlite_vec" ./internal/peoplesweep ./cmd/msgvault/cmd -run 'ProviderModesEndToEnd|OpenAIEndToEnd|ProductionPersonSweep|PersonSweepWorker' -count=1 -timeout 4m
ok  go.kenn.io/msgvault/internal/peoplesweep  2.123s
ok  go.kenn.io/msgvault/cmd/msgvault/cmd  1.523s

$ go vet -tags "fts5 sqlite_vec" ./internal/peoplesweep ./cmd/msgvault/cmd
exit 0

$ git diff --check
exit 0
```

## Broad verification

The exact broad package set was run with an explicit eight-minute ceiling so a
schema-heavy package could not hang indefinitely:

```text
$ go test -tags "fts5 sqlite_vec" ./internal/peoplesweep ./internal/store ./internal/config ./internal/api -count=1 -timeout 8m
ok    go.kenn.io/msgvault/internal/peoplesweep  45.463s
ok    go.kenn.io/msgvault/internal/config       9.038s
ok    go.kenn.io/msgvault/internal/api          247.124s
panic: test timed out after 8m0s
running tests: TestSubsetIncompletePersonMergePacketIsOmitted (5s)
FAIL  go.kenn.io/msgvault/internal/store        480.038s
```

The store stack was in `CopySubsetWithOptions -> InitSchemaContext ->
optimizeSQLite -> database/sql`, consistent with the known schema-heavy path.
This timeout is not counted as a pass. A prior full two-package command run was
also stopped after roughly three minutes with the command package still in
schema setup; focused command-package production and credential slices passed
as shown above.

`make lint` ran and failed with 49 branch-wide findings. They include existing
body-close, exhaustive-switch, gosec, musttag, nilnil, recvcheck, staticcheck,
testifylint, helper, and wrapping findings across earlier provider-registry
work. The linter's `--fix` rewrites were reversed immediately; no generated
churn was retained. The final affected-package `go vet` passes. `make test` was
not started after the bounded broad run had already reached the same schema
ceiling.

## Documentation coverage

The README links the people guide and states that people sweeps do not select
or switch providers automatically. The configuration reference documents the
five protocol identifiers, three output modes, stored/env/none credentials,
the separate check and consent boundary, and a complete example/matrix for GLM
5.3, Kimi K3, OpenRouter, Venice, open-agent-api, Gemini, Anthropic, OpenAI
Responses, and Codex. The people guide gives the custom offline-catalog setup,
status/consent/use/run workflow, safe credential input choices, same-profile
repair behavior, and routed-upstream/privacy/terms warnings.

The docs state that models.dev is onboarding-only, `--custom` skips it, runtime
never uses it, OpenRouter and Venice may route to upstream operators,
subscription/logged-in endpoints remain subject to provider terms, and live
credential checks are never CI requirements.

## Private-data and credential scrub

Exact command:

```text
$ rg -n -i 'secret-canary|api[_ -]?key[=:][[:space:]]*[A-Za-z0-9_-]{16,}|authorization:[[:space:]]*bearer[[:space:]]+[A-Za-z0-9_-]+' --glob '!**/*_test.go' --glob '!docs/superpowers/**' .
```

The command returned seven matches: one release smoke-script fixture, five API
or usage documentation placeholders, and one source comment. Every match was
inspected. They are pre-existing placeholders, not live credentials or private
downstream identifiers. The Task 11 production and documentation changes
introduced no match. The raw placeholder values are intentionally not copied
into this report so the report does not become a new scrub finding.

## Concerns

- Full store and command-package completion is unproven because schema-heavy
  SQLite initialization exceeded or approached the bounded verification window.
- `make lint` is not green; its 49 findings span the wider provider-registry
  branch and should be resolved before merge.
- Codex remains intentionally unavailable behind
  `ErrCodexIsolationUnreleased`; production construction proves it fails closed
  and launches nothing.
- Profile removal deliberately cannot be atomic across daemon consent revoke,
  config publication, and guarded credential deletion. On post-publication
  deletion failure the exact config is restored while consent stays revoked.
