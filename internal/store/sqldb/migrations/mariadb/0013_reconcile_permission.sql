-- 0013_reconcile_permission (MariaDB): the "Can reconcile" permission and the
-- instance switch for bank reconciliation. See the SQLite migration of the same
-- number for the reasoning, above all why "only an administrator holds it" is
-- a CHECK rather than handler logic.
--
-- A column-level CHECK in MariaDB may refer only to its own column, so the
-- cross-column rule is a named table constraint.
ALTER TABLE users ADD COLUMN can_reconcile TINYINT NOT NULL DEFAULT 0
    CHECK (can_reconcile IN (0, 1));
ALTER TABLE users ADD CONSTRAINT chk_users_reconcile_admin
    CHECK (can_reconcile = 0 OR is_admin = 1);

ALTER TABLE instance ADD COLUMN reconcile_enabled TINYINT NOT NULL DEFAULT 0
    CHECK (reconcile_enabled IN (0, 1));
