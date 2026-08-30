---
last_edited: 2026-08-30
---

# Unified Operations Workspace Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build a privacy-bounded Operations workspace that exposes truthful durable history, status, recovery, and eligible actions for all nine background-operation kinds.

**Architecture:** Keep each subsystem's archive-side run authority and add five narrow invocation ledgers only where durable pass history is absent. Merge all nine adapters inside one revision-bound archive snapshot, encrypt every browser reference and cursor with an archive-owned AEAD key, and consume the generated API from a URL-restorable Svelte workspace.

**Tech Stack:** Go 1.24, `database/sql`, SQLite, PostgreSQL, testify, OpenAPI 3.1, generated Go/TypeScript clients, Svelte 5, TypeScript, Vitest, Testing Library, Playwright, Bun.

**Spec:** `docs/superpowers/specs/2026-08-30-unified-operations-workspace-design.md`

## Global Constraints

- Public lane labels are `Messages`, `Facts`, `Contacts`, `Documents`, and `Attachments`; internal lane keys remain `messages`, `person_facts`, `contacts`, `documents`, and `visual_attachments`.
- Every public history authority lives in the archive database; `vectors.db`, generation rows, rebuild rows, and provider ledgers are never public run history.
- One bounded execution pass or native request scope creates at most one durable row; provider batches and internal retries create none.
- Run DTOs contain only an opaque reference, kind, lane, trigger, state, UTC timestamps, allowlisted counters, and fixed public errors. Detail may add only registered related-status and action IDs.
- References and cursors use archive-bound authenticated encryption. Raw IDs, archive UIDs, provider/model/endpoint data, request data, content, filenames, identities, credentials, and raw errors never cross the API.
- SQLite and PostgreSQL implement equivalent schemas, constraints, ordering, recovery, snapshot, and savepoint behavior.
- Go tests use testify with expected values first. Every direct Go test command includes `-tags "fts5 sqlite_vec"`.
- Production behavior is developed test-first. Generated OpenAPI and client files are regenerated from source after their source tests pass.
- The final stacked branch contains one squashed feature commit on top of `feat/issue-639-web-directory` and excludes `.superpowers`, `.skillshare`, Roborev state, and unrelated files.

---

### Task 1: Durable Invocation and Token-Key Storage

**Files:**
- Create: `internal/operations/recorder.go`
- Create: `internal/store/operation_invocations.go`
- Create: `internal/store/operation_token_keys.go`
- Create: `internal/store/operation_invocations_test.go`
- Create: `internal/store/operation_token_keys_test.go`
- Modify: `internal/operations/types.go`
- Modify: `internal/operations/types_test.go`
- Modify: `internal/store/schema.sql`
- Modify: `internal/store/schema_pg.sql`
- Modify: `internal/store/operation_runs_schema_test.go`

**Interfaces:**
- Consumes: existing `operations.Kind`, `Trigger`, `State`, `PublicErrorCode`, `PublicCounter`, and archive `store.Store` transactions.
- Produces: `operations.InvocationSpec`, `InvocationCounters`, `BeginDisposition`, `BeginResult`, and `Recorder`; `Store.BeginOperationInvocation`, `CheckpointOperationInvocation`, `FinishOperationInvocation`, `RecoverOperationInvocations`; `Store.ActiveOperationTokenKey` and `Store.OperationTokenKey`.

- [ ] **Step 1: Write failing domain-contract tests**

Add table-driven tests that prove queued state is valid only for person enrichment, the new counter/unit matrices are closed per kind, final outcomes enforce `attempted = succeeded + failed + skipped`, and fixed errors cannot carry arbitrary messages. Define the wished-for recorder contract in the test:

```go
spec := operations.InvocationSpec{
	Kind: operations.KindMessageEmbedding, Key: "scheduler:2026-08-30T12:00:00Z",
	Trigger: operations.TriggerScheduled, StartedAt: instant,
}
assert.NoError(t, spec.Validate())
assert.Equal(t, operations.InvocationCounters{Attempted: 3, Succeeded: 2, Failed: 1}, counters)
```

- [ ] **Step 2: Run the domain tests and verify the missing contract fails**

Run: `go test -tags "fts5 sqlite_vec" ./internal/operations -run 'Test(Invocation|OperationCounter|OperationRun)'`

Expected: FAIL because `InvocationSpec`, the added counters/units, and the nine-kind validation matrix do not exist.

- [ ] **Step 3: Implement the closed recorder domain**

In `recorder.go`, define the exact storage-neutral contract:

```go
type BeginDisposition string
const (
	BeginCreated BeginDisposition = "created"
	BeginActive BeginDisposition = "active"
	BeginTerminal BeginDisposition = "terminal"
)
type InvocationSpec struct { Kind Kind; Key string; Trigger Trigger; StartedAt time.Time }
type InvocationCounters struct {
	Attempted, Succeeded, Failed, Truncated, Skipped int64
	Requested, Started, Suppressed, IdentityRejected int64
}
type BeginResult struct { ID StableID; Disposition BeginDisposition; Terminal *Run }
type Recorder interface {
	Begin(context.Context, InvocationSpec) (BeginResult, error)
	Checkpoint(context.Context, StableID, InvocationCounters) error
	Finish(context.Context, StableID, InvocationCounters, State, *PublicError) error
}
```

Validate keys as nonempty UTF-8 strings of at most 128 bytes, normalize all times to UTC, reject decreasing checkpoints, and derive final state from final item outcomes plus the returned/cancellation error through one tested helper.

