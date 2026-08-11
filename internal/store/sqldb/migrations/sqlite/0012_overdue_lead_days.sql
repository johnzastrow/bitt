-- 0012_overdue_lead_days: let a reminder rule fire AFTER a due date.
--
-- See the MariaDB copy of this migration for the full reasoning. In short:
-- overdue notices (NOTIF-02) are expressed as NEGATIVE lead times, the reminder
-- tables were written before overdue existed with CHECK (days > 0), and so a
-- negative rule could never be stored.
--
-- The consequence was subtle and bad. Overdue worked from the built-in
-- defaults, which are held in code and never touch the database, so it appeared
-- to work -- while any instance that had saved its reminders through the admin
-- screen (which REPLACES the built-in set) silently had no overdue at all, and
-- saving one failed against this constraint. The feature was reachable only by
-- never configuring it.
--
-- Zero stays refused: "on the due date" is ambiguous between a reminder and an
-- overdue notice, and nobody would agree which they meant.
--
-- SQLite cannot alter a CHECK constraint, so both tables are rebuilt: create
-- new, copy rows, drop old, rename.

CREATE TABLE instance_reminders_new (
    days  INTEGER NOT NULL PRIMARY KEY CHECK (days <> 0 AND days >= -3650 AND days <= 3650),
    title TEXT    NOT NULL,
    body  TEXT    NOT NULL
);

INSERT INTO instance_reminders_new (days, title, body)
    SELECT days, title, body FROM instance_reminders;

DROP TABLE instance_reminders;
ALTER TABLE instance_reminders_new RENAME TO instance_reminders;

CREATE TABLE tab_reminders_new (
    tab_id  INTEGER NOT NULL REFERENCES tabs (id) ON DELETE CASCADE,
    days    INTEGER NOT NULL CHECK (days <> 0 AND days >= -3650 AND days <= 3650),
    title   TEXT    NOT NULL,
    body    TEXT    NOT NULL,
    PRIMARY KEY (tab_id, days)
);

INSERT INTO tab_reminders_new (tab_id, days, title, body)
    SELECT tab_id, days, title, body FROM tab_reminders;

DROP TABLE tab_reminders;
ALTER TABLE tab_reminders_new RENAME TO tab_reminders;
