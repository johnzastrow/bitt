-- 0006_loan_terms: a Payoff loan's term and scheduled payment, and arbitrary
-- schedule intervals.
--
-- Three things land together because they are one feature: a loan cannot
-- suggest a payment without a term, and a term means nothing without knowing
-- how often a period comes round.
--
-- ---------------------------------------------------------------------------
-- schedule_interval: how many of the recurrence's units one period spans.
--
-- 1 is every week or every month and is what every existing row means, so the
-- default backfills the whole table correctly. 3 with schedule_kind='weekly'
-- is every third week; 2 with 'monthly_day' is every second month.
--
-- The interval also fixes the interest basis. A period rate used to be
-- APR / periods-per-year, which has no answer for "every three weeks" -- 52/3
-- is not an integer. It is now a fraction of a year: months/12 for the monthly
-- kinds, the 30/360 basis a US installment loan is quoted on, and days/365 for
-- the weekly ones. A plain monthly loan is still exactly APR/12.
-- ---------------------------------------------------------------------------
ALTER TABLE tabs ADD COLUMN schedule_interval INTEGER NOT NULL DEFAULT 1;

-- ---------------------------------------------------------------------------
-- 'biweekly' becomes 'weekly' with an interval of 2.
--
-- This rewrite is safe for one specific reason, and it is worth stating because
-- rewriting a schedule is otherwise exactly the kind of change that re-bills a
-- tab: both forms compute period n as anchor + 14n, so every period boundary
-- and therefore every period_key is identical before and after. No cycle can
-- come due again under a new key, and no posted_periods claim is orphaned.
-- schedule.TestNormalizeRewritesBiweekly pins that equivalence.
--
-- Schedule.Normalize performs the same rewrite in Go, so a row that somehow
-- escapes this statement still behaves. The 'biweekly' constant is retained as
-- deprecated rather than removed for that reason.
-- ---------------------------------------------------------------------------
UPDATE tabs
   SET schedule_kind = 'weekly', schedule_interval = 2
 WHERE schedule_kind = 'biweekly';

-- ---------------------------------------------------------------------------
-- loan_term_periods: how many periods a Payoff loan is scheduled to run for.
--
-- Zero means open-ended, which is what every existing Payoff tab is today and
-- what a plain IOU with no agreed end should stay. A term is what makes the
-- suggested payment computable at all, and it also corrects two figures that
-- were wrong without it: both the payoff progress and the late-fee expectation
-- used to stop once cumulative payments reached the principal, which on an
-- interest-bearing loan is sooner than the loan can actually be repaid. A
-- current borrower could read as behind.
-- ---------------------------------------------------------------------------
ALTER TABLE tabs ADD COLUMN loan_term_periods INTEGER NOT NULL DEFAULT 0;

-- ---------------------------------------------------------------------------
-- loan_payment_cents: the scheduled payment on a Payoff tab.
--
-- Until now this was the sum of the tab's line items, which is also what a
-- Services tab charges per period. Two different meanings on one field made
-- the Payoff setup screen genuinely ambiguous -- the loan amount is a charge
-- that posts the principal once, while the line items were the expected
-- payment per period, and conflating them produced a loan that read as already
-- settled. The payment gets its own column so each field says one thing.
--
-- The Provider enters this figure rather than the app deriving it, because it
-- comes off a bank's paperwork and the bank's number is the one that has to be
-- paid. The app computes a suggestion beside it and reports the drift.
--
-- Backfilled from the sum of each Payoff tab's active line items, which is
-- exactly what the old code read, so no tab's expectations change on upgrade.
-- The item rows are deliberately left in place: they are the Provider's data,
-- a migration is the wrong place to delete it, and the Payoff screen simply
-- stops reading them.
-- ---------------------------------------------------------------------------
ALTER TABLE tabs ADD COLUMN loan_payment_cents INTEGER NOT NULL DEFAULT 0;

UPDATE tabs
   SET loan_payment_cents = COALESCE((
       SELECT SUM(amount_cents)
         FROM tab_items
        WHERE tab_items.tab_id = tabs.id
          AND tab_items.removed_at IS NULL
   ), 0)
 WHERE kind = 'payoff';
