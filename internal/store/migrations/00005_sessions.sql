-- +goose Up
-- +goose StatementBegin
-- sessions stores opaque bearer session tokens (as sha256 hashes) for authenticated Qovira
-- users.  The plaintext token is never stored; only the sha256 digest is kept as the lookup
-- key.  Expiry is computed from created_at (absolute cap) and last_used_at (sliding idle
-- window) at validation time — no expiry column is needed, so a TTL change requires no
-- migration.  ON DELETE CASCADE ensures sessions are removed when the owning user is deleted.
CREATE TABLE sessions (
  id           TEXT NOT NULL PRIMARY KEY,  -- ULID
  user_id      TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  token_hash   BLOB NOT NULL UNIQUE,       -- sha256(token); the lookup key
  created_at   TEXT NOT NULL,              -- anchors the absolute cap
  last_used_at TEXT NOT NULL               -- anchors the sliding idle window
) STRICT, WITHOUT ROWID;
CREATE INDEX sessions_user_id ON sessions(user_id);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS sessions_user_id;
DROP TABLE sessions;
-- +goose StatementEnd
