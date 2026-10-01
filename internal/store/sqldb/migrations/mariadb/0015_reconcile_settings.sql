-- 0015_reconcile_settings (MariaDB): the Reconciliation setup controls and
-- ignore rules. See the SQLite migration of the same number.
CREATE TABLE reconcile_settings (
    id                    INT          NOT NULL PRIMARY KEY CHECK (id = 1),
    date_window_days      INT          NOT NULL DEFAULT 7    CHECK (date_window_days BETWEEN 0 AND 60),
    tolerance_percent     INT          NOT NULL DEFAULT 10   CHECK (tolerance_percent BETWEEN 0 AND 100),
    tolerance_cap_cents   BIGINT       NOT NULL DEFAULT 2500 CHECK (tolerance_cap_cents BETWEEN 0 AND 1000000),
    tolerance_floor_cents BIGINT       NOT NULL DEFAULT 100  CHECK (tolerance_floor_cents BETWEEN 0 AND 10000),
    min_line_cents        BIGINT       NOT NULL DEFAULT 100  CHECK (min_line_cents BETWEEN 0 AND 1000000),
    use_names             TINYINT      NOT NULL DEFAULT 1    CHECK (use_names IN (0, 1)),
    date_flag_days        INT          NOT NULL DEFAULT 3    CHECK (date_flag_days BETWEEN 0 AND 60),
    updated_by            BIGINT       NULL,
    updated_at            VARCHAR(32)  NOT NULL DEFAULT '',
    CONSTRAINT chk_reconcile_floor_cap CHECK (tolerance_floor_cents <= tolerance_cap_cents),
    CONSTRAINT fk_reconcile_settings_user FOREIGN KEY (updated_by) REFERENCES users (id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;

INSERT INTO reconcile_settings (id) VALUES (1);

CREATE TABLE bank_ignore_rules (
    id          BIGINT       NOT NULL AUTO_INCREMENT PRIMARY KEY,
    format_id   BIGINT       NOT NULL,
    contains    VARCHAR(120) NOT NULL CHECK (CHAR_LENGTH(contains) >= 1),
    created_by  BIGINT       NOT NULL,
    created_at  VARCHAR(32)  NOT NULL,
    UNIQUE KEY uq_bank_ignore_rules (format_id, contains),
    CONSTRAINT fk_bank_ignore_rules_format FOREIGN KEY (format_id) REFERENCES bank_formats (id),
    CONSTRAINT fk_bank_ignore_rules_user FOREIGN KEY (created_by) REFERENCES users (id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;
