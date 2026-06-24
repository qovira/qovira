-- +goose Up
-- +goose StatementBegin
-- Defense-in-depth: enforce at the DB level that every message row's
-- (conversation_id, user_id) pair refers to a conversation owned by that same
-- user. The application-level ownership check in UpsertConversation is the
-- primary guard; this FK is the backstop that prevents a cross-user message
-- from landing even if the app check were somehow bypassed.
--
-- SQLite requires the FK target to be a UNIQUE key (or PRIMARY KEY). Since
-- conversations.id is already the PK (unique), (id, user_id) is also
-- effectively unique, but the FK reference needs an explicit UNIQUE index on
-- the target columns. We add that index first, then recreate messages with the
-- FK declared.
--
-- foreign_keys is set ON per-connection in store.applyPragmas, so this
-- constraint is enforced at runtime.
--
-- HOUSE RULE — future post-release table rebuilds of FK-referenced tables:
-- If conversations (or any table referenced by FKs) ever needs a column-type
-- or constraint change after the schema has shipped, the rebuild MUST use the
-- goose NO TRANSACTION directive with manual PRAGMA foreign_keys=OFF before
-- BEGIN and PRAGMA foreign_key_check before COMMIT (see SQLite guide §7.2).
-- PRAGMA foreign_keys is a no-op inside a goose per-migration transaction, so
-- toggling it inside the migration body is silently ineffective; the NO
-- TRANSACTION directive is the only way to set it before the transaction
-- begins. All in-branch rebuilds (not yet shipped) may use normal goose
-- transactions and the inline PRAGMA foreign_keys=OFF approach, as this schema
-- is not yet published.

-- Step 1: add a UNIQUE index on conversations(id, user_id) so the composite
-- FK target is resolvable.
CREATE UNIQUE INDEX conversations_id_user_id ON conversations (id, user_id);

-- Step 2: SQLite does not support ADD CONSTRAINT after the fact, so we
-- recreate the messages table with the FK declared.
CREATE TABLE messages_new (
    id              TEXT NOT NULL PRIMARY KEY,
    conversation_id TEXT NOT NULL,
    user_id         TEXT NOT NULL,
    role            TEXT NOT NULL,
    content         TEXT NOT NULL,
    tool_calls      TEXT,
    tool_call_id    TEXT,
    finish_reason   TEXT,
    created_at      TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    FOREIGN KEY (conversation_id, user_id) REFERENCES conversations (id, user_id)
) STRICT, WITHOUT ROWID;

INSERT INTO messages_new
SELECT id, conversation_id, user_id, role, content, tool_calls, tool_call_id, finish_reason, created_at
FROM messages;

DROP TABLE messages;

ALTER TABLE messages_new RENAME TO messages;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
-- Restore messages without the FK and drop the unique index.
CREATE TABLE messages_old (
    id              TEXT NOT NULL PRIMARY KEY,
    conversation_id TEXT NOT NULL,
    user_id         TEXT NOT NULL,
    role            TEXT NOT NULL,
    content         TEXT NOT NULL,
    tool_calls      TEXT,
    tool_call_id    TEXT,
    finish_reason   TEXT,
    created_at      TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
) STRICT, WITHOUT ROWID;

INSERT INTO messages_old
SELECT id, conversation_id, user_id, role, content, tool_calls, tool_call_id, finish_reason, created_at
FROM messages;

DROP TABLE messages;

ALTER TABLE messages_old RENAME TO messages;

DROP INDEX IF EXISTS conversations_id_user_id;
-- +goose StatementEnd
