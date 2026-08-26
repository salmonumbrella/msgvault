---
last_edited: 2026-08-26
---

# Task 10 report: named people-provider profile management

## Result

- Commit: `9c88b9f469fe0b6c98e4fb518ebfda8d4efd7ef1`
- Subject: `feat: manage named people providers`
- Base: `8bbdad8799cf487f3bee1bdb7db1de71b810da21`

## Files

- `cmd/msgvault/cmd/person_provider.go`
- `cmd/msgvault/cmd/person_provider_daemon_test.go`
- `cmd/msgvault/cmd/person_provider_routing_test.go`
- `cmd/msgvault/cmd/person_provider_setup.go`
- `cmd/msgvault/cmd/person_provider_setup_test.go`
- `cmd/msgvault/cmd/person_provider_test.go`
- `internal/api/cli_allowlist_person_provider_test.go`
- `internal/api/cli_handlers.go`
- `internal/config/edit.go`
- `internal/config/edit_test.go`
- `internal/peoplesweep/credential_store.go`
- `internal/peoplesweep/credential_store_test.go`
- `internal/peoplesweep/types.go`
- `internal/store/person_sweep_history.go`
- `internal/store/person_sweep_history_test.go`

## Behavior and routing analysis

- `person provider` now exposes `add`, `list`, `use`, `remove`, and named `check`, while preserving named `status`, `consent`, `revoke`, and `history`. Existing sweep history accepts an optional exact provider fingerprint filter before applying its limit. `login` and `models` remain Codex-only.
- models.dev is consulted only by local `add`. Its results are printed as labels and hints; the operator still supplies every protocol, endpoint, model, auth, and privacy value explicitly. Catalog failure prints a fixed warning and leaves the complete custom setup path available.
- Catalog prices are copied only with `--accept-catalog-prices` and only from one complete exact endpoint/model match. Otherwise the budget table is not edited. No saved-profile, check, or runtime path constructs the catalog client.
- `check [name]` resolves the saved named profile and its saved negotiated capabilities, constructs the package-owned synthetic request, and records only an exact successful fingerprint with bounded provider request metadata. It never calls models.dev or the negotiator. `use` reads verification state through the read-only database handle and requires an exact successful check before its ETag-guarded selector edit.
- The daemon allowlist admits only exact `list`, `status`, `consent`, `revoke`, `history`, and `check` grammars. `add`, `use`, `remove`, `login`, `models`, unknown verbs, extra positional values, and flag smuggling are rejected. Local `remove` proxies only exact `revoke <name>` to the daemon database owner, then performs the local config and credential transaction.
- A proxied check sends the profile name and safe flags only. The daemon reloads the current config file, resolves a stored credential from the shared private store, or accepts only the one exact environment variable named by that saved profile. It does not trust stale startup profile data or forward unrelated environment variables.
- Disabled sweep state remains the default authority boundary, while explicit named management remains possible. Active removal is refused while sweeps are enabled; when disabled it selects a deterministic remaining profile before removing the active profile and refuses to remove the only configured profile.

## Transaction analysis

- `EditConfigTables` inserts, edits, or removes exact TOML tables without re-encoding the retained file. It preserves comments, formatting, unrelated keys, unknown tables, line endings, ownership, and mode while reusing the existing ETag, owner validation, atomic replacement, directory fsync, rollback, and recovery-artifact machinery.
- Table edits reject duplicate target tables, duplicate assignments, ambiguous descendant content, mixed legacy/new provider shapes, invalid table paths or values, and stale/missing `ifMatch` values before publication. `RestoreConfigFile` restores the exact retained bytes only when the current ETag is the version created by this transaction.
- `add` is ordered: collect and display explicit values, require confirmation, validate policy, read the credential locally, negotiate locally, snapshot config, publish a create-only credential, ETag-write profile plus selector and explicitly accepted prices, then invoke the normal saved-profile check. It never grants consent.
- `FileCredentialStore.SaveNew` holds the credential-root lock across existence testing and atomic publication, so concurrent setup cannot overwrite an existing exact credential.
- Failure before publication changes nothing. Failure after credential publication but before config publication deletes only the credential created by this call. Failure after config publication ETag-restores the exact previous snapshot and deletes only that newly created credential. Rollback conflicts and cleanup failures are joined with the primary error and fail closed; preexisting and other credentials are never deleted.
- `remove` first revokes the exact profile consent through the database owner, then ETag-edits the selector/profile table, then deletes only that exact stored credential. Credential deletion failure conditionally restores the exact config snapshot. Audit profiles, checks, runs, attempts, and history remain stored. Consent revocation is deliberately not recreated on a later failure, so a partial failure cannot restore inference authority.

## Secret-handling analysis

