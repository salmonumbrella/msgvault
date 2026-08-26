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

## Scoped re-review addendum

- Review base: `d0f9627d59685662fd7ae9e7df75e8e4cc576f11`
- Code commit: `3ed6128fb1212e373b8bfcb44ad81e5175af0592`
- Coverage commit: `47e8f5ce08e0ab37dd0c873fd5eb5180a5ecb360`
- Subject: `fix: harden provider management transactions`

### Routing and secret handling

- Default command dependencies no longer capture `cfg` before root `PersistentPreRunE`. Add, saved-profile check, direct/daemon execution, and remove resolve a credential store lazily from the current loaded config's exact `TokensDir`. A stored credential operation fails explicitly if that store cannot be resolved.
- The root-lifecycle regression constructs the production dependencies while `cfg` is nil, lets the real root pre-run load the selected file, then performs stored add and remove without injecting a credential store. It verifies the exact tokens namespace, `0700` directories, `0600` credential and tombstone, and canary absence from output and config.

### Transaction and rollback identity

- `RestoreConfigFile` now requires the exact post-publication `ConfigFile`, not an ETag. The guard covers logical and physical path, bytes, ETag, permissions, and platform file identity. The initial comparison and final exchange or retirement run under the config edit lock while the live object is retained by descriptor.
- Successful edits verify that the final read is still the object they published. If a byte-identical replacement wins before return, the edit reports `ErrConfigChanged` and returns the transaction object's identity. Normal and uncertain add/remove rollback pass that exact identity directly; there is no separate precheck window.
- Existing-file and originally-missing rollback preserve byte-identical different-inode replacements, symlink swaps, and hardlink swaps at the final restore boundary. The transaction object alone can be restored or recoverably retired.
- Remove derives profile, fingerprint, active replacement, credential source, and exact edit plan from one fresh snapshot. It validates the complete plan and resolves any stored credential namespace before daemon revoke. Only-profile and descendant-extension failures now make zero revoke and zero edit calls.
- Add validates the proposed provider, budgets, and exact selector/profile/price table plan before credential input or provider negotiation. Accepted zero, incomplete, negative, or overflow-prone prices and invalid token/cost caps fail before secret reads, provider calls, credential publication, or config writes. Rejected catalog prices still leave budget bytes untouched.

### Scoped RED evidence

```text
TestPersonProviderDefaultDependenciesResolveCredentialsAfterConfigLoad:
people provider credential store is unavailable

RestoreConfigFile identity API:
cannot use after (variable of struct type ConfigFile) as string value
undefined: restoreConfigFile

TestEditConfigReturnsPublishedIdentityWhenFinalReadSeesReplacement:
Expected error with "config changed despite write error" in chain but got nil

TestPersonProviderRemoveCompletesLocalPreflightBeforeRevoke:
only selected profile: revoke calls = 1
descendant extension table: expected an error, got nil

TestPersonProviderAcceptedCatalogPricesValidateProposedBudgetBeforeSecretOrProvider:
validation occurred only after credential read and negotiation
```

### Scoped GREEN evidence

```text
go test -tags "fts5 sqlite_vec" ./cmd/msgvault/cmd -run '^TestPersonProviderDefaultDependenciesResolveCredentialsAfterConfigLoad$' -count=1 -timeout=3m
ok  go.kenn.io/msgvault/cmd/msgvault/cmd  0.955s

go test -tags "fts5 sqlite_vec" ./cmd/msgvault/cmd ./internal/api ./internal/config -run 'PersonProvider|CLIRunCommandAllowed|CLIAllowlist|ConfigEdit|RestoreConfig|SameConfigFileVersion' -count=1 -timeout=10m
ok  go.kenn.io/msgvault/cmd/msgvault/cmd  18.557s
ok  go.kenn.io/msgvault/internal/api  0.924s
ok  go.kenn.io/msgvault/internal/config  1.280s

go test -tags "fts5 sqlite_vec" ./internal/config -count=1
ok  go.kenn.io/msgvault/internal/config  6.567s

go test -tags "fts5 sqlite_vec" ./internal/peoplesweep -count=1
ok  go.kenn.io/msgvault/internal/peoplesweep  24.096s

go test -tags "fts5 sqlite_vec" ./internal/store -run 'PersonInference|PersonSweep' -count=1 -timeout=10m
ok  go.kenn.io/msgvault/internal/store  52.520s

go test -tags "fts5 sqlite_vec" ./cmd/msgvault/cmd -run '^TestPersonProviderRemoveCompletesLocalPreflightBeforeRevoke$' -count=1 -timeout=3m
ok  go.kenn.io/msgvault/cmd/msgvault/cmd  0.738s

go test -race -tags "fts5 sqlite_vec" ./cmd/msgvault/cmd ./internal/api ./internal/config ./internal/peoplesweep -run 'PersonProvider|CLIRunCommandAllowed|CLIAllowlist|RestoreConfig|SameConfigFileVersion|ValidateConfigTableEdits|CredentialStore' -count=1 -timeout=15m
ok  go.kenn.io/msgvault/cmd/msgvault/cmd  20.457s
ok  go.kenn.io/msgvault/internal/api  1.419s
ok  go.kenn.io/msgvault/internal/config  1.331s
ok  go.kenn.io/msgvault/internal/peoplesweep  14.081s

gofmt -w <changed Go files>
go vet -tags "fts5 sqlite_vec" ./cmd/msgvault/cmd ./internal/api ./internal/config ./internal/peoplesweep
go vet -tags "fts5 sqlite_vec" ./internal/store
git diff --check
[exit 0]
```

