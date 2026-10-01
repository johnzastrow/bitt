-- 0018_payment_reviews: a recorded payment with no bank transaction, set
-- aside as "not in the bank" (cash, another account), with who and when
-- (owner's request 2026-10-01: reconciliation accounts for payments as well as
-- bank lines).
--
-- One standing review per payment: active_entry_seq is set while it stands and
-- NULL once undone, UNIQUE, the same pattern as bank_matches. A payment is
-- never both matched and set aside: confirming and setting aside each lock the
-- payment's entry row and check the other first.
CREATE TABLE payment_reviews (
    id                INTEGER PRIMARY KEY AUTOINCREMENT,
    entry_seq         INTEGER NOT NULL REFERENCES entries (seq),
    tab_id            INTEGER NOT NULL REFERENCES tabs (id),
    note              TEXT    NOT NULL DEFAULT '' CHECK (length(note) <= 500),
    by_user_id        INTEGER NOT NULL REFERENCES users (id),
    at                TEXT    NOT NULL,
    undone_at         TEXT    NOT NULL DEFAULT '',
    undone_by         INTEGER REFERENCES users (id),
    active_entry_seq  INTEGER UNIQUE,
    CHECK ((undone_at = '' AND active_entry_seq IS entry_seq)
        OR (undone_at <> '' AND active_entry_seq IS NULL))
);

CREATE INDEX idx_payment_reviews_entry ON payment_reviews (entry_seq);