- There is no API-key value flag. Stored secrets enter only through bounded `--api-key-stdin` input or a masked local terminal read. `--credential-env NAME` records only that exact name and reads only that variable. Anonymous auth rejects either credential input mode.
- Secret-bearing `add` and Codex `login`/`models` never proxy. A credential value is not placed in process arguments, daemon JSON or metadata, TOML, policy, SQLite, logs, errors, command output, history, recovery files, or this report.
- Production-path canary regressions cover argv, stdout/stderr, retained config and recovery artifacts, daemon request capture, error strings, list output, and history metadata. Stored credential objects also redact `String`, `GoString`, and formatting output.

## RED

Initial production-command build before management implementation:

```text
go test -tags "fts5 sqlite_vec" ./cmd/msgvault/cmd ./internal/api ./internal/config -run 'PersonProvider|CLIRunCommandAllowed|ConfigEdit' -count=1
# go.kenn.io/msgvault/cmd/msgvault/cmd [go.kenn.io/msgvault/cmd/msgvault/cmd.test]
cmd/msgvault/cmd/person_provider_setup_test.go: undefined: personProviderAddOptions
# go.kenn.io/msgvault/internal/config [go.kenn.io/msgvault/internal/config.test]
internal/config/edit_test.go: undefined: TableEdit
FAIL
```

Security and transaction regressions captured during the TDD cycle:

```text
internal/peoplesweep/credential_store_test.go: store.SaveNew undefined
person provider prevalidation: environment lookup calls = 1, negotiation calls = 1
daemon named-env reload: expected forwarded exact variable=true, stale variable=false
config trailing-comment preservation: unexpected missing following-table comment
config duplicate/mixed-shape rejection: An error is expected but got nil
```

Final daemon-ownership regression before the read/proxy split:

```text
cmd/msgvault/cmd/person_provider_test.go: deps.openReadStore undefined
FAIL  go.kenn.io/msgvault/cmd/msgvault/cmd [build failed]
```

## GREEN

Required focused command after the final authority fix:

```text
go test -tags "fts5 sqlite_vec" ./cmd/msgvault/cmd ./internal/api ./internal/config -run 'PersonProvider|CLIRunCommandAllowed|ConfigEdit' -count=1
ok  go.kenn.io/msgvault/cmd/msgvault/cmd  9.034s
ok  go.kenn.io/msgvault/internal/api  1.081s
ok  go.kenn.io/msgvault/internal/config  0.378s
```

Broader relevant packages:

```text
go test -tags "fts5 sqlite_vec" ./cmd/msgvault/cmd -run 'PersonProvider|PersonSweep' -count=1
ok  go.kenn.io/msgvault/cmd/msgvault/cmd  7.197s

go test -tags "fts5 sqlite_vec" ./internal/api -run 'CLIRun|PersonProvider|PersonSweep' -count=1
ok  go.kenn.io/msgvault/internal/api  0.972s

go test -tags "fts5 sqlite_vec" ./internal/config -count=1
ok  go.kenn.io/msgvault/internal/config  4.198s

go test -tags "fts5 sqlite_vec" ./internal/store -run 'PersonInference|PersonSweep' -count=1
ok  go.kenn.io/msgvault/internal/store  55.652s

go test -tags "fts5 sqlite_vec" ./internal/peoplesweep -count=1
ok  go.kenn.io/msgvault/internal/peoplesweep  36.969s
```

Race and static gates:

```text
go test -race -tags "fts5 sqlite_vec" ./cmd/msgvault/cmd ./internal/api -run 'PersonProvider|CLIRunCommandAllowed' -count=1
ok  go.kenn.io/msgvault/cmd/msgvault/cmd  13.730s
ok  go.kenn.io/msgvault/internal/api  1.154s

go test -race -tags "fts5 sqlite_vec" ./cmd/msgvault/cmd ./internal/api ./internal/config ./internal/store ./internal/peoplesweep -run 'PersonProvider|CLIRunCommandAllowed|ConfigEditTables|CredentialStoreSaveNew|PersonSweepHistoryFiltersExactProviderFingerprint' -count=1
ok  go.kenn.io/msgvault/cmd/msgvault/cmd  23.207s
ok  go.kenn.io/msgvault/internal/api  1.352s
ok  go.kenn.io/msgvault/internal/config  1.935s
ok  go.kenn.io/msgvault/internal/store  3.127s
ok  go.kenn.io/msgvault/internal/peoplesweep  1.948s

go fmt ./...
go vet -tags "fts5 sqlite_vec" ./...
go vet -tags "fts5 sqlite_vec" ./cmd/msgvault/cmd/... ./internal/api/...
git diff --check
[exit 0]
```

## Concerns

- One full five-package parallel test command exceeded the 10-minute command timeout while an unrelated store subset-schema test initialized under heavy shared-host CPU contention. The task-relevant serial package runs and both focused race runs passed; no provider-management failure was observed.
- models.dev is mutable advisory input. Unknown, incomplete, unavailable, or ambiguous price data fails closed to explicit custom setup and leaves saved budget bytes unchanged.
- Credential stores that do not provide atomic create-only publication are rejected by `add`; silently weakening overwrite protection would violate the transaction boundary.
