-- 0013_reconcile_permission: the "Can reconcile" permission and the instance
-- switch for bank reconciliation (SPEC-BANK-RECONCILE, RECON-01).
--
-- can_reconcile is per account, off by default, and may be held only by an
-- administrator. That last rule is a CHECK rather than handler logic: there is
-- no screen today that removes an administrator role, and when one is written
-- it cannot forget to drop the permission, because the database refuses a row
-- that is a non-administrator holding it. "Removing the role removes the
-- permission, in the same transaction" is then the only way the write can
-- succeed.
--
-- SQLite allows a column CHECK to name another column, and since 3.37 checks
-- it against existing rows on ADD COLUMN; every existing row has 0, so it holds.
ALTER TABLE users ADD COLUMN can_reconcile INTEGER NOT NULL DEFAULT 0
    CHECK (can_reconcile IN (0, 1) AND (can_reconcile = 0 OR is_admin = 1));

-- The instance switch: the whole feature is hidden until an administrator turns
-- it on. Off by default, so upgrading changes nothing anyone can see.
ALTER TABLE instance ADD COLUMN reconcile_enabled INTEGER NOT NULL DEFAULT 0
    CHECK (reconcile_enabled IN (0, 1));
