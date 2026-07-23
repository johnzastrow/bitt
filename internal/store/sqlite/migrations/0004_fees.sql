-- 0004_fees: late fees and the Payoff repayment schedule.
--
-- FEE-01, FEE-02, FEE-06: a tab's late-fee policy lives as columns on the tab,
-- for the same reason its schedule does -- one policy per tab, and a side table
-- would mean a join on every render. Empty string and zero stand for "no fee",
-- so the columns are never null and comparisons need no COALESCE on either
-- backend (DEPLOY-02).
--
-- Percentage rates are stored as integer basis points (100 bp = 1%), never as a
-- decimal or a float, so a fee amount is derived in integer cents end to end
-- (LEDGER-04, FEE-05).

ALTER TABLE tabs ADD COLUMN fee_kind        TEXT    NOT NULL DEFAULT '';
ALTER TABLE tabs ADD COLUMN fee_fixed_cents INTEGER NOT NULL DEFAULT 0;
ALTER TABLE tabs ADD COLUMN fee_percent_bp  INTEGER NOT NULL DEFAULT 0;
ALTER TABLE tabs ADD COLUMN fee_grace_days  INTEGER NOT NULL DEFAULT 0;
ALTER TABLE tabs ADD COLUMN fee_cap_cents   INTEGER NOT NULL DEFAULT 0;

-- ---------------------------------------------------------------------------
-- posted_fees: which overdue dates have already been assessed a late fee.
--
-- FEE-04: the primary key is the whole double-assessment guard, exactly as
-- posted_periods is for scheduled charges (SCHED-04). An overdue date can incur
-- at most one fee, ever, so a fee cannot compound merely by the tab being read
-- twice, and two concurrent reads cannot both assess the same date -- the
-- loser's INSERT violates the key and its fee entry rolls back with it.
--
-- fee_key is the date the fee is assessed for. base_cents records the overdue
-- amount the fee was computed on, so a percentage fee can be shown to have been
-- taken on the period charge and not on a balance containing fees (FEE-05).
-- ---------------------------------------------------------------------------
CREATE TABLE posted_fees (
    tab_id       INTEGER NOT NULL REFERENCES tabs (id) ON DELETE CASCADE,
    fee_key      TEXT    NOT NULL,
    entry_seq    INTEGER NOT NULL REFERENCES entries (seq),
    assessed_for TEXT    NOT NULL,
    base_cents   INTEGER NOT NULL,
    posted_at    TEXT    NOT NULL,
    PRIMARY KEY (tab_id, fee_key)
);

-- One entry backs at most one fee assessment.
CREATE UNIQUE INDEX idx_posted_fees_entry ON posted_fees (entry_seq);

-- ---------------------------------------------------------------------------
-- A fee claim is as immutable as the entry it points at. Repointing or deleting
-- one would let a date be assessed twice, defeating FEE-04 the same way editing
-- the ledger would defeat LEDGER-01, so it gets the same append-only guard.
--
-- A waiver does NOT delete the claim: it posts a reversing entry against the
-- fee (FEE-07), which cancels the fee's effect on the balance while leaving the
-- date recorded as assessed, so the fee cannot silently come back on the next
-- read.
-- ---------------------------------------------------------------------------
CREATE TRIGGER posted_fees_no_update
BEFORE UPDATE ON posted_fees
BEGIN
    SELECT RAISE(ABORT, 'posted fees are append-only: UPDATE is not permitted');
END;

CREATE TRIGGER posted_fees_no_delete
BEFORE DELETE ON posted_fees
BEGIN
    SELECT RAISE(ABORT, 'posted fees are append-only: DELETE is not permitted');
END;
