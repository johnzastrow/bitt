-- 0003_schedules (MariaDB): recurring billing periods. See the SQLite migration
-- of the same number for the reasoning behind every column and the append-only
-- guard on the claim table.
ALTER TABLE tabs ADD COLUMN schedule_kind    VARCHAR(20) NOT NULL DEFAULT '';
ALTER TABLE tabs ADD COLUMN schedule_anchor  VARCHAR(20) NOT NULL DEFAULT '';
ALTER TABLE tabs ADD COLUMN schedule_billing VARCHAR(20) NOT NULL DEFAULT '';

CREATE TABLE posted_periods (
    tab_id        BIGINT      NOT NULL,
    period_key    VARCHAR(32) NOT NULL,
    entry_seq     BIGINT      NOT NULL,
    period_start  VARCHAR(20) NOT NULL,
    period_end    VARCHAR(20) NOT NULL,
    due_on        VARCHAR(20) NOT NULL,
    posted_at     VARCHAR(32) NOT NULL,
    PRIMARY KEY (tab_id, period_key),
    CONSTRAINT fk_posted_periods_tab   FOREIGN KEY (tab_id)    REFERENCES tabs (id) ON DELETE CASCADE,
    CONSTRAINT fk_posted_periods_entry FOREIGN KEY (entry_seq) REFERENCES entries (seq)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;

CREATE UNIQUE INDEX idx_posted_periods_entry ON posted_periods (entry_seq);
CREATE INDEX idx_posted_periods_due ON posted_periods (tab_id, due_on);

DELIMITER $$

CREATE TRIGGER posted_periods_no_update BEFORE UPDATE ON posted_periods FOR EACH ROW
BEGIN
    SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'posted periods are append-only: UPDATE is not permitted';
END$$

CREATE TRIGGER posted_periods_no_delete BEFORE DELETE ON posted_periods FOR EACH ROW
BEGIN
    SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'posted periods are append-only: DELETE is not permitted';
END$$

DELIMITER ;
