-- 0002_payments (MariaDB): payment method on ledger entries. See the SQLite
-- migration of the same number for the reasoning.
ALTER TABLE entries ADD COLUMN method VARCHAR(20) NOT NULL DEFAULT '';
