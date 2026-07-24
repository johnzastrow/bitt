-- 0005_interest: declining-balance interest on Payoff loans.
--
-- Interest is a charge that is specifically interest. Distinguishing it from the
-- loan principal matters -- progress is measured against the principal, and a
-- statement should name interest as interest -- but the entries.kind CHECK from
-- 0001 lists a fixed set of kinds and SQLite cannot alter a CHECK without
-- rebuilding the table, which the migration harness cannot do safely (it holds
-- foreign_keys on inside the transaction, and that pragma is a no-op there).
--
-- So interest posts with kind='charge' and a category of 'interest'. The column
-- is additive, exactly as 'method' was in 0002: empty for every existing and
-- ordinary entry, 'interest' for the periodic interest charge. The append-only
-- triggers from 0001 already make it as immutable as the amount beside it.
ALTER TABLE entries ADD COLUMN category TEXT NOT NULL DEFAULT '';

-- The annual interest rate, in basis points (100 bp = 1%). Zero means the loan
-- carries no interest, which stays the default and matches an interest-free IOU.
ALTER TABLE tabs ADD COLUMN interest_apr_bp INTEGER NOT NULL DEFAULT 0;

-- ---------------------------------------------------------------------------
-- posted_interest: which periods have already accrued interest.
--
-- The third sibling of posted_periods and posted_fees, and for the same reason:
-- the (tab, period) primary key makes interest accrue at most once per period,
-- so reading a tab twice cannot double-charge interest and two concurrent reads
-- cannot both post it -- the loser's INSERT violates the key and its interest
-- entry rolls back with it.
--
-- base_cents records the balance the interest was computed on, so a declining
-- charge can be shown to have been taken on the outstanding balance at the time.
-- ---------------------------------------------------------------------------
CREATE TABLE posted_interest (
    tab_id       INTEGER NOT NULL REFERENCES tabs (id) ON DELETE CASCADE,
    period_key   TEXT    NOT NULL,
    entry_seq    INTEGER NOT NULL REFERENCES entries (seq),
    accrued_for  TEXT    NOT NULL,
    base_cents   INTEGER NOT NULL,
    posted_at    TEXT    NOT NULL,
    PRIMARY KEY (tab_id, period_key)
);

CREATE UNIQUE INDEX idx_posted_interest_entry ON posted_interest (entry_seq);

-- Immutable, like every other accrual claim.
CREATE TRIGGER posted_interest_no_update
BEFORE UPDATE ON posted_interest
BEGIN
    SELECT RAISE(ABORT, 'posted interest is append-only: UPDATE is not permitted');
END;

CREATE TRIGGER posted_interest_no_delete
BEFORE DELETE ON posted_interest
BEGIN
    SELECT RAISE(ABORT, 'posted interest is append-only: DELETE is not permitted');
END;
