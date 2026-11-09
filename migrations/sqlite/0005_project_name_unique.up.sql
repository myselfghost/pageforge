-- project name 全局唯一（软删除记录不占用唯一约束）
CREATE UNIQUE INDEX uq_projects_name ON projects(name)
    WHERE deleted_at IS NULL;
