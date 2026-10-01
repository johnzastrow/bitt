-- 0015_reconcile_settings: the Reconciliation setup controls and ignore rules
-- (SPEC-BANK-RECONCILE section 7, RECON-03).
--
-- One row of settings, created with the spec's defaults, and who last changed
-- them. Ranges are CHECKed here as well as in internal/reconcile, so a bug in
-- a handler cannot store a negative window or a floor above the cap.
CREATE TABLE reconcile_settings (
    id                   INTEGER PRIMARY KEY CHECK (id = 1),
    date_window_days     INTEGER NOT NULL DEFAULT 7    CHECK (date_window_days BETWEEN 0 AND 60),
    tolerance_percent    INTEGER NOT NULL DEFAULT 10   CHECK (tolerance_percent BETWEEN 0 AND 100),
    tolerance_cap_cents  INTEGER NOT NULL DEFAULT 2500 CHECK (tolerance_cap_cents BETWEEN 0 AND 1000000),
    tolerance_floor_cents INTEGER NOT NULL DEFAULT 100 CHECK (tolerance_floor_cents BETWEEN 0 AND 10000),
    min_line_cents       INTEGER NOT NULL DEFAULT 100  CHECK (min_line_cents BETWEEN 0 AND 1000000),
    use_names            INTEGER NOT NULL DEFAULT 1    CHECK (use_names IN (0, 1)),
    date_flag_days       INTEGER NOT NULL DEFAULT 3    CHECK (date_flag_days BETWEEN 0 AND 60),
    updated_by           INTEGER REFERENCES users (id),
    updated_at           TEXT    NOT NULL DEFAULT '',
    CHECK (tolerance_floor_cents <= tolerance_cap_cents)
);

INSERT INTO reconcile_settings (id) VALUES (1);

-- "Description contains ..." per layout. A matching line is imported as
-- ignored, with the rule as its reason; adding a rule also ignores matching
-- lines that are still open. Removing a rule changes no line.
CREATE TABLE bank_ignore_rules (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    format_id   INTEGER NOT NULL REFERENCES bank_formats (id),
    contains    TEXT    NOT NULL CHECK (length(contains) BETWEEN 1 AND 120),
    created_by  INTEGER NOT NULL REFERENCES users (id),
    created_at  TEXT    NOT NULL,
    UNIQUE (format_id, contains)
);
