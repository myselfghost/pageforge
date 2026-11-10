# PageForge 任务拆解（Todo List）

| 项目 | 内容 |
|---|---|
| 文档版本 | v1.1（评审修订） |
| 状态 | 定稿，作为编码排期输入 |
| 关联文档 | `docs/requirements.md`、`docs/architecture.md` |

说明：

- 任务 ID 格式 `M{阶段}-T{序号}`，每个任务可独立完成、独立验收。
- “依赖”列表示前置任务；无依赖的任务可并行。
- “验收”引用 requirements.md 的 FR 编号和第 7 节场景编号（S1~S10）。
- 每个任务的完成定义（DoD）：代码 + 单元测试通过 + 不破坏已有测试。
- 二期功能拆分为 M6（FR-17~FR-21 已实现；FR-22 发布模式 B、FR-23 鉴权扩展模式明确不做，仅保留架构 27.7 节预留）。

## 阶段总览

| 阶段 | 目标 | 出口条件 |
|---|---|---|
| M0 骨架 | 仓库可构建、可测试、可起空服务 | CI 绿，xcaddy 构建通过 |
| M1 领域与存储 | 元数据模型 + S3 抽象就绪 | alias/project/binding CRUD 可用 |
| M2 发布与 manifest | zip 发布 → 对象 + manifest → 切指针 | S1、S8、S9 控制面部分 |
| M3 数据面 | 请求全链路可用 | S1~S5、S8、S9 全通 |
| M4 鉴权与多节点 | Basic/IP、多节点快照 | S6、S7、S10 |
| M5 生产化 | 观测、GC、部署、E2E、压测 | 需求第 6 节非功能项达标 |

## 进度跟踪

| 阶段 | 进度 | 子任务状态 |
|---|---|---|
| M0 骨架 | 已完成 6/6 | T1 完成 · T2 完成 · T3 完成 · T4 完成 · T5 完成 · T6 完成 |
| M1 领域与存储 | 已完成 6/6 | T1 完成 · T2 完成 · T3 完成 · T4 完成 · T5 完成 · T6 完成 |
| M2 发布与 manifest | 已完成 6/6 | T1 完成 · T2 完成 · T3 完成 · T4 完成 · T5 完成 · T6 完成 |
| M3 数据面请求链路 | 已完成 8/8 | T1 完成 · T2 完成 · T3 完成 · T4 完成 · T5 完成 · T6 完成 · T7 完成 · T8 完成 |
| M4 鉴权与多节点 | 待开始 0/3 | - |
| M5 生产化 | 待开始 0/8 | - |
| M6 二期扩展（预留） | 已完成 5/5（FR-22/FR-23 不做） | FR-17 完成 · FR-18 完成 · FR-19 完成 · FR-20 完成 · FR-21 完成 |

## M0 骨架（0.5 周量级，全部可并行度低）

| ID | 任务 | 产出/涉及 | 依赖 | 验收 |
|---|---|---|---|---|
| M0-T1 | 初始化 Go module 与目录结构 | `cmd/control`、`cmd/proxy`、`caddy/`、`internal/*`、`migrations/`、`testdata/` | - | `go build ./...` 通过 |
| M0-T2 | 控制面配置加载 | `internal/config`：YAML + env 覆盖（`PAGEFORGE_` 前缀），含 listen/internal_listen/reserved_prefix | M0-T1 | 配置单测：默认值、env 覆盖 |
| M0-T3 | 日志与 request_id | `internal/observability`：slog JSON、中间件注入 request_id | M0-T1 | 请求日志含 request_id |
| M0-T4 | store 抽象 + 迁移框架 | `internal/store`：`database/sql` 接口、SQLite/Postgres 双方言迁移目录 | M0-T1 | 迁移上下行单测（SQLite） |
| M0-T5 | Caddy 模块骨架 | `caddy/module.go`：`http.handlers.s3static` 注册、Provision/Validate/Caddyfile 解析、健康检查占位 | M0-T1 | xcaddy build 通过，默认 `:80` 返回占位 200；端口由 Caddyfile site address 决定 |
| M0-T6 | CI | lint + test + 双构建（control、xcaddy proxy） | M0-T1~T5 | CI 绿 |

