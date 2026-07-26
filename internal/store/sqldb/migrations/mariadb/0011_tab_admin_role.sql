-- 0011_tab_admin_role (MariaDB): add 'admin' as a tab participant role. See the
-- SQLite migration of the same number for the full reasoning.
--
-- The table is rebuilt rather than altered in place. The inline CHECK from 0001
-- is auto-named after its column, 'role', which is a reserved word AND collides
-- with the column name -- MariaDB's DROP CONSTRAINT cannot resolve it (error
-- 1091) and DROP CHECK is MySQL-only syntax it rejects (error 1064). A rebuild
-- sidesteps both. Nothing references tab_participants, so dropping it cascades to
-- nothing. The new foreign keys are left unnamed so they cannot collide with the
-- originals while both tables briefly coexist; InnoDB auto-names them.
CREATE TABLE tab_participants_new (
    tab_id    BIGINT      NOT NULL,
    user_id   BIGINT      NOT NULL,
    role      VARCHAR(20) NOT NULL CHECK (role IN ('provider', 'payee', 'admin')),
    added_at  VARCHAR(32) NOT NULL,
    PRIMARY KEY (tab_id, user_id),
    KEY idx_participants_user (user_id),
    FOREIGN KEY (tab_id)  REFERENCES tabs  (id) ON DELETE CASCADE,
    FOREIGN KEY (user_id) REFERENCES users (id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;

INSERT INTO tab_participants_new (tab_id, user_id, role, added_at)
    SELECT tab_id, user_id, role, added_at FROM tab_participants;

DROP TABLE tab_participants;

ALTER TABLE tab_participants_new RENAME TO tab_participants;
