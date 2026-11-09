-- 记录版本对象被 GC 物理清理的时间，避免重复扫描/重复删除（幂等标记）。
ALTER TABLE versions ADD COLUMN objects_purged_at TIMESTAMP;
