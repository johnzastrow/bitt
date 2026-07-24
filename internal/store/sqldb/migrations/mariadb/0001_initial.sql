-- 0001_initial (MariaDB): the SQLite schema of the same number, translated.
--
-- This file exists because DEPLOY-03 offers MariaDB as a second backend. It is
-- deliberately a hand translation rather than a generated one: the table names,
-- column names, and trigger names are IDENTICAL to the SQLite version, because
-- the Go query code and the trigger-policy list in sqldb.go address them by
-- name and must not know which backend answered.
--
-- The choices that differ from SQLite, and why:
--
--   * utf8mb4 with the BINARY collation. SQLite compares strings byte-for-byte;
--     MySQL's default collation is case-INSENSITIVE, under which two period
--     keys or two idempotency keys differing only in case would collide -- and
--     those keys are the entire double-charge guard. Binary collation restores
--     the byte comparison the schema was designed against.
--   * VARCHAR, not TEXT, for any indexed or keyed string. MySQL cannot index a
--     TEXT column without a prefix length, and every key here needs a full
--     index. Lengths are generous; none of this data is large text.
--   * Timestamps are VARCHAR(32) holding the same ISO-8601 UTC strings SQLite
--     stores, so they still compare lexicographically and a row copied between
--     backends means the same instant.
--   * SIGNAL SQLSTATE '45000' replaces RAISE(ABORT, ...) in the append-only
--     triggers, with the SAME message text -- "...append-only..." -- so error
--     translation catches it identically on both backends.

