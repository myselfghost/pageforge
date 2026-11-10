# PageForge

基于 Caddy 的多项目静态站点托管系统。将 S3 兼容对象存储中的静态资源、HTML、SPA 以 URL 形式对外提供，支持多节点部署、版本管理、URL 绑定、分享链接和 Web 管理界面。

**特色功能**

- 一套代理托管 N 个 S3 alias × N 个项目 × N 个版本 × N 条 URL 绑定
- 版本不可变，回滚只改指针，10 秒内全网生效
- 发布支持 zip / tar / tgz 三种格式，魔数自动检测
- 类 nginx `try_files` + SPA fallback + 目录索引
- 本地磁盘缓存（LRU + TTL + 容量淘汰）+ manifest 分片懒加载
- 分享链接（密码/过期/访问次数限制）、版本直链
- 内置 Web 管理界面，无需额外前端部署
- SQLite / PostgreSQL 双支持，单节点即可运行

***

## 1. 部署

### 1.1 需要启动的程序

说明：proxy 可以多节点部署，目前control 只支持单节点部署。control 不可用时，proxy可以继续工作，proxy重启可以从磁盘副本恢复缓存数据。

| 程序                                            | 说明                           | 端口                      |
| --------------------------------------------- | ---------------------------- | ----------------------- |
| `pageforge-control.exe` / `pageforge-control` | 控制面：管理 API、Web 界面、发布流水线      | `:8082`（管理）、`:8081`（内部） |
| `pageforge-proxy.exe` / `pageforge-proxy`     | 数据面：Caddy + s3static 模块，对外服务 | `:80`（可配置）              |
| S3 兼容存储                                       | 如 MinIO、AWS S3、阿里云 OSS 等     | 自定义                     |

### 1.2 生成 Token 和 Key

部署前需生成以下凭证：

```bash
# admin_token：管理 API 鉴权（64 位 hex = 32 字节随机）
openssl rand -hex 32
# 或 PowerShell
[Convert]::ToHexString((1..32 | ForEach-Object { Get-Random -Max 256 }) -join '')

# internal_token：数据面访问控制面内部 API（64 位 hex）
openssl rand -hex 32

# master_key：AES-256 加密密钥（32 字节的 base64 编码）
openssl rand -base64 32
# 或 PowerShell
[Convert]::ToBase64String([System.Guid]::NewGuid().ToByteArray() + [System.Guid]::NewGuid().ToByteArray())
```

将生成的值填入 `control.yaml` 对应字段。