## M1 领域与存储

| ID | 任务 | 产出/涉及 | 依赖 | 验收 |
|---|---|---|---|---|
| M1-T1 | 路径/Host 规范化模块 | `internal/httpx`：`NormalizeKey`、`NormalizeHost`，拒绝 `..`、`%2F`、反斜杠、NUL、超长 | M0-T1 | 单测 + fuzz（FR-13） |
| M1-T2 | 加解密 | `internal/crypto`：AES-256-GCM，master key 从 env 加载 | M0-T2 | roundtrip 单测；日志无明文 |
| M1-T3 | alias 领域 + CRUD + test | `internal/alias`、API 17.1；bucket_allowlist 校验；secret 不返回 | M0-T4、M1-T2 | FR-01 验收全过 |
| M1-T4 | objectstore 抽象与 minio 实现 | `internal/objectstore`：Stat/Get(Range/条件)/Put/List/Remove，path-style，按 alias 建 client 池 | M0-T4 | MinIO 集成测试通过（FR-14 子集） |
| M1-T5 | project/binding/version CRUD | `internal/project` 等、API 17.2/17.3；冲突校验含 `RESERVED_PREFIX_CONFLICT`、`BINDING_CONFLICT`；`response_headers` 禁止列表校验 | M0-T4、M1-T1 | FR-02/FR-05 写入校验单测全过 |
| M1-T6 | 指针与版本状态机 | `internal/version`：版本三态、ready 且未删除才可被指向、CAS 更新、protect、回滚 API | M1-T5 | FR-03 并发单测通过 |

## M2 发布与 manifest（核心难点，建议单人连续完成）

| ID | 任务 | 产出/涉及 | 依赖 | 验收 |
|---|---|---|---|---|
| M2-T1 | 包校验与解包 | `internal/deploy`：`ArchiveReader` 接口 + zip 实现（tar 二期）；zip slip、符号链接、大小写冲突、`_system` 拒绝、配额 | M1-T1 | S9；恶意包测试用例全拒 |
| M2-T2 | manifest 生成器 | `internal/manifest`：spool 收集 → 分片（阈值 2 万/2MiB）→ gzip → root 最后上传；生成 sha256；应用 `content_types` 覆盖 | M1-T4 | 10 万文件发布分片正确（S8 前半） |
| M2-T3 | 发布流水线 模式 A | `internal/deploy`：deployment 与 version 状态机、流式落盘（`max_package_bytes`）、worker 池上传、校验比对、事务切指针、触发 snapshot、幂等（空 key 不参与、TTL 24h）、cancel | M1-T6、M2-T1、M2-T2 | FR-04 验收全过；中断后指针不动 |
| M2-T4 | 包归档 | `package_retention=true` 时写 `_system/packages/` | M2-T3 | 归档存在/缺失行为正确 |
| M2-T5 | GC | `internal/gc`：orphan 清理、`keep_versions`/`keep_days`、protected 版本跳过、versions 软删除、dry-run、幂等 | M2-T3 | FR-12 验收 |
| M2-T6 | pageforge.json 解析 | 发布期读取包根配置；只允许 `index_doc`/`error_doc`/`content_types`；优先级 project > 包配置 > 默认；文件排除出 manifest；非法配置返回 `INVALID_PAGE_CONFIG` | M2-T1、M2-T2 | FR-07 配置相关验收；策略字段被忽略并告警 |

## M3 数据面请求链路（可与 M2 部分并行：M3-T1 依赖控制面 snapshot 生成）

