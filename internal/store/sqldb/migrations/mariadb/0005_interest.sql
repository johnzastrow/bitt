-- 0005_interest (MariaDB): declining-balance interest on Payoff loans. See the
-- SQLite migration of the same number for the reasoning.
--
-- Note on the entries.kind CHECK: the SQLite version explains that interest
-- posts as kind='charge' with category='interest' rather than a new kind,
-- because SQLite cannot alter a CHECK in place. MariaDB can, but this build
-- takes the same route so the two schemas -- and the Go code reading them --
-- stay identical.
ALTER TABLE entries ADD COLUMN category VARCHAR(20) NOT NULL DEFAULT '';

ALTER TABLE tabs ADD COLUMN interest_apr_bp BIGINT NOT NULL DEFAULT 0;

CREATE TABLE posted_interest (
    tab_id       BIGINT      NOT NULL,
    period_key   VARCHAR(32) NOT NULL,
    entry_seq    BIGINT      NOT NULL,
    accrued_for  VARCHAR(20) NOT NULL,
    base_cents   BIGINT      NOT NULL,
    posted_at    VARCHAR(32) NOT NULL,
    PRIMARY KEY (tab_id, period_key),
    CONSTRAINT fk_posted_interest_tab   FOREIGN KEY (tab_id)    REFERENCES tabs (id) ON DELETE CASCADE,
    CONSTRAINT fk_posted_interest_entry FOREIGN KEY (entry_seq) REFERENCES entries (seq)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;

CREATE UNIQUE INDEX idx_posted_interest_entry ON posted_interest (entry_seq);

DELIMITER $$

CREATE TRIGGER posted_interest_no_update BEFORE UPDATE ON posted_interest FOR EACH ROW
BEGIN
    SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'posted interest is append-only: UPDATE is not permitted';
END$$

CREATE TRIGGER posted_interest_no_delete BEFORE DELETE ON posted_interest FOR EACH ROW
BEGIN
    SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'posted interest is append-only: DELETE is not permitted';
END$$

DELIMITER ;