### Scoped concerns

- One full `internal/store` run timed out at 10 minutes in unrelated `TestSubsetPersonMergePacketWithRemappedDefinitionIsOmitted` while another shared-host full-repository run exercised the same schema-heavy package. The provider-relevant store subset passed serially in 52.520 seconds.
- Config identity remains platform-specific behind the existing retained-file implementations. Linux production and final-boundary swap tests passed; the repository's existing DuckDB dependency still prevents a useful Windows package cross-compile.

## Stored-credential removal preflight addendum

- Date: `2026-08-26`
- Review base: `94213e5b5b909caa0f2ef2ebf8c79842a399cb6a`
- Code and test commit: `1cfdcf87576bb46067dd1234ad2efcb0c4ecaade`
- Subject: `fix: preflight provider credential deletion`

### Transaction and filesystem boundary

- `CredentialStore.PreflightDelete` now validates the same production credential namespace, exclusive root lock, exact target access, owner, mode, file type, link count, and no-follow path identity used by deletion. It opens the exact target `O_RDWR|O_NOFOLLOW|O_NONBLOCK`, compares the pinned descriptor with the directory entry, and performs no read, truncate, rename, unlink, or secret serialization.
- Stored-profile removal resolves the live credential store and completes this preflight before daemon ownership routing and guarded revoke. A preflight failure therefore makes zero revoke, config edit, and credential deletion calls. The command regression uses the production file store and Cobra path with a symlinked exact target; the external target, retained credential, and saved profile remain unchanged, and the canary is absent from output and errors.
- Missing exact credentials remain an idempotent success, matching `Delete`; the securely pinned namespace and lock marker are still validated. `Delete` reacquires the lock and repeats every target check after revoke and config publication.

### RED evidence

```text
go test -tags "fts5 sqlite_vec" ./cmd/msgvault/cmd -run '^TestPersonProviderDaemonRemovePreflightsStoredCredentialBeforeRevoke$' -count=1
--- FAIL: TestPersonProviderDaemonRemovePreflightsStoredCredentialBeforeRevoke
person_provider_routing_test.go:307: Should be zero, but was 1
person_provider_routing_test.go:308: Should be zero, but was 1
FAIL  go.kenn.io/msgvault/cmd/msgvault/cmd

go test -tags "fts5 sqlite_vec" ./internal/peoplesweep -run '^TestCredentialStorePreflightDelete' -count=1
credential_store_test.go:100:14: store.PreflightDelete undefined
FAIL  go.kenn.io/msgvault/internal/peoplesweep [build failed]

go test -tags "fts5 sqlite_vec" ./internal/peoplesweep -run '^TestCredentialStorePreflightDeleteRejectsEntrySwapAfterOpen$' -count=1
--- FAIL: TestCredentialStorePreflightDeleteRejectsEntrySwapAfterOpen
credential_store_security_test.go:92: An error is expected but got nil.
FAIL  go.kenn.io/msgvault/internal/peoplesweep
```

### GREEN evidence

```text
go test -tags "fts5 sqlite_vec" ./internal/peoplesweep ./cmd/msgvault/cmd -run '^(TestCredentialStorePreflightDelete|TestPersonProviderDaemonRemovePreflightsStoredCredentialBeforeRevoke|TestPersonProviderRemove)' -count=1
ok  go.kenn.io/msgvault/internal/peoplesweep  0.102s
ok  go.kenn.io/msgvault/cmd/msgvault/cmd  1.361s

go test -race -tags "fts5 sqlite_vec" ./internal/peoplesweep ./cmd/msgvault/cmd -run '^(TestCredentialStorePreflightDelete|TestPersonProviderDaemonRemovePreflightsStoredCredentialBeforeRevoke|TestPersonProviderRemove)' -count=1
ok  go.kenn.io/msgvault/internal/peoplesweep  1.225s
ok  go.kenn.io/msgvault/cmd/msgvault/cmd  4.103s

go vet -tags "fts5 sqlite_vec" ./internal/peoplesweep ./cmd/msgvault/cmd
git diff --check
[exit 0]
```

