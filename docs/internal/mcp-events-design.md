# MCP Events — Design

Status: proposal, 2026-10-05. Nothing here is implemented or approved. It
describes native support for the MCP Events webhook profile that ChatGPT
shipped on 2026-09-29 ([OpenAI guide](https://developers.openai.com/plugins/build/mcp-events)),
built on the draft Triggers & Events extension. Source facts cite `main` at
`aa411818`. Where this design copies a decision from Inline's shipped
implementation (`inline-chat/inline`, read 2026-10-05) the file is named.

## Summary

An agent host (ChatGPT first) subscribes to one exactly-scoped slice of the
archive — one conversation, one calendar — and is woken by a signed webhook
each time msgvault durably commits a new item in that scope. Payloads carry
identifiers only; the woken run reads content, including attachments, with the
tools it already has (`get_message`, `list_thread`, `list_messages`,
`get_attachment`).

The daemon owns everything durable: a commit-time change log written in the
same transaction as ingestion, one subscriptions table, and the delivery
worker. The `msgvault mcp --http` process stays stateless: it advertises the
`events` capability, validates the protocol surface, and proxies
`events/list`, `events/subscribe`, and `events/unsubscribe` to new daemon
routes, exactly as it already proxies every other capability through
`daemonclient`.

The feature is off by default and adds a new egress class (outbound HTTPS
from the daemon to a caller-supplied public URL), so it needs the consents
listed under [Consent and rollout](#consent-and-rollout).

## Goals

- Wake a subscriber within seconds of the archive commit for a new message
  or reaction in one conversation, or a created, updated, or cancelled
  event in one calendar.
- Never emit for historical backfill, re-sync, label, reindex, attachment, or
  projection work on existing rows.
- Keep every promise in the protocol honest: durable subscriptions across
  restarts, stable event IDs on retry, a cursor that never skips an
  undelivered event, `truncated: true` when history is gone.
- Work identically on SQLite and PostgreSQL.
- Add no new content surface: webhooks carry IDs, timestamps, and flags only.

## Non-goals

- Account-wide or inbox-wide subscriptions, filtered or not (see
  [Scope](#scope-one-conversation-only)).
- Polling, push streaming, `gap`/`terminated` control envelopes, or terminal
  callbacks. ChatGPT supports none of them and they are not advertised.
- An OAuth authorization server inside msgvault. The principal is whatever
  the existing inbound bearer identifies; see [Principal](#principal).
- Receivers other than ChatGPT. Any receiver that implements the profile
  works, but none is tested or documented here.
- RSVP changes and Kata issue state changes; both need data msgvault does not
  hold today (see each family).

## Existing behavior and invariants

What the design relies on, verified on `main`:

- **Transport.** `internal/mcp/server.go` serves `/mcp` with go-sdk v1.7.0's
  `NewStreamableHTTPHandler` in `Stateless` + `JSONResponse` mode behind
  `bearerAuthHandler`: one static token (`HTTPOptions.APIKey`, compared by
  SHA-256 in constant time), or no auth at all when the token is empty.
  Protocol `2026-07-28` and `server/discover` have been served since #566;
  `cachePolicyMiddleware` already rewrites `*sdkmcp.DiscoverResult`. Requests
  on a protocol older than `2026-07-28` lose the write tools. The SDK already
  validates `Mcp-Method` and the other `Mcp-*` headers (`-32020`) and
  rejects unsupported versions with `-32022`; v1.7.0 also provides
  `AddReceivingCustomMethod`. `ServerCapabilities` has no `events` field
  (go-sdk#1325 is open), so the top-level key must be injected.
- **Process split.** `cmd/msgvault/cmd/mcp.go` builds `ServeOptions` from a
  `daemonclient.Client`, gating each backend on
  `APISchemaVersionAtLeast`. The MCP process never opens the archive.
- **Canonical write path.** Every source — Gmail, IMAP, Beeper, iMessage,
  WhatsApp, Signal/Telegram through Beeper, Slack, Discord, imports, and
  Google Calendar — lands in `Store.UpsertMessage` →
  `upsertMessageWith` (`internal/store/messages.go`), one transaction with
  `ON CONFLICT (source_id, source_message_id)`. The function already reads
  the prior row first (`bodylessMessageJournalState.found`), so "new versus
  re-upserted" is known inside the transaction. On SQLite it first touches
  `embedding_change_clock` to take the writer lock. Three commit-ordered
  journals already exist (`embedding_changes`, `person_sweep_changes`,
  `attachment_changes`); they are consumer-specific and pruned by their
  consumers, so they are a precedent, not a reusable sequence.
- **Identity.** `messages.id` and `conversations.id` are integer primary
  keys; conversations are unique per `(source_id, source_conversation_id)`
  with `conversation_type` in `email_thread`, `group_chat`, `direct_chat`,
  `channel`, `calendar`. `messages.is_from_me` is baked at write time.
  Reactions are rows in `reactions` (`UpsertReaction`, `ReplaceReactions`),
  unique per `(message_id, participant_id, reaction_type, reaction_value)`,
  with `removed_at`.
- **Calendar.** `internal/calsync` stores each Google Calendar event as a
  `messages` row (`message_type = calendar_event`, one source per calendar,
  `source_message_id` = event ID or `recurringEventId|originalStart`) and
  writes status, sequence, `ical_uid`, and series linkage to
  `messages.metadata` through `SetMessageMetadata`; cancellations flip
  `metadata.status` in `flagCancelled`. Attendee `responseStatus` is parsed
  (`internal/gcal/models.go`) but not persisted. Emailed `.ics` invites are
  not parsed; they are ordinary email messages with attachments.
- **Kata.** The optional task integration (`[integrations.tasks]`,
  `internal/taskclient`, API routes `POST/GET /api/v1/messages/{id}/tasks`,
  `DELETE …/tasks/{task_id}`) writes `mail_links` metadata onto the Kata
  task. msgvault keeps no durable link table; `internal/tasklinks` is a
  disposable reverse-index cache. There is no feed from Kata back to
  msgvault.
- **Backfill versus live.** `sync_runs.sync_type` distinguishes `full`,
  `import-mbox`, `import-emlx`, `import-pst`, and incremental runs, but
  `Message` carries no provenance and `UpsertMessage` cannot tell a
  historical row from a fresh arrival.
- **Reusable pieces.** `internal/netguard` (prohibited IP/hostname policy,
  `ValidateTrustedDestination`), the pinned-IP dialer in
  `internal/remoteimage/fetch.go` and `internal/carddav/transport.go`,
  `internal/httpretry.RetryAfter`, HMAC use in `internal/daemonauth`,
  `providercredentials.EnsureServerKey` for a one-time owner-only key file,
  the scheduler/job pattern in `internal/scheduler` and
  `internal/api/scheduler_jobs.go`, and the modern-HTTP protocol tests in
  `internal/mcp/protocol_test.go`.
- **Read tools.** `get_message` returns the message and its attachment list;
  `get_attachment {id, offset, length ≤ 4 MiB, sha256}` streams chunks (sha256
  from the first chunk is required for every later offset; whole-object
  embedding up to 50 MiB); `list_thread` resolves a conversation from a
  msgvault message ID or a provider `thread_id` + `account`;
  `list_messages {conversation_id, …}` pages one conversation newest-first.
  None of them exposes `is_from_me`.

## Architecture

```text
ChatGPT ──events/subscribe──▶ msgvault mcp --http ──daemonclient──▶ daemon API
   ▲                           (stateless; injects                   /api/v1/mcp/events/*
   │                            capabilities.events)                     │
   │                                                                     ▼
   │                                               ingestion tx ─▶ mcp_event_log ──▶ delivery worker
   └────────────── signed webhook (direct outbound HTTPS from the daemon) ◀──────────┘
ChatGPT run ──get_message / list_thread / get_attachment──▶ msgvault mcp ──▶ daemon
```

Everything stateful is in the daemon because the daemon owns archive access
(`docs/architecture/overview.md`), the ingestion transaction is there, and a
single daemon per archive makes delivery ownership trivial (no leases or
`SKIP LOCKED` as in Inline's multi-process API, `server/src/modules/mcpEvents/repository.ts`).

## Events

Names are prefixed `msgvault.` so a gateway that nests several servers cannot
collide. Argument schemas are closed objects (`additionalProperties: false`);
IDs are decimal strings as Inline does (`catalog.ts`), because JSON numbers
above 2^53 are unsafe and the tools already accept the same strings.

### Scope: one conversation only

`conversation_id` is the msgvault `conversations.id`, which already encodes
source and account. A subscriber resolves it with `list_thread` (by message ID
or provider `thread_id` + `account`) or reads it from any `get_message` /
`list_messages` result; subscribe validates that the row exists and is not
a calendar conversation, and returns `-32602` otherwise. Subject, participant
names, and bare provider thread IDs are never accepted as scope.

A broader "one account's inbox with filters" family is rejected for v1. Every
new message in a mailbox would become an event, a label or sender filter
means different things per source (Gmail labels, IMAP folders, chats have
neither), and the woken run would still need to triage; the cost and the
privacy exposure are unbounded while the benefit is a filter the run can
apply itself after a scoped wake. The schema reserves `scope_kind` so a later
sender-scoped family (`person_id`, tracked people only) can be added without
a migration, but that is a separate design.

### `msgvault.message_archived`

A message or reaction newly committed to the archive in one conversation.
"Archived", not "received": archive time lags provider arrival by the sync
interval.

Arguments: `conversation_id` (required), `include_from_me` (default
`false`), `include_reactions` (default `true`).

Payload (`kind` is `message` or `reaction`; reactions add
`target_message_id` and `reaction_value`, the emoji or tapback name):

```json
{"kind":"message","message_id":"123456","conversation_id":"7890","source_id":"3",
 "from_me":false,"sent_at":"2026-10-05T09:12:44Z","archived_at":"2026-10-05T09:12:49Z"}
```

`from_me` is `messages.is_from_me` for messages; for reactions it is whether
the reacting participant is a confirmed owner identity of the source, the
same predicate that sets `identity_is_from_me`. Edits, label changes, read
flags, soft deletes, attachment downloads, FTS and projection updates, and
reaction removals never emit. A message deleted before delivery is still
delivered (the read tool reports the deletion), matching the "committed
occurrence, not current state" rule in Inline's `source.ts`.

### `msgvault.calendar_event_changed`

A calendar event created, updated, or cancelled in one synced calendar.

Arguments: `calendar_source_id` (required): the calendar's `sources.id`, as
returned in the `source_id` of any `calendar_event` message or by the
calendar tools. Subscribe checks the source exists and has
`source_type = gcal`.

Payload: `kind` (`created` | `updated` | `cancelled`), `message_id`,
`conversation_id` (the series conversation), `source_id`, `ical_uid`,
`sequence`, `starts_at`, `from_me` (the organizer is the owner). The woken
run reads the full event with `get_message`.

`updated` requires a real change: `sequence` increased, or start, end, or
status differ from the stored metadata. Google incremental sync only returns
changed events, but a full sync (`sync_type = full`) re-delivers every event
and is muted like any backfill. Attendee RSVP changes are not emitted because
`responseStatus` is not stored; persisting it is a metadata change for a
later proposal, and `kind: rsvp_changed` is reserved for it. "Invites from or
to a person" is the sender-scoped family above, also deferred.

### `msgvault.task_linked`

Optional, lowest value, and cheap: a task created or linked from a message
in one conversation through msgvault's own API (`createOrLinkMessageTask`,
`unlinkMessageTask`). Arguments: `conversation_id`. Payload: `kind`
(`linked` | `unlinked`), `message_id`, `conversation_id`, `task_id`,
`project`. The log row is written after Kata confirms the write, so it is an
action log, not an ingestion event. Linked-issue state changes cannot be
emitted: msgvault has no feed and the reverse-index cache is explicitly
non-authoritative. They belong in a Kata-side events implementation, which
can cite the `mail_links` metadata to scope by message. Ship this family
only if the maintainers want it; nothing else depends on it.

### Own messages and the loop guard

The owner writes in watched chats himself, and an agent may reply through
msgvault drafts (`agentgrant` `draft.*` permissions) or another client; the
archive cannot tell the two apart. `include_from_me` therefore defaults to
`false`, as Inline's `excludeSelf` does, and the `events/list` description
says that `from_me: true` is never a message to answer. The server
`instructions` repeat it. On top, one counter per subscription: at most 6
`from_me` deliveries per rolling 10 minutes; further `from_me` rows in the
window are skipped (cursor advances, `loop_guard_skips` increments,
reported by the status command). Inline has no guard and relies on skill
text; the counter costs two columns and bounds a runaway loop to 36 runs an
hour.

## Event production

### Commit-time change log

`mcp_event_log` is the smallest durable, commit-ordered sequence that covers
messages, reactions, calendar changes, and task links, since none of the
existing journals does. It is written **inside** the ingestion transaction:

| Column | Note |
|---|---|
| `seq` | `INTEGER PRIMARY KEY AUTOINCREMENT` / `BIGINT GENERATED ALWAYS AS IDENTITY` |
| `scope_kind`, `scope_id` | `conversation`/`conversations.id` or `calendar`/`sources.id` |
| `kind`, `item_key` | `UNIQUE (scope_kind, scope_id, kind, item_key)`; message ID, `reaction:<msg>:<participant>:<type>:<value>`, `task:<task>:<msg>` |
| `message_id`, `conversation_id`, `source_id`, `from_me` | filter and payload fields |
| `occurred_at`, `committed_at` | provider time; commit time |
| `data` | the payload JSON encoded once at commit |

Rows are written only when an active subscription covers the scope
(`EXISTS` on `mcp_event_subscriptions` by `(scope_kind, scope_id)`), so an
archive with no subscribers pays one indexed read per new message when the
flag is on and nothing when it is off. Retention is 7 days, pruned by the
worker; a row is kept while any active subscription's cursor is behind it.

Hook points, all in `internal/store`:

- `upsertMessageWith`: when `prior.found` is false, the row is not deleted,
  and `msg.IngestMode == IngestLive`, append `kind: message` (or the
  calendar `created`). The prior-row read runs for every upsert while the
  flag is on; today it is skipped for deleted rows.
- `UpsertReaction` / `ReplaceReactions`: append `kind: reaction` for each
  reaction row that did not exist before and has no `removed_at`.
- A new `UpsertCalendarEvent(msg, metadata)` that does the upsert, the
  metadata write, and the log append in one transaction, computing `kind`
  from the prior row and metadata diff; `calsync.ingestEvent` and
  `flagCancelled` move onto it. Today they are two separate store calls.
- The task-link API handler appends through a small store method after the
  Kata write succeeds.

Concurrency: SQLite serializes all writers, and `UpsertMessage` already
reserves the writer before reading. On PostgreSQL the log append and
subscription activation both take `pg_advisory_xact_lock` on one constant, so
a message committed during activation is either behind the activation head
(and replayed) or ahead of it (and delivered). Without that lock, an
activation snapshot could miss a message whose upsert did not yet see the
subscription row.

### Live versus backfill

`Message` gains `IngestMode` (`IngestUnknown`, `IngestLive`,
`IngestBackfill`), a Go field, not a column. Only `IngestLive` emits; unknown
is treated as backfill. Each incremental path sets `IngestLive` explicitly —
Gmail history sync (`internal/sync/incremental.go`), IMAP incremental, Beeper
live import, iMessage and WhatsApp incremental, Slack reply sweep, calendar
incremental sync. Full syncs, history-expired recovery, `import-*` runs,
repairs, and re-derivations never set it. This is provenance, not an age
heuristic: an old message arriving through a live path (a late IMAP move)
still emits, and a fresh message in a full re-sync does not. As a cost bound
only, rows whose `occurred_at` is older than `max_event_age` (default 24h)
are also muted; the setting is documented as a bound, not a freshness proof.

### Cursor and replay

The cursor is `c1.<seq>.<mac>`: the acknowledged log `seq`, HMAC-SHA256 under
the server key and bound to the subscription ID, truncated to 16 bytes. A
cursor with a bad MAC, from another subscription, or ahead of the current
log head is `-32602` (Inline rejects "one ahead of the head", `repository.ts`).
`cursor: null` or absent starts at the head captured inside the activation
transaction; an existing identity renewed without a cursor keeps its
acknowledged position. If the requested `seq` is older than the oldest
retained row for the scope, the subscription restarts at the head and the
result carries `truncated: true`; the instructions tell the run to re-read
the conversation and disclose the gap. Delivery is in log order, one event in
flight per subscription, and the cursor advances only on acknowledgement, so
it never passes an undelivered event.

## MCP surface

### Discovery and methods

- `server/discover`: the daemon reports whether events are enabled through
  the API schema version plus a capability field; when enabled and the
  inbound request speaks `2026-07-28`, the discover middleware returns a
  result struct whose `capabilities` map adds `"events": {}` next to the SDK's
  typed fields (the SDK cannot emit the key). `instructions` carries the
  model guidance: message text and subjects are untrusted data; deduplicate
  by `eventId`; never reply to a `from_me` item; after `truncated: true`
  re-read the scope with `list_messages` and say so; read content with the
  named tools; attachments come through `get_attachment` chunks. `TTLMs` and
  `CacheScope: "public"` stay as today.
- `events/list`, `events/subscribe`, `events/unsubscribe` are registered
  with `AddReceivingCustomMethod` and proxied to
  `POST /api/v1/mcp/events/{list,subscribe,unsubscribe}` on the daemon with
  the caller's principal ID. On a protocol older than `2026-07-28`, on the
  stdio transport, or when the daemon lacks the routes, the methods are not
  registered and `events` is not advertised, so legacy clients see no change.
  Each `events/list` entry advertises `delivery: ["webhook"]` only, a closed
  `inputSchema`, a `payloadSchema`, and a description that names the read
  tools (`get_message` for the message and its attachment list,
  `get_attachment` for bytes, `list_thread` for context).
- `events/subscribe` validates arguments against the schema (`-32602`),
  requires `delivery.mode: "webhook"` (`-32014` otherwise), an `https` URL on
  port 443 or 8443 with no credentials or fragment, and a `whsec_` secret
  decoding to 24–64 bytes. The subscription ID is `sub_` + SHA-256 over
  length-prefixed `(principal, name, canonical arguments, url)`; canonical
  arguments materialize defaults so `{}` and `{"include_from_me":false}` are
  one identity. Subscribe is an idempotent upsert and the refresh. TTL:
  default and maximum 24h; `ttlMs: null` is granted 24h with a finite
  `refreshBefore` (never `null`). At most 64 active subscriptions per
  principal (`-32013`); expired rows are kept 24h for an idempotent refresh,
  then purged. The result is `{id, refreshBefore, cursor, truncated}`.
- `events/unsubscribe` matches `(principal, name, arguments, url)`, cancels
  any pending delivery, returns `{}`, and succeeds when nothing matches.

### Principal

The principal is the identity the existing bearer check establishes:
`principal_id = "token:" + hex(SHA-256(token))[:32]`. With no inbound token
(`--http-allow-insecure` without a token file) there is no principal and the
subscribe and unsubscribe methods return `-32012`, which the draft requires.
Rotating the token changes the principal, so every old subscription stops at
its next authorization recheck and is purged after the grace period. Nothing
in the schema assumes one principal: a future OAuth front (a tunnel or gateway
that forwards a user bearer, or an identity provider in msgvault) maps a
token subject into the same column.

Authorization is rechecked at discovery, at subscribe and refresh, before
every dial, and before recording success: the principal matches the current
token, the scope row still exists, the source is not removed, and the
feature flag is still on. A failed recheck moves the subscription to
`expired` and clears the pending delivery.

### Reading the event, including attachments

The webhook names a `message_id`. The run calls `get_message` for the
message and its attachment list (`id`, `filename`, `mime_type`,
`size_bytes`, `content_hash`), then `get_attachment` with the attachment ID:
whole objects up to 50 MiB embedded, or chunks of 1–4 MiB by `offset` and
`length`, passing the first chunk's `sha256` on every later call. Calendar
events have no archived attachments (calsync stores none), and an emailed
invite is an email whose `.ics` is a normal attachment. No new read tool is
added. ChatGPT runs have been observed without the event `data`
(openai/codex#49665); the instructions tell such a run to call
`list_messages {conversation_id, limit: 5}` instead, which is what the event
would have pointed to.

## Delivery

The worker lives in the daemon (`internal/mcpevents`) and follows the
scheduler job conventions: one loop, ticked every second and woken by a
non-blocking channel send after each log commit, that re-reads due
subscriptions from the store on every pass. Per subscription, per pass:

1. Recheck authorization. If there is no pending event, read the next log
   row with `seq > cursor_seq` in the scope that passes the subscription's
   filters; rows that fail a filter or the loop guard advance `cursor_seq`
   without delivery. Build the envelope `{eventId, name, timestamp, data,
   cursor}` with `eventId = "evt_" + SHA-256(subscription_id ‖ seq)`,
   `timestamp = occurred_at`, and persist it as the pending event. The bytes
   are stored once, so every retry sends an identical body.
2. Verify the callback first if it was never verified for this identity or
   the secret changed: POST `{"type":"verification","challenge":<32 random
   bytes, base64url>}` with `webhook-id: msg_verification_<random>`, the
   Standard Webhooks headers, and `X-MCP-Subscription-Id`; require 2xx, a
   body of at most 4 KiB, and a constant-time challenge match within 10 s.
   Failures surface from subscribe as `-32015` with `data.reason` in
   `connection_refused`, `timeout`, `tls_error`, `http_4xx`, `http_5xx`,
   `challenge_failed`. A successful verification is cached per
   `(principal, url)` for 60 s so a burst of refreshes does not re-challenge.
3. POST the envelope, at most 262144 bytes, with `Content-Type:
   application/json`, `webhook-id` = `eventId`, `webhook-timestamp` in Unix
   seconds, `webhook-signature` = `v1,` + base64 HMAC-SHA256 over
   `id.timestamp.body` under the decoded `whsec_` key, and
   `X-MCP-Subscription-Id`. After a rotation both the new and the previous
   key sign, space-separated, for 60 s. Receipt is decided from the status
   line: any 2xx is success.
4. On success, recheck authorization, then acknowledge: `cursor_seq =
   pending_seq`, clear the pending event, reset attempts. On `410`, end the
   subscription (`gone`, pending cleared), as the protocol says. On `413`,
   dead-letter that event and advance. Otherwise retry with
   `min(15m, 1s × 2^(attempt-1))` plus jitter, honoring `Retry-After` on
   429 and 503 through `httpretry.RetryAfter` capped at 1h. After 12
   attempts the event is dead-lettered (`mcp_event_dead_letters`: subscription,
   seq, attempts, last status) and the subscription continues with the next
   row; a subscriber that is down for an hour loses one stale wake-up, not
   its subscription.

Outbound client: HTTPS only; DNS resolved by msgvault; every resolved address
must pass `netguard.ProhibitedIP` (private, loopback, link-local, CGNAT,
multicast, unspecified, IPv6 ULA/link-local/mapped/transition ranges) and the
connection dials the validated address with the original hostname for TLS,
the pinned-dialer pattern from `internal/remoteimage/fetch.go`; redirects are
refused; 10 s per request. Operators who run the receiver on a private
network set `[mcp.events] trusted_callback_origins` and
`trusted_callback_addresses`, validated by `netguard.ValidateTrustedDestination`
exactly as `[carddav] trusted_origin` is; this is how the synthetic
end-to-end test and a self-hosted receiver work.

Secrets: the `whsec_` value and its predecessor are stored AES-256-GCM
encrypted under a 32-byte key in `<data_dir>/mcp-events.key`, created once
with the `providercredentials.EnsureServerKey` pattern (owner-only file,
never in config or the database). Secrets, keys, signatures, and callback
URLs never appear in logs; log fields are the subscription ID, scope, event
seq, attempt, status class, and error class.

Observability: `msgvault mcp events status` (through the daemon API) lists
the caller's subscriptions with state, `refreshBefore`, cursor seq, last
delivery outcome, dead-letter and loop-guard counts, without payloads or
secrets. The daemon logs one line per state transition and one per outage
window, not per attempt.

## Reaching the server from ChatGPT

ChatGPT connects from OpenAI's cloud and needs OAuth (it cannot present a
static bearer). Two routes exist today; neither is verified end to end.

- **Through a nesting MCP gateway.** A gateway that exposes msgvault's tools
  as nested tools must forward `server/discover` with the top-level
  `events` capability, forward `events/*` with the `Mcp-Method` header
  intact, and keep a per-user principal; one that collapses users onto a
  shared backend credential also collapses their subscriptions. The gateway
  in use today does not forward `events/*`; its vendor has said support is
  planned. Until it ships, backend support is verifiable only by calling
  msgvault's `/mcp` directly.
- **Through OpenAI's Secure MCP Tunnel.** The tunnel client forwards raw
  JSON-RPC and the connector's bearer unchanged, and `server/discover`
  already crosses it, but whether OpenAI's hosted end accepts `events/*` for
  a tunnel-backed plugin is undocumented. Webhook deliveries never traverse
  the tunnel: the daemon host needs ordinary outbound HTTPS (observed
  callback host: `connectors.api.openai.com`).

What is verifiable now, with no host involved: run the daemon with the flag
on, call `server/discover`, `events/list`, and `events/subscribe` against a
self-hosted receiver allowed through `trusted_callback_origins`, and watch a
synthetic message in a watched conversation arrive as a signed POST. The
protocol tests below do the same against `httptest`.

## Schema, configuration, and compatibility

Three new tables in both `internal/store/schema.sql` and `schema_pg.sql`
(`CREATE TABLE IF NOT EXISTS`, applied by `InitSchemaContext`; no ledger
migration is needed because no existing row changes):

- `mcp_event_log` as above.
- `mcp_event_subscriptions`: `id`, `principal_id`, `name`, `arguments`
  (canonical JSON), `scope_kind`, `scope_id`, `callback_url`, `secret_enc`,
  `previous_secret_enc`, `previous_secret_until`, `verified_at`,
  `expires_at`, `state` (`active`, `expired`, `unsubscribed`, `gone`, with a
  `CHECK`), `cursor_seq`, `pending_seq`, `pending_envelope`,
  `attempt_count`, `next_attempt_at`, `from_me_window_start`,
  `from_me_window_count`, `loop_guard_skips`, `dead_letter_count`,
  `created_at`, `updated_at`. Indexes on `(scope_kind, scope_id)` for active
  rows and on `(state, next_attempt_at)`. This is Inline's one-table model
  (`server/drizzle/0153_mcp-events.sql`): cursor, one pending event, attempt
  state, and both secrets on the subscription row.
- `mcp_event_dead_letters`: `subscription_id`, `seq`, `attempts`,
  `last_status`, `failed_at`; pruned with the log.

No existing table changes. `Message.IngestMode` is a struct field. The daemon
API gains the three routes plus a status route and an API schema version
bump; `daemonclient` gains the matching methods. go-sdk stays at v1.7.0.

Configuration:

```toml
[mcp.events]
enabled = false                 # default off; daemon refuses subscribe with -32014 when off
max_event_age = "24h"           # cost bound on live rows, not a provenance test
retention = "168h"
trusted_callback_origins = []   # operator-approved private receivers, as [carddav] trusted_origin
trusted_callback_addresses = []
```

Compatibility: with the flag off, nothing is advertised and the ingestion
path is unchanged. Legacy protocol clients and stdio never see the methods.
Payload and argument schemas change only additively; a breaking change takes
a new event name.

## Consent and rollout

Explicit maintainer consent is required for, and recorded in the PR that
makes, each of these:

1. Three new tables on both backends and the `pg_advisory_xact_lock` in the
   upsert path.
2. A new egress class: the daemon POSTs to caller-supplied public HTTPS URLs.
   Opt-in by `[mcp.events] enabled`, documented in the configuration and
   security pages.
3. A key file in the data directory and encrypted secrets at rest.
4. The calsync persistence refactor onto `UpsertCalendarEvent`.
5. Marking each incremental sync path `IngestLive`, one source family per
   change, starting with Gmail incremental, Beeper, and calendar incremental.
6. New daemon API routes and the schema version bump.

Rollout order: store and log with the flag off; daemon routes and worker;
MCP proxy and discovery; calendar; task links if wanted; documentation
(`docs/usage/chat.md`, `docs/configuration.md`, `docs/api-server.md`).

## Testing

Synthetic only: fake sources, synthetic names and reserved example
addresses, `httptest` TLS receivers, injected resolver and allowlist. No live
provider, no real ChatGPT, no private content. testify, table-driven,
against SQLite (`make test`) and PostgreSQL (`make test-pg`).

- Store: a new message in a watched conversation writes exactly one log row
  in the same transaction and a rolled-back upsert writes none; a re-upsert,
  label, body, attachment, FTS, or projection change writes none; `full`,
  `import-*`, and unknown-mode upserts write none; the same provider thread
  ID in two accounts yields two scopes; reactions emit once per (target,
  participant, type, value) and removals never; calendar created / updated
  (sequence or time change) / cancelled / unchanged re-delivery; activation
  racing a commit on both backends never loses the message.
- Protocol (extending `protocol_test.go` and `conformance_endpoint_test.go`):
  discover carries `events: {}` and the instructions only on `2026-07-28`;
  `events/list` entries validate against a pinned fixture of the ChatGPT
  profile checked into `testdata/mcp/`, the way Inline's
  `scripts/ci/check-chatgpt-plugin.mjs` pins a `2026-07-28` schema;
  header mismatch is `-32020` and an unsupported version `-32022`; canonical
  argument idempotency; principal isolation and the missing-principal
  `-32012`; TTL omitted / finite / null; the 64-subscription cap; refresh
  with rotation dual-signs for 60 s; a cursor with a bad MAC, from another
  subscription, or ahead of the head is rejected; unsubscribe racing a
  queued send.
- Webhook: signatures match the Standard Webhooks reference vectors over the
  exact bytes; challenge success, mismatch, replayed challenge, timeout, each
  `-32015` reason; verification body cap; private IPv4/IPv6, mapped and
  transition addresses, DNS rebinding (public then private), ports other
  than 443/8443, and redirects refused; allowlisted private origin accepted.
- Delivery: 2xx, 429 and 503 with `Retry-After`, 5xx, timeout, 410 ends the
  subscription, 413 and the 13th attempt dead-letter and continue; event B
  stays unsent while A retries; restart mid-retry keeps event ID, attempt
  count, and identical body bytes; `include_from_me` and `include_reactions`
  filters; the 7th `from_me` row within 10 minutes is skipped and counted;
  authorization loss before dial and before acknowledgement; retention
  truncation returns `truncated: true` and restarts at the head.
- End to end: fake source → store → worker → `httptest` receiver verifies
  the signature and payload, then the test calls `get_message` and
  `get_attachment` chunks for the delivered `message_id` and reassembles to
  the stored sha256.

## Adopted from Inline

| Inline decision (file) | msgvault |
|---|---|
| Cursor over an existing durable journal, no outbox (`source.ts`, `worker.ts`) | Same model over a new commit-time `mcp_event_log`, because no existing msgvault journal covers messages, reactions, and calendar together |
| One subscriptions table: cursor, one pending occurrence, attempts, both secrets (`server/drizzle/0153_mcp-events.sql`) | Adopted; no deliveries table; a small dead-letter ledger added for observability |
| Reference-only payloads, read tool named in the description (`catalog.ts`, `source.ts` `referenceData`) | Adopted; `get_message`, `get_attachment`, `list_thread` |
| Per-scope events only (`catalog.ts` `parseSelector`) | Adopted; inbox-wide scope rejected |
| `excludeSelf` on message events (`catalog.ts`) | `include_from_me`, default false, plus a counter loop guard Inline lacks |
| Reactions excluded (no journal) (`server/docs/mcp-events.md`) | Differs: reactions are durable rows here and a customer's thumbs-up is a signal, so `kind: reaction` is emitted |
| Retry `1s × 2^n` capped 15 min, `Retry-After` on 429/503, 12 attempts, receipt by status, 410/413 settle one occurrence (`worker.ts`) | Adopted, except 410 ends the subscription as the OpenAI guide states; 413 settles one event |
| Re-authorize before connect and before acknowledge (`worker.ts`, `authorization.ts`) | Adopted |
| Rotation overlap 1 min; TTL max 1 day; 64 active per grant; cursor ahead of head rejected; encrypted bound cursor (`service.ts`, `crypto.ts`) | Adopted; TTL default also 24h; cursor is HMAC-bound rather than encrypted |
| SSRF: public IPs only, ports 443/8443, pinned dial, no redirects, 10 s (`webhook.ts`) | Adopted via `netguard` and the pinned-dialer precedent; operator allowlist added for private receivers |
| Stateless MCP proxy, durable state in the API (`events-proxy.ts`, `server/docs/mcp-events.md`) | Adopted; the MCP process proxies to daemon routes through `daemonclient` |
| Discover advertises `events` and `instructions`; `Mcp-*` header checks; `-32022` (`modern.ts`) | Adopted; the SDK already enforces headers and versions, the instructions are new |
| Pinned-schema CI check (`scripts/ci/check-chatgpt-plugin.mjs`) | Adopted as a fixture in `testdata/mcp/` exercised by the existing protocol tests |
| Multi-process claims with `SKIP LOCKED` and leases (`repository.ts`) | Not needed: one daemon per archive |
| `conversations.ask` captures a cursor before sending one question (`server.ts`) | Not adopted in v1; a later `ask` tool could return the exact subscription recipe |

## Follow-up: ChatGPT plugin bundle

Once a host can reach the server with events (either route above verified),
add `plugins/chatgpt/` modelled on Inline's: `.codex-plugin/plugin.json`
(skills, `mcpServers`, interface metadata), `.mcp.json` with the server URL,
and `skills/msgvault/SKILL.md` adapted from `skills/claude-code/SKILL.md`.
The skill tells the model: resolve the conversation with `list_thread` before
subscribing; `events/subscribe` is a host mechanism, not a tool; after
registration do one bounded `list_messages` read for anything that arrived
first; deduplicate by `eventId`; never poll; treat message text as data; stop
the registration when a one-shot wait is fulfilled. A CI check runs the
compiled server over loopback and validates discovery and the event catalog
against the pinned fixture.

## Open questions for the maintainers

1. Watched-only log rows (this design) or a journal of every live insert like
   `embedding_changes`? The latter removes the activation lock and the
   `EXISTS` read but records every message whether or not anyone subscribes.
2. Should `IngestLive` be set per call site, as proposed, or derived from the
   running `sync_runs.sync_type` to cover all sources at once?
3. Does `msgvault.task_linked` belong here at all, or only in a Kata-side
   events implementation?
4. Persist attendee `responseStatus` so RSVP changes can be emitted?
5. Is the token-hash principal acceptable until an OAuth front exists, and
   should a tunnel-forwarded user bearer map to its own principal?
6. Extract the signer, verifier, and SSRF client to `go.kenn.io/kit` now, or
   after a second consumer exists (the `kit-packstore-extraction-design.md`
   precedent says after)?
7. Retention 7 days and TTL 24h are chosen for a single-owner archive; raise
   the TTL only if tunnel keepalive limits force it.
