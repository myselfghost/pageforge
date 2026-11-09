CREATE TABLE aliases (
    id               TEXT PRIMARY KEY,
    name             TEXT NOT NULL UNIQUE,
    endpoint         TEXT NOT NULL,
    region           TEXT NOT NULL,
    access_key       TEXT NOT NULL,
    secret_cipher    BYTEA NOT NULL,
    path_style       BOOLEAN NOT NULL DEFAULT TRUE,
    bucket_allowlist TEXT NOT NULL DEFAULT '[]',
    tls_skip_verify  BOOLEAN NOT NULL DEFAULT FALSE,
    credential_ver   BIGINT NOT NULL DEFAULT 1,
    created_at       TIMESTAMPTZ NOT NULL,
    updated_at       TIMESTAMPTZ NOT NULL
);

CREATE TABLE projects (
    id                 TEXT PRIMARY KEY,
    name               TEXT NOT NULL,
    description        TEXT NOT NULL DEFAULT '',
    alias_id           TEXT REFERENCES aliases(id) ON DELETE SET NULL,
    bucket             TEXT NOT NULL,
    current_version_id TEXT,
    index_doc          TEXT NOT NULL DEFAULT 'index.html',
    error_doc          TEXT NOT NULL DEFAULT '',
    try_files_enabled  BOOLEAN NOT NULL DEFAULT FALSE,
    package_retention  BOOLEAN NOT NULL DEFAULT TRUE,
    spa_config         TEXT NOT NULL DEFAULT '{}',
    cache_policy       TEXT NOT NULL DEFAULT '{}',
    response_headers   TEXT NOT NULL DEFAULT '[]',
    status             TEXT NOT NULL DEFAULT 'active',
    deleted_at         TIMESTAMPTZ,
    created_at         TIMESTAMPTZ NOT NULL,
    updated_at         TIMESTAMPTZ NOT NULL
);

CREATE TABLE versions (
    id              TEXT PRIMARY KEY,
    project_id      TEXT NOT NULL REFERENCES projects(id),
    deployment_id   TEXT NOT NULL DEFAULT '',
    label           TEXT,
    status          TEXT NOT NULL,
    manifest_prefix TEXT NOT NULL,
    manifest_sha256 TEXT NOT NULL DEFAULT '',
    file_count      BIGINT NOT NULL DEFAULT 0,
    total_bytes     BIGINT NOT NULL DEFAULT 0,
    error_message   TEXT NOT NULL DEFAULT '',
    protected       BOOLEAN NOT NULL DEFAULT FALSE,
    created_at      TIMESTAMPTZ NOT NULL,
    ready_at        TIMESTAMPTZ,
    published_at    TIMESTAMPTZ,
    deleted_at      TIMESTAMPTZ
);

CREATE TABLE bindings (
    id                TEXT PRIMARY KEY,
    host              TEXT NOT NULL DEFAULT '*',
    path_prefix       TEXT NOT NULL,
    project_id        TEXT NOT NULL REFERENCES projects(id),
    pinned_version_id TEXT,
    auth_policy_id    TEXT NOT NULL DEFAULT '',
    response_headers  TEXT NOT NULL DEFAULT '[]',
    enabled           BOOLEAN NOT NULL DEFAULT TRUE,
    created_at        TIMESTAMPTZ NOT NULL,
    updated_at        TIMESTAMPTZ NOT NULL,
    deleted_at        TIMESTAMPTZ
);

CREATE TABLE auth_policies (
    id             TEXT PRIMARY KEY,
    name           TEXT NOT NULL,
    mode           TEXT NOT NULL,
    config         TEXT NOT NULL DEFAULT '{}',
    secret_version BIGINT NOT NULL DEFAULT 1,
    created_at     TIMESTAMPTZ NOT NULL,
    updated_at     TIMESTAMPTZ NOT NULL
);

CREATE TABLE auth_users (
    policy_id     TEXT NOT NULL REFERENCES auth_policies(id),
    username      TEXT NOT NULL,
    password_hash TEXT NOT NULL,
    PRIMARY KEY(policy_id, username)
);

CREATE TABLE deployments (
    id              TEXT PRIMARY KEY,
    project_id      TEXT NOT NULL REFERENCES projects(id),
    mode            TEXT NOT NULL,
    status          TEXT NOT NULL,
    idempotency_key TEXT,
    package_key     TEXT NOT NULL DEFAULT '',
    staging_prefix  TEXT NOT NULL DEFAULT '',
    error_message   TEXT NOT NULL DEFAULT '',
    created_at      TIMESTAMPTZ NOT NULL,
    completed_at    TIMESTAMPTZ
);

CREATE TABLE audit_logs (
    id          TEXT PRIMARY KEY,
    actor       TEXT NOT NULL,
    action      TEXT NOT NULL,
    object_type TEXT NOT NULL,
    object_id   TEXT NOT NULL,
    detail      TEXT NOT NULL DEFAULT '{}',
    result      TEXT NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL
);

CREATE INDEX idx_versions_project ON versions(project_id, published_at DESC);
CREATE INDEX idx_versions_deployment ON versions(deployment_id);
CREATE INDEX idx_bindings_project ON bindings(project_id);
CREATE INDEX idx_deployments_status ON deployments(status, created_at);

CREATE UNIQUE INDEX uq_versions_project_label ON versions(project_id, label)
    WHERE label IS NOT NULL AND deleted_at IS NULL;
CREATE UNIQUE INDEX uq_bindings_host_path ON bindings(host, path_prefix)
    WHERE deleted_at IS NULL;
CREATE UNIQUE INDEX uq_deployments_idem ON deployments(project_id, idempotency_key)
    WHERE idempotency_key IS NOT NULL;
