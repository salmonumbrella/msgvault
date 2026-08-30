---
last_edited: 2026-08-30
---

# Unified Operations Workspace Design

## Status

Approved product and architecture design for Kata `pc4n`.

This work is a stacked full vertical slice. The feature branch starts from
`feat/issue-639-web-directory` at `a1f5c683` and its pull request targets that
branch. After the Directory and Settings pull request merges, this branch is
rebased onto `main` and the pull request is retargeted to `main`.

## Outcome

Add a first-party Operations workspace that answers four questions across
Msgvault's background work:

- What is running now?
- What ran most recently?
- What last completed successfully?
- What failed, stopped partway through, or needs a supported recovery action?

Settings remains the authority for what should run. Operations reports what
did run. Existing subsystem status endpoints remain authoritative for live
coverage, convergence, generation, backlog, and configuration details.

The workspace covers five public lanes:

| Public label | Internal lane | Kinds |
|---|---|---|
| Messages | `messages` | source sync, message embedding |
| Facts | `person_facts` | person sweep, person embedding, person enrichment |
| Contacts | `contacts` | CardDAV sync |
| Documents | `documents` | document extraction, document embedding |
| Attachments | `visual_attachments` | visual embedding |

The public labels are intentionally shorter than the internal lane keys.
Internal keys remain stable API values.

## Scope

This slice delivers durable history, normalized API projections, generated
contracts, worker integration, recovery behavior, and the complete Web
workspace. It also adds cross-links from existing Sources, document, visual,
CardDAV, and Settings surfaces where those views already own the relevant
status or configuration.

The following are not part of this slice:

- storage analytics or attachment-retention analytics;
- a second copy of subsystem coverage, convergence, generation, or backlog;
- provider configuration, credentials, endpoints, model names, or policy
  editing in Operations;
- arbitrary generic retry controls;
- a new TUI Operations workspace;
- provider calls, archive content, or per-record diagnostics in history.

## Existing Foundation

The stacked base already provides the closed `operations.Kind`, `Lane`,
`State`, `Trigger`, stable-ID, counter, error, action, and related-status
contracts. It also provides archive-bound list and detail references, filtered
cursor pagination, lane status, OpenAPI routes, and durable adapters for source
sync, person sweep, and CardDAV sync.

The current API deliberately advertises unavailable history for message
embedding, person embedding, document extraction, document embedding, visual
embedding, and person enrichment. This slice makes those kinds truthful rather
than hiding them or inferring runs from current coverage.

## Architecture

### Native-first projection

Use each subsystem's archive-database run table when it already represents one
bounded execution pass or native request scope. Add a narrow per-kind
invocation ledger where the current table is replaceable, represents a
generation rather than an invocation, or misses incremental maintenance.

Do not add one generic mirror table for every subsystem. A generic mirror would
create two authorities for state and make crash ordering ambiguous. The
normalized `operations.Run` remains a read model assembled by adapters; it does
not become the write authority.

| Kind | Durable authority | Design |
|---|---|---|
| `source_sync` | `sync_runs` | Preserve the existing projection; add startup recovery and history-revision wiring. |
| `person_sweep` | `person_sweep_runs` | Preserve the existing projection and native lease recovery; add history-revision wiring. |
| `carddav_sync` | `carddav_sync_runs` | Preserve the existing projection/recovery; remove automatic history pruning and add history-revision wiring. |
| `message_embedding` | new `message_embedding_runs` ledger | Record one row around each bounded message-worker pass in the archive database. The replaceable vector-backend `embed_runs` table is not public history and its raw `error` column is never projected. |
| `person_embedding` | new `person_embedding_runs` ledger | Record one row around each bounded person-worker pass because person reconciliation currently has no independent durable run boundary. |
| `document_extraction` | new `document_extraction_runs` ledger | Record full-rebuild, resume, and incremental reconciliation invocations. A private optional rebuild reference may connect the invocation to `document_extraction_rebuilds`. |
| `document_embedding` | new `document_embedding_runs` ledger | Record each bounded generation-build or resume invocation. `document_vector_generations` remains the work and serving authority. |
| `visual_embedding` | new `visual_embedding_runs` ledger | Record each build, resume, and post-activation reconciliation invocation. `visual_generations` remains the work and serving authority. |
| `person_enrichment` | `person_enrichment_runs` | Add a direct adapter over the existing run and counter authority. |

