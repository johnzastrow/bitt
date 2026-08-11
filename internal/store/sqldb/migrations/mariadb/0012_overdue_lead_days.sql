-- 0012_overdue_lead_days: let a reminder rule fire AFTER a due date.
--
-- Overdue notices (NOTIF-02) are expressed as NEGATIVE lead times: -1 fires the
-- day after the due date, -7 a week after. The reminder tables were written
-- before overdue existed and carry CHECK (days > 0 AND days <= 3650), so a
-- negative rule could never be stored.
--
-- The consequence was subtle and bad: overdue worked from the built-in defaults,
-- which are held in code and never touch the database, so it appeared to work.
-- But any instance that had saved reminders through the admin screen -- which
-- REPLACES the built-in set -- silently had no overdue at all, and saving one
-- failed against this constraint. The feature was reachable only by never
-- configuring it.
--
-- Zero stays refused. "On the due date" is ambiguous between a reminder and an
-- overdue notice, and nobody would agree which they meant.
--
-- MariaDB names an inline CHECK after its column, and dropping it by name is
-- unreliable across versions (the 1.2.0 role migration hit the same thing), so
-- both tables are rebuilt. Rebuild order matters for tab_reminders: the foreign
-- key must be recreated, and rows are copied before the old table is dropped.

CREATE TABLE instance_reminders_new (
    days  INT           NOT NULL PRIMARY KEY CHECK (days <> 0 AND days >= -3650 AND days <= 3650),
    title VARCHAR(255)  NOT NULL,
    body  VARCHAR(4096) NOT NULL
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;

INSERT INTO instance_reminders_new (days, title, body)
    SELECT days, title, body FROM instance_reminders;

DROP TABLE instance_reminders;
ALTER TABLE instance_reminders_new RENAME TO instance_reminders;

-- The foreign key is added AFTER the old table is gone, deliberately.
--
-- In MariaDB a foreign key constraint name is unique across the whole DATABASE,
-- not per table. Declaring fk_tab_reminders_tab on the new table while the
-- original still holds that name fails with errno 121, "Duplicate key on write
-- or update" -- an error message that says nothing about constraint names and
-- sends you looking at the data instead.
CREATE TABLE tab_reminders_new (
    tab_id  BIGINT        NOT NULL,
    days    INT           NOT NULL CHECK (days <> 0 AND days >= -3650 AND days <= 3650),
    title   VARCHAR(255)  NOT NULL,
    body    VARCHAR(4096) NOT NULL,
    PRIMARY KEY (tab_id, days)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;

INSERT INTO tab_reminders_new (tab_id, days, title, body)
    SELECT tab_id, days, title, body FROM tab_reminders;

DROP TABLE tab_reminders;
ALTER TABLE tab_reminders_new RENAME TO tab_reminders;

-- Now the name is free, so the constraint keeps the name it had before rather
-- than a temporary one that would confuse the next person to read the schema.
ALTER TABLE tab_reminders
    ADD CONSTRAINT fk_tab_reminders_tab FOREIGN KEY (tab_id) REFERENCES tabs (id) ON DELETE CASCADE;
