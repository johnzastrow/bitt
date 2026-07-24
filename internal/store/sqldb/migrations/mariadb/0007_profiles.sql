-- 0007_profiles (MariaDB): account self-service. See the SQLite migration of
-- the same number for why the avatar is a BLOB in the database rather than a
-- file on disk.
--
-- MEDIUMBLOB rather than SQLite's untyped BLOB: MySQL's plain BLOB caps at
-- 64 KB, and while a processed avatar is far smaller, MEDIUMBLOB removes the
-- ceiling as a thing to think about. The bytes are always PNG produced by
-- internal/avatar, so the column needs no content-type.
ALTER TABLE users ADD COLUMN avatar_png MEDIUMBLOB NULL;

ALTER TABLE users ADD COLUMN avatar_updated_at VARCHAR(32) NOT NULL DEFAULT '';
