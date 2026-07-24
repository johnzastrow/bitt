-- 0006_loan_terms (MariaDB): a Payoff loan's term and scheduled payment, and
-- arbitrary schedule intervals. See the SQLite migration of the same number for
-- the reasoning behind each column and the biweekly rewrite.
ALTER TABLE tabs ADD COLUMN schedule_interval INT NOT NULL DEFAULT 1;

UPDATE tabs
   SET schedule_kind = 'weekly', schedule_interval = 2
 WHERE schedule_kind = 'biweekly';

ALTER TABLE tabs ADD COLUMN loan_term_periods INT NOT NULL DEFAULT 0;

ALTER TABLE tabs ADD COLUMN loan_payment_cents BIGINT NOT NULL DEFAULT 0;

-- Backfilled from the sum of each Payoff tab's active line items -- the same
-- figure the pre-term code read. The subquery targets tab_items, a different
-- table from the one being updated, so MySQL permits the correlated form.
UPDATE tabs
   SET loan_payment_cents = COALESCE((
       SELECT SUM(amount_cents)
         FROM tab_items
        WHERE tab_items.tab_id = tabs.id
          AND tab_items.removed_at IS NULL
   ), 0)
 WHERE kind = 'payoff';
