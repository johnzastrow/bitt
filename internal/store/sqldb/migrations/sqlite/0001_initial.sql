-- 0001_initial: users, sessions, instance, tabs, items, and the ledger.
--
-- Portability (DEPLOY-02): column types and constraints here are chosen to
-- translate directly to MariaDB in Phase 5. No SQLite-only behavior is relied
-- upon -- in particular there is no use of UPDATE ... RETURNING, no dynamic
-- typing tricks, and no rowid aliasing beyond the explicit AUTOINCREMENT keys.
-- Timestamps are stored as ISO-8601 UTC text so they compare lexicographically
-- and survive a dialect change without reinterpretation.

-- ---------------------------------------------------------------------------
-- instance: exactly one row, guarded by a CHECK so a second cannot be inserted.
-- AUTH-03: setup_completed_at latches the first-run screen closed permanently.
-- ---------------------------------------------------------------------------
CREATE TABLE instance (
    id                  INTEGER PRIMARY KEY CHECK (id = 1),
    timezone            TEXT    NOT NULL DEFAULT 'UTC',
    setup_completed_at  TEXT,
    created_at          TEXT    NOT NULL
);

INSERT INTO instance (id, timezone, setup_completed_at, created_at)
VALUES (1, 'UTC', NULL, strftime('%Y-%m-%dT%H:%M:%fZ', 'now'));

-- ---------------------------------------------------------------------------
-- users
-- AUTH-01: password_hash holds a full Argon2id PHC string, never a bare digest.
-- email_folded is the case-folded form carrying the uniqueness constraint, so
-- lookups are case-insensitive without depending on a dialect's collation.
-- ---------------------------------------------------------------------------
CREATE TABLE users (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    email           TEXT    NOT NULL,
    email_folded    TEXT    NOT NULL,
    display_name    TEXT    NOT NULL,
    password_hash   TEXT    NOT NULL,
    is_admin        INTEGER NOT NULL DEFAULT 0 CHECK (is_admin IN (0, 1)),
    created_at      TEXT    NOT NULL,
    deactivated_at  TEXT
);

CREATE UNIQUE INDEX idx_users_email_folded ON users (email_folded);

-- ---------------------------------------------------------------------------
-- sessions
-- AUTH-02: token_hash stores a SHA-256 of the session token, never the token
-- itself, so a database read cannot be replayed as a login.
-- ---------------------------------------------------------------------------
CREATE TABLE sessions (
    token_hash   TEXT    NOT NULL PRIMARY KEY,
    user_id      INTEGER NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    created_at   TEXT    NOT NULL,
    expires_at   TEXT    NOT NULL,
    last_seen_at TEXT    NOT NULL
);

CREATE INDEX idx_sessions_user ON sessions (user_id);
CREATE INDEX idx_sessions_expires ON sessions (expires_at);

-- ---------------------------------------------------------------------------
-- tabs
-- TAB-01: 'services' is recurring with no end. 'payoff' arrives in Phase 4;
-- the CHECK admits it now so that adding it needs no schema change.
-- No balance column exists here, by design (LEDGER-03).
-- ---------------------------------------------------------------------------
CREATE TABLE tabs (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    name         TEXT    NOT NULL,
    kind         TEXT    NOT NULL CHECK (kind IN ('services', 'payoff')),
    description  TEXT    NOT NULL DEFAULT '',
    created_by   INTEGER NOT NULL REFERENCES users (id),
    created_at   TEXT    NOT NULL,
    archived_at  TEXT
);

CREATE INDEX idx_tabs_archived ON tabs (archived_at);

-- ---------------------------------------------------------------------------
-- tab_participants
-- AUTH-05 (Phase 2) authorizes against this table. It exists from Phase 1 so
-- the creating Provider is recorded from the first tab rather than inferred.
-- ---------------------------------------------------------------------------
CREATE TABLE tab_participants (
    tab_id    INTEGER NOT NULL REFERENCES tabs (id) ON DELETE CASCADE,
    user_id   INTEGER NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    role      TEXT    NOT NULL CHECK (role IN ('provider', 'payee')),
    added_at  TEXT    NOT NULL,
    PRIMARY KEY (tab_id, user_id)
);

CREATE INDEX idx_participants_user ON tab_participants (user_id);

-- ---------------------------------------------------------------------------
-- tab_items
-- TAB-04: each item carries its own amount; the items sum to the tab's
-- periodic charge. TAB-05: items hold no balance and are never settled
-- individually -- there is deliberately no payment or allocation column here.
-- ---------------------------------------------------------------------------
CREATE TABLE tab_items (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    tab_id        INTEGER NOT NULL REFERENCES tabs (id) ON DELETE CASCADE,
    name          TEXT    NOT NULL,
    amount_cents  INTEGER NOT NULL,
    position      INTEGER NOT NULL DEFAULT 0,
    created_at    TEXT    NOT NULL,
    removed_at    TEXT
);

