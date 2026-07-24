-- 0002_payments: payment method on ledger entries.
--
-- PAY-02: money moves outside BitTabby, so a recorded payment captures how it
-- actually moved. The column lives on the entry rather than on a side table
-- because it is part of what was recorded and must be as immutable as the
-- amount -- the append-only triggers from 0001 already cover it.
--
-- Empty string rather than NULL for "not applicable", so the column is never
-- null and comparisons need no COALESCE on either backend (DEPLOY-02).

ALTER TABLE entries ADD COLUMN method TEXT NOT NULL DEFAULT '';
