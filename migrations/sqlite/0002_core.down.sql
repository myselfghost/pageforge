DROP INDEX IF EXISTS uq_deployments_idem;
DROP INDEX IF EXISTS uq_bindings_host_path;
DROP INDEX IF EXISTS uq_versions_project_label;
DROP INDEX IF EXISTS idx_deployments_status;
DROP INDEX IF EXISTS idx_bindings_project;
DROP INDEX IF EXISTS idx_versions_deployment;
DROP INDEX IF EXISTS idx_versions_project;

DROP TABLE IF EXISTS audit_logs;
DROP TABLE IF EXISTS deployments;
DROP TABLE IF EXISTS auth_users;
DROP TABLE IF EXISTS auth_policies;
DROP TABLE IF EXISTS bindings;
DROP TABLE IF EXISTS versions;
DROP TABLE IF EXISTS projects;
DROP TABLE IF EXISTS aliases;
