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
);

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
);

INSERT INTO messages_old
SELECT id, conversation_id, user_id, role, content, tool_calls, tool_call_id, finish_reason, created_at
FROM messages;

DROP TABLE messages;

ALTER TABLE messages_old RENAME TO messages;

DROP INDEX IF EXISTS conversations_id_user_id;
-- +goose StatementEnd
