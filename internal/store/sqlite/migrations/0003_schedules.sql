-- 0003_schedules: recurring billing periods.
--
-- SCHED-01: a schedule is an anchor date plus a recurrence, plus the Provider's
-- choice of whether a period's charge lands at its start or at its end. These
-- live as columns on tabs rather than in a side table: a tab has at most one
-- schedule, and a side table would mean a join on every dashboard render.
--
-- Empty string rather than NULL for "no schedule", matching how 0002 handled
-- the payment method: the column is never null and comparisons need no COALESCE
-- on either backend. MariaDB has allowed DEFAULT on a TEXT column since
-- 10.2.1, well below what Phase 5 targets (DEPLOY-02).
--
-- Anchors are stored as bare ISO-8601 calendar dates, 'YYYY-MM-DD', and not as
-- timestamps. A period boundary is a date in the instance timezone; storing it
-- as an instant would let it shift a day under a zone change (SCHED-02).

ALTER TABLE tabs ADD COLUMN schedule_kind    TEXT NOT NULL DEFAULT '';
ALTER TABLE tabs ADD COLUMN schedule_anchor  TEXT NOT NULL DEFAULT '';
ALTER TABLE tabs ADD COLUMN schedule_billing TEXT NOT NULL DEFAULT '';

-- ---------------------------------------------------------------------------
-- posted_periods: which billing cycles have already been charged.
--
-- SCHED-04: the primary key is the entire concurrency mechanism. Two parallel
-- reads of an overdue tab compute the same due periods and both attempt to post
-- them. The loser's INSERT violates this key, and because the insert shares a
-- transaction with the entry it is claiming, the rollback takes the duplicate
-- entry with it. Nothing reads-then-writes, so there is no window to lose.
--
-- SCHED-03: this table is also what makes catch-up inherent rather than
-- engineered. What is due is always "the schedule's periods, minus these",
-- recomputed on every read. There is no cursor, no last-run timestamp, and no
-- process that needs to have been running for the answer to be right.
--
-- period_start, period_end, and due_on are denormalized from the schedule so a
-- statement renders without recomputing period arithmetic, and so a cycle keeps
-- the dates it was actually billed under if the Provider later changes the
-- schedule (SCHED-05).
-- ---------------------------------------------------------------------------
CREATE TABLE posted_periods (
    tab_id        INTEGER NOT NULL REFERENCES tabs (id) ON DELETE CASCADE,
    period_key    TEXT    NOT NULL,
    entry_seq     INTEGER NOT NULL REFERENCES entries (seq),
    period_start  TEXT    NOT NULL,
    period_end    TEXT    NOT NULL,
    due_on        TEXT    NOT NULL,
    posted_at     TEXT    NOT NULL,
    PRIMARY KEY (tab_id, period_key)
);

-- One entry backs at most one period, so a bug that reused an entry seq across
-- two periods surfaces as a constraint violation rather than as a tab whose
-- balance is short a charge.
CREATE UNIQUE INDEX idx_posted_periods_entry ON posted_periods (entry_seq);

-- Statements are rendered by due date (CHG-04).
CREATE INDEX idx_posted_periods_due ON posted_periods (tab_id, due_on);

-- ---------------------------------------------------------------------------
-- A period claim is as immutable as the entry it points at. Repointing one at
-- a different entry, or deleting one so a cycle could post again, would defeat
-- SCHED-04 just as surely as editing the ledger would defeat LEDGER-01, so it
-- gets the same treatment.
-- ---------------------------------------------------------------------------
CREATE TRIGGER posted_periods_no_update
BEFORE UPDATE ON posted_periods
BEGIN
    SELECT RAISE(ABORT, 'posted periods are append-only: UPDATE is not permitted');
END;

CREATE TRIGGER posted_periods_no_delete
BEFORE DELETE ON posted_periods
BEGIN
    SELECT RAISE(ABORT, 'posted periods are append-only: DELETE is not permitted');
END;