CREATE INDEX idx_items_tab ON tab_items (tab_id, position);

-- ---------------------------------------------------------------------------
-- entries: the ledger. This table is append-only (LEDGER-01).
--
-- seq         LEDGER-06, the server-assigned monotonic sequence that defines
--             authoritative ordering independent of any client clock.
-- amount_cents LEDGER-04, integer cents. Negative increases what is owed,
--             positive reduces it. There is no float column here or anywhere.
-- effective_at / created_at   LEDGER-05, when it applies vs. when it was recorded.
-- actor_user_id LEDGER-05, who recorded it.
-- idempotency_key  Carries a UNIQUE constraint from Phase 1 even though
--             LEDGER-07 formally lands in Phase 2, because PROJECT.md commits
--             to writes being idempotent from the first commit. Retrofitting
--             this across an existing write path is the expensive version.
-- reverses_seq LEDGER-02, a correction points at the entry it reverses.
-- ---------------------------------------------------------------------------
CREATE TABLE entries (
    seq              INTEGER PRIMARY KEY AUTOINCREMENT,
    tab_id           INTEGER NOT NULL REFERENCES tabs (id),
    kind             TEXT    NOT NULL CHECK (kind IN ('charge', 'payment', 'adjustment', 'fee', 'reversal')),
    amount_cents     INTEGER NOT NULL,
    memo             TEXT    NOT NULL DEFAULT '',
    effective_at     TEXT    NOT NULL,
    created_at       TEXT    NOT NULL,
    actor_user_id    INTEGER NOT NULL REFERENCES users (id),
    idempotency_key  TEXT    NOT NULL,
    reverses_seq     INTEGER REFERENCES entries (seq)
);

CREATE UNIQUE INDEX idx_entries_idempotency ON entries (idempotency_key);
CREATE INDEX idx_entries_tab_seq ON entries (tab_id, seq);
CREATE INDEX idx_entries_tab_effective ON entries (tab_id, effective_at);

-- A reversal may only be issued once against a given entry, so a double-tapped
-- undo cannot post two offsetting corrections. A plain UNIQUE index is used
-- rather than a partial one: both SQLite and MariaDB permit repeated NULLs
-- under UNIQUE, which gives the intended semantics without the partial-index
-- syntax that MariaDB lacks (DEPLOY-02).
CREATE UNIQUE INDEX idx_entries_reverses ON entries (reverses_seq);

-- ---------------------------------------------------------------------------
-- entry_items: LEDGER/CHG snapshot of the item breakdown at post time.
-- CHG-01 (Phase 3) renders cost changes from these rows. Written from Phase 1
-- so that history is complete rather than beginning partway through.
-- ---------------------------------------------------------------------------
CREATE TABLE entry_items (
    entry_seq     INTEGER NOT NULL REFERENCES entries (seq) ON DELETE CASCADE,
    position      INTEGER NOT NULL,
    name          TEXT    NOT NULL,
    amount_cents  INTEGER NOT NULL,
    PRIMARY KEY (entry_seq, position)
);

-- ---------------------------------------------------------------------------
-- LEDGER-01: append-only enforcement.
--
-- These triggers are the guardrail against an application bug, not a barrier
-- against an operator holding the database file -- SQLite has no privilege
-- system, so nothing at this layer can bind someone with write access to the
-- file. In Phase 5, MariaDB gains an equivalent SIGNAL SQLSTATE trigger and can
-- additionally revoke UPDATE and DELETE from the application role.
--
-- Disableable via configuration for development and manual repair; see
-- config.LedgerTriggers.
-- ---------------------------------------------------------------------------
CREATE TRIGGER entries_no_update
BEFORE UPDATE ON entries
BEGIN
    SELECT RAISE(ABORT, 'entries are append-only: UPDATE is not permitted');
END;

CREATE TRIGGER entries_no_delete
BEFORE DELETE ON entries
BEGIN
    SELECT RAISE(ABORT, 'entries are append-only: DELETE is not permitted');
END;

CREATE TRIGGER entry_items_no_update
BEFORE UPDATE ON entry_items
BEGIN
    SELECT RAISE(ABORT, 'entry items are append-only: UPDATE is not permitted');
END;

CREATE TRIGGER entry_items_no_delete
BEFORE DELETE ON entry_items
BEGIN
    SELECT RAISE(ABORT, 'entry items are append-only: DELETE is not permitted');
END;
