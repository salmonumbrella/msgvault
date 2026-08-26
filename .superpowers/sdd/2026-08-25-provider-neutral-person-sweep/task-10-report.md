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

## Review-fix addendum

This addendum supersedes the earlier routing, transaction, and secret-handling descriptions where they differ.

- Review code head: `9074a48ad9fdf05ceb8f0ba03023b47c656a84e7`
- Hardening commit: `719fcdc447df0022a87011b9bea65d454ef7be4a`
- Daemon-removal guard commit: `9074a48ad9fdf05ceb8f0ba03023b47c656a84e7`

### Routing and secret handling

- A proxied check carries only the profile name and safe output flags. Its request environment is always empty. The daemon-side process reloads config and resolves a stored credential locally or reads only the exact configured variable from its own environment. The caller's credential value is absent from the request object, encoded body, metadata, output, logs, and errors.
- Without a compatible daemon, check and add's final saved-profile check open the writable store directly. Daemon discovery is read-only and does not auto-start one. When a daemon owns the database, the check is proxied name-only.
- The shared profile-name grammar is `^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`. Cobra commands, internal selection helpers, API routing, runtime config validation, and credential paths reject empty, flag-like, control-containing, whitespace-padded, and oversized names without reflecting unsafe input.
- Daemon-owned removal now sends a safe exact fingerprint guard with the profile name while the saved table still exists. The daemon reloads and verifies that fingerprint before revocation; the local ETag removal follows. A race can leave the exact old consent revoked, but cannot revoke a changed fingerprint, remove the concurrent profile, or delete its credential. Direct removal retains config-first ordering, so an initial ETag conflict has no consent or credential side effects.

### Transaction and config preservation

- `add` rejects locally knowable flag and policy errors before catalog, credential input, or provider negotiation. After optional catalog discovery it reads and decodes a fresh config snapshot, refuses an exact existing profile, resolves explicitly accepted prices, then reads the credential and negotiates. The profile table is insert-only and the final write uses that snapshot's ETag.
- `use` derives the profile, fingerprint, successful-check lookup, and selector edit from one freshly decoded snapshot. `remove` likewise derives active state, replacement, fingerprint, and credential source from one snapshot.
- Exact-table removal refuses descendant tables and dotted descendant content, so an unknown extension cannot be consumed. Formatting, comments, unrelated tables, mode, ownership, atomic replacement, fsync, and recovery behavior stay in the existing retained-file machinery.
- `RestoreConfigFile` can restore an originally absent optional config by identity-checking and recoverably retiring only the transaction-created file. An uncertain publication is rolled back only when path, bytes, ETag, mode, and inode identity match the exact transaction version; byte-identical concurrent replacements are preserved.
- Masked terminal input uses bounded one-byte reads in raw no-echo mode, caps allocation during input, handles backspace, cancel, EOF, overflow, and read errors, and restores the terminal on every return path. Stdin input remains separately bounded.

### Review RED evidence

```text
proxied named env check: caller environment lookup calls = 1; request Env contained the canary
concurrent exact add: credential/provider input occurred before the fresh config collision was detected
use/remove stale snapshot: startup fingerprint or credential source was used after the retained file changed
missing-config rollback: config rollback snapshot is invalid
uncertain add publication: selector and profile remained installed after ErrConfigChanged
unsafe internal profile names: state/proxy calls occurred before validation
descendant profile removal: unknown child table was consumed
bounded terminal tests: undefined readBoundedMaskedCredentialInput / old unbounded readMasked signature
runtime config names: expected validation error, got nil
exact publication identity: undefined SameConfigFileVersion
daemon remove routing: expected guarded revoke before edit; got edit before unguarded revoke
```

### Review GREEN evidence

Final focused command:

```text
go test -tags "fts5 sqlite_vec" ./cmd/msgvault/cmd ./internal/api ./internal/config -run 'PersonProvider|CLIRunCommandAllowed|ConfigEdit|RestoreConfig|SameConfigFileVersion|ReadBoundedMaskedCredential|ReadProviderCredential' -count=1 -timeout=10m
ok  go.kenn.io/msgvault/cmd/msgvault/cmd  8.868s
ok  go.kenn.io/msgvault/internal/api  0.520s
ok  go.kenn.io/msgvault/internal/config  0.221s
```

Broader serial packages:

```text
go test -tags "fts5 sqlite_vec" ./internal/store -run 'PersonInference|PersonSweep' -count=1 -timeout=10m
ok  go.kenn.io/msgvault/internal/store  49.963s

go test -tags "fts5 sqlite_vec" ./internal/config -count=1 -timeout=10m
ok  go.kenn.io/msgvault/internal/config  10.026s

go test -tags "fts5 sqlite_vec" ./internal/api -count=1 -timeout=10m
ok  go.kenn.io/msgvault/internal/api  202.582s

go test -tags "fts5 sqlite_vec" ./internal/peoplesweep -count=1 -timeout=10m
ok  go.kenn.io/msgvault/internal/peoplesweep  34.461s
```

Race and static gates:

```text
go test -race -tags "fts5 sqlite_vec" ./cmd/msgvault/cmd ./internal/api -run 'PersonProvider|CLIRunCommandAllowed|CLIAllowlist' -count=1 -timeout=15m
ok  go.kenn.io/msgvault/cmd/msgvault/cmd  22.508s
ok  go.kenn.io/msgvault/internal/api  1.219s

go test -race -tags "fts5 sqlite_vec" ./cmd/msgvault/cmd ./internal/api ./internal/config ./internal/peoplesweep -run 'PersonProvider|CLIRunCommandAllowed|CLIAllowlist|RestoreConfig|SameConfigFileVersion|ReadBoundedMaskedCredential|ValidateProviderProfileName|ConfigRejectsUnsafe' -count=1 -timeout=15m
ok  go.kenn.io/msgvault/cmd/msgvault/cmd  25.531s
ok  go.kenn.io/msgvault/internal/api  1.716s
ok  go.kenn.io/msgvault/internal/config  1.213s
ok  go.kenn.io/msgvault/internal/peoplesweep  1.267s

go vet -tags "fts5 sqlite_vec" ./cmd/msgvault/cmd ./internal/api ./internal/config ./internal/peoplesweep
git diff --check
[exit 0]
```

### Review concerns

- A full command-package run reached the repository's 10-minute test timeout in unrelated `TestCacheNeedsBuild_SupersededCirclebackRunWithoutCheckpoint` while SQLite initialized the schema. The final provider-focused command and race runs passed.
- A Windows config cross-compile could not start because the existing DuckDB dependency excludes all files in its Windows AMD64 binding package. Linux production tests and the platform-specific config tests passed.
- Recovery retirement deliberately preserves a quarantined artifact instead of deleting or truncating it because a hardlink may still reference the inode. Provider credential canaries are absent from those artifacts.