A bounded execution pass creates one run row. Provider batches and internal
retries inside that pass update the row; they never create rows. A later pass or
resume creates a new invocation row while continuing the same private native
rebuild or generation. An enclosing CLI loop may therefore produce several
rows, one per pass whose result can independently succeed or fail. Incremental
document extraction and post-activation visual reconciliation remain visible
without reopening a terminal run.

SQLite and PostgreSQL must expose equivalent schemas, indexes, ordering, state
transitions, and projections. Every public history authority lives in the
archive database, so recreating `vectors.db`, purging retired vector data, or
reusing a generation fingerprint cannot remove or rewrite Operations history.
Native rebuild, generation, publication, and vector rows remain private related
status and may keep their existing cleanup lifecycle.

### Invocation ledger contract

Each new per-kind ledger has explicit scalar columns for numeric ID, bounded
opaque invocation key, trigger, state, UTC start/finish, its allowlisted
counters, and nullable fixed public error code. It has no arbitrary metadata,
raw error, provider, model, endpoint, native owner ID, content identifier, or
request/response column. The public error message is derived from the fixed
code at read time.

`begin` is an upsert by invocation key and returns `created`, `active`, or
`terminal` with the same stable ID. Only `created`, or `active` held by the same
native execution owner, may execute the pass. `terminal` returns the stored
outcome without re-executing work. A genuine retry or resume receives a new key
and row. Existing request idempotency and native execution gates remain the
correctness authority; if `begin` fails before work starts, the caller may
continue only when those authorities prove the pass is new and safe. Once work
has started, checkpoint/finalization failures remain observability failures and
never roll back committed primary work.

The invocation key is private, bounded, non-identifying, and never enters the
public DTO. Indexes support newest-first `(started_at, id)` reads and the native
execution gate's active-run constraint. Schema migrations and constraints are
equivalent on SQLite and PostgreSQL.

### Recorder ownership

The recorder belongs to the boundary that returns one bounded result, not to a
provider batch and not automatically to the outermost CLI command.

| Kind | Recorder owner and one public row |
|---|---|
| source sync | The native `StartSync`/finish scope for one source sync execution. |
| person sweep | The native `StartPersonSweepRun`/finish scope for one scheduled or manual sweep request. |
| CardDAV sync | The native `StartCardDAVSyncRunContext`/finish scope for one sync-service request. |
| message embedding | One `embed.Worker` `RunOnce` or `RunBackstop` pass. Scheduler and CLI loops create one row for each pass they invoke. |
| person embedding | One `PersonWorker.RunOnce` reconciliation pass. A generation coordinator creates this row only when it actually calls the person worker. |
| document extraction | One `executeDocumentBuild` incremental, rebuild, or resume pass. |
| document embedding | One bounded document-vector build/checkpoint pass. |
| visual embedding | One `runVisualOperation` or scheduled `runVisualPass` pass. The visual CLI loop creates one row per HTTP pass. |
| person enrichment | The native `StartRun` scope for one scheduled or manual enrichment request; its internal claims and provider jobs remain batches of that row. |

Scheduler occurrence keys are deterministic. CLI-only local passes create one
fresh key when the pass starts. HTTP mutations derive the key from their
existing idempotency/request scope. Coordinators must pass that key through to
the recorder; workers never derive it from provider data or content.

Run rows are append-only except for their own queued/running/terminal lifecycle
and monotonic counter checkpoints. Terminal state, finish time, fixed error,
and final counters are immutable. Ordinary subsystem cleanup never deletes a
run row, and existing bounded run pruning is disabled for histories exposed by
Operations. A future archive-level history-retention feature would require its
own explicit design and cursor-revision rules.

### Stable IDs

Stable IDs are kind-qualified before they enter ordering or cursor logic:

- numeric IDs: source sync, CardDAV sync, message embedding, person embedding,
  document extraction, document embedding, visual embedding, and person
  enrichment;
- text IDs: person sweep.

The Web API continues returning opaque, archive-bound references. It never
returns a raw database ID, generation fingerprint, archive UID, provider ID,
person ID, source ID, attachment ID, or document hash.

References and cursors use a versioned authenticated-encryption envelope, not
base64 JSON. The archive database owns one active random 256-bit token key and
may retain prior keys as decrypt-only. Tokens carry only a non-secret key ID,
random nonce, and AEAD ciphertext. The encrypted payload contains the typed run
position/filter fields, while the token version and current archive UID are
additional authenticated data and are never serialized. Decoding on another
archive, modifying a token, or using an unknown key fails closed.