CREATE TABLE instance (
    id                  INT          NOT NULL PRIMARY KEY CHECK (id = 1),
    timezone            VARCHAR(64)  NOT NULL DEFAULT 'UTC',
    setup_completed_at  VARCHAR(32)  NULL,
    created_at          VARCHAR(32)  NOT NULL
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;

-- The same ISO-8601 millisecond string SQLite's strftime produces. LEFT(...,23)
-- trims MySQL's six-digit microseconds to three, and CONCAT appends the Z.
INSERT INTO instance (id, timezone, setup_completed_at, created_at)
VALUES (1, 'UTC', NULL, CONCAT(LEFT(DATE_FORMAT(UTC_TIMESTAMP(3), '%Y-%m-%dT%H:%i:%s.%f'), 23), 'Z'));

CREATE TABLE users (
    id              BIGINT       NOT NULL AUTO_INCREMENT PRIMARY KEY,
    email           VARCHAR(320) NOT NULL,
    email_folded    VARCHAR(320) NOT NULL,
    display_name    VARCHAR(255) NOT NULL,
    password_hash   VARCHAR(255) NOT NULL,
    is_admin        TINYINT      NOT NULL DEFAULT 0 CHECK (is_admin IN (0, 1)),
    created_at      VARCHAR(32)  NOT NULL,
    deactivated_at  VARCHAR(32)  NULL
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;

CREATE UNIQUE INDEX idx_users_email_folded ON users (email_folded);

CREATE TABLE sessions (
    token_hash   VARCHAR(64)  NOT NULL PRIMARY KEY,
    user_id      BIGINT       NOT NULL REFERENCES users (id),
    created_at   VARCHAR(32)  NOT NULL,
    expires_at   VARCHAR(32)  NOT NULL,
    last_seen_at VARCHAR(32)  NOT NULL,
    CONSTRAINT fk_sessions_user FOREIGN KEY (user_id) REFERENCES users (id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;

CREATE INDEX idx_sessions_user ON sessions (user_id);
CREATE INDEX idx_sessions_expires ON sessions (expires_at);

CREATE TABLE tabs (
    id           BIGINT        NOT NULL AUTO_INCREMENT PRIMARY KEY,
    name         VARCHAR(255)  NOT NULL,
    kind         VARCHAR(20)   NOT NULL CHECK (kind IN ('services', 'payoff')),
    description  VARCHAR(1000) NOT NULL DEFAULT '',
    created_by   BIGINT        NOT NULL,
    created_at   VARCHAR(32)   NOT NULL,
    archived_at  VARCHAR(32)   NULL,
    CONSTRAINT fk_tabs_creator FOREIGN KEY (created_by) REFERENCES users (id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;

CREATE INDEX idx_tabs_archived ON tabs (archived_at);

CREATE TABLE tab_participants (
    tab_id    BIGINT      NOT NULL,
    user_id   BIGINT      NOT NULL,
    role      VARCHAR(20) NOT NULL CHECK (role IN ('provider', 'payee')),
    added_at  VARCHAR(32) NOT NULL,
    PRIMARY KEY (tab_id, user_id),
    CONSTRAINT fk_participants_tab  FOREIGN KEY (tab_id)  REFERENCES tabs (id)  ON DELETE CASCADE,
    CONSTRAINT fk_participants_user FOREIGN KEY (user_id) REFERENCES users (id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;

CREATE INDEX idx_participants_user ON tab_participants (user_id);

CREATE TABLE tab_items (
    id            BIGINT       NOT NULL AUTO_INCREMENT PRIMARY KEY,
    tab_id        BIGINT       NOT NULL,
    name          VARCHAR(255) NOT NULL,
    amount_cents  BIGINT       NOT NULL,
    position      INT          NOT NULL DEFAULT 0,
    created_at    VARCHAR(32)  NOT NULL,
    removed_at    VARCHAR(32)  NULL,
    CONSTRAINT fk_items_tab FOREIGN KEY (tab_id) REFERENCES tabs (id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;

CREATE INDEX idx_items_tab ON tab_items (tab_id, position);

CREATE TABLE entries (
    seq              BIGINT        NOT NULL AUTO_INCREMENT PRIMARY KEY,
    tab_id           BIGINT        NOT NULL,
    kind             VARCHAR(20)   NOT NULL CHECK (kind IN ('charge', 'payment', 'adjustment', 'fee', 'reversal')),
    amount_cents     BIGINT        NOT NULL,
    memo             VARCHAR(1000) NOT NULL DEFAULT '',
    effective_at     VARCHAR(32)   NOT NULL,
    created_at       VARCHAR(32)   NOT NULL,
    actor_user_id    BIGINT        NOT NULL,
    -- The web layer derives per-form keys by suffixing a 64-hex-char base
    -- ("...-charge", "...-adjustment"), so the stored value runs to ~75 chars.
    -- 100 leaves headroom. Getting this wrong is not a cosmetic truncation:
    -- under STRICT_ALL_TABLES an over-long key is rejected outright (which is
    -- how this width was found), and without STRICT two distinct keys could
    -- truncate to one and silently drop a charge.
    idempotency_key  VARCHAR(100)  NOT NULL,
    reverses_seq     BIGINT        NULL,
    CONSTRAINT fk_entries_tab     FOREIGN KEY (tab_id)        REFERENCES tabs (id),
    CONSTRAINT fk_entries_actor   FOREIGN KEY (actor_user_id) REFERENCES users (id),
    CONSTRAINT fk_entries_reverse FOREIGN KEY (reverses_seq)  REFERENCES entries (seq)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;

CREATE UNIQUE INDEX idx_entries_idempotency ON entries (idempotency_key);
CREATE INDEX idx_entries_tab_seq ON entries (tab_id, seq);
CREATE INDEX idx_entries_tab_effective ON entries (tab_id, effective_at);

-- A reversal may be issued once against a given entry. Both backends permit
-- repeated NULLs under a UNIQUE index, which gives the intended semantics
-- without a partial index (DEPLOY-02).
CREATE UNIQUE INDEX idx_entries_reverses ON entries (reverses_seq);

CREATE TABLE entry_items (
    entry_seq     BIGINT       NOT NULL,
    position      INT          NOT NULL,
    name          VARCHAR(255) NOT NULL,
    amount_cents  BIGINT       NOT NULL,
    PRIMARY KEY (entry_seq, position),
    CONSTRAINT fk_entry_items_entry FOREIGN KEY (entry_seq) REFERENCES entries (seq) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;

-- ---------------------------------------------------------------------------
-- LEDGER-01: append-only enforcement.
--
-- SIGNAL SQLSTATE '45000' is the MariaDB equivalent of SQLite's RAISE(ABORT).
-- The message text is identical so error translation treats both the same, and
-- unlike SQLite MariaDB can additionally REVOKE UPDATE and DELETE from the
-- application role -- a defence the SQLite build has no privilege system to
-- offer. See docs/DEPLOY.md.
--
-- DELIMITER is client-side syntax the migration splitter understands; it never
-- reaches the server. It is needed because a trigger body contains the
-- semicolons that would otherwise end the statement early.
-- ---------------------------------------------------------------------------
DELIMITER $$

CREATE TRIGGER entries_no_update BEFORE UPDATE ON entries FOR EACH ROW
BEGIN
    SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'entries are append-only: UPDATE is not permitted';
END$$

CREATE TRIGGER entries_no_delete BEFORE DELETE ON entries FOR EACH ROW
BEGIN
    SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'entries are append-only: DELETE is not permitted';
END$$

CREATE TRIGGER entry_items_no_update BEFORE UPDATE ON entry_items FOR EACH ROW
BEGIN
    SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'entry items are append-only: UPDATE is not permitted';
END$$

CREATE TRIGGER entry_items_no_delete BEFORE DELETE ON entry_items FOR EACH ROW
BEGIN
    SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'entry items are append-only: DELETE is not permitted';
END$$

DELIMITER ;