- [ ] **Step 4: Write failing SQLite/PostgreSQL storage tests**

For each of the five new ledger kinds, test `created → active → terminal` replay, monotonic checkpoint replacement, illegal terminal rewrites, a different key producing a different row, restart recovery, and byte-identical schema invariants. Add token-key tests for one active key, restart persistence, rotation to decrypt-only, explicit retired-key deletion, and concurrent first-use creation.

- [ ] **Step 5: Run the storage tests and verify schema/method failures**

Run: `go test -tags "fts5 sqlite_vec" ./internal/store -run 'Test(OperationInvocation|OperationTokenKey|OperationRunOrderIndexes)'`

Expected: FAIL because the tables and Store methods are absent.

- [ ] **Step 6: Add the five ledgers and private token-key table**

Create equivalent `message_embedding_runs`, `person_embedding_runs`, `document_extraction_runs`, `document_embedding_runs`, and `visual_embedding_runs` tables. Each has numeric ID, unique bounded invocation key, trigger, lifecycle timestamps/state, fixed error code, and only its allowlisted integer counters. Add `operation_token_keys(key_id, key_bytes, state, created_at, retired_at)` with exactly one active row, and indexes on `(started_at DESC, id DESC)` plus active-state constraints. Terminal-row update triggers reject changes to terminal state, finish time, error code, and final counters.

- [ ] **Step 7: Implement Store lifecycle and keyring methods**

Use one kind-to-table allowlist rather than interpolating caller input. `BeginOperationInvocation` uses an insert-or-select transaction and returns `created`, `active`, or `terminal`; checkpoint and finish use state predicates so retries are idempotent. Generate token keys with `crypto/rand`, create the first key transactionally, and copy key bytes before returning them.

- [ ] **Step 8: Run focused tests and format**

Run: `go test -tags "fts5 sqlite_vec" ./internal/operations ./internal/store -run 'Test(OperationInvocation|OperationTokenKey|OperationRun)' && gofmt -w internal/operations/recorder.go internal/operations/types.go internal/operations/types_test.go internal/store/operation_invocations.go internal/store/operation_invocations_test.go internal/store/operation_token_keys.go internal/store/operation_token_keys_test.go internal/store/operation_runs_schema_test.go`

Expected: PASS with no test warnings.

- [ ] **Step 9: Commit the storage foundation**

```bash
git add internal/operations/recorder.go internal/operations/types.go internal/operations/types_test.go internal/store/operation_invocations.go internal/store/operation_invocations_test.go internal/store/operation_token_keys.go internal/store/operation_token_keys_test.go internal/store/schema.sql internal/store/schema_pg.sql internal/store/operation_runs_schema_test.go
git commit -m "feat(operations): add durable invocation ledgers"
```

### Task 2: Revision-Bound Mixed History Reader

**Files:**
- Create: `internal/store/operation_history.go`
- Create: `internal/store/operation_history_test.go`
- Modify: `internal/operations/types.go`
- Modify: `internal/operations/types_test.go`
- Modify: `internal/store/operation_runs.go`
- Modify: `internal/store/operation_history_reader_test.go`
- Modify: `internal/store/operation_runs_export_test.go`
- Modify: `internal/store/schema.sql`
- Modify: `internal/store/schema_pg.sql`

**Interfaces:**
- Consumes: Task 1's five invocation ledgers and the existing source, person-sweep, CardDAV, and person-enrichment native tables.
- Produces: `operations.HistorySnapshot`, dynamic per-kind availability, membership revision, date-bounded `operations.Query`, and all nine `HistoryReader` adapters.

- [ ] **Step 1: Write failing snapshot and adapter tests**

Seed all nine kinds at identical and neighboring timestamps. Assert exact newest-first `(started_at, kind, typed ID)` ordering, half-open `[StartedFrom, StartedBefore)` filtering, `limit+1`, all status roles, and person-enrichment omission until `started_at` is present. Inject one adapter SQL/decode error and assert the remaining kinds plus `UnavailableKinds` return from the same snapshot.

- [ ] **Step 2: Write failing revision/savepoint tests**

Assert insert, deletion, start/order change, and state transition advance a singleton revision while counter-only checkpoints do not. On PostgreSQL, force one adapter statement failure after a savepoint and prove the transaction can still read another adapter; mirror the same result on SQLite.

- [ ] **Step 3: Run the history tests and verify the old reader fails**

Run: `go test -tags "fts5 sqlite_vec" ./internal/operations ./internal/store -run 'Test(OperationHistory|OperationMembership|OperationAdapter)'`

Expected: FAIL because `HistorySnapshot`, dynamic availability, date bounds, five adapters, enrichment projection, revision triggers, and adapter savepoints are absent.

- [ ] **Step 4: Extend the domain reader contract**

Replace the run-slice return with:

```go
type HistorySnapshot struct {
	Runs []Run
	AvailableKinds []Kind
	UnavailableKinds []Kind
	MembershipRevision int64
}
type Query struct {
	Kinds []Kind; States []State; StartedFrom, StartedBefore *time.Time
	Position *Position; Limit int
}
```

`HistoryReader.ListRuns` returns `HistorySnapshot`; validation requires sorted unique kind/state sets, UTC date bounds, and `StartedFrom < StartedBefore`.

- [ ] **Step 5: Add transactional revision ownership**

Add `operation_history_state(singleton, membership_revision)` and backend-equivalent triggers for all nine histories. Triggers advance only when ordering/filter membership can change. Keep counter-only updates outside the revision predicate.

- [ ] **Step 6: Implement the nine adapters and partial snapshot**

