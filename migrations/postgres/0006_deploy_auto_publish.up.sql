-- deployments 记录发布意图：auto_publish=false 时仅上传版本，不切换当前指针
ALTER TABLE deployments ADD COLUMN auto_publish BOOLEAN NOT NULL DEFAULT TRUE;