Token keys live in an archive-side `operation_token_keys` table with a random
non-secret key ID, 256-bit key bytes, `active` or `decrypt_only` state, and UTC
creation/retirement timestamps. A database constraint permits exactly one
active key. Key creation and rotation are transactional, and readers load keys
only through the private token codec; adapters, public storage interfaces, and
API DTOs cannot enumerate or return key material.

The active key survives daemon restart and archive backup. Rotation starts
issuing with a new key while retained prior keys continue to open bookmarked
details and in-flight cursors. Explicitly purging a retired key invalidates its
tokens with the existing typed invalid-reference/invalid-cursor response; there
is no automatic rotation or key-management UI in this slice. Key bytes never
enter logs, configuration responses, generated fixtures, or public DTOs.
Unsigned v1 base64-JSON tokens from the stacked foundation are rejected rather
than accepted through a privacy-weak compatibility path.

Newest-first order remains `started_at`, then kind, then typed stable ID. All
timestamps are normalized to UTC. Legacy rows whose trigger cannot be proven
omit `trigger`; migrations never guess whether old work was manual or
scheduled.

### Lifecycle recorder

Every instrumented execution boundary uses a small begin/checkpoint/finish
recorder:

1. `begin` creates or opens the one durable logical-run row before work starts.
2. `checkpoint` replaces bounded aggregate counters at existing safe
   checkpoints; it never applies an unkeyed increment.
3. `finish` makes one terminal transition with a fixed public outcome.

The recorder receives normalized facts only. It never accepts request bodies,
provider responses, content snippets, filenames, identities, endpoints,
credential material, or arbitrary error strings.

A `begin` failure occurs before primary work and is logged with a fixed internal
event. Work may continue only when the existing request-idempotency and native
execution authorities prove that this pass is new and safe; otherwise the
boundary fails before primary work starts. After work has started, recorder
failure is an observability failure: a failed checkpoint does not roll back
completed work, and a failed `finish` may leave a running row for startup
recovery. Recorder writes remain idempotent so retrying a checkpoint cannot
increment counters twice.

`finish` terminates the invocation, not the related generation. A bounded pass
that completes normally is `succeeded` even when related status says more work
remains. A resumable error can finish the invocation as `partial` or `failed`
while the native rebuild or generation remains recoverable. This keeps
`running` truthful without adding claim ownership to long-lived generation
rows.

### State rules

Public states keep their existing meanings:

- `queued`: a previously started native request is eligible to resume;
- `running`: the current daemon owns an active invocation;
- `succeeded`: the logical run completed without item failures;
- `partial`: useful work committed, but one or more bounded items failed;
- `failed`: no useful result completed, or the logical operation terminated;
- `cancelled`: an explicit cancellation stopped the operation.

For embedding, extraction, and visual invocations, terminal state is calculated
at `finish` from final invocation outcomes and the returned error, never from
later coverage or transient retry failures. Explicit cancellation wins. Final
failed items plus useful committed outcomes produce `partial`; final failed
items with no useful outcome produce `failed`. With no final failed items, a
returned error plus useful committed outcomes produces `partial`, a returned
error with no useful outcome produces `failed`, and only an error-free outcome
becomes `succeeded`.

Document rebuild/vector and visual generation state is never projected as run
state. It appears through related status. Generation activation, retirement,
cleanup, resume, and post-activation reconciliation cannot rewrite a terminal
invocation row.

Person enrichment maps its native queued/running/succeeded/partial/failed
states directly. Source sync, person sweep, and CardDAV retain their established
state matrices. A never-started enrichment request is not public run history;
the adapter begins projecting it only after its first claim sets `started_at`.
A recovered queued run keeps that original UTC start for ordering and displays
as waiting to resume rather than “not started.”

### Restart recovery

Daemon startup runs recovery before status is advertised. Recovery is
idempotent, terminal rows remain unchanged, and no invocation row may appear
`running` after its owning process has ended.

