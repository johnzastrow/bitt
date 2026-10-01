-- 0018_payment_reviews (MariaDB): payments set aside as "not in the bank".
-- See the SQLite migration of the same number.
CREATE TABLE payment_reviews (
    id                BIGINT       NOT NULL AUTO_INCREMENT PRIMARY KEY,
    entry_seq         BIGINT       NOT NULL,
    tab_id            BIGINT       NOT NULL,
    note              VARCHAR(500) NOT NULL DEFAULT '',
    by_user_id        BIGINT       NOT NULL,
    at                VARCHAR(32)  NOT NULL,
    undone_at         VARCHAR(32)  NOT NULL DEFAULT '',
    undone_by         BIGINT       NULL,
    active_entry_seq  BIGINT       NULL,
    UNIQUE KEY uq_payment_reviews_active (active_entry_seq),
    KEY idx_payment_reviews_entry (entry_seq),
    CONSTRAINT chk_payment_reviews_active CHECK (
        (undone_at = '' AND active_entry_seq <=> entry_seq)
     OR (undone_at <> '' AND active_entry_seq IS NULL)),
    CONSTRAINT fk_payment_reviews_entry FOREIGN KEY (entry_seq) REFERENCES entries (seq),
    CONSTRAINT fk_payment_reviews_tab   FOREIGN KEY (tab_id) REFERENCES tabs (id),
    CONSTRAINT fk_payment_reviews_by    FOREIGN KEY (by_user_id) REFERENCES users (id),
    CONSTRAINT fk_payment_reviews_undo  FOREIGN KEY (undone_by) REFERENCES users (id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;
