-- 0011_tab_admin_role (SQLite): add 'admin' as a tab participant role.
--
-- A tab administrator is a per-tab manager -- someone who can change the tab's
-- settings, schedule, items, and people, and who transacts on it as a member --
-- distinct from the Provider (the single biller) and a Payee. It is added when a
-- household wants a second person to help run a tab.
--
-- SQLite cannot alter a CHECK constraint in place, so the table is rebuilt.
-- Nothing references tab_participants (no foreign key points at it), so this is
-- safe even with foreign_keys on: copying the existing rows passes the FK checks
-- to tabs/users, and dropping the old table cascades to nothing. The one index
-- is recreated.
CREATE TABLE tab_participants_new (
    tab_id    INTEGER NOT NULL REFERENCES tabs (id) ON DELETE CASCADE,
    user_id   INTEGER NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    role      TEXT    NOT NULL CHECK (role IN ('provider', 'payee', 'admin')),
    added_at  TEXT    NOT NULL,
    PRIMARY KEY (tab_id, user_id)
);

INSERT INTO tab_participants_new (tab_id, user_id, role, added_at)
    SELECT tab_id, user_id, role, added_at FROM tab_participants;

DROP TABLE tab_participants;

ALTER TABLE tab_participants_new RENAME TO tab_participants;

CREATE INDEX idx_participants_user ON tab_participants (user_id);