Split safe per-kind projection helpers into `operation_history.go`. Begin one repeatable-read snapshot, read the revision, and run each selected adapter inside `SAVEPOINT operation_adapter_N`. Roll back only a failing adapter and record that kind unavailable; abort the whole call on transaction/savepoint/connection/revision failures. Re-read the revision before commit and return a typed consistency conflict if it changed.

- [ ] **Step 7: Preserve adapter privacy and status truth**

Map only final counters and fixed errors. Project stale native source/CardDAV rows according to their durable useful counters. Project enrichment queued/running/terminal rows only after first claim sets `started_at`; use the original start for recovered queued order. Remove the static history-availability assumption from `LaneHistoryStatus.Validate`.

- [ ] **Step 8: Run focused tests and format**

Run: `go test -tags "fts5 sqlite_vec" ./internal/operations ./internal/store -run 'Test(OperationHistory|OperationMembership|OperationAdapter)' && gofmt -w internal/operations/types.go internal/operations/types_test.go internal/store/operation_history.go internal/store/operation_history_test.go internal/store/operation_runs.go internal/store/operation_history_reader_test.go internal/store/operation_runs_export_test.go`

Expected: PASS for both default SQLite fixtures and PostgreSQL fixtures when `MSGVAULT_TEST_DB` is configured.

- [ ] **Step 9: Commit the mixed reader**

```bash
git add internal/operations/types.go internal/operations/types_test.go internal/store/operation_history.go internal/store/operation_history_test.go internal/store/operation_runs.go internal/store/operation_history_reader_test.go internal/store/operation_runs_export_test.go internal/store/schema.sql internal/store/schema_pg.sql
git commit -m "feat(operations): read revision-bound mixed history"
```

### Task 3: Encrypted References, Cursors, and API Snapshot Semantics

**Files:**
- Create: `internal/api/operation_tokens.go`
- Create: `internal/api/operation_tokens_test.go`
- Modify: `internal/api/operations.go`
- Modify: `internal/api/operations_test.go`
- Modify: `internal/api/operations_cursor_test.go`
- Modify: `cmd/msgvault/cmd/operations_api_e2e_test.go`

**Interfaces:**
- Consumes: Task 1 token-key Store methods and Task 2 `HistorySnapshot`.
- Produces: archive-bound AEAD run references/cursors, revision/filter/adapter-bound pagination, dynamic unavailable-kind responses, and direct encrypted detail restoration.

- [ ] **Step 1: Write failing token codec tests**

Assert a reference contains no JSON, archive UID, raw stable ID, or filter value; survives daemon/store reconstruction; rejects bit flips, truncation, unknown key IDs, cross-archive use, unsigned v1 input, and malformed envelopes; opens with a retained decrypt-only key; and fails with the existing typed error after key purge.

- [ ] **Step 2: Run token tests and verify unsigned encoding fails expectations**

Run: `go test -tags "fts5 sqlite_vec" ./internal/api -run 'TestOperation(Token|RunReference|Cursor)'`

Expected: FAIL because `operations.go` still uses base64-encoded JSON carrying the archive UID.

- [ ] **Step 3: Implement the private AEAD codec**

Create `operationTokenCodec` backed by a narrow keyring interface. Use AES-256-GCM with a random nonce and wire format `op2.<key-id>.<base64url(nonce+ciphertext)>`. Use `op2\x00<archiveUID>` as additional authenticated data. Encrypt only the strict typed payload; never serialize the archive UID. Reject all non-`op2` input.

- [ ] **Step 4: Write failing API pagination tests**

Test page-one adapter/revision capture, same-snapshot continuation, 409 on revision or available/unavailable-kind drift, 400 on filter/date drift, a single unavailable-kind 503, and unfiltered partial results with explicit `unavailable_kinds`. Assert status remains independently available when one history adapter fails.

- [ ] **Step 5: Run API tests and verify snapshot contract failures**

Run: `go test -tags "fts5 sqlite_vec" ./internal/api ./cmd/msgvault/cmd -run 'TestOperation'`

Expected: FAIL because cursors lack revision/adapter binding and list responses use static unavailable metadata.

- [ ] **Step 6: Wire snapshot semantics into the API**

Put membership revision, ordered participating kinds, ordered unavailable kinds, filter hash, and last position inside the encrypted cursor. Parse `started_from` and `started_before` as canonical UTC RFC 3339. Return 409 `operation_history_conflict` on snapshot drift, 503 only for a requested unavailable kind, and otherwise return the partial page with its explicit unavailable set.

- [ ] **Step 7: Run focused API/e2e tests and format**

Run: `go test -tags "fts5 sqlite_vec" ./internal/api ./cmd/msgvault/cmd -run 'TestOperation' && gofmt -w internal/api/operation_tokens.go internal/api/operation_tokens_test.go internal/api/operations.go internal/api/operations_test.go internal/api/operations_cursor_test.go cmd/msgvault/cmd/operations_api_e2e_test.go`

Expected: PASS with no raw identifiers in response bodies.

- [ ] **Step 8: Commit encrypted API history**

```bash
git add internal/api/operation_tokens.go internal/api/operation_tokens_test.go internal/api/operations.go internal/api/operations_test.go internal/api/operations_cursor_test.go cmd/msgvault/cmd/operations_api_e2e_test.go
git commit -m "feat(api): encrypt operation history tokens"
```

### Task 4: Message and Person Embedding Pass Recording

