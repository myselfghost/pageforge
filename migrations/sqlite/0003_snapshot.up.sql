CREATE TABLE snapshot_state (
    id         INTEGER PRIMARY KEY,
    generation BIGINT NOT NULL DEFAULT 0,
    updated_at TIMESTAMP NOT NULL
);

INSERT INTO snapshot_state (id, generation, updated_at) VALUES (1, 0, CURRENT_TIMESTAMP);
