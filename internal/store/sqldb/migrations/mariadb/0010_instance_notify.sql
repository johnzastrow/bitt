-- 0010_instance_notify (MariaDB): instance-wide notification settings an
-- administrator can set through the interface. See the SQLite migration of the
-- same number for the full reasoning -- above all, why the SMTP password, the
-- ntfy token, and the tick secret are NOT here and stay environment-only.
ALTER TABLE instance ADD COLUMN smtp_host     VARCHAR(253) NOT NULL DEFAULT '';
ALTER TABLE instance ADD COLUMN smtp_port     INT          NOT NULL DEFAULT 0;
ALTER TABLE instance ADD COLUMN smtp_username VARCHAR(320) NOT NULL DEFAULT '';
ALTER TABLE instance ADD COLUMN email_from    VARCHAR(320) NOT NULL DEFAULT '';
ALTER TABLE instance ADD COLUMN ntfy_url      VARCHAR(255) NOT NULL DEFAULT '';

CREATE TABLE instance_reminders (
    days  INT           NOT NULL PRIMARY KEY CHECK (days > 0 AND days <= 3650),
    title VARCHAR(255)  NOT NULL,
    body  VARCHAR(4096) NOT NULL
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;