**Files:**
- Modify: `internal/vector/embed/worker.go`
- Modify: `internal/vector/embed/worker_test.go`
- Modify: `internal/vector/embed/person_worker.go`
- Modify: `internal/vector/embed/person_worker_test.go`
- Modify: `internal/scheduler/embed_job.go`
- Modify: `internal/scheduler/scheduler_test.go`
- Modify: `cmd/msgvault/cmd/embed_vector.go`
- Modify: `cmd/msgvault/cmd/embed_vector_test.go`
- Modify: `cmd/msgvault/cmd/serve_vector.go`

**Interfaces:**
- Consumes: Task 1 `operations.Recorder` and invocation lifecycle contract.
- Produces: explicit pass scope propagated by CLI/scheduler, one message row per `RunOnce`/`RunBackstop`, and one person row only when `PersonWorker.RunOnce` executes.

- [ ] **Step 1: Write failing pass-ownership tests**

Assert two provider batches inside one worker call create one row, two CLI loop passes create two rows, backstop creates a fresh row, a skipped person scan creates no person row, and first/final converged scans each create their own person row. Use a real in-memory recorder that validates transitions rather than asserting mock calls.

- [ ] **Step 2: Write failing final-outcome tests**

Cover a lone permanently rejected person where `RunOnce` returns nil but `Failed == 1`; assert terminal state is `failed`, not `succeeded`. Cover mixed success/failure as `partial`, context cancellation as `cancelled`, and transient retries that later succeed without incrementing final failure.

- [ ] **Step 3: Run embedding tests and verify recording is absent**

Run: `go test -tags "fts5 sqlite_vec" ./internal/vector/embed ./internal/scheduler ./cmd/msgvault/cmd -run 'Test.*(Operation|Invocation|Pass|PersonWorker.*Failure)'`

Expected: FAIL because pass identity/trigger and archive recorder integration do not exist.

- [ ] **Step 4: Thread an explicit pass scope through callers**

Define `operations.PassScope{Key, Trigger, StartedAt}` and change scheduler/CLI runner calls to pass it explicitly. Scheduler keys use the canonical scheduled occurrence plus kind and generation; each CLI loop iteration generates one fresh random key. Keep existing generation and operation gates as execution authority.

- [ ] **Step 5: Record message and person outcomes at their true owners**

Wrap `Worker.RunOnce`/`RunBackstop` around one message invocation lifecycle and `PersonWorker.RunOnce` around one person lifecycle. Checkpoint aggregate final-item counters after durable batches. Finish from counters plus returned/cancellation error, detached from cancelled contexts. A pre-work `begin` failure aborts unless the existing gate proves safe; checkpoint/finish failures are fixed internal log events and do not undo primary work.

- [ ] **Step 6: Run embedding/scheduler/CLI tests and format**

Run: `go test -tags "fts5 sqlite_vec" ./internal/vector/embed ./internal/scheduler ./cmd/msgvault/cmd -run 'Test.*(Operation|Invocation|Pass|Embedding|PersonWorker)' && gofmt -w internal/vector/embed/worker.go internal/vector/embed/worker_test.go internal/vector/embed/person_worker.go internal/vector/embed/person_worker_test.go internal/scheduler/embed_job.go internal/scheduler/scheduler_test.go cmd/msgvault/cmd/embed_vector.go cmd/msgvault/cmd/embed_vector_test.go cmd/msgvault/cmd/serve_vector.go`

Expected: PASS and every test-created pass has exactly one terminal ledger row.

- [ ] **Step 7: Commit embedding instrumentation**

```bash
git add internal/vector/embed/worker.go internal/vector/embed/worker_test.go internal/vector/embed/person_worker.go internal/vector/embed/person_worker_test.go internal/scheduler/embed_job.go internal/scheduler/scheduler_test.go cmd/msgvault/cmd/embed_vector.go cmd/msgvault/cmd/embed_vector_test.go cmd/msgvault/cmd/serve_vector.go
git commit -m "feat(operations): record embedding passes"
```

### Task 5: Document and Visual Pass Recording

**Files:**
- Modify: `cmd/msgvault/cmd/documents.go`
- Modify: `cmd/msgvault/cmd/documents_test.go`
- Modify: `cmd/msgvault/cmd/documents_vector_runtime.go`
- Modify: `cmd/msgvault/cmd/documents_vector_test.go`
- Modify: `cmd/msgvault/cmd/multimodal.go`
- Modify: `cmd/msgvault/cmd/serve_vector_init.go`
- Modify: `cmd/msgvault/cmd/serve_vector_init_test.go`
- Modify: `cmd/msgvault/cmd/serve_vector_visual_credentials_test.go`
- Modify: `internal/vector/document/worker.go`
- Modify: `internal/vector/document/worker_test.go`
- Modify: `internal/vector/visual/worker.go`
- Modify: `internal/vector/visual/worker_test.go`

**Interfaces:**
- Consumes: Task 1 recorder and Task 4 `operations.PassScope`.
- Produces: one extraction row per `executeDocumentBuild`, one document-vector row per bounded worker pass, and one visual row per `runVisualOperation` or scheduled `runVisualPass`.

- [ ] **Step 1: Write failing bounded-boundary tests**

Cover incremental extraction, full rebuild, rebuild resume, document-vector resume, visual build, visual resume, scheduled visual maintenance, and post-activation reconciliation. Assert each boundary creates one row while each CLI-over-HTTP loop creates one row per HTTP pass.

- [ ] **Step 2: Write failing outcome/counter tests**

Assert document attempted/succeeded/failed counts distinct final documents or chunks, visual skipped is included in the attempted invariant, durable partial work plus an error becomes `partial`, and private rebuild/generation IDs never enter public rows.

