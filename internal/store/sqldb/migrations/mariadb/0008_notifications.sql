-- 0008_notifications (MariaDB): Phase 5 delivery state. See the SQLite migration
-- of the same number for the reasoning, including why the claim table is
-- at-least-once rather than the ledger's exactly-once.
ALTER TABLE users ADD COLUMN ntfy_topic   VARCHAR(64) NOT NULL DEFAULT '';
ALTER TABLE users ADD COLUMN notify_email TINYINT     NOT NULL DEFAULT 1 CHECK (notify_email IN (0, 1));
ALTER TABLE users ADD COLUMN notify_ntfy  TINYINT     NOT NULL DEFAULT 0 CHECK (notify_ntfy IN (0, 1));

CREATE TABLE sent_notifications (
    tab_id     BIGINT       NOT NULL,
    event_key  VARCHAR(128) NOT NULL,
    channel    VARCHAR(20)  NOT NULL,
    user_id    BIGINT       NOT NULL,
    sent_at    VARCHAR(32)  NOT NULL,
    PRIMARY KEY (tab_id, event_key, channel),
    CONSTRAINT fk_sent_notifications_tab  FOREIGN KEY (tab_id)  REFERENCES tabs (id) ON DELETE CASCADE,
    CONSTRAINT fk_sent_notifications_user FOREIGN KEY (user_id) REFERENCES users (id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;

CREATE INDEX idx_sent_notifications_user ON sent_notifications (user_id);

DELIMITER $$

CREATE TRIGGER sent_notifications_no_update BEFORE UPDATE ON sent_notifications FOR EACH ROW
BEGIN
    SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'sent notifications are append-only: UPDATE is not permitted';
END$$

CREATE TRIGGER sent_notifications_no_delete BEFORE DELETE ON sent_notifications FOR EACH ROW
BEGIN
    SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'sent notifications are append-only: DELETE is not permitted';
END$$

DELIMITER ;