| Kind | Recovery of stale public state |
|---|---|
| source sync | Make the native row terminal with `daemon_restarted`; the adapter projects `partial` when durable added/updated counters show useful outcomes and `failed` otherwise. A later sync is a new run. |
| person sweep | Reclaim expired attempts through the native lease path, then finish the run `partial` or `failed` with its fixed sweep failure class. Never leave a run active with no live attempt. |
| CardDAV sync | Keep the existing native restart transition and fixed code; project `partial` if durable useful counters exist and `failed` otherwise. |
| message embedding | Finish with `daemon_restarted`; project `partial` when durable final checkpoints contain useful outcomes and `failed` otherwise. Unstamped messages remain discoverable by scan-and-fill. |
| person embedding | Finish with `daemon_restarted`; project `partial` when durable final checkpoints contain useful outcomes and `failed` otherwise. The next revision scan is a new run. |
| document extraction | Finish with `daemon_restarted`; project `partial` when durable final checkpoints contain useful outcomes and `failed` otherwise. Preserve native rebuild/extraction checkpoints for a later invocation. |
| document embedding | Finish with `daemon_restarted`; project `partial` when durable final checkpoints contain useful outcomes and `failed` otherwise. Preserve native generation/build progress. |
| visual embedding | Finish with `daemon_restarted`; project `partial` when durable final checkpoints contain useful outcomes and `failed` otherwise. Preserve native generation/publication progress. |
| person enrichment | If native pending work and attempt state pass recovery validation, move the same run `running` to `queued`; otherwise finish with `daemon_restarted` and derive `partial` or `failed` from durable useful outcomes. |

For document and visual work, recovery never advertises a run-level resume.
Related native status decides whether a new invocation can resume the private
rebuild or generation. For enrichment, the existing durable run is itself the
request authority, so a validated queued transition preserves its stable ID.
Startup adds an oldest-first queued-run reader and atomic `queued → running`
claim. Each enrichment wake claims and drains queued runs before it creates the
next scheduled occurrence, preserving the original `started_at` for a resumed
run. The API never infers resumability from row age alone.

## Public Counters and Errors

Each kind receives a closed allowlist of nonnegative `int64` counters. Public
item counters are distinct final outcomes for that invocation, not provider
attempt events. Transient retries do not increment `failed`. For kinds that use
`attempted`, finish enforces `attempted = succeeded + failed + skipped` when
`skipped` exists and `attempted = succeeded + failed` otherwise. `truncated` is
orthogonal and may overlap a successful or failed item.

The implementation may omit a zero or unavailable counter, but cannot invent a
new name at runtime. If the worker cannot produce a truthful distinct-final
counter, it omits that counter until instrumentation is added; it never
relabels claim events or retry attempts as item outcomes. Final aggregates are
stored on the invocation row before derivative cleanup and are not recomputed
from mutable publications later.

| Kind | Allowed aggregates |
|---|---|
| message embedding | attempted, succeeded, failed, truncated messages |
| person embedding | attempted, succeeded, failed, truncated people |
| document extraction | attempted, succeeded, failed documents |
| document embedding | attempted, succeeded, failed chunks |
| visual embedding | attempted, succeeded, failed, skipped attachments |
| person enrichment | requested, started, succeeded, failed, suppressed, identity-rejected people |

Existing source-sync, person-sweep, and CardDAV counter contracts remain
unchanged. The legacy vector `embed_runs.claimed` and retry-oriented failure
fields are not public counters. Counters describe the invocation only; they do
not claim current archive coverage or generation convergence.

Public errors use a closed code-to-message map. Adapters translate internal
errors into fixed categories such as cancellation, timeout, rate limit,
authentication, upstream failure, invalid output, safety limit, archive drift,
daemon restart, or internal failure. Unknown and unsafe errors map to a generic
redacted code. The fixed public message is generated from the code; stored or
logged raw errors never cross the DTO boundary.

## API

Keep the existing endpoints:

- `GET /api/v1/operations/status`
- `GET /api/v1/operations/runs`
- `GET /api/v1/operations/runs/{id}`

The status response continues to return one entry per registered kind, with
configured/history availability, active, latest, latest successful, related
status, and supported actions. The Web workspace groups those kind entries
into the five lane cards; the API does not collapse distinct kind histories.

The list endpoint supports URL-restorable filters for lane, kind, state,
`started_from`, and `started_before`. Date bounds are canonical UTC RFC 3339
and define the half-open interval `[started_from, started_before)`. The cursor
remains bound to the archive UID and the complete normalized filter set.

The archive database owns a singleton monotonic Operations membership revision.
Database triggers advance it in the same transaction for run insertion,
deletion, start/order changes, and state transitions across all nine native or
new ledgers. Counter-only checkpoints do not advance it because they cannot
change ordering or filter membership. Page one captures the revision and the
exact set of participating and unavailable kinds. The cursor binds that
revision, adapter set, archive UID, filter hash, and last ordering position. A
later page returns 409 and requires a page-one restart when the revision or
adapter availability changes. This deliberately prefers a visible restart over
skipped or duplicated mixed-adapter rows.