The full combined package command was bounded after `196.295s`: `internal/peoplesweep` passed in `36.181s`, while `cmd/msgvault/cmd` was still running unrelated schema-heavy tests and was interrupted without a reported assertion failure. This is not recorded as a command-package pass; the focused command-package and race runs above are the completion evidence.

### Limitation

- Preflight establishes only locally knowable validity of the current pinned target and access while it holds the credential-root lock. It deliberately does not claim that a later delete must succeed: the lock is released before daemon revoke, and a subsequent filesystem race is handled by `Delete` revalidation plus the existing fail-closed config rollback and partial-failure reporting.

## Read-only credential preflight addendum

- Date: `2026-08-26`
- Review base: `08d9e380f7d5ab9ccbbea5f3edcfab110aca4f63`
- Code and test commit: `7f12ecbbb0a3d8fdb07d8a7678f5e3a8b442898a`
- Subject: `fix: make credential deletion preflight read-only`

### Filesystem and routing boundary

- `FileCredentialStore.PreflightDelete` no longer enters `withCredentialRoot`. Its platform-specific path opens only existing objects, pins the tokens directory, resolves the namespace, marker, and credential descriptor-relatively with no-follow checks, and rejects missing state. It validates exact directory and file classes, current-user ownership, `0700` directory modes, `0600` file modes, link counts, writable access, and descriptor-to-entry identity without reading credential bytes.
- Preflight performs no create, chmod, chown, rename, remove, truncate, write, fsync, or content read. It takes the same exclusive `flock` used by store writers on the already-open namespace directory; this changes kernel lock state only. The existing marker is opened without `O_CREAT` and its contents and metadata remain untouched.
- A zero-length deletion tombstone is absent credential state for preflight and is rejected with `ErrCredentialNotFound`. `Delete` remains independently idempotent, reopens the namespace and target after daemon revoke and config publication, and repeats its no-follow, owner, mode, type, link-count, and identity checks.
- Production stored-profile removal still completes preflight before daemon ownership discovery or revoke. The real Cobra regression shows that a missing tokens root causes zero revoke, zero config edit, zero `Delete`, no filesystem creation, and no profile removal.

### RED evidence

```text
go test -v -tags "fts5 sqlite_vec" ./internal/peoplesweep -run '^TestCredentialStorePreflightDeleteRejectsMissingStateWithoutCreatingIt$' -count=1 -timeout=3m
--- FAIL: TestCredentialStorePreflightDeleteRejectsMissingStateWithoutCreatingIt
    --- FAIL: .../tokens_root
        An error is expected but got nil.
        entries changed: [] -> [tokens]
    --- FAIL: .../credential_namespace
        An error is expected but got nil.
        entries changed: [] -> [people-providers]
    --- FAIL: .../lock_marker
        An error is expected but got nil.
        entries changed: [profile.json] -> [.credentials.lock profile.json]
    --- FAIL: .../credential_target
        expected ErrCredentialNotFound in chain, got nil
    --- FAIL: .../credential_tombstone
        expected ErrCredentialNotFound in chain, got nil
FAIL

go test -v -tags "fts5 sqlite_vec" ./internal/peoplesweep -run '^TestCredentialStorePreflightDeleteRejectsWrongModesWithoutRepair$' -count=1 -timeout=3m
--- FAIL: TestCredentialStorePreflightDeleteRejectsWrongModesWithoutRepair
    --- FAIL: .../tokens_root
        An error is expected but got nil.
        mode changed: 0750 -> 0700
    --- FAIL: .../credential_namespace
        An error is expected but got nil.
        mode changed: 0750 -> 0700
FAIL

go test -v -tags "fts5 sqlite_vec" ./cmd/msgvault/cmd -run '^TestPersonProviderDaemonRemoveMissingCredentialRootHasZeroSideEffects$' -count=1 -timeout=3m
--- FAIL: TestPersonProviderDaemonRemoveMissingCredentialRootHasZeroSideEffects
    person_provider_routing_test.go:373: An error is expected but got nil.
FAIL
```

### GREEN evidence

