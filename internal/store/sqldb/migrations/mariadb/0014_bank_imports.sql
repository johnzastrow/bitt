-- 0014_bank_imports (MariaDB): bank statement imports for reconciliation. See
-- the SQLite migration of the same number for the reasoning.
--
-- Free text from a bank file is TEXT: the parser refuses any row over 16 KB,
-- so every cell fits.
CREATE TABLE bank_formats (
    id                BIGINT       NOT NULL AUTO_INCREMENT PRIMARY KEY,
    name              VARCHAR(120) NOT NULL CHECK (CHAR_LENGTH(name) >= 1),
    header_signature  CHAR(64)     NOT NULL CHECK (CHAR_LENGTH(header_signature) = 64),
    header_text       TEXT         NOT NULL,
    date_col          INT          NOT NULL CHECK (date_col >= 0),
    date_layout       VARCHAR(20)  NOT NULL,
    amount_col        INT          NOT NULL CHECK (amount_col >= -1),
    debit_col         INT          NOT NULL CHECK (debit_col >= -1),
    credit_col        INT          NOT NULL CHECK (credit_col >= -1),
    incoming_negative TINYINT      NOT NULL CHECK (incoming_negative IN (0, 1)),
    decimal_comma     TINYINT      NOT NULL CHECK (decimal_comma IN (0, 1)),
    description_col   INT          NOT NULL CHECK (description_col >= 0),
    account_col       INT          NOT NULL CHECK (account_col >= -1),
    note_col          INT          NOT NULL CHECK (note_col >= -1),
    reference_col     INT          NOT NULL CHECK (reference_col >= -1),
    created_by        BIGINT       NOT NULL,
    created_at        VARCHAR(32)  NOT NULL,
    UNIQUE KEY uq_bank_formats_signature (header_signature),
    CONSTRAINT fk_bank_formats_creator FOREIGN KEY (created_by) REFERENCES users (id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;

CREATE TABLE bank_imports (
    id              BIGINT       NOT NULL AUTO_INCREMENT PRIMARY KEY,
    format_id       BIGINT       NOT NULL,
    file_name       VARCHAR(255) NOT NULL,
    file_sha256     CHAR(64)     NOT NULL CHECK (CHAR_LENGTH(file_sha256) = 64),
    uploaded_by     BIGINT       NOT NULL,
    uploaded_at     VARCHAR(32)  NOT NULL,
    kept            INT          NOT NULL CHECK (kept >= 0),
    duplicates      INT          NOT NULL CHECK (duplicates >= 0),
    outgoing        INT          NOT NULL CHECK (outgoing >= 0),
    ignored         INT          NOT NULL CHECK (ignored >= 0),
    refused         INT          NOT NULL CHECK (refused >= 0),
    first_date      VARCHAR(10)  NOT NULL DEFAULT '',
    last_date       VARCHAR(10)  NOT NULL DEFAULT '',
    refused_detail  TEXT         NOT NULL,
    KEY idx_bank_imports_uploaded (uploaded_at),
    CONSTRAINT fk_bank_imports_format FOREIGN KEY (format_id) REFERENCES bank_formats (id),
    CONSTRAINT fk_bank_imports_uploader FOREIGN KEY (uploaded_by) REFERENCES users (id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;

CREATE TABLE bank_lines (
    id             BIGINT       NOT NULL AUTO_INCREMENT PRIMARY KEY,
    import_id      BIGINT       NOT NULL,
    file_row       INT          NOT NULL CHECK (file_row >= 2),
    original_text  TEXT         NOT NULL,
    account        TEXT         NOT NULL,
    posted_on      CHAR(10)     NOT NULL CHECK (CHAR_LENGTH(posted_on) = 10),
    amount_cents   BIGINT       NOT NULL CHECK (amount_cents > 0),
    description    TEXT         NOT NULL,
    note           TEXT         NOT NULL,
    reference      TEXT         NOT NULL,
    fingerprint    CHAR(64)     NOT NULL CHECK (CHAR_LENGTH(fingerprint) = 64),
    state          VARCHAR(10)  NOT NULL DEFAULT 'open'
                   CHECK (state IN ('open', 'matched', 'recorded', 'ignored')),
    ignore_reason  VARCHAR(255) NOT NULL DEFAULT '',
    UNIQUE KEY uq_bank_lines_fingerprint (fingerprint),
    KEY idx_bank_lines_import (import_id, file_row),
    KEY idx_bank_lines_state (state, posted_on),
    CONSTRAINT fk_bank_lines_import FOREIGN KEY (import_id) REFERENCES bank_imports (id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;
