-- 0014_bank_imports: bank statement imports for reconciliation
-- (SPEC-BANK-RECONCILE, RECON-02).
--
-- Beside the ledger, never inside it: nothing here references or changes
-- entries, and no ledger rule is relaxed. Matches (RECON-04) arrive later.
--
-- Bank lines are kept forever (decided 2026-10-01), so there is no purge and no
-- ON DELETE CASCADE from an import to its lines. Only INCOMING lines are
-- stored; an export's outgoing lines are counted on the import and dropped.

-- A saved column mapping, found again by the export's header signature.
-- Columns are 0-based indexes into the header; -1 means not mapped.
CREATE TABLE bank_formats (
    id                INTEGER PRIMARY KEY AUTOINCREMENT,
    name              TEXT    NOT NULL CHECK (length(name) BETWEEN 1 AND 120),
    header_signature  TEXT    NOT NULL UNIQUE CHECK (length(header_signature) = 64),
    header_text       TEXT    NOT NULL,
    date_col          INTEGER NOT NULL CHECK (date_col >= 0),
    date_layout       TEXT    NOT NULL,
    amount_col        INTEGER NOT NULL CHECK (amount_col >= -1),
    debit_col         INTEGER NOT NULL CHECK (debit_col >= -1),
    credit_col        INTEGER NOT NULL CHECK (credit_col >= -1),
    incoming_negative INTEGER NOT NULL CHECK (incoming_negative IN (0, 1)),
    decimal_comma     INTEGER NOT NULL CHECK (decimal_comma IN (0, 1)),
    description_col   INTEGER NOT NULL CHECK (description_col >= 0),
    account_col       INTEGER NOT NULL CHECK (account_col >= -1),
    note_col          INTEGER NOT NULL CHECK (note_col >= -1),
    reference_col     INTEGER NOT NULL CHECK (reference_col >= -1),
    created_by        INTEGER NOT NULL REFERENCES users (id),
    created_at        TEXT    NOT NULL
);

-- One row per upload, with what happened to each of its rows.
CREATE TABLE bank_imports (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    format_id       INTEGER NOT NULL REFERENCES bank_formats (id),
    file_name       TEXT    NOT NULL,
    file_sha256     TEXT    NOT NULL CHECK (length(file_sha256) = 64),
    uploaded_by     INTEGER NOT NULL REFERENCES users (id),
    uploaded_at     TEXT    NOT NULL,
    kept            INTEGER NOT NULL CHECK (kept >= 0),
    duplicates      INTEGER NOT NULL CHECK (duplicates >= 0),
    outgoing        INTEGER NOT NULL CHECK (outgoing >= 0),
    ignored         INTEGER NOT NULL CHECK (ignored >= 0),
    refused         INTEGER NOT NULL CHECK (refused >= 0),
    -- The date range of the file's incoming lines, kept or duplicate; ''
    -- when it had none. Suggestions search payments around this range.
    first_date      TEXT    NOT NULL DEFAULT '',
    last_date       TEXT    NOT NULL DEFAULT '',
    -- Why rows were refused, the first few, for the import summary.
    refused_detail  TEXT    NOT NULL DEFAULT ''
);

CREATE INDEX idx_bank_imports_uploaded ON bank_imports (uploaded_at);

-- One incoming line. The fingerprint is UNIQUE, which is what makes
-- re-importing an overlapping file add nothing (spec section 5), even when two
-- uploads race.
CREATE TABLE bank_lines (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    import_id      INTEGER NOT NULL REFERENCES bank_imports (id),
    file_row       INTEGER NOT NULL CHECK (file_row >= 2),
    original_text  TEXT    NOT NULL,
    account        TEXT    NOT NULL DEFAULT '',
    posted_on      TEXT    NOT NULL CHECK (length(posted_on) = 10),
    amount_cents   INTEGER NOT NULL CHECK (amount_cents > 0),
    description    TEXT    NOT NULL,
    note           TEXT    NOT NULL DEFAULT '',
    reference      TEXT    NOT NULL DEFAULT '',
    fingerprint    TEXT    NOT NULL UNIQUE CHECK (length(fingerprint) = 64),
    state          TEXT    NOT NULL DEFAULT 'open'
                   CHECK (state IN ('open', 'matched', 'recorded', 'ignored')),
    ignore_reason  TEXT    NOT NULL DEFAULT ''
);

CREATE INDEX idx_bank_lines_import ON bank_lines (import_id, file_row);
CREATE INDEX idx_bank_lines_state ON bank_lines (state, posted_on);
