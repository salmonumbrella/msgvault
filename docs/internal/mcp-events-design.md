# MCP Events — Design

Status: proposal, revised 2026-10-05 after a pre-implementation design review
of the first draft. Nothing here is implemented or approved. It describes
native support for the MCP Events webhook profile that ChatGPT shipped on
2026-09-29 ([OpenAI guide](https://developers.openai.com/plugins/build/mcp-events)),
built on the draft Triggers & Events extension. Source facts cite `main` at
`aa411818` plus the open pull requests named inline. Where a decision copies
Inline's shipped implementation (`inline-chat/inline`, read 2026-10-05) the
file is named.

## Summary

An agent host (ChatGPT first) subscribes to one exactly-scoped slice of the
archive — one conversation, one calendar — and is woken by a signed webhook
each time msgvault durably commits an occurrence in that scope: a new message
or reaction, a calendar transition, a managed draft changing, and later a
Kata issue filed from the archive or an attachment whose extracted text or
transcript became readable. Payloads carry identifiers only; the woken run
reads content, including attachments in chunks, with the tools it already
has plus two small read tools this design adds.

The daemon owns everything durable: a commit-ordered journal written inside
the persistence transaction, one subscriptions table with generation fences,
and a delivery service with one worker per active subscription. The
`msgvault mcp --http` process stays a stateless protocol adapter: it
advertises the `events` capability, validates the protocol surface, and
proxies `events/list`, `events/subscribe`, and `events/unsubscribe` to new
daemon routes through `daemonclient`, as it already does for every other
capability.

Work is phased. Phase 1 ships the families whose commit points and read
paths exist today: `msgvault.message_archived`,
`msgvault.calendar_event_changed`, `msgvault.draft_changed`. Phase 2 ships
`msgvault.kata_issue_filed` and `msgvault.attachment_processed` behind
explicit dependencies. The feature is off by default and adds a new egress
class, so it needs the consents listed under [Consent and rollout](#consent-and-rollout).

## Goals

- Wake a subscriber within seconds of the archive commit of an occurrence in
  one conversation or one calendar.
- Never emit for historical backfill, re-sync, label, reindex, attachment
  download, or projection work on existing rows.
- Keep every protocol promise honest: subscriptions survive restarts, event
  IDs and bytes are stable across retries, a cursor never skips an
  unsettled occurrence, and `truncated: true` is returned whenever
  continuity cannot be proven.
- Work identically on SQLite and PostgreSQL.
- Exclude an agent's own replies deterministically where the archive can
  prove authorship, and bound the cost where it cannot.
- Add no new content surface: webhooks carry IDs, timestamps, and flags only.

## Non-goals

- Account-wide or inbox-wide subscriptions, filtered or not.
- Polling, push streaming, `gap`/`terminated` control envelopes, or terminal
  callbacks. ChatGPT supports none of them and they are not advertised.
- An OAuth authorization server inside msgvault, or treating a forwarded
  bearer as a verified subject.
- Sending messages. msgvault never sends (`docs/usage/chat.md` in #1092);
  send-outcome events wait for #666's send path.
- RSVP changes (no normalized attendee projection exists) and Kata issue
  state changes (Kata holds the state and has no feed to msgvault).

## Existing behavior and invariants

What the design relies on, verified on `main` unless a PR is named:

- **Transport.** `internal/mcp/server.go` serves `/mcp` with go-sdk v1.7.0's
  `NewStreamableHTTPHandler` in `Stateless` + `JSONResponse` mode behind
  `bearerAuthHandler`, which captures one static token at construction. The
  inbound token defaults to the daemon's owner API key; `--http-token-file`
  and `--http-token-env` substitute an independent one
  (`cmd/msgvault/cmd/mcp.go`). Protocol `2026-07-28` and `server/discover`
  have been served since #566; `cachePolicyMiddleware` type-switches on
  `*sdkmcp.DiscoverResult` to set `TTLMs` and `CacheScope`. Requests on an
  older protocol lose the write tools. The SDK validates `Mcp-Method` and
  the other `Mcp-*` headers (`-32020`), rejects unsupported versions with
  `-32022`, and provides `AddReceivingCustomMethod`. `ServerCapabilities`
  has no `events` field (go-sdk#1325), so the top-level key must be injected.
- **Process split.** `cmd/msgvault/cmd/mcp.go` builds `ServeOptions` from a
  `daemonclient.Client`, gating each backend on `APISchemaVersionAtLeast`.
  `daemonclient` authenticates the second hop with its configured daemon API
  key or agent token, never with the inbound MCP bearer. The daemon's
  `humaAuthMiddleware` admits API-key, keyless-loopback, browser-session,
  and delegated (`X-Msgvault-Agent-Token`, `AuthModeDelegated`) callers
  (`internal/api/middleware.go`, `routes.go`).
- **Agent grants.** `internal/agentgrant`: `Grant{ID, Label, Permissions
  (draft.create|edit|delete, calendar.*), Sources []SourceRef{Type,
  Identifier, SenderKeys}}`, issued and revoked through
  `/api/v1/agent-tokens`. The registry is in-memory and process-scoped:
  grants do not survive a daemon restart. #1092 (slice 9a of #666) serves
  delegated MCP sessions over stdio only and refuses `--http`; #1023 is a
  competing stacked wiring (`getMCPCapabilities`, typed receipts,
  `--allow-draft-writes`). Both rely on the merged daemon-side grant model.
- **Persistence.** The low-level upsert is `upsertMessageWith`
  (`internal/store/messages.go`), but there is no universal call chain.
  Composite persistence (`persistMessageWith` with `MessagePersistData`)
  writes header, metadata, body, raw, recipients, and final attribution in
  one transaction; the Beeper importer commits the header first and body,
  raw, attachments, mentions, and reactions in later calls
  (`internal/beeper/importer.go`). Final `from_me` attribution can change
  within composite persistence. `UpsertReaction` is not transactional and
  `ReplaceReactions` deletes the whole set before reinserting. The
  `embedding_change_clock` singleton row is the repository's commit-ordering
  pattern (`schema.sql`, `dialect_pg.go`, `embedding_changes.go`).
- **Identity.** `messages.id` and `conversations.id` are integer primary
  keys; conversations are unique per `(source_id, source_conversation_id)`
  with `conversation_type` in `email_thread`, `group_chat`, `direct_chat`,
  `channel`, `calendar`. Reactions are rows in `reactions`, unique per
  `(message_id, participant_id, reaction_type, reaction_value)`, with
  `removed_at`; Beeper and Slack borrow the target message's timestamp for
  embedded reactions.
- **Calendar.** `internal/calsync` stores each Google Calendar event as a
  `messages` row (`message_type = calendar_event`, one source per calendar,
  `sent_at` = event start) and writes status, sequence, `ical_uid`, and
  series linkage to `messages.metadata`; `ingestEvent` persists message,
  metadata, body, raw event, recipients, and FTS in separate calls, and
  `flagCancelled` merges `status: cancelled` into existing metadata while
  preserving the other fields. `gcal.Event` carries `Created` and `Updated`.
  Attendee `responseStatus` is parsed but not persisted. Calendar
  write-through (`internal/calcontrol`) has no sync run. Emailed `.ics`
  invites are ordinary email attachments.
- **Provenance.** `sync_runs.sync_type` distinguishes `full`, `import-*`, and
  incremental runs, but Beeper and Slack runs contain mixed phases under one
  type and `Message` carries no provenance.
- **Drafts** (#880, #882, #1003, #1006, #1008 merged). Four tables share one
  lifecycle (`internal/store/draft_lifecycle.go`): `gmail_drafts`,
  `imap_drafts`, `beeper_drafts`, `chat_drafts` (local Slack/Teams/Discord
  text). Each has a stable local `draft_id`, `revision`, `discarded_at`,
  `pending_operation ∈ {edit, delete}`, and `pending_code` for an uncertain
  provider outcome (`remote_unknown`; CLI receipts add
  `operation_may_have_completed`). Gmail and IMAP drafts are archived
  immediately as `messages` rows (`current_message_id`, `is_from_me`, label
  `DRAFT`) carrying an RFC822 Message-ID. Beeper drafts live in the chat
  composer; `chat_drafts` never reach a provider. No table records who
  created a draft. There is no send path: `draft-send-as` only lists Gmail
  aliases, and the operator sends from their own client.
- **Kata.** The task-link integration (`[integrations.tasks]`,
  `internal/taskclient`, `/api/v1/messages/{id}/tasks`) writes `mail_links`
  metadata onto the Kata task after the remote call and keeps no local
  record; `createOrLinkMessageTask` replies after the remote commit. #1104
  (open) adds `[integrations.kata]`, `POST /api/v1/integrations/kata/issues`
  with an `Idempotency-Key` whose action marker is written into the issue
  metadata and looked up with `FindActionTask` on retry, `…/issues/{ref}/evidence`,
  `…/evidence/prepare` with stable `PassageID`s, and MCP tools behind
  `--allow-kata-writes`. msgvault keeps no local record of the issues.
- **Docbank** (#876, #939, #999 merged; #1077, #1094 open). Document text
  extraction publishes through `PublishDocumentExtraction` (atomic:
  derivatives, `document_extractions.state = ready`, head switch, search
  revision) and fails through `FailDocumentExtraction` (terminal or
  scheduled retry); `document_occurrences` maps attachment → message →
  source by canonical blob hash. Stored audio is routed to Docbank through
  `beeper_media_occurrences` and `beeper_media_deliveries` (`phase ∈
  {pending-artifact, pending-process, observing, done, blocked,
  source_unavailable}`), with `FinishBeeperMediaOperation` as the commit
  point; msgvault stores no transcript text. #1077 adds
  `GET /api/v1/messages/{id}/recordings`, reading live from Docbank; no MCP
  tool exposes it yet. MCP document tools are `search_document_attachments`
  (`message_id`, `attachment_id`, `query`, …), `search_in_message`, and
  `search_person_files`; no tool returns a whole extracted text.
- **Read tools.** `get_message` returns the message and its attachment list
  but neither `source_id`, `is_from_me`, nor calendar metadata
  (`internal/mcp/handlers.go`); `get_attachment {id, offset, length ≤ 4 MiB,
  sha256}` streams chunks (sha256 from the first chunk required for every
  later offset; whole objects up to 50 MiB); `list_thread` resolves a
  conversation from a message ID or a provider `thread_id` + `account`;
  `list_messages {conversation_id, …}` pages one conversation newest-first.
  Calendar control tools take Google calendar IDs, not archive sources.
- **Reusable pieces.** `internal/netguard` (`ProhibitedIP`,
  `ProhibitedHostname`, `ValidateTrustedDestination`: one origin paired with
  its pins), the pinned-IP dialer in `internal/remoteimage/fetch.go` (which
  follows redirects, so it is a pattern, not a drop-in),
  `internal/httpretry.RetryAfter`, HMAC use in `internal/daemonauth`, the
  owner-only key-file creation in `providercredentials`, the media scheduler
  that keeps network work outside the archive gate, and the modern-HTTP
  protocol tests in `internal/mcp/protocol_test.go`. `Scheduler.runJob`
  holds its work tracker — which includes the archive operation gate — for
  a whole job, so a delivery loop cannot be an ordinary scheduler job.

## Architecture and package ownership

```text
ChatGPT ──events/subscribe──▶ msgvault mcp --http ──daemonclient──▶ daemon API
   ▲                           (stateless adapter)                /api/v1/mcp/events/*
   │                                                                     │
   │                      persistence tx ─▶ mcp_event_log ─▶ mcpevents.Service (workers)
   └────────── signed webhook (direct outbound HTTPS from the daemon) ◀───┘
ChatGPT run ──get_mcp_event / get_message / get_attachment──▶ msgvault mcp ──▶ daemon
```

Dependency direction, kept narrow so Store never imports the service:

```text
cmd / api wiring -> mcpevents -> store, netguard, httpretry
internal/mcp -> EventsBackend interface <- daemonclient
store -> archive values and SQL only; never mcpevents, the MCP SDK, or HTTP
```

- `internal/store/mcp_events.go` owns journal append, the clock, subscription
  transitions, pruning, and backend-specific SQL. Journal collection and
  finalization are explicit steps of the shared persistence transaction;
  the transaction returns the appended sequences and the caller issues one
  nonblocking after-commit wake. No Store method opens a second transaction
  after a mutation to append an event.
- `internal/mcpevents` (a few files, not a package per helper) owns
  orchestration, callback transport and signing, retry decisions, secret
  handling, and the one place that canonicalizes closed arguments
  (defaults materialized before hashing, `encoding/json/v2`
  conventions; the daemon revalidates at its trust boundary). It exposes a
  blocking `Run(ctx) error`.
- API handlers call the service; they assemble no transactions. Wire DTOs
  go through the API schema and generated client (`Makefile` generate
  target); protocol adaptation lives in `internal/mcp`.
- Startup passes narrow options (enabled flag, retention, trusted
  callbacks, key path), never the whole config.
- No extraction to `go.kenn.io/kit` now. msgvault has two dialects,
  transaction fences, and daemon auth that a shared engine would have to
  abstract; after two real consumers exist, extract only pure signing and
  verification or a narrowly specified callback transport. Test vectors and
  synthetic conformance fixtures are shared immediately.

## Principal and authorization

v1 is single-owner and the daemon is the only authority:

- Events requires the MCP inbound key to be the daemon's owner API key (the
  default). When `--http-token-file` or `--http-token-env` is set, the MCP
  process serves its existing tools but registers no `events/*` methods and
  advertises no `events` capability. The MCP process forwards no principal;
  the daemon derives it from the credential it authenticated.
- The Events routes require `AuthModeAPIKey` explicitly. Keyless-loopback,
  browser-session, and delegated callers receive `403` from the daemon and
  `-32012` from MCP. Delegated calendar authority is not permission to
  subscribe to the owner's archive.
- `principal_id = "owner:" + hex(SHA-256(owner key))[:16]`, computed by the
  daemon from the key it loaded at startup. Rotating the owner key is a
  daemon restart; after it, a stale MCP process fails with `401` at the
  daemon, every subscription of the old principal fails its next recheck
  and ends with reason `principal_revoked`, and its pending delivery is
  dropped.
- Authorization is rechecked at subscribe and refresh, before every dial,
  and before recording success: the principal matches the current owner
  key, the scope row exists and its source is not removed, the source
  family is still capable, and the feature is still enabled.

Agent principals (`agent:<grant.ID>`) use the same column and vocabulary but
are not served in Phase 1. Prerequisites, each a separate change: a
persisted grant registry (today in-memory), an HTTP delegated session
(neither #1092 nor #1023 provides one; #1092 refuses `--http`), an
`events.subscribe` permission checked with `Grant.Allows` against the
scope's source, and a decision between the #1092 and #1023 wirings. Until
then delegated callers get `-32012`.

A gateway or tunnel that forwards a user bearer does not create a principal:
the static bearer checker rejects a different bearer, and hashing arbitrary
bytes authenticates nothing. A verified OAuth subject needs a trusted
verifier or a signed gateway assertion, which is additional scope.

## Event families

Names are prefixed `msgvault.`. Argument schemas are closed objects; IDs are
decimal strings as in Inline's `catalog.ts`. Every `events/list` description
names the read tools for its payload and says that `get_mcp_event` recovers
a lost payload from the `eventId`.

### Scope: one conversation or one calendar

`conversation_id` is the msgvault `conversations.id`; a subscriber resolves
it with `list_thread` or reads it from any `get_message` / `list_messages`
result. `calendar_source_id` is a calendar's `sources.id`, listed by the new
`list_calendar_sources` tool. Subscribe validates existence, type, and that
the source's family is Events-capable (`-32602`, `data.reason` one of
`unknown_scope`, `wrong_scope_type`, `source_not_capable`). Subject,
participant names, and bare provider IDs are never accepted. Account-wide
scope is rejected: every mailbox message would wake the run, label and
sender filters mean different things per source, and the run can filter
after a scoped wake.

### Phase 1: `msgvault.message_archived`

A message or reaction newly committed in one conversation. "Archived", not
"received": archive time lags provider arrival by the sync interval.

Arguments: `conversation_id` (required), `include_from_me` (default
`false`), `include_reactions` (default `true`).

Payload:

```json
{"kind":"message","message_id":"123456","conversation_id":"7890","source_id":"3",
 "from_me":false,"sent_at":"2026-10-05T09:12:44Z","archived_at":"2026-10-05T09:12:49Z"}
```

```json
{"kind":"reaction","target_message_id":"123456","reaction_key":"👍","reactor_participant_id":"88",
 "conversation_id":"7890","source_id":"3","from_me":false,"reacted_at":"2026-10-05T09:13:02Z"}
```

- `kind: message` is appended once per archived message, at the ready
  boundary below. Edits, labels, read flags, soft deletes, attachment
  downloads, FTS, and projections never emit. A message deleted before
  delivery is still delivered; the read tool reports the deletion.
- `kind: reaction` has no `message_id` because a reaction row is not a
  message. `reaction_key` is the emoji or tapback name (the relay's
  vocabulary). One occurrence per `(target, reactor, type, value)` within
  retention; a removal and re-add inside that window emits nothing again,
  and removals never emit. `reacted_at` is the real reaction time when the
  source gives one, else the observation time; borrowed target timestamps
  are never used.
- `from_me` is the message's final `is_from_me` or, for reactions, whether
  the reactor is a confirmed owner identity of the source (the
  `identity_is_from_me` predicate). Payloads of messages correlated to a
  managed draft add `draft_id` and `draft_actor` (see `draft_changed`).
- `archived_at` is the timestamp the persistence transaction recorded, not
  an exact database commit time.

### Phase 1: `msgvault.calendar_event_changed`

A calendar object created, updated, or cancelled in one synced calendar.

Arguments: `calendar_source_id` (required).

Payload: `kind` (`created` | `updated` | `cancelled`), `message_id`,
`conversation_id` (the series conversation), `source_id`, `ical_uid`,
`sequence`, `starts_at` (RFC 3339, or `YYYY-MM-DD` with `all_day: true`),
`changed_at` (provider `updated` when present, else observation time),
`from_me` (the owner is the organizer; it says nothing about who made this
change, so the own-message filter and guard do not apply to this family).

Entity identity and occurrence identity are separate: the message ID names
the object, each transition is a new journal sequence, and there is no
uniqueness constraint on calendar rows. One Store operation,
`PersistCalendarEvent`, owns the classification after an atomic old/new
comparison under the object's row lock, and calendar rows are excluded from
the generic message producer:

- `created`: no prior row.
- `updated`: prior row exists and at least one of status (other than to
  `cancelled`), start, end, all-day flag, or time zone differs, or
  `sequence` increased. A sequence decrease or reset alone is not an update.
  Identical redelivery (incremental sync, write-through followed by sync)
  emits nothing because the persisted state already equals the new state.
- `cancelled`: status becomes `cancelled`. A cancellation for an object
  never seen produces one `cancelled` occurrence, not `created` plus
  `cancelled`, through the sparse-tombstone path that preserves prior
  details. Cancel → restore → cancel produces three occurrences.
- Full syncs are muted by provenance. RSVP is outside the comparison and
  outside the advertised enum; "invites from or to a person" is a later,
  person-scoped family.

### Phase 1: `msgvault.draft_changed`

A managed draft in one conversation changed. This covers what msgvault can
prove about outbound work today: drafts prepared through the CLI, Web UI, or
MCP draft tools (#1092 or #1023), and the moment a Gmail or IMAP draft turns
into an archived sent message.

Arguments: `conversation_id` (required), `include_actors` (array of
`owner`, `session`, `agent`; default all).

Payload: `kind` (`created` | `updated` | `deleted` | `uncertain` | `sent`),
`draft_id`, `revision`, `draft_kind` (`gmail` | `imap` | `beeper` | `chat`),
`conversation_id`, `source_id`, `message_id` (the archived draft row for
Gmail and IMAP; for `sent`, the archived sent message), `actor`
(`owner:<hash>` | `session` | `agent:<grant id>`), and for `uncertain`
`pending_operation` and `pending_code`.

Commit points are the existing lifecycle transactions, which already run
inside Store (`inTx` in `draft_lifecycle.go`):

| Kind | Gmail / IMAP | Beeper | Local chat |
|---|---|---|---|
| `created` | `PersistGmailDraft`, `PersistIMAPDraft` | `CreateBeeperDraft` | `CreateChatDraft` |
| `updated` | `PublishGmailDraftReplacement`, `PublishIMAPDraftReplacement`, recovery that publishes | `FinishBeeperDraft` (edit) | `UpdateChatDraft` |
| `deleted` | `FinishGmailDraftDelete`, `FinishIMAPDraftRemoval` | `FinishBeeperDraft` (delete) | `DeleteChatDraft` |
| `uncertain` | `RecordGmailDraftOutcome`, `RecordIMAPDraftOutcome` with a `pending_code` | the Beeper equivalent | — |
| `sent` | composite persistence of a `from_me` message whose `rfc822_message_id` equals the draft's | — | — |

Occurrence key: `draft:<draft_id>:<revision>:<kind>` (`uncertain` adds the
code), so every revision transition is a new occurrence and a retried
lifecycle step that changes nothing emits nothing. Drafts are user actions,
always live provenance.

`sent` is emitted only on exact correlation: inside the persistence
transaction of a live `from_me` email, the Store looks up a non-discarded
or recently discarded (≤ retention) managed draft of the same source with
the same `rfc822_message_id`; on a match it appends `draft_changed/sent`
and stamps the `message_archived` row with `draft_id` and `draft_actor`.
Whether Gmail and IMAP clients preserve the Message-ID of a draft they send
is verified per provider with synthetic fixtures before the family is
enabled for that source; without a match the sent message is an ordinary
`from_me` message. Beeper has no message identity for a sent draft (Beeper
`Message` carries only `isSender`), so a composer-emptied-plus-equal-text
match is documented as a heuristic and is not used for `sent` or for
exclusion. Send-outcome kinds (`send_failed`, `send_uncertain`) are not in
the enum: #666's send path must append them in its own transaction when it
exists.

The actor comes from a new `created_by_principal` column on the four draft
tables, set by the daemon from the authenticated mode of the creating call:
`owner:<hash>` for API-key callers (CLI and MCP), `session` for the Web UI,
`agent:<grant id>` for delegated tokens. Read path: `draft_get` (#1092) or,
until it lands, `get_message` on the Gmail/IMAP draft row.

### Phase 2: `msgvault.kata_issue_filed`

An issue filed or evidence added from one conversation through msgvault
(#1104), or a task linked or unlinked through the legacy
`[integrations.tasks]` route. A remote write followed by a local append is
not a transaction, so this family gets the smallest durable action record:

- `mcp_action_records(id, kind, idempotency_key, conversation_id,
  message_id, attachment_id, principal_id, state ∈ {pending, confirmed,
  unknown, abandoned}, issue_ref, project, passage_id, created_at,
  settled_at)`. The handler inserts the record as `pending` before the
  remote call, reusing #1104's idempotency key as the marker.
- After Kata confirms, one transaction marks the record `confirmed` and
  appends the occurrence (key `action:<record id>`). A crash in between
  leaves a `pending` record; the daemon's next maintenance pass resolves it
  through #1104's `FindActionTask` marker lookup (or the task-link reverse
  index for legacy links), then confirms or abandons it. Every link/unlink
  cycle is its own record, so repeats are distinct occurrences.
- Payload: `kind` (`created` | `evidence_added` | `linked` | `unlinked`),
  `issue_ref`, `project`, `message_id`, `attachment_id`, `passage_id`,
  `conversation_id`, `actor`. Read path: the issue lives in Kata; the
  passage is read with `get_message` or `search_document_attachments`.
- Dependencies: #1104 merged; the record table (consent). Issue state
  changes remain a Kata-side events dependency; msgvault observes none.

### Phase 2: `msgvault.attachment_processed`

An attachment of a message in one conversation whose derived content became
readable, or whose processing failed terminally. Use case: a voice note
arrives (`message_archived`), and later its transcript exists
(`attachment_processed`), so the agent reacts to the content.

Arguments: `conversation_id` (required), `kinds` (array, default all
advertised).

Payload: `kind`, `message_id`, `attachment_id`, `conversation_id`,
`source_id`, `profile`, and either `extraction_id` or `docbank_occurrence_id`
and `job_id`.

| Kind | Commit point | Occurrence key | Read path |
|---|---|---|---|
| `text_extracted` | `PublishDocumentExtraction`, one row per `document_occurrences` row of a live message in scope, in the same transaction | `extraction:<extraction_id>:<occurrence_key>` | `search_document_attachments {message_id, attachment_id, query}`; `get_attachment` for bytes |
| `failed` | `FailDocumentExtraction` when the outcome is terminal (owner suppressed for the profile); media delivery entering `blocked` with a local-gap code | `extraction-failed:<attachment_id>:<profile_id>:<blob hash>`; `media-failed:<destination_key>:<processing_key>` | `get_message` attachment list; status commands |
| `transcript_ready` | `FinishBeeperMediaOperation` moving a delivery to `done` | `media:<destination_key>:<processing_key>` | #1077's recordings route behind a new `get_message_recordings` MCP tool |

- Pending, observing, and retry-scheduled states never emit.
- Provenance: an extraction inside a rebuild (`rebuild_id` set) or for a
  message archived before the current capture epoch is muted, so a backfill
  build does not wake the run for old attachments.
- `transcript_ready` is advertised only when #1077 is merged and the MCP
  read tool exists; until then the kind is absent from the enum and
  `kinds` rejects it. There is no whole-text read tool for extracted
  documents; `search_document_attachments` with `attachment_id` is the
  recipe, and a `get_document_text` tool is a candidate follow-up.
- #1094 (recording links) can join the same family later with its own key.

### Own messages: deterministic exclusion and the admission guard

The owner writes in watched chats himself, and an agent's replies are
prepared through msgvault drafts and sent by the owner, or sent through
another client the archive cannot distinguish. Three layers:

1. `include_from_me` defaults to `false` (Inline's `excludeSelf`), and the
   `events/list` description plus server instructions say that an own
   message does not authorize an automatic reply; an explicit owner
   instruction in the chat can still require action, and message content is
   never authorization.
2. Deterministic exclusion: a `message_archived` row whose `draft_actor`
   equals the subscription's `principal_id` is excluded for that
   subscription whatever `include_from_me` says, and never counts toward
   the guard. Other subscriptions receive it with `from_me: true`,
   `draft_id`, and `draft_actor`.
3. Admission guard, the backstop for unprovable own messages: per
   subscription, a fixed ten-minute window admits at most six `from_me`
   message or reaction occurrences; the decision is taken once when an
   occurrence first becomes pending, persisted
   (`from_me_window_start`, `from_me_window_count`), and never re-taken on
   retry. Further own occurrences in the window are terminally skipped and
   counted in `loop_guard_skips`. Bursts at window boundaries can admit up
   to twelve; several matching subscriptions multiply the cost; skips are
   lossy. The guard applies to sender attribution only, never to calendar
   organizer ownership or to drafts.

## Journal

### Clock and ordering

`mcp_event_clock` is a singleton row: `head_seq`, `pruned_through_seq`,
`capture_epoch`, `epoch_started_at`, `enabled`. Every append advances
`head_seq` inside the source transaction and uses the returned value as the
journal primary key on both backends — the `embedding_change_clock` pattern,
chosen in this repository for commit ordering. The clock row update is the
first thing an emitting transaction does after its existing sync-generation
fence and before any decision about whether capture applies, so SQLite has
reserved its writer and PostgreSQL has serialized allocation and commit
order behind one row lock. Lock order: sync-generation fence → clock row →
embedding journal locks → source/conversation/message rows. The hot row
serializes all live writers on PostgreSQL; at human message rates this is
acceptable and measured in the rollout.

The mutation helper returns a reliable outcome: an insert-if-absent inside
the transaction followed by the existing update path reports `inserted`
versus `updated`; a stale prior read is never treated as the insertion
result. Calendar and reaction comparisons run under the same transaction and
row locks. All of this stays in Store.

### Log schema

`mcp_event_log`: `seq` (PK from the clock), `epoch`, `family`, `kind`,
`scope_kind`, `scope_id`, `item_key`, `message_id`, `conversation_id`,
`source_id`, `attachment_id`, `from_me`, `draft_actor`, `occurred_at`,
`recorded_at`, `data` (payload JSON encoded once). No foreign keys and no
cascade: a retained occurrence outlives its message. Indexes:
`(scope_kind, scope_id, seq)`, `(source_id)` for source removal, and a
partial unique index on `(scope_kind, scope_id, item_key)` for
`kind = 'reaction'` and for `family = 'draft'`; messages are deduplicated by
the insert outcome and calendar transitions are intentionally repeatable.

All eligible live occurrences are journaled while Events is enabled,
filtered at delivery. This removes subscriber-dependent capture gaps and the
per-write subscription lookup, at the cost of journaling every live message
for seven days, which the consent list names.

### Provenance and ready boundary

Every emitting mutation receives an explicit immutable
`IngestContext{Mode ∈ {live, backfill, unknown}, ObservedAt}` carried
through each source phase and composite persistence; `unknown` is muted. It
is never inferred from the currently running sync: Beeper and Slack runs
mix history and live phases, calendar write-through has no run, and
reconciliation inside an incremental run is not its live delta. `ScopedToSync`
views keep their generation fence and may carry the context immutably.

The ready boundary is the commit of the core archived message — header,
body, relevant metadata, recipients, final attribution — in one
transaction, with the journal append after those writes. Each source family
becomes Events-capable only when its persistence meets that boundary:
composite `MessagePersistData` sources first (Gmail incremental, IMAP
incremental), calendar after `PersistCalendarEvent` exists (full projection
in one transaction, sparse-cancellation merge under the same transaction,
Google parsing kept in `calsync`), Beeper only after its header-first
persistence is moved onto the composite boundary. Network requests and
media downloads stay outside transactions; attachments are readable only
when already archived, with the existing unavailable/retry behavior, and the
design does not promise every attachment byte at the first wake.

Reactions: the old and new sets are diffed inside one transaction before
replacement; only additions under live provenance emit, and a newly
backfilled target gets a muted baseline. `UpsertReaction` becomes
transactional with the same context.

Event time per family: message `sent_at`; reaction real time or observation
time; calendar provider `updated` or observation time; drafts and Phase 2
families the transaction's observation time. No age cap is applied:
provenance is the rule.

### Retention, epochs, and continuity

- Retention is seven days, hard. Pruning deletes rows older than that and
  advances `pruned_through_seq` in the same transaction, even when it
  deletes the final row, so the boundary survives an empty log.
- Cursors are bound to `(subscription_id, epoch, seq)`. A cursor from an
  older epoch, or with `seq < pruned_through_seq`, truncates: the
  subscription restarts at `head_seq` and the result says `truncated: true`.
  A conservative global boundary may report an unnecessary gap; it never
  conceals one.
- Disabling capture ends the epoch (`capture_epoch + 1`); ordinary daemon
  restarts do not.
- An active subscription whose settled cursor falls below the boundary is
  stopped with reason `retention`, its cursor and reason preserved; the next
  refresh returns `truncated: true` and the head. The worker never skips to
  the head on its own.
- Source removal deletes that source's log rows and ends subscriptions in
  its scope with reason `scope_removed`. Expired and revoked subscriptions
  are purged after the 24-hour read grace.

## Subscriptions and cursors

### State machine

All transitions are Store commands with private transactions, illustratively
`ActivateMCPSubscription`, `PrepareMCPDelivery`, `FinishMCPDelivery`,
`EndMCPSubscription`, each fenced by a `generation` on the subscription row.

`events/subscribe`:

1. Validate arguments against the closed schema (`-32602`); require
   `delivery.mode: "webhook"` (`-32014`), an `https` URL on port 443 or
   8443 with no credentials or fragment, a `whsec_` secret decoding to 24–64
   bytes; authorize the principal and scope.
2. Prepare a candidate `(subscription_id, secret_revision)`. The ID is
   `sub_` + SHA-256 over length-prefixed `(principal, name, canonical
   arguments, url)`.
3. If no durable verification exists for exactly that subscription and
   secret revision, challenge the callback outside any transaction: POST
   `{"type":"verification","challenge":<32 random bytes, base64url>}` with
   `webhook-id: msg_verification_<random>`, the Standard Webhooks headers,
   and `X-MCP-Subscription-Id`; require 2xx, a body of at most 4 KiB, and a
   constant-time match within 10 s. Failure returns `-32015` with
   `data.reason` in `connection_refused`, `timeout`, `tls_error`,
   `http_4xx`, `http_5xx`, `challenge_failed`, and leaves any existing
   subscription and its secrets untouched. There is no cross-subscription
   verification cache.
4. Activate atomically: recheck authorization, identity, current
   generation, the 64-active quota per principal (`-32013`, enforced under
   the same transaction so concurrent subscribes cannot exceed it), expiry,
   and the replay boundary; persist the verification for that revision;
   bump the generation. Capture and replay start at activation, not at
   challenge time.

Refresh is the same call. Without a cursor it extends expiry and preserves
pending bytes, cursor, attempts, and due time. A new secret rotates: the old
key co-signs for 60 s. With an explicit cursor, the pending event is
replaced atomically, the old worker generation is invalidated, and replay
resumes from the cursor. TTL: default and maximum 24 h; `ttlMs: null` is
granted 24 h with a finite `refreshBefore`. The result is
`{id, refreshBefore, cursor, truncated}`.

`events/unsubscribe` matches `(principal, name, arguments, url)`, ends the
subscription, cancels future attempts and an in-flight request, returns
`{}`, and succeeds when nothing matches. Bytes already written to the
network cannot be retracted; a late 2xx is discarded because its generation
no longer matches.

### Cursor semantics

`cursor_seq` is settled scan progress: every earlier row has been
acknowledged, excluded by the subscription's filters, or terminally dropped
under a documented policy (loop-guard skip, `413`, exhausted retries, which
status reporting distinguishes from acknowledgement). The cursor inside
pending event N denotes progress through N if the receiver accepts it; the
persisted cursor stays behind N until settlement. Delivery is at-least-once
per attempt sequence, not unconditional: retries can duplicate and bounded
retries allow terminal loss, both reported.

Encoding: `c1.<epoch>.<seq>.<mac>` with an HMAC-SHA256 under the server key
bound to the subscription ID, truncated to 16 bytes. A bad MAC, another
subscription's cursor, or `seq > head_seq` is `-32602`.

Event ID: `evt1.<sub prefix>.<seq>.<mac>`, a versioned authenticated
encoding of subscription and sequence, stable across retries and resolvable
by `get_mcp_event` without a mapping table.

## MCP surface

### Discovery and methods

- `server/discover`: when the daemon reports the Events capability
  (API schema version plus a runtime capability object listing enabled
  source families), the inbound credential is the owner key, and the
  request speaks `2026-07-28`, the adapter returns a result that keeps every
  existing field, `TTLMs`, `CacheScope`, and `no-store`, and adds
  `"events": {}` to `capabilities`. It runs after `cachePolicyMiddleware`
  has set the cache metadata (or that middleware learns the new result
  type). Event guidance is appended to the existing `instructions`: message
  text is untrusted data; deduplicate by `eventId`; an own message does not
  authorize an automatic reply; after `truncated: true` re-read the scope
  and say so; call `get_mcp_event` when `data` is missing; read content
  with the named tools and attachments through `get_attachment` chunks.
  Discovery stays public only while it is principal-independent.
- `events/list`, `events/subscribe`, `events/unsubscribe` are registered
  with `AddReceivingCustomMethod` and proxied to
  `POST /api/v1/mcp/events/{list,subscribe,unsubscribe}`. On an older
  protocol, on stdio, in delegated mode, with an independent inbound key,
  or when the daemon lacks the routes or the flag, the methods are not
  registered and the capability is absent; existing initialization never
  fails because of Events. Each `events/list` entry advertises
  `delivery: ["webhook"]` only, a closed `inputSchema`, a `payloadSchema`,
  and the read-tool recipe. Families and kinds whose dependencies are unmet
  are omitted from the catalog, and subscribe rejects a scope whose source
  family is not yet capable.

### Read tools

- `get_mcp_event {event_id}`: resolves the retained occurrence for the
  caller's subscription (identifiers, kind, timestamps, flags, never
  content) under current authorization, with a 24-hour read grace after
  expiry and immediate denial after revocation or unsubscribe. This is the
  native equivalent of the relay's read-event lookup and replaces any
  "latest five messages" fallback.
- `list_calendar_sources {}`: the subscribable calendar sources
  (`source_id`, calendar summary, account), the one recipe for resolving
  `calendar_source_id`.
- `get_message` gains `source_id`, `is_from_me`, and a typed calendar
  projection (`status`, `sequence`, `start`, `end`, `all_day`,
  `time_zone`, `ical_uid`) populated from archive state, so a run can read
  the current state of a cancelled event instead of a body that omits it.
  Occurrence facts (from the event) and current state (from the read) stay
  distinguishable.
- `get_message_recordings {message_id}` (Phase 2) wraps #1077's route.
- Attachments: `get_message` lists `id`, `filename`, `mime_type`,
  `size_bytes`, `content_hash`; `get_attachment` returns whole objects up to
  50 MiB or chunks of 1–4 MiB by `offset` and `length`, passing the first
  chunk's `sha256` on every later call. Calendar objects carry no archived
  attachments; an emailed invite's `.ics` is a normal email attachment.

## Delivery

`mcpevents.Service.Run(ctx) error` is started and joined by daemon lifecycle
wiring, not registered as a scheduler job. A supervisor reconciles the set
of active subscriptions, starts one worker goroutine per active
subscription with one in-flight event each, cancels workers whose
generation ended, and runs a bounded maintenance pass (expiry sweep every
minute, pruning hourly, Phase 2 action-record reconciliation). Workers drain
ready occurrences until a retry deadline, expiry, or an empty queue, then
wait on a coalesced wake or timer; one subscriber's I/O never stalls
another. Wakes are nonblocking hints sent after a successful outer commit;
startup and the one-second reconciliation scan recover lost hints. Callback
verification and delivery run outside Store transactions and outside the
archive operation gate, which is taken only around the short Store steps.
Workers are cancelled and joined before Store closes. The runbook preserves
the single-daemon-per-archive invariant that makes leases unnecessary.

Per occurrence:

1. `PrepareMCPDelivery`: recheck authorization, read the next journal row
   after `cursor_seq` in scope, apply the filters and the exclusion and
   admission policies (settling skipped rows), build the envelope
   `{eventId, name, timestamp, data, cursor}` with `timestamp` = the
   family's event time, and persist its bytes as the pending event with the
   current generation. Every retry sends identical bytes.
2. POST at most 262144 bytes with `Content-Type: application/json`,
   `webhook-id` = `eventId`, `webhook-timestamp` in Unix seconds,
   `webhook-signature` = `v1,` + base64 HMAC-SHA256 over
   `id.timestamp.body` under the decoded `whsec_` key (two space-separated
   signatures for 60 s after rotation), and `X-MCP-Subscription-Id`.
   Receipt is decided from the status line: any 2xx is success.
3. `FinishMCPDelivery` applies only when generation and pending sequence
   still match; a stale result is discarded. Success: recheck
   authorization, settle the cursor, clear the pending event, reset
   attempts. `410`: end the subscription (`gone`). `413`: terminal drop of
   that event (dead-lettered). Otherwise retry with
   `min(15m, 1s × 2^(attempt-1))` plus jitter, honoring `Retry-After` on
   429 and 503 through `httpretry.RetryAfter` capped at 1 h. Twelve total
   attempts including the first; on the twelfth failure the event is
   dead-lettered (`mcp_event_dead_letters`: subscription, seq, attempts,
   last status class) and the subscription continues.

Outbound transport: a dedicated webhook client with `Proxy: nil`; HTTPS
only; DNS resolved by msgvault with every resolved address required to pass
`netguard.ProhibitedIP`; the connection dials the validated address with the
original hostname as TLS server name; redirects refused; bounded response
reads with bodies closed; 10 s per request. Private receivers are an
operator exception: `[mcp.events] trusted_callbacks = [{origin, addresses}]`
repeated objects validated with `netguard.ValidateTrustedDestination`,
matched by the complete normalized origin including port before its pins
are selected; the 443/8443 port policy still applies. Tests inject a
resolver, dialer, or transport for `httptest` receivers without widening
production policy.

Secrets: the `whsec_` value and its predecessor are stored AES-256-GCM
encrypted under exactly 32 key bytes read from `<data_dir>/mcp-events.key`,
created with the owner-only file pattern only when Events is first enabled,
with a fresh nonce per write and the subscription ID and secret role as
associated data. If encrypted subscription state exists and the key is
missing or corrupt, Events fails closed and the status command says so; no
replacement key is generated silently. Restoring subscriptions needs the
matching key, and an archive clone must not resume callbacks automatically.

Failures are classified at their boundary. Logs carry allowlisted fields
(subscription ID, scope, seq, attempt, status class, error class); raw
`url.Error` values, callback URLs, secrets, and signatures never reach
generic logs or protocol responses, including the Store's driver-error and
rollback log sites, which the tests probe with synthetic markers.

`msgvault mcp events status` lists the caller's subscriptions with state and
stop reason, `refreshBefore`, settled cursor, pending attempt, last
delivery outcome, dead-letter and loop-guard counts.

## Reaching the server from ChatGPT

Experimental until demonstrated. ChatGPT connects from OpenAI's cloud and
needs OAuth; neither route below has a verified auth bridge or event
forwarding.

- **Nesting MCP gateway.** Must forward `server/discover` with the top-level
  `events` capability and `events/*` with `Mcp-Method` intact, and present
  the owner key to msgvault. The gateway in use today does not forward
  `events/*`; its vendor has said support is planned. Users collapsed onto
  the owner key share one principal.
- **OpenAI Secure MCP Tunnel.** Forwards raw JSON-RPC and the connector's
  bearer unchanged; whether the hosted end accepts `events/*` for a
  tunnel-backed plugin is undocumented, and a forwarded ChatGPT bearer is
  rejected by the static checker. Deliveries never traverse the tunnel.

Native protocol conformance ships first and is verifiable directly against
`/mcp` with the owner key and a self-hosted receiver allowed through
`trusted_callbacks`. A usable ChatGPT connection is claimed only after its
auth bridge and forwarding are demonstrated.

## Schema, configuration, consent, and rollout

New tables in both `internal/store/schema.sql` and `schema_pg.sql`
(`CREATE TABLE IF NOT EXISTS`, applied by `InitSchemaContext`; no ledger
migration, since no existing row is rewritten):

- `mcp_event_clock` (singleton), `mcp_event_log`, `mcp_event_subscriptions`,
  `mcp_event_dead_letters`; Phase 2 adds `mcp_action_records`.
- `mcp_event_subscriptions`: `id`, `principal_id`, `name`, `arguments`,
  `scope_kind`, `scope_id`, `callback_url`, `secret_enc`,
  `previous_secret_enc`, `previous_secret_until`, `secret_revision`,
  `verified_revision`, `verified_at`, `generation`, `state` (`active`,
  `expired`, `unsubscribed`, `gone`, `stopped`, with a `CHECK`),
  `stop_reason`, `expires_at`, `cursor_epoch`, `cursor_seq`,
  `pending_seq`, `pending_envelope`, `pending_generation`, `attempt_count`,
  `next_attempt_at`, `from_me_window_start`, `from_me_window_count`,
  `loop_guard_skips`, `dead_letter_count`, `created_at`, `updated_at`, with
  `CHECK`s tying the pending columns together. Indexes: active rows by
  `(scope_kind, scope_id)`, due rows by `(state, next_attempt_at)`,
  dead-letter cleanup by `failed_at`.
- Column additions: `created_by_principal` on `gmail_drafts`, `imap_drafts`,
  `beeper_drafts`, `chat_drafts` (`ADD COLUMN`, legacy-migration style, NULL
  for existing rows).

Daemon API: the three Events routes, `get_mcp_event`,
`list_calendar_sources`, status, the `get_message` projection fields, and a
runtime capability object; API schema version bump and generated client
regenerated through the existing workflow. go-sdk stays at v1.7.0.

```toml
[mcp.events]
enabled = false            # off: no capture reads or locks, no key, no worker, no DNS, no egress
retention = "168h"
sources = ["gmail", "imap"]  # families that passed the ready-boundary gate on this build
trusted_callbacks = []     # [{ origin = "https://receiver.example.net:8443", addresses = ["10.0.0.5"] }]
```

Consent, each recorded in the PR that makes it:

1. The new tables and indexes on both backends, the clock row touched by
   every live mutation, and the journal's coverage of every live message
   for seven days.
2. A new egress class: the daemon POSTs to caller-supplied public HTTPS
   URLs, opt-in by `[mcp.events] enabled`, documented on the configuration
   and security pages.
3. The key file and encrypted secrets at rest.
4. `IngestContext` plumbing through source phases and composite persistence,
   per source family; the Beeper persistence-boundary correction; the
   `PersistCalendarEvent` and reaction-diff refactors.
5. `created_by_principal` on the four draft tables.
6. The daemon API additions and read-projection changes.
7. Phase 2: `mcp_action_records`, the Docbank hook points, and the
   `get_message_recordings` tool.

Rollout: schema and clock with the flag off; Store commands and the
service; MCP adapter and discovery; Gmail and IMAP incremental as the first
capable families; drafts; calendar; the remaining proven sources and
Beeper after its boundary fix; Phase 2 families as their dependencies
merge. Documentation lands with each step (`docs/usage/chat.md`,
`docs/configuration.md`, `docs/api-server.md`).

## Testing

Synthetic only: real importer code with synthetic provider responses,
synthetic names and reserved example addresses, `httptest` TLS receivers
with injected resolver and transport, no live provider, no real ChatGPT, no
private content. testify with `(want, got)`, `make test` (`fts5 sqlite_vec`
tags) and `make test-pg` with an explicit database. Races use barriers and
channels with deadlines that bound completion; pure retry, TTL, and state
logic uses virtual time and an in-memory transport; real PostgreSQL and TLS
never run inside a fake-time bubble. Property tests cover cursor and
state-machine transitions and canonical argument identity; signature
reference vectors from the Standard Webhooks repository are the oracle;
malformed cursor, secret, and argument fuzz targets are bounded.

| Area | Required observable result |
|---|---|
| Journal ordering | Two PostgreSQL writers, activation, rollback, and pruning forced through controlled interleavings; no cursor skips a later-committing lower sequence; no false `created`. Repeated on SQLite for granular and composite entry points. |
| Replay continuity | Empty retained log, cursor just before the first retained scope row, disable/re-enable (epoch), continuously refreshed slow subscriber; truncation stays honest and retention bounded. |
| Source provenance | Full recovery, mixed incremental/backfill phases, new reaction on an old target, replacement of an unchanged reaction set, calendar write-through followed by sync redelivery. |
| Archive readiness | Pause after header persistence and inject a later body/metadata failure; no premature notification; a delivered occurrence reads the committed body and the typed calendar status. |
| Calendar transitions | Two updates of one object, unchanged sequence with changed time, cancel/restore/cancel, sparse tombstone, all-day dates, concurrent changes. |
| Drafts | Each lifecycle transition emits once per revision; uncertain outcome carries its code; Message-ID correlation per provider fixture; the creating principal's subscription never receives its own sent reply while another does; Beeper heuristic documented, not asserted. |
| Subscription lifecycle | Failed rotation preserves old state; two concurrent rotations and activations; quota race; replay during pending send; late success cannot resurrect or overwrite a newer generation. |
| Delivery | One blocked receiver while a healthy one progresses; coalesced and lost wakes; restart preserves pending bytes and due time; 2xx, 429/503 with `Retry-After`, 5xx, timeout, 410, 413, twelfth failure; shutdown joins workers before Store closes. |
| Security and reads | The real MCP → daemonclient → daemon path with an independent inbound key, no daemon key, a delegated token, a forged principal field, and owner-key rotation during a pending delivery; `get_mcp_event` for a lost payload, wrong principal, revocation; trusted-callback origin/pin pairing; private IPv4/IPv6, mapped and transition addresses, DNS rebinding, ports other than 443/8443, redirects refused; callback error redaction through the Store logger; missing or corrupt restored key fails closed. |
| Phase 2 | Action record pending → confirmed and the crash-then-reconcile path; extraction publish inside and outside a rebuild; terminal versus retried failure; media delivery reaching `done`. |
| Compatibility | Old daemon, flag off, 2025-06-18 HTTP, 2026-07-28 HTTP, unsupported version, stdio, delegated mode, independent inbound key; existing tools, instructions, cache metadata, and `no-store` unchanged; `events/list` validates against a pinned fixture of the ChatGPT profile in `testdata/mcp/`. |

## Adopted from Inline

| Inline decision (file) | msgvault |
|---|---|
| Cursor over an existing durable journal, no outbox (`source.ts`, `worker.ts`) | Same model over a new commit-ordered `mcp_event_log` of all live occurrences, since no existing journal covers messages, reactions, calendar, and drafts |
| One subscriptions table: cursor, one pending occurrence, attempts, both secrets, generation fence (`server/drizzle/0153_mcp-events.sql`, `repository.ts`) | Adopted, including the generation fence; a clock row and a dead-letter ledger added; no leases (one daemon per archive) |
| Reference-only payloads, read tool named in the description (`catalog.ts`, `source.ts` `referenceData`) | Adopted; `get_mcp_event` added for lost payloads |
| Per-scope events only (`catalog.ts` `parseSelector`) | Adopted |
| `excludeSelf` on message events (`catalog.ts`) | `include_from_me` default false, plus deterministic draft-based exclusion and a fixed-window admission guard Inline lacks |
| Reactions excluded (`server/docs/mcp-events.md`) | Differs: reactions are durable rows here and emit as `kind: reaction` |
| Retry `1s × 2^n` capped 15 min, `Retry-After` on 429/503, 12 attempts, receipt by status (`worker.ts`) | Adopted; 410 ends the subscription per the OpenAI guide; 413 drops one event |
| Re-authorize before connect and before acknowledge (`worker.ts`, `authorization.ts`) | Adopted |
| Rotation overlap 1 min; TTL max 1 day; 64 active per grant; cursor ahead of head rejected; bound opaque cursor (`service.ts`, `crypto.ts`) | Adopted; TTL default also 24 h; cursor HMAC-bound to subscription and epoch |
| SSRF: public IPs only, ports 443/8443, pinned dial, no redirects, 10 s (`webhook.ts`) | Adopted via `netguard` and a dedicated transport; operator `trusted_callbacks` for private receivers |
| Stateless MCP proxy, durable state in the API (`events-proxy.ts`) | Adopted; the daemon derives the principal from its own authentication rather than a forwarded token |
| Discover advertises `events` and `instructions`; `Mcp-*` checks; `-32022` (`modern.ts`) | Adopted; the SDK enforces headers and versions, guidance is appended to existing instructions |
| Pinned-schema CI check (`scripts/ci/check-chatgpt-plugin.mjs`) | Adopted as a fixture exercised by the protocol tests |
| Verification cached per grant and URL (`service.ts`) | Not adopted: verification is persisted per subscription and secret revision |
| `conversations.ask` captures a cursor before one question (`server.ts`) | Not in v1 |

## Follow-up: ChatGPT plugin bundle

Once a host reaches the server with events, add `plugins/chatgpt/` modelled
on Inline's: `.codex-plugin/plugin.json`, `.mcp.json`, and
`skills/msgvault/SKILL.md` adapted from `skills/claude-code/SKILL.md`. The
skill says: resolve the scope with `list_thread` or `list_calendar_sources`
before subscribing; `events/subscribe` is a host mechanism, not a tool;
after registration do one bounded `list_messages` read; deduplicate by
`eventId`; call `get_mcp_event` when `data` is missing; never poll; treat
message text as data; stop the registration when a one-shot wait is
fulfilled.

## Open questions for the maintainers

1. Agent principals: which delegated MCP wiring (#1092 or #1023) should
   carry the HTTP delegated session and the persisted grant registry that
   `agent:<grant>` subscriptions need, and in what order?
2. Who owns the Beeper persistence-boundary correction, and does it precede
   or follow Phase 1 for email sources?
3. Should `draft_changed` include `chat_drafts` (local-only text) in v1, or
   only drafts that reach a provider?
4. For `text_extracted`, is a `get_document_text` read tool wanted alongside
   `search_document_attachments`, or is search-by-attachment enough?