> **凭证变更须知**
>
> - **`admin_token`** **/** **`internal_token`**：仅在内存中用于鉴权，不落库，可随时更换。
>   更换后须同步更新所有调用方——管理 API 的客户端（Web 控制台、脚本、CI），
>   以及 Caddyfile 中 `internal_token` 或 `internal_token_file` 的内容（需 reload Caddy）。
> - **`master_key`（`encryption.master_key`）**：⚠️ **有历史数据时不能直接更换**。
>   该密钥用 AES-256-GCM 加密了 `aliases` 表中每条 S3 连接的 `secret_key`。
>   更换后旧的 `secret_cipher` 无法解密，**所有已配置的 S3 凭据都会失效**。
>   如需更换，须先用旧 key 读出每个 alias 的 secret，再用新 key 通过
>   `PATCH /aliases/{id}` 重新录入，等于全量重配凭据。

### 1.3 控制面配置 `control.yaml`

```yaml
server:
  # 管理 API 与 Web 控制台监听地址（0.0.0.0 便于远程访问；只开内网时改为 127.0.0.1:8080）
  listen: "0.0.0.0:8082"
  # 内部 API，仅数据面（Caddy 代理）访问，保持本机回环
  internal_listen: "127.0.0.1:8081"
  admin_token: "ea5d49f4541a43099b51fa1bab0bad8846d9aae4304c0f8dcc56023d986fbaa2"
  internal_token: "8e0629dd617885e71690d9932a1a75b7f7e4d5eb8ba0dc23ff4489cfff0a4fdb"
  reserved_prefix: "/__pageforge"
  snapshot_warn_bytes: 33554432

database:
  driver: sqlite
  # 相对当前工作目录；控制面启动时会自动建库
  dsn: "./control.db"
  # 多节点或已有 PostgreSQL 时改用：
  #driver: postgres
  #dsn: "postgres://erdp_test_pg:491_test@10.81.129.156:8432/pageforge"

encryption:
  # base64 编码的 32 字节 AES 主密钥，可用 PAGEFORGE_MASTER_KEY 覆盖
  master_key: "ZFIFZSjwMFc0CzY75HIdHpKQ8TlYZRy+FxA652VZV+w="

storage:
  manifest_prefix: "_system/manifests"
  package_prefix: "_system/packages"
  staging_prefix: "_system/staging"

publish:
  max_files: 200000
  max_file_bytes: 2147483648
  max_total_bytes: 53687091200
  max_package_bytes: 4294967296
  idempotency_ttl: 24h
  # 并发上传文件的 worker 数（I/O 密集型，受出口带宽与对象存储限流约束）。
  # 8 核 VM 建议 8~16；同机/单链路 S3 取小值更稳，避免触发 503 SlowDown 导致整包发布失败。
  workers: 16

gc:
  enabled: true
  interval: 1h
  # 孤儿对象回收宽限期：判定可删除后需再等这么久才真正物理删除（含失败上传残留、手动软删除及超窗版本的对象），用于避免误删。
  orphan_grace: 24h
  # 版本保留窗口，二者为「或」关系：满足任一条件即保留，只有同时不满足才被回收。
  #   keep_versions: 保留最新 N 个 ready 版本（按发布时间从新到旧计，0=不按数量保留）
  #   keep_days:     保留最近 N 天的版本（0=不按时间保留）
  # 两者都为 0 时不删除任何版本。
  # 注意：单独用 keep_versions（keep_days=0）时，若一天内发布超过 N 个版本，
  # 旧版本会立即被回收，即使刚发布不久；建议两个都设置取并集更稳妥。
  # 另外，current 版本、被 binding 固定的版本、被活跃分享链接引用的版本、
  # 以及 Protected 标记的版本始终受保护，不计入上述窗口，也不受删除影响。
  # 以上为全局默认；每个项目可在项目配置中单独设置 keep_versions / keep_days（留空继承本全局值，
  # 项目有设置时以项目设置为准，项目优先级高于此处全局配置）。
  keep_versions: 20
  keep_days: 0

log:
  # 主日志输出路径（除 HTTP 请求日志外的全部日志）；留空则只输出到 stdout
  file: "./logs/control.log"
  # HTTP 请求访问日志输出路径；留空则只输出到 stdout。注意不要与 proxy 的 access.log/error.log 重名
  access_file: "./logs/control-access.log"
  # 日志级别（两个通道共用）：debug / info / warn / error；可用 PAGEFORGE_LOG_LEVEL 覆盖
  level: info
  # 按天轮转（control-YYYY-MM-DD.log / control-access-YYYY-MM-DD.log），保留天数；0 表示不删除
  max_age_days: 30

```

**参数说明**

| 参数                           | 含义              | 取值                                                |
| ---------------------------- | --------------- | ------------------------------------------------- |
| `server.listen`              | 管理 API 监听地址     | 如 `0.0.0.0:8082`，可用 `PAGEFORGE_CONTROL_LISTEN` 覆盖 |
| `server.internal_listen`     | 内部 API 监听地址     | 建议 `127.0.0.1:8081`，仅数据面访问                        |
| `server.admin_token`         | 管理 API 鉴权 token | 64 位 hex                                          |
| `server.internal_token`      | 内部 API 鉴权 token | 64 位 hex                                          |
| `server.reserved_prefix`     | 保留路径前缀          | 默认 `/__pageforge`                                 |
| `server.snapshot_warn_bytes` | 快照体积告警阈值        | 字节数，`0` 关闭告警                                      |
| `database.driver`            | 数据库驱动           | `sqlite` 或 `postgres`                             |
| `database.dsn`               | 数据库连接串          | SQLite 文件路径 / PostgreSQL DSN                      |
| `encryption.master_key`      | AES-256 主密钥     | base64 编码 32 字节                                   |
| `publish.max_files`          | 单版本最大文件数        | 整数                                                |
| `publish.max_package_bytes`  | 上传包大小上限         | 字节                                                |
| `publish.idempotency_ttl`    | 幂等键有效期          | 如 `24h`                                           |
| `gc.enabled`                 | 是否启用垃圾回收        | `true`/`false`                                    |
| `gc.interval`                | GC 执行周期         | 如 `1h`                                            |
| `gc.keep_versions`           | 保留版本数           | 整数                                                |
| `log.file`                   | 日志文件路径          | 空则仅 stdout                                        |
| `log.max_age_days`           | 日志保留天数          | `0` 不删除                                           |

### 1.4 数据面配置 `Caddyfile`

```caddyfile
{
	admin off
	# 纯 HTTP 人工测试，不启用自动 HTTPS
	auto_https off

	# 错误日志：Caddy 自身运行日志（启动、配置重载、模块告警与错误）。
	# 目录与保留天数可用环境变量覆盖；保留天数按天为单位，例如 7d、30d、90d。
	#
	# 调试时如需把全部运行日志（含 s3static 模块日志）输出到控制台：
	#   1) 取消下一行的 debug，打开 debug 级别（默认只输出 INFO 及以上）
	#   2) 注释掉下面的 output file 块，改为 output stdout / format console
	#debug
	log {
		#output stdout
		#format console
		output file {$PAGEFORGE_LOG_DIR:./logs}/error.log {
			roll_keep 365
			roll_keep_for {$PAGEFORGE_LOG_KEEP:30d}
			roll_at 00:00
			roll_local_time
		}
		format json
	}
}

# 数据面监听端口由 site address 决定；非 root 运行时可改为 :8080 等高端口
:80 {
	# 访问日志：客户端请求，按天轮换，保留天数由 PAGEFORGE_LOG_KEEP 控制。
	#
	# 调试时如需把访问日志输出到控制台：
	#   注释掉下面的 output file 块，改为 output stdout / format console
	log {
		#output stdout
		#format console
		output file {$PAGEFORGE_LOG_DIR:./logs}/access.log {
			roll_keep 365
			roll_keep_for {$PAGEFORGE_LOG_KEEP:30d}
			roll_at 00:00
			roll_local_time
		}
		format json
	}

	route {
		s3static {
			control_url http://127.0.0.1:8081
			internal_token 8e0629dd617885e71690d9932a1a75b7f7e4d5eb8ba0dc23ff4489cfff0a4fdb
			# 与控制面共享的 AES-256 主密钥（base64），必须与 control.yaml 的
			# encryption.master_key 完全一致。
			# 控制面下发的 alias secret 是密文（与数据库存的完全一致），
			# proxy 只在内存中解密，磁盘副本里也不存在明文 secret。
			# 也可改用 master_key_file 从文件读取。
			master_key ZFIFZSjwMFc0CzY75HIdHpKQ8TlYZRy+FxA652VZV+w=
			poll_interval 5s
			# 系统保留前缀，可自定义，建议 /__pageforge
			reserved_prefix /__pageforge
			cache_dir ./cache
			# 快照磁盘副本目录：每次成功从 control 更新快照后，把
			# snapshot.json / auth-policies.json / aliases.json / shares.json 写一份到此处。
			# 启动时先尝试 control（首次 + 3 次重试），仍不可用则降级加载此副本。
			# 只有显式配置本项才会落盘；不配置则该目录下不写任何文件，
			# 重启时 control 不可用将无法服务（与 cache_dir 无关）。
			snapshot_cache_dir ./cache/.snapshot
			cache_max_bytes 10737418240
			cache_ttl 24h
			#cache_max_bytes 10240
			#cache_ttl 3m
			cache_evict_ratio 0.6
			# 与对象存储建立连接的超时，默认 10s
			upstream_connect_timeout 10s
			# 等待存储返回首字节（响应头）的超时（，默认 10s
			upstream_read_timeout 10s
			# 回源读取 body 的空闲超时：
			# 只要有数据流动就重置，仅当连续无数据超过该值才中断，默认 60s
			upstream_idle_timeout 60s
			# 单次回源的整体硬上限（0 表示不限制）
			upstream_total_timeout 0s
			# 与客户端之间的空闲超时：持续有数据则永不超时，默认 60s
			client_idle_timeout 60s
			# 与客户端之间的整体硬上限（0 表示不限制）
			client_total_timeout 0s
		}
	}
}

```

将 `internal_token` 值写入 `internal.token` 文件（与 Caddyfile 同目录或绝对路径）。

**s3static 参数说明**

| 参数                         | 含义                  | 取值                                                                                                                                                                                               |
| -------------------------- | ------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| `control_url`              | 控制面内部 API 地址        | 如 `http://127.0.0.1:8081`                                                                                                                                                                        |
| `internal_token_file`      | 内部 token 文件路径       | 文件内为纯 token 文本                                                                                                                                                                                   |
| `master_key`               | AES-256 主密钥（base64） | **必填**，须与 control 的 `encryption.master_key` 一致；用于在内存中解密 alias secret 密文                                                                                                                          |
| `master_key_file`          | 主密钥文件路径             | 与 `master_key` 二选一，文件内为纯 base64 文本                                                                                                                                                               |
| `poll_interval`            | 快照轮询间隔              | 如 `2s`                                                                                                                                                                                           |
| `reserved_prefix`          | 保留路径前缀              | 默认 `/__pageforge`                                                                                                                                                                                |
| `snapshot_cache_dir`       | 快照与内部目录磁盘副本目录       | **仅显式配置时才落盘**，与 `cache_dir` 无关；不配置则该目录下不写任何文件。写入 `snapshot.json`、`auth-policies.json`、`aliases.json`、`shares.json`，重启且 control 不可达时全部从磁盘加载。`aliases.json` 中的 secret 为 AES-256-GCM 密文，与数据库一致，不含明文 |
| `cache_dir`                | 本地对象缓存目录            | 绝对/相对路径                                                                                                                                                                                          |
| `cache_max_bytes`          | 缓存容量上限              | 字节，如 `10737418240`（10GiB）                                                                                                                                                                        |
| `cache_ttl`                | 缓存空闲过期时间            | 如 `24h`                                                                                                                                                                                          |
| `cache_evict_ratio`        | 容量淘汰比例              | `(0,1)`，如 `0.6`                                                                                                                                                                                  |
| `manifest_cache_max_bytes` | manifest 内存缓存预算     | 字节，默认 `268435456`（256MiB）                                                                                                                                                                        |
| `manifest_cache_ttl`       | manifest 缓存 TTL     | `0` 不过期                                                                                                                                                                                          |
| `upstream_connect_timeout` | 连接存储超时（建连/握手）       | 如 `10s`，默认 `10s`                                                                                                                                                                                 |
| `upstream_read_timeout`    | 等待存储首字节超时           | 如 `10s`，默认 `10s`                                                                                                                                                                                 |
| `upstream_idle_timeout`    | 回源 body 空闲超时        | 有数据流动即重置，默认 `60s`                                                                                                                                                                                |
| `upstream_total_timeout`   | 单次回源整体硬上限           | `0` 表示不限制                                                                                                                                                                                        |
| `client_idle_timeout`      | 与客户端空闲超时            | 有数据流动即重置，默认 `60s`                                                                                                                                                                                |
| `client_total_timeout`     | 与客户端整体硬上限           | `0` 表示不限制                                                                                                                                                                                        |

**超时语义（参考 nginx）**

- `upstream_connect_timeout`：建立到对象存储的 TCP/TLS 连接的最大时长（对应 nginx `proxy_connect_timeout`）。
- `upstream_read_timeout`：请求发出后等待存储返回首个响应字节的最大时长（对应 `proxy_read_timeout` 的头部阶段）。
- `upstream_idle_timeout`：读取存储响应 body 时，**两次数据之间的最大间隔**。只要持续有字节流动就不断重置，因此长时间的大文件下载不会被中断；只有真正卡住（连续无数据）才会超时（对应 `proxy_read_timeout` 的 body 阶段）。
- `upstream_total_timeout`：单次回源的整体硬上限，兜底防止半死不活的慢连接长期占用，`0` 关闭。
- `client_idle_timeout`：代理与客户端之间**无数据互通**的最大时长（对应 `send_timeout`）。持续下载不会触发，仅当客户端停滞时中断。
- `client_total_timeout`：整个客户端请求的整体硬上限，`0` 关闭。

### 1.5 启动

```bash
# 1. 启动控制面
./pageforge-control run --config ./control.yaml

# 2. 启动数据面（监听 :80）
./pageforge-proxy run --config ./Caddyfile

# 多数据面节点：每台机器各跑一个 caddy-pageforge，
# control_url 指向控制面的 internal_listen 地址即可
```

### 1.6 环境变量

| 变量                           | 覆盖                      |
| ---------------------------- | ----------------------- |
| `PAGEFORGE_CONTROL_LISTEN`   | `server.listen`         |
| `PAGEFORGE_CONTROL_LOG_FILE` | `log.file`              |
| `PAGEFORGE_MASTER_KEY`       | `encryption.master_key` |
| `PAGEFORGE_LOG_DIR`          | Caddy 日志目录（默认 `./logs`） |
| `PAGEFORGE_LOG_KEEP`         | Caddy 日志保留天数（默认 `30d`）  |

***

## 2. 快速开始

### 2.1 创建 S3 别名

```bash
curl -X POST http://127.0.0.1:8082/api/v1/aliases \
  -H "Authorization: Bearer <admin_token>" \
  -H "Content-Type: application/json" \
  -d '{
    "name": "minio-prod",
    "endpoint": "http://127.0.0.1:9000",
    "region": "us-east-1",
    "access_key": "minioadmin",
    "secret_key": "minioadmin",
    "path_style": true,
    "bucket_allowlist": ["static"]
  }'
```

### 2.2 创建项目

```bash
curl -X POST http://127.0.0.1:8082/api/v1/projects \
  -H "Authorization: Bearer <admin_token>" \
  -H "Content-Type: application/json" \
  -d '{
    "name": "portal",
    "alias_id": "<alias_id>",
    "bucket": "static",
    "index_doc": "index.html",
    "error_doc": "404.html",
    "try_files_enabled": true,
    "spa": {"enabled": true}
  }'
```

### 2.3 发布站点

```bash
# 1. 创建发布任务（mode 可省略，自动检测 zip/tar/tgz）
curl -X POST http://127.0.0.1:8082/api/v1/deployments \
  -H "Authorization: Bearer <admin_token>" \
  -H "Content-Type: application/json" \
  -d '{"project_id": "<project_id>", "label": "v1.0"}'
# 返回 deployment_id 和 version_id

# 2. 上传发布包（zip / tar / tgz 均可）
curl -X PUT http://127.0.0.1:8082/api/v1/deployments/<dep_id>/package \
  -H "Authorization: Bearer <admin_token>" \
  --data-binary @site.zip

# 3. 完成发布
curl -X POST http://127.0.0.1:8082/api/v1/deployments/<dep_id>/complete \
  -H "Authorization: Bearer <admin_token>"
```

### 2.4 绑定 URL

```bash
curl -X POST http://127.0.0.1:8082/api/v1/bindings \
  -H "Authorization: Bearer <admin_token>" \
  -H "Content-Type: application/json" \
  -d '{
    "host": "*",
    "path_prefix": "/portal",
    "project_id": "<project_id>"
  }'
```

访问 `http://<host>/portal/` 即可看到站点。

### 2.5 Web 管理界面

浏览器打开 `http://<host>:8082/` 即可访问内置管理界面，支持项目、版本、绑定、发布等全部操作的可视化管理。

***

## 3. API 文档

统一前缀 `/api/v1`，鉴权 `Authorization: Bearer <admin_token>`。
错误格式：`{"error":{"code":"...","message":"...","request_id":"..."}}`

### 3.1 Alias（S3 连接）

| 方法     | 路径                   | 说明            |
| ------ | -------------------- | ------------- |
| GET    | `/aliases`           | 列表            |
| POST   | `/aliases`           | 创建            |
| GET    | `/aliases/{id}`      | 详情            |
| PATCH  | `/aliases/{id}`      | 更新（不含 secret） |
| DELETE | `/aliases/{id}`      | 删除            |
| POST   | `/aliases/{id}/test` | 测试连通性         |

### 3.2 Project（项目）

| 方法     | 路径                                      | 说明        |
| ------ | --------------------------------------- | --------- |
| GET    | `/projects`                             | 列表        |
| POST   | `/projects`                             | 创建        |
| GET    | `/projects/{id}`                        | 详情        |
| PATCH  | `/projects/{id}`                        | 更新        |
| DELETE | `/projects/{id}`                        | 删除（软删除）   |
| GET    | `/projects/{id}/versions`               | 版本列表      |
| GET    | `/projects/{id}/versions/{vid}`         | 版本详情      |
| DELETE | `/projects/{id}/versions/{vid}`         | 删除版本      |
| POST   | `/projects/{id}/pointer`                | 切换当前版本    |
| POST   | `/projects/{id}/versions/{vid}/protect` | 设置/取消版本保护 |
| GET    | `/projects/{id}/versions/{vid}/package` | 下载原始发布包   |

### 3.3 Binding（URL 绑定）

| 方法     | 路径                   | 说明   |
| ------ | -------------------- | ---- |
| GET    | `/bindings`          | 列表   |
| POST   | `/bindings`          | 创建   |
| GET    | `/bindings/{id}`     | 详情   |
| PATCH  | `/bindings/{id}`     | 更新   |
| DELETE | `/bindings/{id}`     | 删除   |
| POST   | `/bindings/validate` | 校验冲突 |

### 3.4 Deployment（发布）

| 方法   | 路径                           | 说明                 |
| ---- | ---------------------------- | ------------------ |
| POST | `/deployments`               | 创建发布任务             |
| PUT  | `/deployments/{id}/package`  | 上传发布包（zip/tar/tgz） |
| POST | `/deployments/{id}/complete` | 完成发布               |
| GET  | `/deployments/{id}`          | 查询状态               |
| POST | `/deployments/{id}/cancel`   | 取消发布               |

### 3.5 Auth Policy（鉴权策略）

| 方法     | 路径                                     | 说明                                  |
| ------ | -------------------------------------- | ----------------------------------- |
| GET    | `/auth-policies`                       | 列表                                  |
| POST   | `/auth-policies`                       | 创建（`{"name":"...","mode":"basic"}`） |
| GET    | `/auth-policies/{id}`                  | 详情（含用户列表）                           |
| PATCH  | `/auth-policies/{id}`                  | 更新                                  |
| DELETE | `/auth-policies/{id}`                  | 删除                                  |
| PUT    | `/auth-policies/{id}/users/{username}` | 设置密码                                |
| DELETE | `/auth-policies/{id}/users/{username}` | 删除用户                                |

### 3.6 Share Links（分享链接）

| 方法     | 路径                  | 说明                     |
| ------ | ------------------- | ---------------------- |
| GET    | `/share-links`      | 列表（可按 `project_id` 过滤） |
| POST   | `/share-links`      | 创建                     |
| GET    | `/share-links/{id}` | 详情                     |
| PATCH  | `/share-links/{id}` | 更新                     |
| DELETE | `/share-links/{id}` | 删除                     |

分享链接访问路径：`/__pageforge/s/{token}/*`（支持密码、过期、访问次数限制）。

### 3.7 版本直链

访问路径：`/__pageforge/v/{project_id}/{version_id}/*`，无需 binding 即可直接访问指定版本。

### 3.8 系统

| 方法   | 路径                         | 说明            |
| ---- | -------------------------- | ------------- |
| GET  | `/healthz`                 | 健康检查          |
| GET  | `/api/v1/snapshot`         | 获取当前快照        |
| POST | `/api/v1/snapshot/refresh` | 强制刷新快照        |
| GET  | `/metrics`                 | Prometheus 指标 |

### 3.9 内部 API（数据面专用）

鉴权：`X-Internal-Token`。仅监听内网地址。

| 方法   | 路径                                           | 说明                                     |
| ---- | -------------------------------------------- | -------------------------------------- |
| GET  | `/internal/v1/snapshot`                      | 拉取快照（ETag = generation）                |
| GET  | `/internal/v1/alias-credentials`             | 拉取全部 alias 的 S3 凭证目录（数据面随快照轮询，不再逐请求拉取） |
| GET  | `/internal/v1/aliases/{id}/credentials`      | 拉取单个 alias 的 S3 凭证（兼容旧调用）              |
| GET  | `/internal/v1/auth-policies`                 | 拉取鉴权策略（含 bcrypt 哈希）                    |
| GET  | `/internal/v1/share-links`                   | 拉取全部分享链接目录（含 bcrypt 哈希，支持离线鉴权）         |
| POST | `/internal/v1/share-links/{token}/views`     | 数据面回写分享链接访问计数（尽力而为，control 不可达时本地累计）   |
| POST | `/internal/v1/share-links/{token}/authorize` | 分享链接鉴权（数据面目录缺失时的回退路径）                  |
| POST | `/internal/v1/reload`                        | 强制数据面刷新                                |

***

## 4. 编译打包

### 4.1 windows机器上编译二进制

```bash
#编译出exe
& "C:\Program Files\Go\bin\go.exe" build -trimpath -ldflags "-s -w" -o bin\pageforge-control.exe ./cmd/control; if($LASTEXITCODE -eq 0){ & "C:\Program Files\Go\bin\go.exe" build -trimpath -ldflags "-s -w" -o bin\pageforge-proxy.exe ./cmd/proxy }; Write-Output ("exit=" + $LASTEXITCODE); Get-ChildItem bin | Select-Object Name,Length,LastWriteTime  

#编译出Linux二进制
$env:GOOS="linux"; $env:GOARCH="amd64"; $env:CGO_ENABLED="0";& "C:\Program Files\Go\bin\go.exe" build -trimpath -ldflags "-s -w" -o dist\pageforge-linux-amd64\pageforge-control ./cmd/control; if($LASTEXITCODE -eq 0){ & "C:\Program Files\Go\bin\go.exe" build -trimpath -ldflags "-s -w" -o dist\pageforge-linux-amd64\pageforge-proxy ./cmd/proxy };Write-Output ("linux_exit=" + $LASTEXITCODE);Remove-Item Env:GOOS,Env:GOARCH,Env:CGO_ENABLED;Get-ChildItem dist\pageforge-linux-amd64 | Select-Object Name,Length,LastWriteTime  


```

### 4.2 Linux编译

```bash
# 参考上面
# 产出 caddy，重命名为 caddy-pageforge
```

### 4.3 运行测试

```bash
go test ./...
```

***

## 5. 目录结构

```
├── cmd/
│   ├── control/          # 控制面入口
│   ├── proxy/            # 数据面入口（独立 Caddy 进程）
│   ├── e2e/              # E2E 测试
│   └── cachetest/        # 缓存压测
├── caddy/                # Caddy s3static 模块
├── internal/
│   ├── alias/            # S3 别名管理
│   ├── api/              # 管理 API 路由与 handler
│   ├── authpolicy/       # 鉴权策略
│   ├── binding/          # URL 绑定
│   ├── cache/            # 本地对象缓存
│   ├── config/           # 配置加载
│   ├── credential/       # 凭证加密
│   ├── crypto/           # AES-256-GCM
│   ├── deploy/           # 发布流水线
│   ├── gc/               # 垃圾回收
│   ├── handler/          # 数据面请求处理（try_files/SPA/分享/直链）
│   ├── ids/              # ULID 生成
│   ├── internalapi/      # 内部 API
│   ├── manifest/         # manifest 分片生成
│   ├── model/            # 数据模型
│   ├── objectstore/      # S3 客户端（MinIO）
│   ├── pageconfig/       # pageforge.json 解析
│   ├── project/          # 项目管理
│   ├── router/           # 路由匹配（host + path trie）
│   ├── share/            # 分享链接
│   ├── snapshot/         # 快照生成
│   ├── store/            # 数据库访问
│   ├── version/          # 版本管理
│   └── webui/            # Web 管理界面
├── migrations/           # 数据库迁移（sqlite + postgres）
├── bin/                  # 运行时配置与二进制
└── docs/                 # 设计文档
```

