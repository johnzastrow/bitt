-- 0016_bank_matches: confirmed matches between a bank line and a payment
-- (SPEC-BANK-RECONCILE section 6, RECON-04).
--
-- Which row of which file matched which payment on which tab: line_id ->
-- bank_lines (row, original text) -> bank_imports (file name), and entry_seq
-- with tab_id stored alongside, so a tab's history and a file's are each one
-- lookup. The bank and recorded figures are copied as they were at confirm
-- time. Undone matches keep their row; nothing here is ever deleted.
--
-- ONE ACTIVE MATCH PER LINE AND PER PAYMENT, on both backends. MariaDB has no
-- partial unique index, so the rule is not "UNIQUE where undone_at = ''".
-- active_line_id and active_entry_seq are set while the match stands and NULL
-- once it is undone, each UNIQUE; both backends allow many NULLs. A second
-- match of the same line or payment is then a constraint violation, not a
-- race.
CREATE TABLE bank_matches (
    id                INTEGER PRIMARY KEY AUTOINCREMENT,
    line_id           INTEGER NOT NULL REFERENCES bank_lines (id),
    import_id         INTEGER NOT NULL REFERENCES bank_imports (id),
    entry_seq         INTEGER NOT NULL REFERENCES entries (seq),
    tab_id            INTEGER NOT NULL REFERENCES tabs (id),
    -- The difference posted on confirm: a payment or a debit adjustment.
    -- NULL when the bank and recorded amounts were equal.
    delta_entry_seq   INTEGER REFERENCES entries (seq),
    bank_date         TEXT    NOT NULL CHECK (length(bank_date) = 10),
    bank_cents        INTEGER NOT NULL CHECK (bank_cents > 0),
    recorded_date     TEXT    NOT NULL CHECK (length(recorded_date) = 10),
    recorded_cents    INTEGER NOT NULL CHECK (recorded_cents > 0),
    -- Dates are flagged, never posted (decided 2026-10-01).
    date_flagged      INTEGER NOT NULL CHECK (date_flagged IN (0, 1)),
    date_flag_reason  TEXT    NOT NULL DEFAULT '',
    -- Whether the bank note was copied into the delta entry's memo.
    note_in_memo      INTEGER NOT NULL CHECK (note_in_memo IN (0, 1)),
    confirmed_by      INTEGER NOT NULL REFERENCES users (id),
    confirmed_at      TEXT    NOT NULL,
    undone_at         TEXT    NOT NULL DEFAULT '',
    undone_by         INTEGER REFERENCES users (id),
    active_line_id    INTEGER UNIQUE,
    active_entry_seq  INTEGER UNIQUE,
    -- Active columns are set exactly while the match stands. IS, not =:
    -- "NULL = line_id" is unknown, and a CHECK lets unknown through.
    CHECK ((undone_at = '' AND active_line_id IS line_id AND active_entry_seq IS entry_seq)
        OR (undone_at <> '' AND active_line_id IS NULL AND active_entry_seq IS NULL))
);

CREATE INDEX idx_bank_matches_tab ON bank_matches (tab_id, id);
CREATE INDEX idx_bank_matches_import ON bank_matches (import_id, line_id);
CREATE INDEX idx_bank_matches_line ON bank_matches (line_id);
