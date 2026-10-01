-- 0017_bank_line_actions (MariaDB): recording and ignoring bank lines, their
-- history, and why a match was unmade. See the SQLite migration.
ALTER TABLE bank_matches ADD COLUMN undo_reason VARCHAR(255) NOT NULL DEFAULT '';

ALTER TABLE bank_lines ADD COLUMN recorded_entry_seq BIGINT NULL;
ALTER TABLE bank_lines ADD CONSTRAINT fk_bank_lines_recorded
    FOREIGN KEY (recorded_entry_seq) REFERENCES entries (seq);
ALTER TABLE bank_lines ADD CONSTRAINT chk_bank_lines_recorded
    CHECK ((state = 'recorded') = (recorded_entry_seq IS NOT NULL));

ALTER TABLE bank_lines ADD COLUMN ignore_note VARCHAR(500) NOT NULL DEFAULT '';

CREATE TABLE bank_line_events (
    id          BIGINT       NOT NULL AUTO_INCREMENT PRIMARY KEY,
    line_id     BIGINT       NOT NULL,
    action      VARCHAR(12)  NOT NULL CHECK (action IN ('recorded', 'unrecorded', 'ignored', 'unignored')),
    entry_seq   BIGINT       NULL,
    note        VARCHAR(500) NOT NULL DEFAULT '',
    by_user_id  BIGINT       NOT NULL,
    at          VARCHAR(32)  NOT NULL,
    KEY idx_bank_line_events_line (line_id, id),
    CONSTRAINT fk_bank_line_events_line  FOREIGN KEY (line_id) REFERENCES bank_lines (id),
    CONSTRAINT fk_bank_line_events_entry FOREIGN KEY (entry_seq) REFERENCES entries (seq),
    CONSTRAINT fk_bank_line_events_user  FOREIGN KEY (by_user_id) REFERENCES users (id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;