- [ ] **Step 3: Run document/visual tests and verify recording is absent**

Run: `go test -tags "fts5 sqlite_vec" ./internal/vector/document ./internal/vector/visual ./cmd/msgvault/cmd -run 'Test.*(Document|Visual).*(Operation|Invocation|Pass|Build|Resume)'`

Expected: FAIL because those execution boundaries do not own archive invocation rows.

- [ ] **Step 4: Add recorder ownership at command/runtime boundaries**

Pass a recorder and explicit scope into `executeDocumentBuild`, document-vector bounded execution, `runVisualOperation`, and `runVisualPass`. Keep provider batches inside the owning row and give every later resume a fresh invocation key. Store only an optional private document rebuild reference; never expose generation/build IDs.

- [ ] **Step 5: Normalize final outcomes from worker results**

Translate existing worker results to distinct item outcomes at the boundary. Where a worker result only exposes retry-attempt counters, extend that result with final `Attempted`, `Succeeded`, `Failed`, and `Skipped` fields derived at the durable publication decision, then test that transient retries do not become public failures.

- [ ] **Step 6: Run focused tests and format**

Run: `go test -tags "fts5 sqlite_vec" ./internal/vector/document ./internal/vector/visual ./cmd/msgvault/cmd -run 'Test.*(Document|Visual)' && gofmt -w cmd/msgvault/cmd/documents.go cmd/msgvault/cmd/documents_test.go cmd/msgvault/cmd/documents_vector_runtime.go cmd/msgvault/cmd/documents_vector_test.go cmd/msgvault/cmd/multimodal.go cmd/msgvault/cmd/serve_vector_init.go cmd/msgvault/cmd/serve_vector_init_test.go cmd/msgvault/cmd/serve_vector_visual_credentials_test.go internal/vector/document/worker.go internal/vector/document/worker_test.go internal/vector/visual/worker.go internal/vector/visual/worker_test.go`

Expected: PASS with one terminal public row per bounded pass.

- [ ] **Step 7: Commit document and visual instrumentation**

```bash
git add cmd/msgvault/cmd/documents.go cmd/msgvault/cmd/documents_test.go cmd/msgvault/cmd/documents_vector_runtime.go cmd/msgvault/cmd/documents_vector_test.go cmd/msgvault/cmd/multimodal.go cmd/msgvault/cmd/serve_vector_init.go cmd/msgvault/cmd/serve_vector_init_test.go cmd/msgvault/cmd/serve_vector_visual_credentials_test.go internal/vector/document/worker.go internal/vector/document/worker_test.go internal/vector/visual/worker.go internal/vector/visual/worker_test.go
git commit -m "feat(operations): record document and visual passes"
```

### Task 6: Native Recovery and Person-Enrichment Resumption

**Files:**
- Modify: `internal/store/sync.go`
- Modify: `internal/store/sync_test.go`
- Modify: `internal/store/carddav_sync_runs.go`
- Modify: `internal/store/carddav_sync_runs_test.go`
- Modify: `internal/store/person_sweep_history.go`
- Modify: `internal/store/person_sweep_history_test.go`
- Modify: `internal/store/person_enrichment_runs.go`
- Modify: `internal/store/person_enrichment_runs_test.go`
- Modify: `cmd/msgvault/cmd/serve.go`
- Modify: `cmd/msgvault/cmd/person_enrichment_schedule_test.go`
- Modify: `cmd/msgvault/cmd/serve_lifecycle_test.go`

**Interfaces:**
- Consumes: Task 1 recovery methods and Task 2 native adapters.
- Produces: startup recovery for every kind, oldest-first queued enrichment claims, and immutable native public history.

- [ ] **Step 1: Write failing recovery-matrix tests**

Seed stale running rows for all nine kinds. Assert source/CardDAV and five invocation ledgers become `partial` when durable useful outcomes exist and `failed` otherwise; person-sweep uses lease recovery; document/visual private resumable state remains intact; terminal rows do not change; and no recovered row remains falsely running.

- [ ] **Step 2: Write failing enrichment claimant tests**

Seed two validated running requests with pending attempts and one invalid request. Assert startup moves the valid requests to queued without changing `started_at`, terminalizes the invalid request from durable outcomes, and each scheduler wake atomically claims/drains the oldest queued run before creating a scheduled occurrence.

- [ ] **Step 3: Run recovery tests and verify gaps**

Run: `go test -tags "fts5 sqlite_vec" ./internal/store ./cmd/msgvault/cmd -run 'Test.*(Recovery|Restart|QueuedEnrichment|EnrichmentSchedule|PruneCardDAV)'`

Expected: FAIL because source recovery, queued enrichment claiming, counter-aware stale finalization, and immutable CardDAV history are incomplete.

- [ ] **Step 4: Implement idempotent startup recovery**

Run recovery before the daemon advertises status or starts schedulers. Give every recovered terminal row fixed `daemon_restarted` error metadata and calculate `partial`/`failed` from durable final counters. Preserve native generation/rebuild/publication checkpoints for later fresh invocations.

- [ ] **Step 5: Implement queued enrichment ownership**

Add `ListQueuedPersonEnrichmentRunsContext` ordered by original `started_at, id` and `ClaimQueuedPersonEnrichmentRunContext` using a state-predicate transaction. Change scheduler wakes to drain queued requests first. Set `started_at` only on the first claim; never project never-started scheduled requests.

- [ ] **Step 6: Stop ordinary public-history pruning**