| ID | 任务 | 产出/涉及 | 依赖 | 验收 |
|---|---|---|---|---|
| M3-T1 | snapshot 生成与轮询 | 控制面 `internal/snapshot` 生成器 + `/internal/v1/snapshot`；`ProjectView`/`BindingView`/`AuthPolicyView` 类型定义；数据面 poller：2s、ETag、generation 单调、atomic.Pointer 原子替换、失败保留旧值 | M1-T3、M1-T5 | 变更后 ≤10s 生效；旧 snapshot 兜底（FR-10） |
| M3-T2 | 内部凭证接口 | `/internal/v1/alias-credentials` 整目录下发（随快照轮询、数据面落盘镜像），`/internal/v1/aliases/{id}/credentials` 保留兼容，token 鉴权，按 credential_version 失效 | M1-T3、M3-T1 | 凭证不进 snapshot/日志；逐请求不再回源 control |
| M3-T3 | 路由器 | `internal/router`：host exact/wildcard/`*` + path 段 trie + 根节点 fallback（`/` binding）；`enabled=false` 的 binding 排除 | M3-T1 | FR-05 全部验收用例（含托底、边界、冲突） |
| M3-T4 | manifest reader | `internal/manifest` Reader：root/shard 统一加权 LRU（按解压字节、预算可配）、祖先路径选择 shard、singleflight、负缓存、可选 TTL、`Stats` | M3-T1、M2-T2 | 10 万文件仅加载命中 shard（S8 后半）；内存不超 `manifest_cache_max_bytes` |
| M3-T5 | 请求生命周期 | `internal/handler`：保留前缀拦截（含 remainder 边界二次拦截）、exactLookup（根路径回退 index_doc）、try_files、SPA 判定、301、project disabled 503、404/error_doc、错误码 | M3-T3、M3-T4 | S1~S5；FR-06/FR-16 单测全过 |
| M3-T6 | 本地对象缓存 | `internal/cache`：目录镜像 `alias/bucket/project/version/object_key`、temp+rename、空闲 TTL、LRU 低水位淘汰、singleflight、负缓存 30s、`X-Cache` | M3-T5 | FR-09 验收（二次访问零回源、丢文件自愈、满容量跳过、并发合并） |
| M3-T7 | HTTP 语义 | Range 206/416、If-None-Match/If-Modified-Since 304、HEAD、405、ETag=原始对象 sha256、Cache-Control 策略（`cache.*`）、预压缩 `.br/.gz` 协商 | M3-T5、M3-T6 | 第 16 节行为表逐条通过 |
| M3-T8 | 保留前缀 handler | `/__pageforge/healthz`、`/readyz`、内部 API 按前缀剥离转发（含 auth-policies）；`applyResponseHeaders` 空实现 hook 就位 | M3-T5 | FR-16 验收；hook 存在性测试 |

## M4 鉴权与多节点

| ID | 任务 | 产出/涉及 | 依赖 | 验收 |
|---|---|---|---|---|
| M4-T1 | auth policy CRUD + Basic | 管理 API `/auth-policies` 及 `users` 子资源、bcrypt(cost 10)、策略名称作为 realm；内部接口 `GET /internal/v1/auth-policies` 下发用户哈希；公开/401/503 语义；控制台鉴权页与绑定选择 | M3-T5 | FR-08 验收 |
| M4-T2 | Postgres 适配 | store 方言、迁移、连接池；配置切换 | M0-T4 | 同一测试集在两种 DB 通过 |
| M4-T3 | 多节点联调 | 2 个数据面 + 1 控制面 compose；切版本双节点生效；控制面停机继续服务 | M3-T1、M4-T2 | S7、S10 |

## M5 生产化

| ID | 任务 | 产出/涉及 | 依赖 | 验收 |
|---|---|---|---|---|
| M5-T1 | 指标 | Prometheus：第 20.1 节全部指标 | M3-T7 | 指标可抓取且标签正确 |
| M5-T2 | 访问日志 | 第 20.2 节字段；脱敏 | M3-T7 | 日志含 binding/project/version/cache |
| M5-T3 | 审计 | 在 M1 统一写操作入口时接入，全部写操作落 audit_logs | M1-T5 | FR-11 验收 |
| M5-T4 | 部署件 | Dockerfile（control、xcaddy proxy）、docker-compose（含 MinIO） | M4-T3 | 一键起栈冒烟通过 |
| M5-T5 | E2E 自动化 | 需求第 7 节 S1~S10 全量脚本 | M4-T3 | 全绿 |
| M5-T6 | 压测与调优 | 缓存命中 5k QPS、冷回源限流、大文件 Range | M5-T4 | 达到第 6 节目标或出瓶颈报告 |
| M5-T7 | 二期预留自检 | 确认 `ObjectStore.PresignPut/Copy`、`ArchiveReader`、`AuthPolicyView`、`applyResponseHeaders`、`deployments.mode` 枚举、`auth` 模式枚举均已预留（FR-22/FR-23） | M4-T1 | 架构 27.7~27.9 检查项逐条通过 |
| M5-T8 | S3 兼容矩阵 | 对 MinIO、Ceph RGW、Cloudflare R2、阿里 OSS、腾讯 COS 各跑一遍核心 E2E（发布/回源/Range/条件请求） | M5-T5 | FR-14 验收；不兼容项写入文档 |