An unfiltered or lane-filtered request may return available runs plus a dynamic
`unavailable_kinds` set. A request filtered to one unavailable kind returns the
existing 503. List merging occurs inside one archive-database read transaction;
no public adapter reads the replaceable vector database. Archive changes,
filter drift, malformed references, and inconsistent snapshots fail closed
with the existing typed 400/409/503 responses.

The history-reader contract returns runs, dynamic availability, membership
revision, and cursor position as one snapshot rather than returning only a run
slice. The status path still loads each kind independently, so one degraded
adapter cannot erase other lane cards.

Each adapter read runs inside a savepoint within that snapshot. An adapter-local
schema, decode, or validation failure rolls back to its savepoint and marks only
that kind unavailable; this is required on PostgreSQL so a statement error does
not poison the transaction, and SQLite follows the same behavior for parity.
Failure to establish the transaction/savepoint, read or recheck membership
revision, or keep the connection healthy aborts the whole list with the typed
history failure instead of returning a partial snapshot.

List and detail DTOs are allowlists containing only:

- opaque run reference;
- kind, lane, trigger, and state;
- start and finish timestamps, from which the Web client derives duration;
- bounded public counters;
- fixed public error code and message.

Detail may add related-status and supported-action identifiers already present
in the normalized registry. It does not expose a debug payload or arbitrary
metadata map.

Mutations keep each existing endpoint's API-key authentication, CSRF,
idempotency, and operation-gate behavior. Operations adds no mutation that can
bypass those controls. The workspace renders an action only when the
status/detail response advertises its exact action ID.

| Action ID | Kind and eligibility | Existing endpoint and target | Result |
|---|---|---|---|
| `carddav_sync` | configured CardDAV with no active sync | `POST /api/v1/carddav/sync`; current configured account is server-selected | Creates a new CardDAV run; busy or stale state returns the endpoint's typed conflict. |
| `visual_build` | visual status advertises an eligible unconsented building generation and the build handler is available | `POST /api/v1/multimodal/build`; current generation is server-selected and consent uses the existing request contract | Creates a new visual invocation run, then refreshes status and history. |
| `visual_resume` | visual status advertises an incomplete consented generation and the run handler is available | `POST /api/v1/multimodal/run`; current generation is server-selected | Creates a new visual invocation run, then refreshes status and history. |

Document extraction/vector recovery remains a related-status or Settings/CLI
drilldown because no equivalent Web mutation exists. It is never advertised as
an Operations action. There is no generic retry endpoint and no UI that guesses
a CLI command, native ID, or recovery target.

## Web Workspace

### Navigation and URL state

Add `operations` as a first-class `ExploreWorkspace` and AppShell tab. Extend
`ExploreURLState` with normalized Operations fields for lane, kind, state,
start-date bounds, and selected opaque run reference. Use the shell's existing
commit/replace/popstate flow so Back, Forward, reload, and shared URLs restore
the same filters and detail selection.

Pagination cursors stay controller-owned and ephemeral. Restoring a selected
opaque run loads detail directly; it does not scan pages to rediscover the row.
A filter change clears pagination and selection in the same committed URL
transition.

### Layout

The workspace is dense and operational:

1. Five lane summary cards: Messages, Facts, Contacts, Documents, Attachments.
2. Filter bar for lane, kind, state, and date range.
3. Paginated newest-first run table.
4. Run detail pane with counters, fixed error, related-status links, and only
   advertised actions.

Each lane card shows configured/history availability and, across its kinds,
the active run, latest run, and last successful run. When a lane contains
multiple kinds, entries remain labeled by kind so the card never implies one
kind's success covers another.

The table shows kind, trigger, state, start time, duration, and compact bounded
counters. Selecting a row commits its opaque reference to URL state and opens
detail. Status uses the existing semantic status components and never relies on
color alone.

On narrow screens, cards and filters stack, the table becomes a compact
row-list, and detail occupies the focused content region. Closing detail
restores focus to the invoking row. Desktop uses the established split-pane
and density conventions from the stacked Directory workspace.

### Cross-links

Operations links out to the existing authority instead of recreating it:

- source sync to Sources;
- document extraction to document index status;
- document embedding to document vector status;
- visual embedding to visual attachment status;
- CardDAV sync to CardDAV Settings;
- configuration issues to the relevant Settings section.

Existing Sources and CardDAV views add an Operations link with an appropriate
kind/lane filter. Links use shell navigation and URL state rather than ad hoc
query strings.

