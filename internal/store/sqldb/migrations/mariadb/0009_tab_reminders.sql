-- 0009_tab_reminders (MariaDB): per-tab, Provider-set payment reminders. See
-- the SQLite migration of the same number for the full reasoning, including why
-- this is configuration and carries no append-only triggers.
--
-- title is VARCHAR because it is bounded (notify.MaxTitleTemplate, 120 bytes)
-- and small; body is VARCHAR(4096) because that is exactly ntfy.sh's free-tier
-- message limit, which the app already enforces -- so a value that fits the app
-- fits the column, and the column documents the same ceiling.
CREATE TABLE tab_reminders (
    tab_id  BIGINT        NOT NULL,
    days    INT           NOT NULL CHECK (days > 0 AND days <= 3650),
    title   VARCHAR(255)  NOT NULL,
    body    VARCHAR(4096) NOT NULL,
    PRIMARY KEY (tab_id, days),
    CONSTRAINT fk_tab_reminders_tab FOREIGN KEY (tab_id) REFERENCES tabs (id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;