```text
gofmt -w <changed Go files>
go test -tags "fts5 sqlite_vec" ./internal/peoplesweep -run 'CredentialStore' -count=1 -timeout=5m
ok  go.kenn.io/msgvault/internal/peoplesweep  15.160s

go test -tags "fts5 sqlite_vec" ./cmd/msgvault/cmd -run '^(TestPersonProviderDaemonRemoveMissingCredentialRootHasZeroSideEffects|TestPersonProviderDaemonRemovePreflightsStoredCredentialBeforeRevoke|TestPersonProviderRemove.*)$' -count=1 -timeout=3m
ok  go.kenn.io/msgvault/cmd/msgvault/cmd  2.015s

go test -race -tags "fts5 sqlite_vec" ./internal/peoplesweep ./cmd/msgvault/cmd -run '^(TestCredentialStorePreflightDelete.*|TestCredentialStoreRejectsFIFOWithoutBlocking|TestPersonProviderDaemonRemoveMissingCredentialRootHasZeroSideEffects|TestPersonProviderDaemonRemovePreflightsStoredCredentialBeforeRevoke|TestPersonProviderRemove.*)$' -count=1 -timeout=5m
ok  go.kenn.io/msgvault/internal/peoplesweep  1.307s
ok  go.kenn.io/msgvault/cmd/msgvault/cmd  5.490s

go vet -tags "fts5 sqlite_vec" ./internal/peoplesweep ./cmd/msgvault/cmd
git diff --check
[exit 0]
```

### Limitations

- Preflight and delete cannot be atomic across daemon revoke. The preflight lock is released before routing; `Delete` therefore reopens and revalidates, and a post-preflight race can still produce the existing partial-failure result with consent left revoked and the exact config rollback attempted.
- The unsupported-platform implementation returns the same fail-closed `errCredentialStoreUnsupported` without touching the filesystem, and its unit test now covers preflight. A FreeBSD cross-test could not compile the package because the repository's existing DuckDB and sqlite bindings exclude the required target definitions; no unsupported-platform execution is claimed.

## Fail-closed existing-credential deletion addendum

- Date: `2026-08-26`
- Review base: `9dd117e6ac30d76b1427a94e36d72487616d5f98`
- Code and test commit: `503852ac3d7c1adeb3cedef07284f231369ddd52`
- Subject: `fix: fail closed on provider credential deletion`

### Filesystem and removal boundary

- `FileCredentialStore.Delete` no longer enters the mutating `withCredentialRoot` bootstrap. Preflight and deletion now share one existing-only traversal that opens the live tokens root, namespace, existing lock marker, and exact non-empty credential with no-follow descriptor-relative operations. Save and `SaveNew` retain their existing bootstrap behavior.
- The shared traversal rejects missing objects, zero-length tombstones, wrong owners, wrong `0700`/`0600` modes, symlinks, non-regular files, and extra hard links without creating, chmodding, replacing, truncating, or otherwise repairing them. Unsupported platforms return the existing fail-closed unsupported error for both preflight and deletion.
- The namespace descriptor remains exclusively flocked while deletion compares the pinned tokens descriptor with the live tokens path, the pinned namespace descriptor with the live namespace entry, the pinned marker with its live entry, and the exact target descriptor with its live entry. Those checks run after the target opens and again immediately before truncation. A root, namespace, marker, or target swap therefore aborts before the pinned credential is touched.
- A valid deletion truncates and fsyncs the same exact `0600`, single-link credential inode to a bounded zero-length tombstone. It does not unlink the pathname, accumulate artifacts, or change another credential. A second delete reports `ErrCredentialNotFound` instead of converting absence into success.
- Production stored-profile removal still completes read-only preflight before daemon revoke. Its later `Delete` independently repeats every check. The post-preflight target-disappearance regression revokes once, publishes the config edit once, receives `ErrCredentialNotFound`, restores the exact config snapshot once, and leaves the retained credential inode and bytes unchanged. Consent is not recreated or reported as restored.
- Filesystem assertions snapshot identities, modes, directory entries, sizes, and content digests. Errors and test output do not include credential bytes.

### RED evidence

