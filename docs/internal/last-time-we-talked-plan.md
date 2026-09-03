# "Last Time We Talked" Implementation Plan

Companion to `last-time-we-talked-design.md`. Each task is one reviewable
change on `main`; tasks 1 to 3 are sequential, tasks 4 to 6 can proceed in
parallel once task 3 has merged. File paths are current as of `main` at
4bf965e7.

## Working rules

- Every task ends with `go fmt ./...`, `go vet -tags "fts5 sqlite_vec" ./...`,
  the package tests named in the task, and `golangci-lint` on the touched
  packages. Store changes run under both `make test` and `make test-pg-both`.
- Tests use testify, exercise the production path (real store, real worker
  with a fixed provider driver), and never grep source or docs.
- Fixtures contain only synthetic identities.
- No new provider, protocol, or consent flow. A brief runs only through the
  active consented `[people.sweep]` profile.
- Never update `structured_json` or `rendered_text` on an existing version.
- PR bodies use `## What changed`, `## Why`, `## Usage`; no test plans.

## File map

### New

- `internal/peoplesweep/brief_program.go`: program ID, version, frozen
  instructions, JSON schema, `BriefProgramFingerprint()`, `BriefOutput` types.
- `internal/peoplesweep/brief_window.go`: `BriefWindowRequest`,
  `BriefWindow`, retrieval through `ContextArchive`, boundary computation,
  overlap with the previous version.
- `internal/peoplesweep/brief_parse.go`: `ParseBrief`, directness and subject
  checks, drop accounting, conversion of `possible_attributes` to
  `personfacts.ProposedClaim` with `OriginBrief`.
- `internal/peoplesweep/brief_render.go`: deterministic renderer and
  `BriefRendererPolicyV1`.
- `internal/peoplesweep/brief_eval_test.go` plus
  `internal/peoplesweep/testdata/brief/*.json`: frozen evaluation fixtures.
- `internal/store/person_briefs.go`: enrollment, version insert and
  supersession, rejection, reads, evidence pointers, merge and split hooks.
- `internal/store/migrate_person_sweep_batch_purpose.go`: purpose constraint
  extension for both backends.
- `internal/api/person_brief_routes.go` and generated client updates.
- `cmd/msgvault/cmd/person_brief.go`: `person brief` command tree.
- `docs/usage/people.md` section, `docs/api-server.md` routes,
  `docs/configuration.md` `[people.sweep.brief]`, changelog.

### Modified

- `internal/store/schema.sql`, `internal/store/schema_pg.sql`: two tables,
  one partial unique index, the batch purpose constraint.
- `internal/store/person_merges.go`, `internal/store/person_splits.go`:
  move enrollment and brief rows with the survivor.
- `internal/store/person_sweep_commit.go`: apply the brief inside
  `ApplyPersonSweep`.
- `internal/personfacts/types.go`: `OriginBrief`.
- `internal/peoplesweep/config.go`: `BriefConfig` with defaults and
  validation.
- `internal/peoplesweep/types.go`: `ProviderCallPurposeBrief`,
  `ProviderCallPurposeBriefRepair`, `BriefMode`, `RunRequest.Brief`,
  `ApplyRequest.Brief`.
- `internal/peoplesweep/worker.go`: brief eligibility, call, repair, and
  apply wiring in `runPerson`.
