-- 0004_fees (MariaDB): late fees and the Payoff repayment schedule. See the
-- SQLite migration of the same number for the reasoning.
ALTER TABLE tabs ADD COLUMN fee_kind        VARCHAR(20) NOT NULL DEFAULT '';
ALTER TABLE tabs ADD COLUMN fee_fixed_cents BIGINT      NOT NULL DEFAULT 0;
ALTER TABLE tabs ADD COLUMN fee_percent_bp  BIGINT      NOT NULL DEFAULT 0;
ALTER TABLE tabs ADD COLUMN fee_grace_days  INT         NOT NULL DEFAULT 0;
ALTER TABLE tabs ADD COLUMN fee_cap_cents   BIGINT      NOT NULL DEFAULT 0;

CREATE TABLE posted_fees (
    tab_id       BIGINT      NOT NULL,
    fee_key      VARCHAR(32) NOT NULL,
    entry_seq    BIGINT      NOT NULL,
    assessed_for VARCHAR(20) NOT NULL,
    base_cents   BIGINT      NOT NULL,
    posted_at    VARCHAR(32) NOT NULL,
    PRIMARY KEY (tab_id, fee_key),
    CONSTRAINT fk_posted_fees_tab   FOREIGN KEY (tab_id)    REFERENCES tabs (id) ON DELETE CASCADE,
    CONSTRAINT fk_posted_fees_entry FOREIGN KEY (entry_seq) REFERENCES entries (seq)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;

CREATE UNIQUE INDEX idx_posted_fees_entry ON posted_fees (entry_seq);

DELIMITER $$

CREATE TRIGGER posted_fees_no_update BEFORE UPDATE ON posted_fees FOR EACH ROW
BEGIN
    SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'posted fees are append-only: UPDATE is not permitted';
END$$

CREATE TRIGGER posted_fees_no_delete BEFORE DELETE ON posted_fees FOR EACH ROW
BEGIN
    SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'posted fees are append-only: DELETE is not permitted';
END$$

DELIMITER ;