### Explicit UI states

The controller and component distinguish:

- initial loading and background refresh;
- no runs for the current filters;
- configured kind with unavailable history;
- unconfigured kind;
- partial, failed, and cancelled terminal runs;
- queued resumable work and related-status recovery availability;
- stale requests discarded after filters/archive change;
- archive revision or cursor conflict;
- status available while one history adapter is degraded;
- action in progress, action conflict, and action failure.

Partial adapter failure does not blank the entire workspace. Lane cards load
from per-kind status and retain configuration and related-status links.
Unfiltered history merges the exact available adapter set returned by the API;
unavailable kinds stay explicit. If that set or its history revision changes
during pagination, the table shows the typed conflict and restarts from page
one after user confirmation or the existing safe refresh interaction.

## Privacy and Security

The browser and public API must never render or serialize:

- message subjects, bodies, excerpts, or raw archive content;
- names, email addresses, phone numbers, person/source identifiers, or public
  profile URLs;
- filenames, attachment IDs, hashes, paths, media details, or extracted text;
- provider or model names, endpoints, credential names or values, policy
  payloads, request bodies, upstream responses, or raw errors;
- archive UIDs, database IDs, generation fingerprints, or internal lease data.

Opaque run references and cursors are authenticated, encrypted, archive-bound
envelopes.
Error messages are fixed. Counters are aggregate and bounded. New DTOs use
concrete fields with no passthrough maps. Synthetic browser fixtures use only
obviously fake values.

Before publication, inspect the complete diff, generated contracts, browser
fixtures, screenshots, commit message, and pull request body with a private-data
scrub. Do not commit `.superpowers`, generated local agent configuration, or
other local tooling.

## Testing and Verification

### Domain and storage

- Kind/lane/stable-ID/state/error/counter matrices for all nine kinds.
- Begin/checkpoint/finish idempotency and invalid transition rejection.
- `begin` created/active/terminal replay behavior and execution-gate fallback.
- One bounded-pass row across provider batches; enclosing CLI loops and later
  generation resumes create one row per pass.
- Startup recovery for every kind, including preservation of private resumable
  generation/rebuild state.
- Oldest-first person-enrichment queued claims before new scheduled occurrence.
- Native-adapter ordering and lane active/latest/latest-successful projection.
- SQLite/PostgreSQL schema and reader parity, including bytewise text-ID order.
- Mixed-adapter pagination, dynamic adapter-set binding, archive/history
  revision binding, adapter savepoint rollback, date filters, and filter drift
  conflicts.

### Worker and API

- Scheduler and manual invocation integration for every new kind.
- Recorder failures do not corrupt primary work.
- Raw provider/internal errors project only fixed public errors.
- All registered kinds appear in status and complete list/detail history.
- Unsupported actions are absent and cannot be invoked.
- Auth, CSRF, idempotency, stale archive, and revision-conflict behavior.
- Authenticated-encryption token tampering, cross-archive rejection, restart,
  retained-key rotation, and retired-key purge behavior.
- OpenAPI generation and checked-in TypeScript contract parity.

All Go tests use testify. Every direct Go test command includes
`-tags "fts5 sqlite_vec"`.

### Web

- Controller tests for URL normalization, request cancellation, pagination,
  direct detail restoration, partial availability, and action refresh.
- Component tests for all explicit states, keyboard operation, focus return,
  semantic status, and lane aggregation.
- AppShell tests for tab navigation, Back/Forward restoration, and cross-links.
- Browser tests for desktop and narrow layouts, accessibility, action gating,
  archive revision drift, and no sensitive fields in rendered text.
- Synthetic screenshots at the established viewport sizes, inspected before
  publication.

### Final gates

Run focused tests during implementation, then generated API checks, Web unit
and browser checks, formatting/lint, tagged Go tests, documentation checks,
the private-data scrub, and the repository's CI-equivalent remote verification.
The final pull request contains one squashed feature commit on top of the
stacked base and excludes `.superpowers`, local Skillshare state, Roborev state,
and unrelated files.

## Acceptance

The slice is complete when every registered configured kind has truthful
active/latest/latest-successful history or an explicit unavailable state;
every bounded execution pass or native request scope creates at most one stable
durable row while provider batches create none; restart recovery removes false
running state; SQLite and PostgreSQL return equivalent ordered history; filters
and detail restore from the URL; only advertised actions are rendered; and
neither API responses nor browser output expose sensitive or internal data.