Remove the `DELETE FROM carddav_sync_runs` retention path and its scheduler call. Keep cleanup of private attempt/publication/vector rows unchanged. Add a regression test showing more than the old retention limit remains queryable.

- [ ] **Step 7: Run recovery tests and format**

Run: `go test -tags "fts5 sqlite_vec" ./internal/store ./cmd/msgvault/cmd -run 'Test.*(Recovery|Restart|QueuedEnrichment|EnrichmentSchedule|OperationHistory|CardDAV)' && gofmt -w internal/store/sync.go internal/store/sync_test.go internal/store/carddav_sync_runs.go internal/store/carddav_sync_runs_test.go internal/store/person_sweep_history.go internal/store/person_sweep_history_test.go internal/store/person_enrichment_runs.go internal/store/person_enrichment_runs_test.go cmd/msgvault/cmd/serve.go cmd/msgvault/cmd/person_enrichment_schedule_test.go cmd/msgvault/cmd/serve_lifecycle_test.go`

Expected: PASS with recovery safe to run twice.

- [ ] **Step 8: Commit recovery behavior**

```bash
git add internal/store/sync.go internal/store/sync_test.go internal/store/carddav_sync_runs.go internal/store/carddav_sync_runs_test.go internal/store/person_sweep_history.go internal/store/person_sweep_history_test.go internal/store/person_enrichment_runs.go internal/store/person_enrichment_runs_test.go cmd/msgvault/cmd/serve.go cmd/msgvault/cmd/person_enrichment_schedule_test.go cmd/msgvault/cmd/serve_lifecycle_test.go
git commit -m "feat(operations): recover durable run history"
```

### Task 7: Complete Status, Detail, Action, and Generated Contracts

**Files:**
- Modify: `internal/api/operations.go`
- Modify: `internal/api/operations_test.go`
- Modify: `internal/api/openapi.go`
- Modify: `internal/api/openapi_test.go`
- Modify: `internal/api/routes.go`
- Modify: `api/openapi.yaml`
- Modify: `pkg/client/openapi.yaml`
- Modify: `pkg/client/generated/types.go`
- Modify: `pkg/client/generated/client.go`
- Modify: `web/src/lib/api/generated/schema.d.ts`

**Interfaces:**
- Consumes: Tasks 2–6 complete history/status and existing authenticated CardDAV/visual mutation endpoints.
- Produces: final OpenAPI DTOs for all nine kinds, dynamic status/detail actions, and generated Go/Web clients.

- [ ] **Step 1: Write failing API contract tests**

Assert all nine registered kinds appear in status, configured/history availability is truthful, active/latest/latest-successful come from independent adapter reads, detail adds related status and exact supported actions, and only `carddav_sync`, `visual_build`, or `visual_resume` are advertised under their eligibility rules.

- [ ] **Step 2: Write failing mutation-control tests**

Exercise the advertised existing endpoints through production routes and assert API-key auth, CSRF, idempotency, serial operation gates, busy conflicts, stale archives, and consent rules are unchanged. Assert no generic retry or document mutation route is added.

- [ ] **Step 3: Run API/OpenAPI tests and verify incomplete contracts**

Run: `go test -tags "fts5 sqlite_vec" ./internal/api ./cmd/msgvault/cmd -run 'Test(Operation|OpenAPI.*Operation|CardDAV|Visual)'`

Expected: FAIL until the status/detail DTOs and dynamic history/action schemas describe the completed slice.

- [ ] **Step 4: Complete server projection and OpenAPI schema**

Make status query each kind independently, preserve unaffected lanes on one adapter failure, and attach related status/action IDs only from the closed registry. Define list metadata for membership revision and unavailable kinds, canonical date parameters, encrypted opaque references/cursors, and fixed error responses without passthrough maps.

- [ ] **Step 5: Regenerate committed clients**

Run: `make api-generate && make web-generate`

Expected: only the two OpenAPI documents, generated Go client files, and `web/src/lib/api/generated/schema.d.ts` change from generation.

- [ ] **Step 6: Verify generated parity and format**

Run: `make api-check && make web-check && go test -tags "fts5 sqlite_vec" ./internal/api ./cmd/msgvault/cmd -run 'Test(Operation|OpenAPI.*Operation)' && gofmt -w internal/api/operations.go internal/api/operations_test.go internal/api/openapi.go internal/api/openapi_test.go internal/api/routes.go`

Expected: PASS and regeneration leaves no diff.

- [ ] **Step 7: Commit API contracts**

```bash
git add internal/api/operations.go internal/api/operations_test.go internal/api/openapi.go internal/api/openapi_test.go internal/api/routes.go api/openapi.yaml pkg/client/openapi.yaml pkg/client/generated web/src/lib/api/generated/schema.d.ts
git commit -m "feat(api): publish complete operation contracts"
```

### Task 8: Operations URL State and Controller

**Files:**
- Create: `web/src/lib/operations/models.ts`
- Create: `web/src/lib/operations/controller.svelte.ts`
- Create: `web/src/lib/operations/controller.svelte.test.ts`
- Modify: `web/src/lib/explore/models.ts`
- Modify: `web/src/lib/explore/state.svelte.ts`
- Modify: `web/src/lib/explore/state.test.ts`

**Interfaces:**
- Consumes: Task 7 generated `OperationStatusResponse`, `OperationRunsResponse`, and `OperationRunDetail` schemas.
- Produces: normalized Operations URL fields and `OperationsController` snapshot/actions for the workspace.

- [ ] **Step 1: Write failing URL normalization tests**

