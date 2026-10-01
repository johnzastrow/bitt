-- 0016_bank_matches (MariaDB): confirmed matches between a bank line and a
-- payment. See the SQLite migration of the same number, above all for how
-- "one active match per line and per payment" is enforced without partial
-- unique indexes.
CREATE TABLE bank_matches (
    id                BIGINT       NOT NULL AUTO_INCREMENT PRIMARY KEY,
    line_id           BIGINT       NOT NULL,
    import_id         BIGINT       NOT NULL,
    entry_seq         BIGINT       NOT NULL,
    tab_id            BIGINT       NOT NULL,
    delta_entry_seq   BIGINT       NULL,
    bank_date         CHAR(10)     NOT NULL CHECK (CHAR_LENGTH(bank_date) = 10),
    bank_cents        BIGINT       NOT NULL CHECK (bank_cents > 0),
    recorded_date     CHAR(10)     NOT NULL CHECK (CHAR_LENGTH(recorded_date) = 10),
    recorded_cents    BIGINT       NOT NULL CHECK (recorded_cents > 0),
    date_flagged      TINYINT      NOT NULL CHECK (date_flagged IN (0, 1)),
    date_flag_reason  VARCHAR(255) NOT NULL DEFAULT '',
    note_in_memo      TINYINT      NOT NULL CHECK (note_in_memo IN (0, 1)),
    confirmed_by      BIGINT       NOT NULL,
    confirmed_at      VARCHAR(32)  NOT NULL,
    undone_at         VARCHAR(32)  NOT NULL DEFAULT '',
    undone_by         BIGINT       NULL,
    active_line_id    BIGINT       NULL,
    active_entry_seq  BIGINT       NULL,
    UNIQUE KEY uq_bank_matches_active_line (active_line_id),
    UNIQUE KEY uq_bank_matches_active_entry (active_entry_seq),
    KEY idx_bank_matches_tab (tab_id, id),
    KEY idx_bank_matches_import (import_id, line_id),
    KEY idx_bank_matches_line (line_id),
    CONSTRAINT chk_bank_matches_active CHECK (
        -- <=> is NULL-safe: "NULL = line_id" is unknown, which a CHECK passes.
        (undone_at = '' AND active_line_id <=> line_id AND active_entry_seq <=> entry_seq)
     OR (undone_at <> '' AND active_line_id IS NULL AND active_entry_seq IS NULL)),
    CONSTRAINT fk_bank_matches_line   FOREIGN KEY (line_id) REFERENCES bank_lines (id),
    CONSTRAINT fk_bank_matches_import FOREIGN KEY (import_id) REFERENCES bank_imports (id),
    CONSTRAINT fk_bank_matches_entry  FOREIGN KEY (entry_seq) REFERENCES entries (seq),
    CONSTRAINT fk_bank_matches_delta  FOREIGN KEY (delta_entry_seq) REFERENCES entries (seq),
    CONSTRAINT fk_bank_matches_tab    FOREIGN KEY (tab_id) REFERENCES tabs (id),
    CONSTRAINT fk_bank_matches_by     FOREIGN KEY (confirmed_by) REFERENCES users (id),
    CONSTRAINT fk_bank_matches_undoer FOREIGN KEY (undone_by) REFERENCES users (id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;