## M6 二期扩展（预留能力落地）

| ID | 需求 | 任务 | 产出/涉及 | 状态 | 验收 |
|---|---|---|---|---|---|
| M6-T1 | FR-17 | Web 管理界面（与控制面同进程） | `internal/webui`：go:embed 静态资源，`/` 由控制面进程直接提供；同源访问 admin API，无 CORS；覆盖别名/项目/绑定/发布/版本/日志指标 | 完成 | 单进程启动后浏览器可完成连接、建项目、发布、查看日志指标 |
| M6-T2 | FR-18 | 分享链接 | 迁移 `0004_share_links`；`internal/share`（token、bcrypt 密码、过期、max_views、独立 cache）；API `/api/v1/share-links` CRUD；`/__pageforge/s/{token}/*`；内部 `/internal/v1/share-links` 目录下发 + `/views` 计数回写 + `/authorize` 兼容回退 | 完成 | 密码/过期/停用/超出次数分别返回 401/410/403/403；页面请求计数、静态资源不计数；分享缓存策略独立生效；control 停机时仍可离线鉴权 |
| M6-T3 | FR-19 | 版本直链 | `/__pageforge/v/{project_id}/{version_id}/*` 独立路由；默认匿名，与 binding 鉴权无关；版本不存在/未 ready 返回 `VERSION_NOT_AVAILABLE` | 完成 | 旧版本直链不受指针切换影响；项目响应头规则在直链响应生效 |
| M6-T4 | FR-20 | 原包下载 | `GET/HEAD /api/v1/projects/{id}/versions/{vid}/package`；仅 ready 且保留原包的项目可下载；`Content-Type: application/zip`、`Content-Disposition`、缺失返回 `PACKAGE_NOT_AVAILABLE` | 完成 | 下载内容与发布包一致；HEAD 无 body；未保留原包返回 404 |
| M6-T5 | FR-21 | 响应头规则 | `model.HeaderRule.Replace *bool`；项目默认 + binding 覆盖合并；禁止列表跳过；`$project_id/$version_id/$binding_id/$request_id` 变量；301/404/SPA 响应均生效 | 完成 | 单测覆盖默认/覆盖/追加/禁止头/301/404；E2E 验证 200 与 404 均携带规则头 |
| M6-T6 | FR-22 | 发布模式 B（直传 / 服务端拷贝） | —— | **不做** | 仅保留 `deployments.mode` 枚举与 `ObjectStore.PresignPut/Copy` 接口预留 |
| M6-T7 | FR-23 | 鉴权扩展模式（JWT / SSO） | —— | **不做** | 仅保留 `auth` 模式枚举与 `AuthPolicyView` 结构预留 |

## 并行建议

- M1-T4（objectstore）与 M1-T5（CRUD）可并行。
- M3-T3（router）与 M2-T2（manifest 生成）可并行，二者只通过接口契约耦合。
- M4-T2（Postgres）可与 M5-T1/T2 并行。
- M2 阶段（发布+manifest）是最关键路径，优先排期。

## 风险与检查点

1. **M2-T2 分片正确性**：上线前必须用 1k/20k/100k 三档文件数验证；默认上限 20 万，验证 100 万需调大 `publish.max_files`。
2. **M3-T1 一致性窗口**：确认 10s 内新旧混合对业务可接受（需求已确认）。
3. **M3-T5 SPA 误判**：重点测试“浏览器直接访问 .js URL”返回 404 的场景；同时覆盖 try_files 关闭时的项目根回退与保留段边界。
4. **S3 兼容矩阵**：已拆为 M5-T8，必须在 M5-T5 全量 E2E 通过后执行，覆盖 MinIO/Ceph/R2/OSS/COS。

