-- msgvault unified schema
-- Supports: Gmail, Apple Messages, Google Messages, WhatsApp

CREATE TABLE IF NOT EXISTS archive_metadata (
    key TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

-- ============================================================================
-- SOURCES & IDENTITY
-- ============================================================================

-- Message sources (accounts from different platforms)
CREATE TABLE IF NOT EXISTS sources (
    id INTEGER PRIMARY KEY,
    source_type TEXT NOT NULL,  -- 'gmail', 'apple_messages', 'google_messages', 'whatsapp'
    identifier TEXT NOT NULL,   -- email, phone number, or account ID
    display_name TEXT,

    -- Gmail-specific (for backward compatibility during transition)
    google_user_id TEXT UNIQUE,

    -- Sync state
    last_sync_at DATETIME,
    sync_cursor TEXT,           -- platform-specific: historyId, rowid, timestamp
    sync_config JSON,           -- platform-specific sync settings
    oauth_app TEXT,             -- named OAuth app binding (NULL = default)

    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,

    UNIQUE(source_type, identifier)
);

-- Participants (unified contacts across platforms)
CREATE TABLE IF NOT EXISTS participants (
    id INTEGER PRIMARY KEY,
    email_address TEXT,         -- for email participants
    phone_number TEXT,          -- normalized E.164 format
    display_name TEXT,
    domain TEXT,                -- extracted from email for aggregation

    -- For cross-platform dedup (normalized phone/email)
    canonical_id TEXT,

    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

-- Participant identifiers (for linking multiple contact methods)
CREATE TABLE IF NOT EXISTS participant_identifiers (
    id INTEGER PRIMARY KEY,
    participant_id INTEGER NOT NULL REFERENCES participants(id) ON DELETE CASCADE,

    identifier_type TEXT NOT NULL,  -- 'email', 'phone', 'apple_id', 'whatsapp'
    identifier_value TEXT NOT NULL, -- normalized value
    display_value TEXT,             -- original format for display

    is_primary BOOLEAN DEFAULT FALSE,

    UNIQUE(identifier_type, identifier_value)
);

-- Durable, user-curated people. A person's vCard UID is generated once and
-- never depends on mutable participant identifiers or link-graph topology.
-- UID lifecycle contract: UIDs are random and never reused. Deleting a
-- person retires its UID forever (no tombstones; a later re-promotion of
-- the same cluster creates a new person with a new UID), and a future
-- person-merge must keep the surviving person's UID and retire the other.
-- AUTOINCREMENT (IDENTITY on PostgreSQL) matters here: person IDs are
-- durable external handles, so a deleted person's ID must never be
-- recycled for a later person the way plain rowid allocation would.
CREATE TABLE IF NOT EXISTS persons (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    vcard_uid    TEXT NOT NULL UNIQUE,
    display_name TEXT,
    revision     INTEGER NOT NULL DEFAULT 1,
    created_at   DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at   DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

-- Bindings are deliberately participant-local and are the source of truth
-- for person membership: a person covers exactly its bound participants,
-- never "whatever cluster a binding sits in". Link/unlink changes the
-- observed identity graph without rewriting curated person membership;
-- within one cluster, link/merge/promotion keep bindings all-or-none to at
-- most one person, while unlink may leave one person spanning the split
-- clusters until the user re-links or deletes the profile.
CREATE TABLE IF NOT EXISTS person_participants (
    person_id      INTEGER NOT NULL REFERENCES persons(id) ON DELETE CASCADE,
    participant_id INTEGER NOT NULL REFERENCES participants(id) ON DELETE CASCADE,
    PRIMARY KEY (person_id, participant_id),
    UNIQUE(participant_id)
);

-- ============================================================================
-- CONVERSATIONS & MESSAGES
-- ============================================================================

-- Conversations (threads for email, chats for messaging)
CREATE TABLE IF NOT EXISTS conversations (
    id INTEGER PRIMARY KEY,
    source_id INTEGER NOT NULL REFERENCES sources(id) ON DELETE CASCADE,

    -- Platform-specific ID for dedup on re-import
    source_conversation_id TEXT,

    -- Type and metadata
    conversation_type TEXT NOT NULL,  -- 'email_thread', 'group_chat', 'direct_chat', 'channel'
    title TEXT,                       -- email subject, group name, or NULL for DMs

    -- Denormalized stats (updated on message insert)
    participant_count INTEGER DEFAULT 0,
    message_count INTEGER DEFAULT 0,
    unread_count INTEGER DEFAULT 0,
    last_message_at DATETIME,
    last_message_preview TEXT,

    -- Platform-specific metadata
    metadata JSON,

    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,

    UNIQUE(source_id, source_conversation_id)
);

-- Conversation participants (who's in each conversation)
CREATE TABLE IF NOT EXISTS conversation_participants (
    conversation_id INTEGER NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
    participant_id INTEGER NOT NULL REFERENCES participants(id) ON DELETE CASCADE,

    role TEXT DEFAULT 'member',  -- 'owner', 'admin', 'member' for groups
    joined_at DATETIME,
    left_at DATETIME,

    PRIMARY KEY (conversation_id, participant_id)
);

-- Messages (unified across all platforms)
CREATE TABLE IF NOT EXISTS messages (
    id INTEGER PRIMARY KEY,
    conversation_id INTEGER NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
    source_id INTEGER NOT NULL REFERENCES sources(id) ON DELETE CASCADE,

    -- Platform-specific ID for dedup
    source_message_id TEXT,

    -- RFC822 Message-ID for cross-mailbox dedup (IMAP)
    rfc822_message_id TEXT,

    -- Message classification
    message_type TEXT NOT NULL,  -- 'email', 'imessage', 'sms', 'mms', 'rcs', 'whatsapp', 'fbmessenger', 'teams'

    -- Timestamps (sent_at is canonical, others platform-specific)
    sent_at DATETIME,
    received_at DATETIME,
    read_at DATETIME,
    delivered_at DATETIME,
    internal_date DATETIME,      -- Gmail internal date

    -- Sender
    sender_id INTEGER REFERENCES participants(id),
    is_from_me BOOLEAN DEFAULT FALSE,

    -- Content
    subject TEXT,               -- email subject, NULL for chat
    snippet TEXT,               -- preview/excerpt

    -- Threading (for email and replies)
    reply_to_message_id INTEGER REFERENCES messages(id),
    thread_position INTEGER,    -- position in thread/conversation

    -- Status flags
    is_read BOOLEAN DEFAULT TRUE,
    is_delivered BOOLEAN,
    is_sent BOOLEAN DEFAULT TRUE,
    is_edited BOOLEAN DEFAULT FALSE,
    is_forwarded BOOLEAN DEFAULT FALSE,

    -- Size and attachment tracking
    size_estimate INTEGER,
    has_attachments BOOLEAN DEFAULT FALSE,
    attachment_count INTEGER DEFAULT 0,

    -- Soft delete tracking
    deleted_at DATETIME,
    deleted_from_source_at DATETIME,
    delete_batch_id TEXT,

    -- Archival info
    archived_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    indexing_version INTEGER DEFAULT 1,

    -- Row-level last-modified watermark, maintained ENTIRELY by the
    -- database (triggers below), never by application write paths. Used by
    -- the embed worker as an optimistic-CAS token: it captures this value
    -- when it reads a message's content and stamps embed_gen only if the
    -- value is unchanged at stamp time, so a concurrent content edit
    -- (e.g. repair-encoding) that lands between read and stamp leaves the
    -- row unstamped and it is re-embedded with the corrected content.
    last_modified DATETIME DEFAULT CURRENT_TIMESTAMP,

    -- Platform-specific metadata
    metadata JSON,

    -- Vector-embedding watermark: the index generation this message's
    -- embeddings were last written for. NULL means "needs embedding"
    -- (new rows default to NULL); a value equal to the active/building
    -- generation id means "covered". The scan-and-fill embed worker
    -- finds work via (embed_gen IS NULL OR embed_gen <> <target>) and
    -- stamps this column after a successful upsert (or skip).
    embed_gen INTEGER,

    UNIQUE(source_id, source_message_id)
);

-- Message recipients (To/Cc/Bcc for email, participants for group messages)
CREATE TABLE IF NOT EXISTS message_recipients (
    id INTEGER PRIMARY KEY,
    message_id INTEGER NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
    participant_id INTEGER NOT NULL REFERENCES participants(id) ON DELETE CASCADE,

    recipient_type TEXT NOT NULL,  -- 'to', 'cc', 'bcc', 'mention'
    display_name TEXT,             -- as it appeared in the message

    UNIQUE(message_id, participant_id, recipient_type)
);

-- ============================================================================
-- REACTIONS & INTERACTIONS
-- ============================================================================

-- Reactions (tapbacks, emoji reactions)
CREATE TABLE IF NOT EXISTS reactions (
    id INTEGER PRIMARY KEY,
    message_id INTEGER NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
    participant_id INTEGER NOT NULL REFERENCES participants(id) ON DELETE CASCADE,

    -- Reaction type and value
    reaction_type TEXT NOT NULL,  -- 'tapback', 'emoji', 'like'
    reaction_value TEXT NOT NULL, -- 'heart', 'thumbsup', etc. or emoji

    -- Apple tapback types: 'love', 'like', 'dislike', 'laugh', 'emphasis', 'question'

    created_at DATETIME,
    removed_at DATETIME,

    UNIQUE(message_id, participant_id, reaction_type, reaction_value)
);

-- ============================================================================
-- ATTACHMENTS & MEDIA
-- ============================================================================

-- Attachments (content-addressed storage)
CREATE TABLE IF NOT EXISTS attachments (
    id INTEGER PRIMARY KEY,
    message_id INTEGER NOT NULL REFERENCES messages(id) ON DELETE CASCADE,

    -- File identification
    filename TEXT,
    mime_type TEXT,
    size INTEGER,

    -- Content-addressed storage (deduplication)
    content_hash TEXT,              -- SHA-256 of content
    storage_path TEXT NOT NULL,     -- relative path: ab/abcd1234...

    -- Media metadata
    media_type TEXT,                -- 'image', 'video', 'audio', 'document', 'sticker', 'gif', 'voice_note'
    width INTEGER,
    height INTEGER,
    duration_ms INTEGER,            -- for audio/video

    -- Thumbnail (for images/videos)
    thumbnail_hash TEXT,
    thumbnail_path TEXT,

    -- Platform-specific
    source_attachment_id TEXT,      -- original ID from platform
    attachment_metadata JSON,       -- EXIF, etc.

    -- Encryption
    encryption_version INTEGER DEFAULT 0,

    created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

-- ============================================================================
-- LABELS & ORGANIZATION
-- ============================================================================

-- Labels (Gmail labels, user tags)
CREATE TABLE IF NOT EXISTS labels (
    id INTEGER PRIMARY KEY,
    source_id INTEGER REFERENCES sources(id) ON DELETE CASCADE,  -- NULL for user-created

    source_label_id TEXT,           -- Gmail label ID
    name TEXT NOT NULL,
    label_type TEXT,                -- 'system', 'user', 'auto'
    color TEXT,

    UNIQUE(source_id, name)
);

-- Message labels
CREATE TABLE IF NOT EXISTS message_labels (
    message_id INTEGER NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
    label_id INTEGER NOT NULL REFERENCES labels(id) ON DELETE CASCADE,

    PRIMARY KEY (message_id, label_id)
);

-- ============================================================================
-- RAW DATA STORAGE
-- ============================================================================

-- Message bodies (separated from messages to keep messages B-tree small)
CREATE TABLE IF NOT EXISTS message_bodies (
    message_id INTEGER PRIMARY KEY REFERENCES messages(id) ON DELETE CASCADE,
    body_text TEXT,
    body_html TEXT
);

-- ============================================================================
-- LAST-MODIFIED TRIGGERS
-- ============================================================================
-- messages.last_modified is bumped to CURRENT_TIMESTAMP on ANY change to a
-- message row OR any insert/update of its body row. This is a TRUE row-level
-- last-modified (blanket, not column-specific): the embed worker uses it as an
-- optimistic-CAS token, so it must move whenever any embeddable content could
-- have changed. No application write path bumps it manually — the database
-- owns it via these triggers. InitSchema re-execs schema.sql idempotently, so
-- `IF NOT EXISTS` makes these safe on both fresh and existing databases.

-- On messages: re-stamp last_modified after any UPDATE. The WHEN guard
-- (OLD.last_modified = NEW.last_modified) prevents infinite recursion: the
-- trigger's own UPDATE changes last_modified, so on the re-fire
-- OLD.last_modified <> NEW.last_modified and WHEN evaluates false, regardless
-- of the recursive_triggers pragma. It also yields to an explicit
-- last_modified write in the original UPDATE rather than clobbering it.
CREATE TRIGGER IF NOT EXISTS trg_messages_last_modified
AFTER UPDATE ON messages FOR EACH ROW
WHEN OLD.last_modified = NEW.last_modified
BEGIN
    UPDATE messages SET last_modified = CURRENT_TIMESTAMP WHERE id = NEW.id;
END;

-- On message_bodies: a body change must bump the parent message's
-- last_modified so the worker's CAS token covers body edits too.
CREATE TRIGGER IF NOT EXISTS trg_message_bodies_last_modified_upd
AFTER UPDATE ON message_bodies FOR EACH ROW
BEGIN
    UPDATE messages SET last_modified = CURRENT_TIMESTAMP WHERE id = NEW.message_id;
END;

CREATE TRIGGER IF NOT EXISTS trg_message_bodies_last_modified_ins
AFTER INSERT ON message_bodies FOR EACH ROW
BEGIN
    UPDATE messages SET last_modified = CURRENT_TIMESTAMP WHERE id = NEW.message_id;
END;

-- Original message data (for re-parsing/export)
CREATE TABLE IF NOT EXISTS message_raw (
    message_id INTEGER PRIMARY KEY REFERENCES messages(id) ON DELETE CASCADE,

    raw_data BLOB NOT NULL,
    raw_format TEXT NOT NULL,       -- 'mime', 'imessage_archive', 'whatsapp_json', 'rcs_json'

    compression TEXT DEFAULT 'zlib',
    encryption_version INTEGER DEFAULT 0
);

-- ============================================================================
-- SYNC STATE
-- ============================================================================

-- Sync runs (for debugging and resumability)
CREATE TABLE IF NOT EXISTS sync_runs (
    id INTEGER PRIMARY KEY,
    source_id INTEGER NOT NULL REFERENCES sources(id) ON DELETE CASCADE,

    started_at DATETIME NOT NULL,
    completed_at DATETIME,
    status TEXT DEFAULT 'running',  -- 'running', 'completed', 'failed', 'cancelled'

    messages_processed INTEGER DEFAULT 0,
    messages_added INTEGER DEFAULT 0,
    messages_updated INTEGER DEFAULT 0,
    errors_count INTEGER DEFAULT 0,

    error_message TEXT,
    cursor_before TEXT,
    cursor_after TEXT
);

-- Per-item sync outcomes, for diagnosing partial sync completion.
-- status='error' is actionable and contributes to sync_runs.errors_count.
-- status='skipped' records expected item churn, such as Gmail messages that
-- were deleted between a history/list response and raw-message fetch.
CREATE TABLE IF NOT EXISTS sync_run_items (
    id INTEGER PRIMARY KEY,
    sync_run_id INTEGER NOT NULL REFERENCES sync_runs(id) ON DELETE CASCADE,
    source_message_id TEXT NOT NULL,
    phase TEXT NOT NULL,
    status TEXT NOT NULL,
    error_kind TEXT NOT NULL,
    error_message TEXT NOT NULL,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

-- Sync checkpoints (for resumable imports)
CREATE TABLE IF NOT EXISTS sync_checkpoints (
    source_id INTEGER NOT NULL REFERENCES sources(id) ON DELETE CASCADE,
    checkpoint_type TEXT NOT NULL,  -- 'message_id', 'timestamp', 'page_token'
    checkpoint_value TEXT NOT NULL,

    updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,

    PRIMARY KEY (source_id, checkpoint_type)
);

-- Per-mailbox IMAP sync state from the last fully completed sync.
-- UIDVALIDITY/UIDNEXT let subsequent syncs skip mailboxes that have
-- not changed instead of re-enumerating every folder.
CREATE TABLE IF NOT EXISTS imap_folder_state (
    source_id INTEGER NOT NULL REFERENCES sources(id) ON DELETE CASCADE,
    mailbox TEXT NOT NULL,
    uidvalidity INTEGER NOT NULL,
    uidnext INTEGER NOT NULL,

    updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,

    PRIMARY KEY (source_id, mailbox)
);

-- Imported source items (files/objects already processed for resumable adapters)
CREATE TABLE IF NOT EXISTS source_import_items (
    id INTEGER PRIMARY KEY,
    source_id INTEGER NOT NULL REFERENCES sources(id) ON DELETE CASCADE,
    provider TEXT NOT NULL,
    provider_id TEXT NOT NULL,
    name TEXT NOT NULL,
    checksum TEXT,
    size INTEGER DEFAULT 0,
    modified_at DATETIME,
    imported_at DATETIME,
    status TEXT NOT NULL DEFAULT 'pending',
    records_imported INTEGER DEFAULT 0,
    error_message TEXT,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    UNIQUE(source_id, provider, provider_id)
);

-- ============================================================================
-- INDEXES
-- ============================================================================

-- Sources
CREATE INDEX IF NOT EXISTS idx_sources_type ON sources(source_type);

-- Participants
CREATE UNIQUE INDEX IF NOT EXISTS idx_participants_email ON participants(email_address)
    WHERE email_address IS NOT NULL;
-- idx_participants_phone is created (and upgraded from the legacy
-- non-unique form) in Go by Store.ensureParticipantsPhoneUniqueIndex
-- so existing DBs whose IF NOT EXISTS no-op'd the schema bump still
-- end up with a UNIQUE partial index.
CREATE INDEX IF NOT EXISTS idx_participants_canonical ON participants(canonical_id)
    WHERE canonical_id IS NOT NULL;

-- Participant identifiers
CREATE INDEX IF NOT EXISTS idx_participant_identifiers_value ON participant_identifiers(identifier_value);
CREATE INDEX IF NOT EXISTS idx_participant_identifiers_participant ON participant_identifiers(participant_id);

-- Conversations
CREATE INDEX IF NOT EXISTS idx_conversations_source ON conversations(source_id);
CREATE INDEX IF NOT EXISTS idx_conversations_last_message ON conversations(last_message_at DESC);
CREATE INDEX IF NOT EXISTS idx_conversations_type ON conversations(conversation_type);

-- Messages
CREATE INDEX IF NOT EXISTS idx_messages_conversation ON messages(conversation_id, sent_at DESC);
CREATE INDEX IF NOT EXISTS idx_messages_source ON messages(source_id);
CREATE INDEX IF NOT EXISTS idx_messages_sender ON messages(sender_id);
CREATE INDEX IF NOT EXISTS idx_messages_sent_at ON messages(sent_at DESC);
CREATE INDEX IF NOT EXISTS idx_messages_type ON messages(message_type);
CREATE INDEX IF NOT EXISTS idx_messages_deleted ON messages(source_id, deleted_from_source_at);
CREATE INDEX IF NOT EXISTS idx_messages_source_message_id ON messages(source_message_id);

-- Message recipients
CREATE INDEX IF NOT EXISTS idx_message_recipients_message ON message_recipients(message_id);
CREATE INDEX IF NOT EXISTS idx_message_recipients_participant ON message_recipients(participant_id, recipient_type);

-- Reactions
CREATE INDEX IF NOT EXISTS idx_reactions_message ON reactions(message_id);

-- Attachments
CREATE INDEX IF NOT EXISTS idx_attachments_message ON attachments(message_id);
CREATE INDEX IF NOT EXISTS idx_attachments_hash ON attachments(content_hash);
CREATE INDEX IF NOT EXISTS idx_attachments_content_hash_lower ON attachments(LOWER(content_hash));
CREATE INDEX IF NOT EXISTS idx_attachments_thumbnail_hash ON attachments(thumbnail_hash);
CREATE INDEX IF NOT EXISTS idx_attachments_thumbnail_hash_lower ON attachments(LOWER(thumbnail_hash));
CREATE INDEX IF NOT EXISTS idx_attachments_thumbnail_path ON attachments(thumbnail_path);
CREATE INDEX IF NOT EXISTS idx_attachments_storage_path ON attachments(storage_path);
-- The partial unique index on (message_id, content_hash) for
-- UpsertAttachment idempotency is created in Go (Store.InitSchema)
-- after a one-shot dedupe of legacy duplicate rows.

-- Labels
CREATE INDEX IF NOT EXISTS idx_labels_source ON labels(source_id);
CREATE INDEX IF NOT EXISTS idx_message_labels_label ON message_labels(label_id);

-- Sync
CREATE INDEX IF NOT EXISTS idx_sync_runs_source ON sync_runs(source_id, started_at DESC);
CREATE INDEX IF NOT EXISTS idx_sync_run_items_run_status
    ON sync_run_items(sync_run_id, status, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_source_import_items_source_provider
    ON source_import_items(source_id, provider, status);

-- ============================================================================
-- COLLECTIONS
-- ============================================================================

-- Collections (named groupings of sources treated as a single logical archive)
CREATE TABLE IF NOT EXISTS collections (
    id          INTEGER PRIMARY KEY,
    name        TEXT    NOT NULL UNIQUE,
    description TEXT    NOT NULL DEFAULT '',
    created_at  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

-- Collection membership (many sources per collection)
CREATE TABLE IF NOT EXISTS collection_sources (
    collection_id INTEGER NOT NULL REFERENCES collections(id) ON DELETE CASCADE,
    source_id     INTEGER NOT NULL REFERENCES sources(id)     ON DELETE CASCADE,
    PRIMARY KEY (collection_id, source_id)
);

CREATE INDEX IF NOT EXISTS idx_collection_sources_source_id
    ON collection_sources(source_id);

-- Daemon-owned analytical Saved Views. Canonical state contains only the
-- query/view definition; result rows and transient selection remain client
-- state and are never persisted here.
CREATE TABLE IF NOT EXISTS saved_views (
    id              INTEGER PRIMARY KEY,
    name            TEXT NOT NULL UNIQUE,
    description     TEXT,
    canonical_state JSON NOT NULL,
    schema_version  INTEGER NOT NULL,
    revision        INTEGER NOT NULL DEFAULT 1,
    created_at      DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at      DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

-- ============================================================================
-- ACCOUNT IDENTITIES
-- ============================================================================

-- Confirmed per-account "me" identities used by sent-message detection
-- in dedup. Identity is account-scoped: an address confirmed for one
-- source does not imply it is "me" in any other source.
CREATE TABLE IF NOT EXISTS account_identities (
    source_id    INTEGER NOT NULL REFERENCES sources(id) ON DELETE CASCADE,
    address      TEXT NOT NULL,             -- case-preserved
    source_signal TEXT NOT NULL DEFAULT '', -- sorted comma-separated signal set, e.g. 'manual' or 'account-identifier,manual'
    confirmed_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (source_id, address)
);

CREATE INDEX IF NOT EXISTS idx_account_identities_address
    ON account_identities(address);

-- User-asserted identity links between participants. Edges are normalized
-- (participant_a < participant_b) and the graph is kept a forest: every
-- edge joins two previously distinct clusters, so deleting an edge
-- deterministically splits one cluster in two. Connected components resolve
-- to a canonical cluster (smallest member ID) at read time.
CREATE TABLE IF NOT EXISTS participant_links (
    participant_a INTEGER NOT NULL REFERENCES participants(id) ON DELETE CASCADE,
    participant_b INTEGER NOT NULL REFERENCES participants(id) ON DELETE CASCADE,
    created_at    DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (participant_a, participant_b),
    CHECK (participant_a < participant_b)
);

CREATE INDEX IF NOT EXISTS idx_participant_links_b
    ON participant_links(participant_b);

-- ============================================================================
-- PORTABLE FIELD METADATA AND TYPED ATTRIBUTES
-- ============================================================================

-- Definitions are metadata rows: adding one never issues DDL. universal_id is
-- the stable external identity, slug is the immutable machine name, and label
-- is mutable human-facing text. There is deliberately no is_unique column:
-- only constraints backed by portable database indexes are advertised.
CREATE TABLE IF NOT EXISTS attribute_definitions (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    universal_id   TEXT NOT NULL UNIQUE,
    object_type    TEXT NOT NULL,
    slug           TEXT NOT NULL,
    label          TEXT NOT NULL,
    description    TEXT,
    value_type     TEXT NOT NULL,
    field_type     TEXT NOT NULL,
    record_target  TEXT,
    cardinality    TEXT NOT NULL DEFAULT 'single',
    display_order  INTEGER NOT NULL DEFAULT 0,
    is_required    BOOLEAN NOT NULL DEFAULT FALSE,
    ownership      TEXT NOT NULL DEFAULT 'user',
    ui_creatable   BOOLEAN NOT NULL DEFAULT TRUE,
    ui_editable    BOOLEAN NOT NULL DEFAULT TRUE,
    api_mutable    BOOLEAN NOT NULL DEFAULT TRUE,
    is_searchable  BOOLEAN NOT NULL DEFAULT FALSE,
    is_audited     BOOLEAN NOT NULL DEFAULT TRUE,
    is_deletable   BOOLEAN NOT NULL DEFAULT TRUE,
    history_exempt BOOLEAN NOT NULL DEFAULT FALSE,
    derived_source TEXT,
    options        JSON,
    vcard_property TEXT,
    is_active      BOOLEAN NOT NULL DEFAULT TRUE,
    revision       INTEGER NOT NULL DEFAULT 1,
    created_at     DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at     DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    CHECK (LENGTH(universal_id) > 0),
    CHECK (LENGTH(slug) > 0),
    CHECK (LENGTH(label) > 0),
    CHECK (display_order >= 0)
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_attribute_definitions_object_slug
    ON attribute_definitions(object_type, slug);

CREATE INDEX IF NOT EXISTS idx_attribute_definitions_active
    ON attribute_definitions(object_type, display_order, id)
    WHERE is_active = TRUE;

-- One row is one typed value over one world-time interval. active_from and
-- active_until describe when the fact was true; created_at and superseded_at
-- describe when Msgvault knew it. Current means both closing timestamps are
-- NULL, including future retractions that close only transaction time.
CREATE TABLE IF NOT EXISTS person_attribute_values (
    id                INTEGER PRIMARY KEY AUTOINCREMENT,
    person_id         INTEGER NOT NULL REFERENCES persons(id) ON DELETE CASCADE,
    definition_id     INTEGER NOT NULL
                          REFERENCES attribute_definitions(id) ON DELETE RESTRICT,
    ordinal           INTEGER NOT NULL DEFAULT 0,
    value_text        TEXT,
    value_integer     BIGINT,
    value_real        REAL,
    value_boolean     BOOLEAN,
    -- A complete YYYY-MM-DD date. Go validates digits and calendar validity;
    -- the portable CHECK below only pins separator positions and length.
    value_date        TEXT,
    value_timestamp   DATETIME,
    value_json        JSON,
    value_record_type TEXT,
    value_record_id   INTEGER,
    active_from       DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    active_until      DATETIME,
    created_at        DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    superseded_at     DATETIME,
    source            TEXT NOT NULL,
    source_ref        TEXT,
    confidence        REAL,
    actor             TEXT,
    CHECK (ordinal >= 0),
    CHECK (source IN ('user', 'carddav_import', 'vcard_import',
                      'archive_observation', 'extraction', 'enrichment',
                      'system')),
    CHECK (active_until IS NULL OR active_until >= active_from),
    CHECK (confidence IS NULL OR (confidence >= 0.0 AND confidence <= 1.0)),
    CHECK (value_date IS NULL OR value_date LIKE '____-__-__'),
    CHECK (
        (CASE WHEN value_text      IS NOT NULL THEN 1 ELSE 0 END) +
        (CASE WHEN value_integer   IS NOT NULL THEN 1 ELSE 0 END) +
        (CASE WHEN value_real      IS NOT NULL THEN 1 ELSE 0 END) +
        (CASE WHEN value_boolean   IS NOT NULL THEN 1 ELSE 0 END) +
        (CASE WHEN value_date      IS NOT NULL THEN 1 ELSE 0 END) +
        (CASE WHEN value_timestamp IS NOT NULL THEN 1 ELSE 0 END) +
        (CASE WHEN value_json      IS NOT NULL THEN 1 ELSE 0 END) +
        (CASE WHEN value_record_id IS NOT NULL THEN 1 ELSE 0 END) = 1
    ),
    CHECK ((value_record_id IS NULL) = (value_record_type IS NULL))
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_person_attribute_values_current
    ON person_attribute_values(person_id, definition_id, ordinal)
    WHERE active_until IS NULL AND superseded_at IS NULL;

CREATE INDEX IF NOT EXISTS idx_person_attribute_values_history
    ON person_attribute_values(person_id, definition_id, active_from DESC, id DESC);

CREATE INDEX IF NOT EXISTS idx_person_attribute_values_definition
    ON person_attribute_values(definition_id, active_until);

CREATE INDEX IF NOT EXISTS idx_person_attribute_values_current_text
    ON person_attribute_values(definition_id, value_text)
    WHERE value_text IS NOT NULL
      AND active_until IS NULL AND superseded_at IS NULL;

-- ============================================================================
-- APPLIED MIGRATIONS
-- ============================================================================

-- Marks one-time data migrations that have already run. Schema DDL is
-- idempotent via IF NOT EXISTS; this table is for *data* migrations
-- (e.g. moving legacy config into per-account records) that must run
-- exactly once.
CREATE TABLE IF NOT EXISTS applied_migrations (
    name        TEXT PRIMARY KEY,
    applied_at  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

-- Packed attachment storage (docs/internal/packed-attachments-design.md).
-- attachment_pack_index maps content-addressed blobs (attachment content and
-- thumbnails) to sealed pack files under attachments/packs/. Rows exist only
-- for live packed blobs; loose files have no row. pack_offset et al mirror
-- the pack footer's entry so reads need no footer parse ("offset" is a
-- reserved word in SQLite and PostgreSQL, hence the prefix).
CREATE TABLE IF NOT EXISTS attachment_pack_index (
    blob_hash   TEXT PRIMARY KEY,
    pack_id     TEXT NOT NULL,
    pack_offset BIGINT NOT NULL,
    stored_len  BIGINT NOT NULL,
    raw_len     BIGINT NOT NULL,
    flags       INTEGER NOT NULL,
    crc32c      BIGINT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_attachment_pack_index_pack
    ON attachment_pack_index(pack_id);

-- Immutable per-pack totals captured at seal/adoption. GC derives dead bytes
-- as stored_bytes minus the sum of the pack's live index rows.
CREATE TABLE IF NOT EXISTS attachment_packs (
    pack_id      TEXT PRIMARY KEY,
    entry_count  BIGINT NOT NULL,
    stored_bytes BIGINT NOT NULL,
    created_at   TEXT NOT NULL
);

-- ============================================================================
-- DATED ACTIVITY SPINE
-- ============================================================================

-- One row per archived communication projected into the date-indexed spine.
-- The native stable reference is derived as "<ref_kind>:<message_id>".
CREATE TABLE IF NOT EXISTS activity_events (
    message_id                         INTEGER PRIMARY KEY
                                                   REFERENCES messages(id) ON DELETE CASCADE,
    ref_kind                           TEXT NOT NULL
                                                   CHECK (ref_kind IN ('message', 'meeting')),
    source_id                          INTEGER NOT NULL
                                                   REFERENCES sources(id) ON DELETE CASCADE,
    conversation_id                    INTEGER
                                                   REFERENCES conversations(id) ON DELETE SET NULL,
    channel                            TEXT NOT NULL
                                                   CHECK (channel IN ('email', 'chat', 'meeting', 'other')),
    occurred_at                        DATETIME NOT NULL,
    date_origin                        TEXT NOT NULL
                                                   CHECK (date_origin IN ('sent_at', 'received_at', 'internal_date')),
    date_precision                     TEXT NOT NULL
                                                   CHECK (date_precision IN ('timestamp', 'day')),
    timezone                           TEXT NOT NULL CHECK (LENGTH(timezone) > 0),
    utc_offset_minutes                 INTEGER NOT NULL
                                                   CHECK (utc_offset_minutes BETWEEN -840 AND 840),
    local_date                         TEXT NOT NULL
                                                   CHECK (LENGTH(local_date) = 10
                                                       AND SUBSTR(local_date, 1, 1) IN ('0','1','2','3','4','5','6','7','8','9')
                                                       AND SUBSTR(local_date, 2, 1) IN ('0','1','2','3','4','5','6','7','8','9')
                                                       AND SUBSTR(local_date, 3, 1) IN ('0','1','2','3','4','5','6','7','8','9')
                                                       AND SUBSTR(local_date, 4, 1) IN ('0','1','2','3','4','5','6','7','8','9')
                                                       AND SUBSTR(local_date, 5, 1) = '-'
                                                       AND SUBSTR(local_date, 6, 1) IN ('0','1','2','3','4','5','6','7','8','9')
                                                       AND SUBSTR(local_date, 7, 1) IN ('0','1','2','3','4','5','6','7','8','9')
                                                       AND SUBSTR(local_date, 8, 1) = '-'
                                                       AND SUBSTR(local_date, 9, 1) IN ('0','1','2','3','4','5','6','7','8','9')
                                                       AND SUBSTR(local_date, 10, 1) IN ('0','1','2','3','4','5','6','7','8','9')),
    direction                          TEXT NOT NULL
                                                   CHECK (direction IN ('inbound', 'outbound', 'observed')),
    owner_source_id                    INTEGER
                                                   REFERENCES sources(id) ON DELETE SET NULL,
    owner_address                      TEXT NOT NULL DEFAULT '',
    projected_last_modified            DATETIME NOT NULL,
    projected_identity_revision        INTEGER NOT NULL
                                                   CHECK (projected_identity_revision >= 0),
    projected_account_identity_revision INTEGER NOT NULL
                                                   CHECK (projected_account_identity_revision >= 0),
    created_at                          DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_activity_events_local_date
    ON activity_events(local_date, occurred_at DESC);
CREATE INDEX IF NOT EXISTS idx_activity_events_source
    ON activity_events(source_id, occurred_at DESC);

-- Each person has at most one link to a native event. Classification stores
-- the strongest evidence and a deterministic representative role.
CREATE TABLE IF NOT EXISTS activity_event_persons (
    message_id INTEGER NOT NULL REFERENCES activity_events(message_id) ON DELETE CASCADE,
    person_id  INTEGER NOT NULL REFERENCES persons(id) ON DELETE CASCADE,
    role       TEXT NOT NULL
                    CHECK (role IN ('sender', 'addressed', 'organizer', 'attendee', 'member')),
    evidence   TEXT NOT NULL CHECK (evidence IN ('direct', 'co_presence')),
    local_date TEXT NOT NULL CHECK (
        LENGTH(local_date) = 10
        AND SUBSTR(local_date, 1, 1) IN ('0','1','2','3','4','5','6','7','8','9')
        AND SUBSTR(local_date, 2, 1) IN ('0','1','2','3','4','5','6','7','8','9')
        AND SUBSTR(local_date, 3, 1) IN ('0','1','2','3','4','5','6','7','8','9')
        AND SUBSTR(local_date, 4, 1) IN ('0','1','2','3','4','5','6','7','8','9')
        AND SUBSTR(local_date, 5, 1) = '-'
        AND SUBSTR(local_date, 6, 1) IN ('0','1','2','3','4','5','6','7','8','9')
        AND SUBSTR(local_date, 7, 1) IN ('0','1','2','3','4','5','6','7','8','9')
        AND SUBSTR(local_date, 8, 1) = '-'
        AND SUBSTR(local_date, 9, 1) IN ('0','1','2','3','4','5','6','7','8','9')
        AND SUBSTR(local_date, 10, 1) IN ('0','1','2','3','4','5','6','7','8','9')
    ),
    PRIMARY KEY (message_id, person_id)
);

CREATE INDEX IF NOT EXISTS idx_activity_event_persons_person_date
    ON activity_event_persons(person_id, local_date, message_id);
CREATE INDEX IF NOT EXISTS idx_activity_event_persons_date_person
    ON activity_event_persons(local_date, person_id, message_id);

CREATE TABLE IF NOT EXISTS person_contact_state (
    person_id                 INTEGER PRIMARY KEY REFERENCES persons(id) ON DELETE CASCADE,
    first_contact_at          DATETIME,
    first_contact_message_id  INTEGER,
    last_contact_at           DATETIME,
    last_contact_message_id   INTEGER,
    last_contact_channel      TEXT
                                      CHECK (last_contact_channel IS NULL
                                          OR last_contact_channel IN ('email', 'chat', 'meeting', 'other')),
    last_contact_source_id    INTEGER,
    last_contact_owner        TEXT,
    last_inbound_at           DATETIME,
    last_inbound_message_id   INTEGER,
    last_outbound_at          DATETIME,
    last_outbound_message_id  INTEGER,
    interaction_count         INTEGER NOT NULL DEFAULT 0 CHECK (interaction_count >= 0),
    identity_revision         INTEGER NOT NULL DEFAULT 0 CHECK (identity_revision >= 0),
    account_identity_revision INTEGER NOT NULL DEFAULT 0
                                      CHECK (account_identity_revision >= 0),
    dirty_at                  DATETIME,
    computed_at               DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_person_contact_state_dirty
    ON person_contact_state(person_id) WHERE dirty_at IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_person_contact_state_last_contact
    ON person_contact_state(last_contact_at DESC);

-- ============================================================================
-- AUTHORED DAILY NOTES
-- ============================================================================

CREATE TABLE IF NOT EXISTS daily_note_day_sequences (
    local_date TEXT NOT NULL PRIMARY KEY CHECK (
        LENGTH(local_date) = 10
        AND SUBSTR(local_date, 1, 1) IN ('0','1','2','3','4','5','6','7','8','9')
        AND SUBSTR(local_date, 2, 1) IN ('0','1','2','3','4','5','6','7','8','9')
        AND SUBSTR(local_date, 3, 1) IN ('0','1','2','3','4','5','6','7','8','9')
        AND SUBSTR(local_date, 4, 1) IN ('0','1','2','3','4','5','6','7','8','9')
        AND SUBSTR(local_date, 5, 1) = '-'
        AND SUBSTR(local_date, 6, 1) IN ('0','1','2','3','4','5','6','7','8','9')
        AND SUBSTR(local_date, 7, 1) IN ('0','1','2','3','4','5','6','7','8','9')
        AND SUBSTR(local_date, 8, 1) = '-'
        AND SUBSTR(local_date, 9, 1) IN ('0','1','2','3','4','5','6','7','8','9')
        AND SUBSTR(local_date, 10, 1) IN ('0','1','2','3','4','5','6','7','8','9')
    ),
    last_ordinal INTEGER NOT NULL CHECK (last_ordinal > 0)
);

CREATE TABLE IF NOT EXISTS daily_note_entries (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    local_date TEXT NOT NULL CHECK (
        LENGTH(local_date) = 10
        AND SUBSTR(local_date, 1, 1) IN ('0','1','2','3','4','5','6','7','8','9')
        AND SUBSTR(local_date, 2, 1) IN ('0','1','2','3','4','5','6','7','8','9')
        AND SUBSTR(local_date, 3, 1) IN ('0','1','2','3','4','5','6','7','8','9')
        AND SUBSTR(local_date, 4, 1) IN ('0','1','2','3','4','5','6','7','8','9')
        AND SUBSTR(local_date, 5, 1) = '-'
        AND SUBSTR(local_date, 6, 1) IN ('0','1','2','3','4','5','6','7','8','9')
        AND SUBSTR(local_date, 7, 1) IN ('0','1','2','3','4','5','6','7','8','9')
        AND SUBSTR(local_date, 8, 1) = '-'
        AND SUBSTR(local_date, 9, 1) IN ('0','1','2','3','4','5','6','7','8','9')
        AND SUBSTR(local_date, 10, 1) IN ('0','1','2','3','4','5','6','7','8','9')
    ),
    ordinal    INTEGER NOT NULL CHECK (ordinal > 0),
    body       TEXT NOT NULL,
    author     TEXT NOT NULL DEFAULT '',
    source     TEXT NOT NULL DEFAULT 'user' CHECK (source = 'user'),
    source_ref TEXT,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE(local_date, ordinal)
);

CREATE INDEX IF NOT EXISTS idx_daily_note_entries_date
    ON daily_note_entries(local_date, ordinal, id);

CREATE TABLE IF NOT EXISTS daily_note_entry_persons (
    entry_id  INTEGER NOT NULL REFERENCES daily_note_entries(id) ON DELETE CASCADE,
    person_id INTEGER NOT NULL REFERENCES persons(id) ON DELETE CASCADE,
    PRIMARY KEY (entry_id, person_id)
);

CREATE INDEX IF NOT EXISTS idx_daily_note_entry_persons_person
    ON daily_note_entry_persons(person_id, entry_id);

-- Mutations enqueue the native message rather than relying on a timestamp
-- watermark. The monotone revision lets the projector consume with CAS.
CREATE TABLE IF NOT EXISTS activity_projection_queue (
    message_id         INTEGER PRIMARY KEY REFERENCES messages(id) ON DELETE CASCADE,
    revision           INTEGER NOT NULL DEFAULT 1 CHECK (revision >= 1),
    processed_revision INTEGER NOT NULL DEFAULT 0
                               CHECK (processed_revision >= 0
                                  AND processed_revision <= revision),
    queued_at           DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_activity_projection_queue_pending
    ON activity_projection_queue(message_id)
    WHERE revision > processed_revision;

CREATE TRIGGER IF NOT EXISTS trg_activity_queue_messages_insert
AFTER INSERT ON messages FOR EACH ROW
BEGIN
    INSERT INTO activity_projection_queue (message_id, revision, queued_at)
    VALUES (NEW.id, 1, CURRENT_TIMESTAMP)
    ON CONFLICT(message_id) DO UPDATE SET
        revision = activity_projection_queue.revision + 1,
        queued_at = CURRENT_TIMESTAMP;
END;

CREATE TRIGGER IF NOT EXISTS trg_activity_queue_messages_update
AFTER UPDATE ON messages FOR EACH ROW
BEGIN
    INSERT INTO activity_projection_queue (message_id, revision, queued_at)
    VALUES (NEW.id, 1, CURRENT_TIMESTAMP)
    ON CONFLICT(message_id) DO UPDATE SET
        revision = activity_projection_queue.revision + 1,
        queued_at = CURRENT_TIMESTAMP;
END;

CREATE TRIGGER IF NOT EXISTS trg_activity_queue_recipients_insert
AFTER INSERT ON message_recipients FOR EACH ROW
BEGIN
    INSERT INTO activity_projection_queue (message_id, revision, queued_at)
    VALUES (NEW.message_id, 1, CURRENT_TIMESTAMP)
    ON CONFLICT(message_id) DO UPDATE SET
        revision = activity_projection_queue.revision + 1,
        queued_at = CURRENT_TIMESTAMP;
END;

CREATE TRIGGER IF NOT EXISTS trg_activity_queue_recipients_update
AFTER UPDATE ON message_recipients FOR EACH ROW
BEGIN
    INSERT INTO activity_projection_queue (message_id, revision, queued_at)
    SELECT id, 1, CURRENT_TIMESTAMP
    FROM messages
    WHERE id = OLD.message_id
    ON CONFLICT(message_id) DO UPDATE SET
        revision = activity_projection_queue.revision + 1,
        queued_at = CURRENT_TIMESTAMP;
    INSERT INTO activity_projection_queue (message_id, revision, queued_at)
    VALUES (NEW.message_id, 1, CURRENT_TIMESTAMP)
    ON CONFLICT(message_id) DO UPDATE SET
        revision = activity_projection_queue.revision + 1,
        queued_at = CURRENT_TIMESTAMP;
END;

CREATE TRIGGER IF NOT EXISTS trg_activity_queue_recipients_delete
AFTER DELETE ON message_recipients FOR EACH ROW
BEGIN
    INSERT INTO activity_projection_queue (message_id, revision, queued_at)
    SELECT id, 1, CURRENT_TIMESTAMP
    FROM messages
    WHERE id = OLD.message_id
    ON CONFLICT(message_id) DO UPDATE SET
        revision = activity_projection_queue.revision + 1,
        queued_at = CURRENT_TIMESTAMP;
END;

CREATE TRIGGER IF NOT EXISTS trg_activity_queue_conversation_people_insert
AFTER INSERT ON conversation_participants FOR EACH ROW
BEGIN
    INSERT INTO activity_projection_queue (message_id, revision, queued_at)
    SELECT id, 1, CURRENT_TIMESTAMP
    FROM messages
    WHERE conversation_id = NEW.conversation_id
    ON CONFLICT(message_id) DO UPDATE SET
        revision = activity_projection_queue.revision + 1,
        queued_at = CURRENT_TIMESTAMP;
END;

CREATE TRIGGER IF NOT EXISTS trg_activity_queue_conversation_people_update
AFTER UPDATE ON conversation_participants FOR EACH ROW
BEGIN
    INSERT INTO activity_projection_queue (message_id, revision, queued_at)
    SELECT id, 1, CURRENT_TIMESTAMP
    FROM messages
    WHERE conversation_id = OLD.conversation_id
    ON CONFLICT(message_id) DO UPDATE SET
        revision = activity_projection_queue.revision + 1,
        queued_at = CURRENT_TIMESTAMP;
    INSERT INTO activity_projection_queue (message_id, revision, queued_at)
    SELECT id, 1, CURRENT_TIMESTAMP
    FROM messages
    WHERE conversation_id = NEW.conversation_id
    ON CONFLICT(message_id) DO UPDATE SET
        revision = activity_projection_queue.revision + 1,
        queued_at = CURRENT_TIMESTAMP;
END;

CREATE TRIGGER IF NOT EXISTS trg_activity_queue_conversation_people_delete
AFTER DELETE ON conversation_participants FOR EACH ROW
BEGIN
    INSERT INTO activity_projection_queue (message_id, revision, queued_at)
    SELECT id, 1, CURRENT_TIMESTAMP
    FROM messages
    WHERE conversation_id = OLD.conversation_id
    ON CONFLICT(message_id) DO UPDATE SET
        revision = activity_projection_queue.revision + 1,
        queued_at = CURRENT_TIMESTAMP;
END;

-- Conversation type is classification input: changing email/chat/channel
-- semantics must reproject every native message already in the conversation.
CREATE TRIGGER IF NOT EXISTS trg_activity_queue_conversation_type_update
AFTER UPDATE OF conversation_type ON conversations FOR EACH ROW
WHEN OLD.conversation_type IS NOT NEW.conversation_type
BEGIN
    INSERT INTO activity_projection_queue (message_id, revision, queued_at)
    SELECT id, 1, CURRENT_TIMESTAMP
    FROM messages
    WHERE conversation_id IN (OLD.id, NEW.id)
    ON CONFLICT(message_id) DO UPDATE SET
        revision = activity_projection_queue.revision + 1,
        queued_at = CURRENT_TIMESTAMP;
END;

-- Cascades can remove direct evidence without passing through the projector.
-- Dirty only an existing row: person deletion must never resurrect state.
CREATE TRIGGER IF NOT EXISTS trg_activity_direct_link_delete_dirty
AFTER DELETE ON activity_event_persons FOR EACH ROW
WHEN OLD.evidence = 'direct'
BEGIN
    UPDATE person_contact_state
    SET dirty_at = CURRENT_TIMESTAMP
    WHERE person_id = OLD.person_id;
END;
