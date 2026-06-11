-- +goose Up
-- +goose StatementBegin
CREATE TABLE instance (
    id         INTEGER PRIMARY KEY CHECK (id = 1),
    created_at TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);
-- +goose StatementEnd
-- +goose StatementBegin
INSERT INTO instance (id) VALUES (1);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE instance;
-- +goose StatementEnd