```text
go test -v -tags "fts5 sqlite_vec" ./internal/peoplesweep -run '^(TestCredentialStoreLifecycleUsesPrivateFilesAndExactDeletion|TestCredentialStoreDeleteRetiresOnlyExactPinnedTargetAsBoundedTombstone|TestCredentialStoreDeleteRejectsMissingStateAfterPreflightWithoutRecreatingIt|TestCredentialStoreDeleteRejectsWrongModesAfterPreflightWithoutRepair|TestCredentialStoreDeleteRejectsUnsafeTargetAfterPreflightWithoutChangingIt|TestCredentialStoreDeleteRejectsTokensRootSwapAfterCredentialOpen|TestCredentialStoreDeleteRejectsNamespaceSwapAfterCredentialOpen)$' -count=1 -timeout=3m
--- FAIL: TestCredentialStoreDeleteRejectsTokensRootSwapAfterCredentialOpen
    An error is expected but got nil.
--- FAIL: TestCredentialStoreDeleteRejectsNamespaceSwapAfterCredentialOpen
    An error is expected but got nil.
--- FAIL: TestCredentialStoreLifecycleUsesPrivateFilesAndExactDeletion
    Expected error with "people provider credential not found" in chain but got nil.
--- FAIL: TestCredentialStoreDeleteRejectsMissingStateAfterPreflightWithoutRecreatingIt
    --- FAIL: .../tokens_root
        An error is expected but got nil.
        entries changed: [tokens.retained] -> [tokens tokens.retained]
    --- FAIL: .../credential_namespace
        An error is expected but got nil.
        entries changed: [people-providers.retained] -> [people-providers people-providers.retained]
    --- FAIL: .../lock_marker
        An error is expected but got nil.
        credential size changed from 22 to 0
    --- FAIL: .../credential_target
        Expected ErrCredentialNotFound in chain but got nil.
    --- FAIL: .../credential_tombstone
        Expected ErrCredentialNotFound in chain but got nil.
--- FAIL: TestCredentialStoreDeleteRejectsWrongModesAfterPreflightWithoutRepair
    --- FAIL: .../tokens_root
        An error is expected but got nil; mode changed from 0750 to 0700; credential size changed from 22 to 0.
    --- FAIL: .../credential_namespace
        An error is expected but got nil; mode changed from 0750 to 0700; credential size changed from 22 to 0.
FAIL  go.kenn.io/msgvault/internal/peoplesweep  0.950s

go test -v -tags "fts5 sqlite_vec" ./internal/peoplesweep -run '^TestCredentialStoreDeleteDetectsSwapAtRemovalBoundary$' -count=1 -timeout=3m
--- FAIL: TestCredentialStoreDeleteDetectsSwapAtRemovalBoundary
    profile.original size changed from 60 to 0
    profile.original contents changed
FAIL  go.kenn.io/msgvault/internal/peoplesweep  0.175s

go test -v -tags "fts5 sqlite_vec" ./cmd/msgvault/cmd -run '^TestPersonProviderDaemonRemovePostPreflightCredentialRaceRollsBackConfig$' -count=1 -timeout=3m
--- FAIL: TestPersonProviderDaemonRemovePostPreflightCredentialRaceRollsBackConfig
    Expected error with "people provider credential not found" in chain but got nil.
    expected config restores: 1; actual: 0
    final provider map does not contain "beta"
FAIL  go.kenn.io/msgvault/cmd/msgvault/cmd  0.439s
```

### GREEN evidence

```text
go test -tags "fts5 sqlite_vec" ./internal/peoplesweep ./cmd/msgvault/cmd ./internal/config -run 'CredentialStore|PersonProvider.*Remove|RestoreConfig' -count=1 -timeout=8m
ok  go.kenn.io/msgvault/internal/peoplesweep  16.843s
ok  go.kenn.io/msgvault/cmd/msgvault/cmd  4.172s
ok  go.kenn.io/msgvault/internal/config  0.435s

go test -race -tags "fts5 sqlite_vec" ./internal/peoplesweep ./cmd/msgvault/cmd ./internal/config -run 'CredentialStore(Delete|PreflightDelete|RejectsFIFO)|PersonProvider.*Remove|RestoreConfig' -count=1 -timeout=8m
ok  go.kenn.io/msgvault/internal/peoplesweep  2.125s
ok  go.kenn.io/msgvault/cmd/msgvault/cmd  4.866s
ok  go.kenn.io/msgvault/internal/config  1.394s

go fmt ./...
go vet -tags "fts5 sqlite_vec" ./internal/peoplesweep ./cmd/msgvault/cmd ./internal/config
git diff --check
[exit 0]
```

### Limitations

- Preflight and deletion remain intentionally separate across daemon revoke. A post-preflight race now fails deletion and attempts exact config rollback, but consent may remain revoked; this flow does not restore or claim to restore consent.
- The final identity comparison and truncate are adjacent under the namespace flock. That lock coordinates credential-store participants but remains an advisory Unix lock; it does not prevent an unrelated process that ignores the lock from attempting a rename after the final comparison.
- Linux exercised the production path, swap hooks, race detector, and static checks. The unsupported-platform implementation and test were updated to fail closed for `Delete`, but no cross-platform execution is claimed because the repository's existing native dependencies still prevent a useful FreeBSD package test.