Round-trip `workspace: 'operations'`, lane, kind, state, start-date bounds, and selected opaque run reference. Assert malformed/unknown values normalize away, filters clear cursor/selection atomically, Back/Forward restores filters and detail, and pagination cursors never serialize.

- [ ] **Step 2: Run state tests and verify Operations is rejected**

Run: `cd web && bun test src/lib/explore/state.test.ts`

Expected: FAIL because `operations` and its restorable fields are absent.

- [ ] **Step 3: Extend Explore URL state**

Add:

```ts
operationLane: '' | 'messages' | 'person_facts' | 'contacts' | 'documents' | 'visual_attachments';
operationKind: '' | OperationKind;
operationState: '' | OperationState;
operationStartedFrom: string;
operationStartedBefore: string;
operationRunID: string | null;
```

Normalize incompatible lane/kind pairs by clearing kind, use canonical RFC 3339 strings, and add the fields to restoration invalidation only where a committed filter change must clear selection.

- [ ] **Step 4: Write failing controller behavior tests**

Using full generated-shape fetch responses, assert initial status/list loading, background refresh without blanking data, request cancellation on filter/archive change, `limit+1` cursor continuation, direct detail restoration, stale response discard, partial adapter availability, 409 restart state, and CardDAV/visual action refresh.

- [ ] **Step 5: Run controller tests and verify the class is absent**

Run: `cd web && bun test src/lib/operations/controller.svelte.test.ts`

Expected: FAIL because `OperationsController` does not exist.

- [ ] **Step 6: Implement the generated-client controller**

Expose one readonly snapshot containing status lanes, rows, unavailable kinds, selected detail, initial/background loading, paging, conflict, and action state. Use an `AbortController` plus monotonically increasing request generation. Map URL fields directly to generated query parameters and invoke only the three generated existing mutation methods advertised by detail/status.

- [ ] **Step 7: Run state/controller tests and type-check**

Run: `cd web && bun test src/lib/explore/state.test.ts src/lib/operations/controller.svelte.test.ts && bun run check`

Expected: PASS with no handwritten API response types.

- [ ] **Step 8: Commit URL/controller behavior**

```bash
git add web/src/lib/operations/models.ts web/src/lib/operations/controller.svelte.ts web/src/lib/operations/controller.svelte.test.ts web/src/lib/explore/models.ts web/src/lib/explore/state.svelte.ts web/src/lib/explore/state.test.ts
git commit -m "feat(web): add operations workspace state"
```

### Task 9: Operations Workspace, Shell Navigation, and Cross-Links

**Files:**
- Create: `web/src/lib/components/operations/OperationsWorkspace.svelte`
- Create: `web/src/lib/components/operations/OperationsWorkspace.test.ts`
- Create: `web/src/lib/components/operations/OperationLaneCards.svelte`
- Create: `web/src/lib/components/operations/OperationRunTable.svelte`
- Create: `web/src/lib/components/operations/OperationRunDetail.svelte`
- Modify: `web/src/lib/components/shell/AppShell.svelte`
- Modify: `web/src/lib/components/shell/AppShell.test.ts`
- Modify: `web/src/lib/components/sources/SourcesWorkspace.svelte`
- Modify: `web/src/lib/components/sources/SourcesWorkspace.test.ts`
- Modify: `web/src/lib/components/settings/CardDAVOperations.svelte`
- Modify: `web/src/lib/components/settings/CardDAVOperations.test.ts`

**Interfaces:**
- Consumes: Task 8 `OperationsController` and Operations URL fields.
- Produces: the first-class Operations tab, five lane cards, filter bar, responsive run list/table, detail pane, actions, and authority cross-links.

- [ ] **Step 1: Write failing workspace rendering tests**

Assert the exact public card labels, kind labels within multi-kind cards, configured/unconfigured/unavailable distinctions, active/latest/latest-successful values, semantic non-color-only states, canonical filter callbacks, bounded counter labels, fixed errors, and only advertised action buttons.

- [ ] **Step 2: Write failing keyboard/responsive tests**

Assert row selection commits the opaque ID, Escape closes detail, closing detail restores focus to the invoking row, narrow layout uses the focused content region, loading/empty/background/conflict/action states have appropriate status or alert roles, and unavailable kinds never blank available rows/cards.

- [ ] **Step 3: Run component tests and verify components are absent**

Run: `cd web && bun test src/lib/components/operations/OperationsWorkspace.test.ts`

Expected: FAIL because the Operations components do not exist.

- [ ] **Step 4: Build the dense workspace components**

Use existing Kit UI `StatusDot`, `Button`, `SelectDropdown`, and shell density tokens. Cards render `Messages`, `Facts`, `Contacts`, `Documents`, and `Attachments`; the table renders kind, trigger, state, start, derived duration, and compact counters; detail renders only allowlisted fields, related links, and advertised actions.

- [ ] **Step 5: Write failing shell and cross-link tests**

Assert Operations appears between Sources and Deletions, shell Back/Forward restores it, Sources links to `source_sync`, CardDAV settings links to `carddav_sync`, and related-status links navigate through shell state without hand-built query strings.

- [ ] **Step 6: Integrate controller and navigation**

Instantiate/destroy `OperationsController` in `AppShell`, apply URL state through the established history-restoration effect, render the workspace branch, announce action/navigation results through the existing live region, and add authority cross-links with normalized lane/kind patches.

- [ ] **Step 7: Run workspace/shell tests and type-check**

