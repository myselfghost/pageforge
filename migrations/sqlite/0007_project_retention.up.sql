-- 项目级版本保留策略（可选）：NULL 表示继承全局 gc.keep_versions / gc.keep_days。
ALTER TABLE projects ADD COLUMN keep_versions INTEGER;
ALTER TABLE projects ADD COLUMN keep_days INTEGER;