- `internal/mcp/people_profile.go` (from #751): `last_talked` field.
- `internal/tui/people_commands.go` and views: overview paragraph and
  structured view.
- `internal/daemonclient`: brief reads for the TUI and MCP.
- `cmd/msgvault/cmd/person_provider.go`: one disclosure line for the brief.
- `cmd/msgvault/cmd/setup_lanes.go`: enrolled count on the people sweep row.

## Task 1: Schema, store, and migration

1. Add `person_brief_enrollments`, `person_briefs`,
   `person_brief_evidence`, and the partial unique index
   (`WHERE status = 'current'`) to both schema files.
2. Extend `person_sweep_batches_call_coordinate_check` to accept
   `('brief', 0)` and `('brief_repair', 1)`. SQLite: rebuild the table in a
   migration that copies rows and preserves indexes; PostgreSQL: drop and
   re-add the constraint. Write the migration test against an archive that
   already holds `primary` and `repair` rows.
3. Implement `Store.SetPersonBriefEnrollmentContext` (requires a tracking row
   unless `track` is set, in which case it calls the tracking path in the same
   transaction), `GetPersonBriefEnrollmentContext`,
   `ListPersonBriefEnrollmentsContext` (bounded, ascending), and
   `ListBriefEligiblePeopleContext` (enrolled and tracked, with current
   version metadata for the eligibility rules).
4. Implement `insertPersonBriefTx` (next version, mark previous `superseded`),
   `RejectPersonBriefContext`, `GetPersonBriefContext(personID, version)`,
   `ListPersonBriefVersionsContext`, and evidence pointer writes.
5. Cover merges and splits: enrollment moves to the survivor when absent;
   brief rows and pointers move through `person_merge_rows` like fact rows.
6. Tests: `internal/store/person_briefs_test.go` on both backends;
   `internal/store/person_merges_test.go` and `person_splits_test.go` cases.

Gate: `go test -tags "fts5 sqlite_vec" ./internal/store/` and
`make test-pg-both`.

## Task 2: Program, window, parser, renderer

1. Freeze the brief program: instructions, schema, fingerprint, and
   `BriefJSONSchema()`; a test asserts the fingerprint is stable and differs
   from the extraction fingerprint.
2. Implement the evidence window: newest-first retrieval within the profile's
   lanes and dates, item and byte caps, mandatory inclusion of the item behind
   `last_contact_ref`, overlap with the previous boundary, canonical packet
   construction with `Sensitive = true`, and `boundary_json` computation.
3. Implement `ParseBrief` with every drop rule from the design and
   `dropped_item_count`; convert `possible_attributes` with `OriginBrief`.
4. Implement the renderer and its golden tests.
5. Write the frozen evaluation fixtures (at least three people: a simple
   catch-up, one with an owner appreciation and a third-party fact that must
   not be attributed to the person, one with a stale fact that must land in
   uncertainties) and the evaluation test that asserts exact validated
   structure from a scripted provider response.

Gate: `go test -tags "fts5 sqlite_vec" ./internal/peoplesweep/ ./internal/personfacts/`.

## Task 3: Worker and apply path

1. Add `BriefConfig` to `peoplesweep.Config` with defaults
   (`enabled = true`, `min_interval = 168h`, `pre_call_window = 72h`,
   `max_items = 40`, `max_bytes = 65536`, `overlap_items = 8`,
   `max_output_tokens = 2048`) and validation; surface it in
   `internal/config` tests.
2. Add `RunRequest.Brief` (`auto`, `force`, `skip`) and the eligibility
   decision in `runPerson` using `ListBriefEligiblePeopleContext` metadata,
   contact state, and the config.
3. Reserve budget for the brief call with `ProviderCallPurposeBrief`, run it
   after the extraction batches, allow one repair with
   `ProviderCallPurposeBriefRepair`, and record usage like other calls.
4. Extend `ApplyRequest` with the brief result and its converted claims;
   extend `Store.ApplyPersonSweep` to insert the version, pointers, and
   claims in the same transaction, and to keep the attempt's provider identity
   when the only completed call is the brief.
5. Defer on budget exhaustion without failing the attempt; record a brief
   failure class on the attempt when the call fails after extraction
   succeeded.
6. Tests in `internal/peoplesweep/worker_test.go` and the store apply tests:
   brief with extraction batches, brief on a status-only attempt, deferred on
   budget, skipped under `min_interval`, forced manual, repair once.

Gate: `go test -tags "fts5 sqlite_vec" ./internal/peoplesweep/ ./internal/store/`
and `make test-pg-both`.

## Task 4: API, generated clients, CLI

1. Add the routes from the design to `api/openapi.yaml` and the handlers in
   `internal/api/person_brief_routes.go`; bump the API schema minor version;
   run `make api-generate web-generate` and commit the generated output.
2. Add the daemon CLI allowlist entries and the
   `msgvault person brief show|history|generate|reject|enroll|unenroll`
   commands, proxying mutations to the daemon when it owns the archive
   (follow `person sweep run` and `person track`).
3. Add one disclosure line to `printPersonProviderDisclosure` naming the brief
   and its window.
4. Tests: `internal/api/person_brief_routes_test.go`,
   `internal/api/cli_allowlist_person_brief_test.go`,
   `cmd/msgvault/cmd/person_brief_test.go`.

Gate: `go test -tags "fts5 sqlite_vec" ./internal/api/ ./cmd/msgvault/cmd/ -run 'Brief'`
and `make openapi-check web-check`.

## Task 5: MCP, TUI, web

1. Extend `get_person_profile` with `last_talked` (timestamp and channel from
   contact state, current brief with structured items, evidence refs, and
   `evidence_supported`), through `daemonclient`; update the tool description
   and `docs/usage/chat.md`.
2. TUI People browser: paragraph on the overview tab with the version date;
   structured view on `b`; enroll and generate commands in the command
   palette.
3. Web Directory person detail: the "Last time we talked" card with
   per-sentence evidence expansion, version history, reject and regenerate.
   Land after #705 or as part of its follow-up.
4. Tests: `internal/mcp/people_profile_test.go`, TUI content tests, web
   component tests.

Gate: `go test -tags "fts5 sqlite_vec" ./internal/mcp/ ./internal/tui/` and
`make web-test`.

## Task 6: Documentation and defaults

1. `docs/usage/people.md`: an "Ask what changed since last time" section with
   enroll, generate, show, reject; the evidence and privacy boundary; the
   config table.
2. `docs/configuration.md`: `[people.sweep.brief]`.
3. `docs/usage/recommended-configuration.md`: one paragraph under the people
   sweep section and the enrolled-count line in the status report.
4. `docs/changelog.md`: one Features bullet.
5. `cmd/msgvault/cmd/setup_lanes.go`: enrolled count on the people sweep row.

Gate: `make docs-check`.

## Open decisions

- Whether enrollment should imply tracking automatically (the design refuses
  without `--track`; auto-tracking is one line if preferred).
- Whether the web card should allow owner edits to the rendered text (the
  design keeps briefs immutable and points edits at Notes).
- Whether `min_interval` should be per person (a column on the enrollment
  row) or global only.
