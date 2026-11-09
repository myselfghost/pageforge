CREATE TABLE share_links (
    id             TEXT PRIMARY KEY,
    token          TEXT NOT NULL,
    project_id     TEXT NOT NULL REFERENCES projects(id),
    version_id     TEXT,
    password_hash  TEXT NOT NULL DEFAULT '',
    expires_at     TIMESTAMP,
    max_views      INTEGER NOT NULL DEFAULT 0,
    views          INTEGER NOT NULL DEFAULT 0,
    cache_policy   TEXT NOT NULL DEFAULT '{}',
    enabled        INTEGER NOT NULL DEFAULT 1,
    created_at     TIMESTAMP NOT NULL,
    updated_at     TIMESTAMP NOT NULL,
    deleted_at     TIMESTAMP
);

CREATE UNIQUE INDEX uq_share_links_token ON share_links(token)
    WHERE deleted_at IS NULL;
CREATE INDEX idx_share_links_project ON share_links(project_id);