Run: `cd web && bun test src/lib/components/operations/OperationsWorkspace.test.ts src/lib/components/shell/AppShell.test.ts src/lib/components/sources/SourcesWorkspace.test.ts src/lib/components/settings/CardDAVOperations.test.ts && bun run check && bun run check:kit-ui`

Expected: PASS with accessible names and no new Kit UI violations.

- [ ] **Step 8: Commit the Web workspace**

```bash
git add web/src/lib/components/operations web/src/lib/components/shell/AppShell.svelte web/src/lib/components/shell/AppShell.test.ts web/src/lib/components/sources/SourcesWorkspace.svelte web/src/lib/components/sources/SourcesWorkspace.test.ts web/src/lib/components/settings/CardDAVOperations.svelte web/src/lib/components/settings/CardDAVOperations.test.ts
git commit -m "feat(web): build operations workspace"
```

### Task 10: Browser Coverage, Privacy Audit, and Release Gates

**Files:**
- Create: `web/tests/operations.spec.ts`
- Create: `web/tests/e2e/operations.spec.ts`
- Create: `web/tests/e2e/fixtures/operations.ts`
- Modify: `web/tests/e2e/accessibility.spec.ts`
- Modify: `web/tests/e2e/keyboard.spec.ts`
- Modify: `web/tests/e2e/security.spec.ts`

**Interfaces:**
- Consumes: the complete backend, generated contract, controller, and workspace from Tasks 1–9.
- Produces: production-path browser proof and a fully verified feature tree ready for the controller's final review and publication sequence.

- [ ] **Step 1: Write failing browser journeys**

Use obviously synthetic fixtures to cover desktop and narrow layouts, lane/kind/state/date filters, pagination, direct detail reload, Back/Forward, one unavailable adapter, revision drift and restart, CardDAV action success/conflict, visual action gating, focus return, and status/detail cross-links.

- [ ] **Step 2: Add privacy and accessibility assertions**

Inspect rendered text and intercepted JSON for synthetic private sentinel values representing a name, address, filename, provider, model, endpoint, raw error, archive UID, database ID, generation fingerprint, and credential. Assert none appear. Run axe against workspace, detail, failure, and narrow states; cover keyboard-only filtering/selection/action/close flows.

- [ ] **Step 3: Run browser tests and fix only witnessed failures test-first**

Run: `cd web && bun run test:browser -- operations.spec.ts e2e/operations.spec.ts e2e/accessibility.spec.ts e2e/keyboard.spec.ts e2e/security.spec.ts`

Expected: PASS. For any witnessed product defect, first retain the failing regression assertion, then change production code and re-run this command.

- [ ] **Step 4: Run generated, Web, Go, docs, formatting, and lint gates**

Run:

```bash
make api-check
make web-check
make web-test
make web-build
gofmt -w internal cmd/msgvault/cmd
go vet -tags "fts5 sqlite_vec" ./...
make lint-ci
make test
make docs-check
```

Expected: every command exits 0 and generated checks leave no diff.

- [ ] **Step 5: Run PostgreSQL parity when the configured test database is available**

Run: `make test-pg-both`

Expected: PASS for shipped and pgvector configurations. If `MSGVAULT_TEST_DB` is genuinely absent, record that exact environment limitation in the final handoff rather than claiming PostgreSQL execution.

- [ ] **Step 6: Inspect the complete diff and privacy boundary**

Run:

```bash
git diff --check feat/issue-639-web-directory...HEAD
git status --short --untracked-files=all
rg -n -i '<private-company>|<private-person>|<private-account>|@gmail|api[_ -]?key\s*[:=]|bearer\s+[A-Za-z0-9]' $(git diff --name-only feat/issue-639-web-directory...HEAD)
git diff --name-only feat/issue-639-web-directory...HEAD | rg '(^|/)\.superpowers/|(^|/)\.skillshare/|roborev'
```

Expected: clean diff check; only intentional tracked files; both privacy/local-state searches return no matches.

- [ ] **Step 7: Commit browser proof**

```bash
git add web/tests/operations.spec.ts web/tests/e2e/operations.spec.ts web/tests/e2e/fixtures/operations.ts web/tests/e2e/accessibility.spec.ts web/tests/e2e/keyboard.spec.ts web/tests/e2e/security.spec.ts
git commit -m "test(web): verify operations workspace end to end"
```

## Controller Finish

These steps are controller-owned and begin only after Task 10's task-scoped review is clean. They are not delegated to a task implementer.

- [ ] **Step 1: Obtain final whole-branch review and close findings**

Generate the Subagent-Driven Development review package from `a1f5c68304ac1d3a9efaad964b11522132e4a66d` through HEAD. Dispatch the most capable available reviewer against the spec, plan, ledger rulings, and full diff. Send any Critical/Important findings through the skill's single final fix wave and scoped re-review.

- [ ] **Step 2: Squash, re-run decisive verification, and publish the stacked PR**

Squash all branch commits to one feature commit on `a1f5c68304ac1d3a9efaad964b11522132e4a66d`, re-run `make test`, `make web-test`, `make web-build`, `make api-check`, `make docs-check`, and the privacy/local-state searches against the squashed tree, then push `feat/operations-workspace` and open its PR with base `feat/issue-639-web-directory`. The PR body states rationale, behavior, and stacked review boundary without a test-plan section.

- [ ] **Step 3: Attach Kata/GitHub monitoring**

Search the hosted Kata roadmap parent and its remaining Web issues, reuse the matching Operations issue, and attach the new PR URL plus exact merge trigger. Monitor the stacked base and Operations PR without closing roadmap items until their acceptance/merge conditions are authoritative.
