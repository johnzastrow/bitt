-- 0017_bank_line_actions: recording a bank line as a payment, marking it not
-- BitTabby's, and a history of every such action (RECON-05); and why a match
-- was unmade.
--
-- Who did what to a line, and when, is a row in bank_line_events: recorded,
-- unrecorded, ignored, unignored. Matches keep their own history in
-- bank_matches (confirmed_by/at, undone_by/at, and now undo_reason).

-- Why a match was unmade: from Reconciliation, or because the payment or the
-- difference was undone on its tab (decided 2026-10-01: a manual change
-- unmakes the match rather than being refused).
ALTER TABLE bank_matches ADD COLUMN undo_reason TEXT NOT NULL DEFAULT '';

-- The payment a line was recorded as, while it stands. The CHECK ties it to
-- the state: recorded exactly when there is an entry.
ALTER TABLE bank_lines ADD COLUMN recorded_entry_seq INTEGER REFERENCES entries (seq)
    CHECK ((state = 'recorded') = (recorded_entry_seq IS NOT NULL));

-- An optional note when a line is marked not BitTabby's.
ALTER TABLE bank_lines ADD COLUMN ignore_note TEXT NOT NULL DEFAULT '';

CREATE TABLE bank_line_events (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    line_id     INTEGER NOT NULL REFERENCES bank_lines (id),
    action      TEXT    NOT NULL CHECK (action IN ('recorded', 'unrecorded', 'ignored', 'unignored')),
    entry_seq   INTEGER REFERENCES entries (seq),
    note        TEXT    NOT NULL DEFAULT '',
    by_user_id  INTEGER NOT NULL REFERENCES users (id),
    at          TEXT    NOT NULL
);

CREATE INDEX idx_bank_line_events_line ON bank_line_events (line_id, id);
