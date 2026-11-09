# PageForge 需求文档

| 项目 | 内容 |
|---|---|
| 文档版本 | v1.2 |
| 状态 | 定稿，作为编码输入 |
| 目标读者 | 产品、后端、前端、测试、AI 编码代理 |
| 关联文档 | `docs/architecture.md` |

## 1. 背景与目标

需要一个 HTTP Proxy 软件，把 S3 兼容对象存储中的静态资源、HTML、前端 SPA 以 URL 形式对外提供。对象存储保持私有，代理负责鉴权、路由、版本解析、缓存和 HTTP 语义。

目标：

1. 用一套代理同时托管多个项目，支持 N 个 S3 alias、N 个 project、N 个版本、N 个 URL 绑定。
2. 发布以“一个版本 = 一个包”为单位，版本不可变，默认指向最近一次成功发布的版本。
3. 支持类似 nginx 的 `try_files`，并正确支持目录索引、尾斜杠、SPA fallback。
4. 支持多节点部署，切版本后 10 秒内生效。
5. 单节点即可运行，后续可平滑扩展为控制面 + 多数据面。

## 2. 术语

| 术语 | 定义 |
|---|---|
| alias | 一个 S3 兼容存储的连接配置，包含 endpoint、region、凭证、path-style 等 |
| project | 一个可发布的静态站点，绑定到一个 alias + bucket + 对象前缀 |
| version | 一次成功发布产物的不可变快照，包含若干文件和一份 manifest |
| binding | URL 路由绑定，描述 `host + path_prefix -> project (+ 可选固定版本)` |
| manifest | 服务端生成的文件清单，分片存储，用于路由和 try_files 判定 |
| shard | manifest 的一个分片文件 |
| snapshot | 控制面下发给数据面的路由/版本/策略快照 |
| deployment | 一次发布任务，状态为 `created`/`uploading`/`validating`/`preparing`/`ready`/`failed`/`canceled` |
| pointer | project 的 `current_version_id`，指向默认版本 |
| latest | 最近一次成功发布且状态为 ready 的版本 |

## 3. 角色

| 角色 | 能力 |
|---|---|
| 管理员 | 管理 alias、project、binding、版本、查看审计 |
| 发布者 / CI | 创建 deployment、上传包、完成发布、查询状态 |
| 终端用户 | 通过绑定 URL 访问静态资源 |

## 4. 范围

### 4.1 In Scope

- alias、project、version、binding 的增删改查。
- zip / tar / tgz 包发布、校验、解包（魔数自动检测）、对象上传、manifest 生成（presigned 直传模式为二期，见 FR-22）。
- 版本指针切换、回滚、固定版本绑定。
- HTTP 静态资源代理、try_files、SPA fallback、Range、条件请求。
- binding 级鉴权（默认无鉴权）。
- 单机 SQLite 与多节点 Postgres + snapshot 两种部署形态。
- 本地磁盘缓存。
- 管理 API、内部快照 API、健康检查、基础指标。
- Web 管理界面（FR-17）。
- 分享链接（FR-18）。
- 版本直链（FR-19）。
- 原包下载（FR-20）。
- 响应头 add_header（FR-21）。

### 4.2 Out of Scope

- 多租户 SaaS、计费、租户级隔离。
- 自动 HTTPS / ACME。默认只监听 `:80`，TLS 由外部 LB/CDN 终止。
- 通用 S3 API 网关（不暴露 PUT/DELETE/List 给终端用户）。
- 动态计算、函数、边缘脚本。
- 图片处理、视频转码。
- 跨地域复制、全局一致性数据库。

## 5. 功能需求

### FR-01 S3 Alias 管理

- 支持创建、查询、修改、删除 alias。
- alias 字段：
  - `name`，唯一；
  - `endpoint`，如 `https://minio.example.com`；
  - `region`；
  - `access_key`、`secret_key`；
  - `path_style`，默认 true；
  - `bucket_allowlist`，可选，限制该 alias 可访问的 bucket；
  - `tls_skip_verify`，默认 false，仅用于内网自签证书。
- secret 必须加密存储，任何 API 响应不得返回明文。
- 删除 alias 前必须确认没有 project 引用；已软删除（`deleted_at` 非空）的 project 不计入引用。
- 提供“测试连接”接口，验证凭证和 bucket 访问。

验收：

- 创建 alias 后可以列出一个指定 bucket 的对象。
- 修改凭证后，新版本凭证在 10 秒内被数据面使用。
- 响应中只出现 `secret_key_set: true/false`，不出现 secret 明文。

### FR-02 Project 管理

- 支持创建、查询、修改、删除 project。
- project 必须引用一个 alias、一个 bucket；对象前缀固定为 `projects/{project_id}/{version_id}/`，由服务端生成且不可配置。
- project 字段：
  - `id`，不可变，格式 `prj_<ULID>`；
  - `name`：全局唯一（已软删除的 project 不占用），1-128 字符，仅允许字母、数字及 `-`、`_`、`.`，且首字符必须是字母或数字；重名创建或改名返回 `PROJECT_NAME_TAKEN`；
  - `description`；
  - `alias_id`、`bucket`；
  - `current_version_id`；
  - `index_doc`，默认 `index.html`；
  - `error_doc`，可选，如 `404.html`；
  - `try_files_enabled`，默认 `false`；关闭时只做精确文件匹配（仅保留“项目根 → `index_doc`”一条回退，并在绑定挂载根缺少尾斜杠时先 301），不存在就 404；开启时才执行目录索引、尾斜杠规范化；
  - `spa` 配置（结构见 architecture 6.7）；**SPA fallback 是 try_files 流程中的一步，因此 `spa.enabled=true` 必须同时 `try_files_enabled=true`**；不满足该组合时创建/修改返回 `INVALID_PROJECT`，避免 SPA 配置静默失效；
  - `cache` 配置（结构见 architecture 6.7）；
  - `package_retention`，默认 `true`，是否保留原始 zip 归档（tar 为二期）；
  - `keep_versions`、`keep_days`，可选的项目级版本保留策略；未设置（`null`）时继承全局 `gc.keep_versions` / `gc.keep_days`，设置后以项目值为准（0 表示不按该维度保留，两者都为 0 表示不删除任何版本）；非负整数，负值返回 `INVALID_PROJECT`；
  - `status`，`active` / `disabled`。
- project 名称可以修改，但对象路径永远使用 `project_id`，改名不迁移对象。
- 删除 project 为软删除：删除事务内同时软删除（置 `deleted_at`）其全部 binding，解除 `host + path_prefix` 的唯一占用；对象由 GC 异步清理。

验收：

- 修改 project 名称后，已有 binding 和对象路径不变。
- 禁用 project 后，其所有 binding 返回 503 `PROJECT_DISABLED`（统一且固定）。
- 创建两个同名 project，第二个返回 `PROJECT_NAME_TAKEN`；含空格/中文等非法字符的 name 返回 `INVALID_PROJECT`。
- 软删除 project 后，其 name 可以被新项目复用。

### FR-03 版本与指针

- 版本不可变，ID 格式 `ver_<ULID>`，可附带用户可读 `label`（如 git sha），同一 project 内 label 唯一；未提供 label 时置 NULL，不参与唯一约束。
- 版本状态只有 `uploading`、`ready`、`failed` 三种。`active` 为派生状态（`version_id == current_version_id`），不落库；`draft` 不引入。发布任务（deployment）状态独立定义，见 FR-04。
- `latest` 定义为最近一次成功发布且 `ready` 的版本，按 `published_at` 排序。
- `current_version_id` 只在发布事务成功提交后更新。
- 发布失败、校验失败、manifest 上传失败，指针不得移动。
- 同一 project 的发布必须串行化，或使用 CAS 防止并发覆盖。
- 支持回滚：把指针指向旧版本，不复制、不重传对象；目标版本必须为 `ready` 且未删除，否则返回 `VERSION_NOT_AVAILABLE`。
- 支持将 binding 固定到某个版本，`pinned_version_id` 优先于 project 指针；固定目标同样必须是 `ready` 且未删除。
- 支持版本保留策略：按数量（`keep_versions`）或天数（`keep_days`，0 表示不按该维度保留）保留。全局默认来自 `gc.keep_versions` / `gc.keep_days`；project 可覆盖（`keep_versions` / `keep_days` 字段，`null` 表示继承全局），**project 优先级高于全局**；被 binding 固定或被标记保护的版本不得删除。
- 提供 `POST /api/v1/projects/{project_id}/versions/{version_id}/protect`（body `{"protected": true|false}`）用于设置/取消保护标记，受保护版本不进入 GC 删除集合。
- 支持手动删除单个版本：`DELETE /api/v1/projects/{project_id}/versions/{version_id}`，软删除后版本不再可见，其对象由 GC 异步清理（超过 `gc.orphan_grace` 后移除）。限制：当前指针指向的版本、被 binding 固定的版本返回 `VERSION_IN_USE`；受保护版本返回 `VERSION_PROTECTED`（需先取消保护）；`uploading` 状态返回 `VERSION_NOT_DELETABLE`（需先失败或完成）。

验收：

- 发布 v2 成功后，默认 URL 在 10 秒内返回 v2 内容。
- 回滚到 v1 后，默认 URL 在 10 秒内返回 v1 内容，且不需要重新上传对象。
- 并发发布 v2 和 v3，最终指针只指向其中一个成功版本，不出现中间态。

### FR-04 发布

- 本期实现模式 A：包上传到控制面，服务端解包并上传对象。支持三种归档格式：`zip`、`tar`（非压缩）、`tgz`（gzip 压缩 tar），通过魔数自动检测，也可显式指定 `mode`。模式 B（presigned 直传）列为二期，见 FR-22。
- 发布任务（deployment）状态：`created` → `uploading` → `validating` → `preparing` → `ready` / `failed` / `canceled`；`cancel` 仅对 `created`、`uploading`、`validating` 有效，取消后残留对象由 GC 清理。
- 版本（version）状态按 FR-03 的三态流转，与 deployment 状态分离。
- 发布接口：
  1. `POST /api/v1/deployments` 创建任务，目标 project 用 `project_id` 或全局唯一的 `project_name` 二选一指定（同时提供返回 `INVALID_DEPLOYMENT`，`project_name` 查无对应 project 返回 `PROJECT_NOT_FOUND`）；`mode` 取值 `auto`（默认，魔数检测）、`zip`、`tar`、`tgz`；可选 `auto_publish`（默认 `true`），为 `false` 时包发布完成后版本只进入 `ready`，不切换 `current_version_id`，`published_at` 留空，后续通过 `POST /api/v1/projects/{id}/pointer` 手动上线并补写 `published_at`；
  2. 上传发布包（zip / tar / tgz）；
  3. `POST /api/v1/deployments/{id}/complete` 完成发布；
  4. `GET /api/v1/deployments/{id}` 查询状态。
- 服务端校验：
  - 魔数检测格式（`PK\x03\x04` → zip，`\x1f\x8b` → tgz，其余 → tar）；显式指定 mode 时校验一致性，不匹配返回 `INVALID_PACKAGE`；
  - 拒绝 zip slip / tar slip、绝对路径、`..`、反斜杠、NUL；
  - 拒绝符号链接、设备文件；
  - 拒绝大小写冲突的同名文件；
  - 拒绝写入保留前缀 `_system/`；
  - 校验最大文件数、单文件大小、总字节数；
  - 校验路径长度和非法字符；
  - 条目名统一转为 UTF-8：非 UTF-8 的条目名（Windows 打包含中文文件名常见的 GBK 编码）按 GB18030 解码，保证 manifest key 可被 UTF-8 URL 请求命中；无法解码时拒绝包并返回 `INVALID_PACKAGE`。
- 服务端负责：
  - 计算 sha256、检测 Content-Type、设置缓存头；
  - 生成分片 manifest；
  - 上传 manifest 并校验；
  - 写入数据库并切换指针（`auto_publish=false` 时不切换指针）；
  - 发布新 snapshot。
- 原始包可选保留到 `_system/packages/{project_id}/{version_id}.zip|.tar|.tgz`（按实际格式），仅用于审计/重导入，不参与服务。
- 发布必须幂等：同一 project 下 `idempotency_key` 相同且在 `publish.idempotency_ttl`（默认 24h）内的请求返回同一 deployment；未提供 key 的请求不参与幂等匹配。
- 包体上限由 `publish.max_package_bytes`（默认 4GiB）约束，必须流式写入临时目录，不得整体读入内存。

验收：

- 上传含 `../evil.js` 的包被拒绝，且不产生任何对象。
- 上传 10 万文件的项目，manifest 被拆成多个 shard，发布成功。
- 发布中断后，指针不变，残留对象可被 GC 清理。
- 以 `auto_publish=false` 发布成功后，版本为 `ready`、指针不变、`published_at` 为空；调用指针接口上线后 `published_at` 被补写，默认 URL 在 10 秒内返回新版本内容。

### FR-05 URL Binding 与虚拟主机

- binding 是独立实体，格式 `bnd_<ULID>`。
- 字段：
  - `host`，可选，空或 `*` 表示默认；支持精确域名和 `*.example.com`；
  - `path_prefix`，如 `/url1`；
  - `project_id`；
  - `pinned_version_id`，可选；
  - `auth_policy_id`，可选；
  - `enabled`。
- 匹配规则：
  - host 规范化：小写、去端口、去尾部点；
  - host 优先级：精确 > 通配符（最长后缀）> 默认 `*`；
  - 同一 host 内按路径段最长前缀匹配；
  - `/url1` 不匹配 `/url10`；
  - 同一 host + 同一 path_prefix 的重复 binding 在写入时拒绝（软删除的 binding 不占用唯一约束）；
  - 前缀匹配必须落在 `/` 边界；
  - `enabled=false` 的 binding 视同不存在：路由时跳过，按无匹配返回 404，不影响其他 binding。
- 支持同一监听端口配置不同域名，类似 nginx 的 server。
- **域名托底项目（fallback）**：等价于 nginx 的 `location / {}`。通过创建一条 `path_prefix = "/"` 的 binding 表达，该 binding 指向的项目即为该域名的托底项目。同 host 下没有更长前缀命中时，请求路由到托底项目；托底 binding 与其他 binding 一样支持鉴权、固定版本、启用开关。同一 host 只能有一条 `/` binding（由唯一约束保证）。
- 全局托底：`host = "*"` 且 `path_prefix = "/"` 的 binding，用于所有未配置域名的请求。
- 未配置托底时：该 host 下无匹配返回 404；host 完全不匹配且无全局托底时返回 404。
- 路径语义：
  - `remaining = path - path_prefix`；
  - `path_prefix = "/url1"` 时，`/url1` 和 `/url1/` 都映射到项目根；
  - `path_prefix = "/"` 时，`remaining` 为完整路径（不含前导 `/`）；
  - 保留 query string。

验收：

- `/url1` → project1，`/url1/r1` → project2，请求 `/url1/r1/app.js` 命中 project2。
- 请求 `/url10/x` 不命中 `/url1`。
- 两个不同域名在同一监听端口返回不同项目。
- host `example.com` 配置 `/url1` 和 `/`（托底 project3），请求 `/url1/app.js` 命中 project1，请求 `/other/xyz` 命中 project3。
- host `example.com` 只配置 `/url1` 无托底，请求 `/other/xyz` 返回 404。
- 将 binding 置 `enabled=false` 后，该路径按无匹配返回 404，其他 binding 不受影响。

### FR-06 请求解析与 try_files

try_files **默认关闭**，由 project 字段 `try_files_enabled` 控制。

- 关闭（exact 模式）时：
  - `rel = ""`（命中项目根，如 `/url1` 或 `/url1/`）→ 返回 `index_doc`；
  - 其他请求把 `rel` 当作完整 key 精确查找，命中返回 200，未命中直接 404；
  - 不执行子目录索引、子目录尾斜杠 301、SPA fallback。
- 开启时才执行完整的 try_files 流程（见下）。
- 绑定挂载根尾斜杠规范化（与 `try_files_enabled` 无关）：binding 的 `path_prefix` 不为 `/` 时，请求 `{path_prefix}` 且不带尾斜杠，返回 301 到 `{path_prefix}/`（保留 query），随后由带尾斜杠的请求返回 `index_doc`。浏览器以当前 URL 的最后一个 `/` 作为相对路径基准，缺少尾斜杠会让 HTML 中的 `./a.css`、`assets/a.js` 等相对引用解析到上一级目录而 404。
- `spa.enabled=true` 依赖 `try_files_enabled=true`：SPA fallback 只在 try_files 流程内执行，两者组合不成立时创建/修改返回 `INVALID_PROJECT`；历史数据若处于该组合，数据面仍按 try_files 关闭处理，不执行 SPA fallback。

请求处理顺序必须固定：

1. 拦截保留前缀 `/__pageforge/*`（见 FR-16）；
2. host + path 匹配 binding（`enabled=false` 的 binding 跳过）；
3. project 为 `disabled` 时返回 503 `PROJECT_DISABLED`；
4. binding 鉴权；
5. 解析版本：`pinned_version_id` 优先，否则 project `current_version_id`；
6. 加载 manifest；
7. 若 `try_files_enabled=true` 则执行 try_files；否则执行 exact 匹配（根路径回退 `index_doc`）；
8. 命中后从本地缓存或 S3 读取并返回；
9. 未命中按规则返回 404 或 fallback。

try_files 顺序：

1. `rel` 为空或以 `/` 结尾：查找 `rel + index_doc`；
2. `rel` 精确命中文件：返回 200；
3. manifest 中存在目录 `rel`，但请求没有尾斜杠：301 到 `rel + "/"`；
4. 存在 `rel + "/" + index_doc`：返回 200；
5. 满足 SPA 条件：返回 project 的 `index_doc`，状态 200；
6. 否则 404，若配置 `error_doc` 则返回该文件内容，状态仍为 404。

规则：

- 只允许 `GET` 和 `HEAD`；其他方法返回 405。
- 路径必须规范化：拒绝 `..`、`.` 段、重复斜杠、反斜杠、NUL、编码后的 `%2F`、`%5C`。
- 对象 key 大小写敏感。
- query 不参与 key 计算，但重写/重定向时保留。
- HEAD 与 GET 使用同一解析逻辑，HEAD 不返回 body。
- 目录判断只读 manifest，不调用 S3 ListObjects。

验收：

- `/docs/` 命中 `docs/index.html`。
- `/docs` 且存在 `docs/index.html` 时 301 到 `/docs/`。
- `/missing.js` 返回 404，不返回 index.html。
- `/route/abc` 在 SPA 模式下返回 index.html，状态 200。
- `try_files_enabled=false` 时，请求 `/docs`（存在 `docs/index.html`）直接返回 404，不做子目录索引和重定向。
- `try_files_enabled=false` 时，请求绑定挂载根 `/url1` 先 301 到 `/url1/`，`/url1/` 返回 `index_doc`；根绑定 `/` 直接返回 `index_doc`。
- `spa.enabled=true` 且 `try_files_enabled=false` 时，创建或修改 project 返回 `INVALID_PROJECT`。

### FR-15 监听端口可配置

- 数据面（Caddy 自定义构建）监听端口由 Caddyfile 的 site address 决定（如 `:80`、`:8080`），不固定 `:80`；`s3static` 块只承载 handler 配置，不支持设置端口。
- 控制面管理 API 监听地址和端口可通过 YAML 或环境变量 `PAGEFORGE_CONTROL_LISTEN` 修改。
- 控制面内部 API（snapshot/凭证）监听地址与端口独立配置，只对数据面开放。
- 默认值：
  - 数据面 `:80`；
  - 控制面管理 API `127.0.0.1:8080`；
  - 控制面内部 API `127.0.0.1:8081`。

验收：

- 修改 Caddyfile 的 site address 后重启，在新端口可正常访问绑定的项目。
- 控制面 `listen` 字段改变后，管理 API 和内部 API 端口同步生效。

### FR-16 软件保留 URL 命名空间

- 保留前缀 `/__pageforge` 及其子路径，用于本软件的内部和管理用途，禁止被配置为 project URL binding 的 `path_prefix` 或其子前缀。
- 保留前缀在数据面请求生命周期中 **最优先拦截**，在任何 binding 匹配之前处理。
- 本期占用路径：
  - `/__pageforge/healthz`、`/__pageforge/readyz` —— 健康检查，本地处理，不依赖控制面；
  - `/__pageforge/internal/v1/snapshot`、`/__pageforge/internal/v1/aliases/{id}/credentials`、`/__pageforge/internal/v1/auth-policies/{id}`、`/__pageforge/internal/v1/reload` —— 内部 API，转发到控制面同路径（转发时只去掉 `/__pageforge` 前缀，其余与控制面 `/internal/v1/*` 完全一致）；
  - 内部转发失败时按 502/504 返回，健康检查不受影响；
  - `/__pageforge/s/{share_token}` —— 分享链接入口（FR-18）；
  - `/__pageforge/v/{project_id}/{version_id}/*` —— 任意版本直链（FR-19）；
  - 以上路径在绑定冲突校验时同样禁止，且路由优先级高于普通 binding。
- 对包含保留前缀的 binding `path_prefix`，创建/修改 API 必须返回 `RESERVED_PREFIX_CONFLICT` 错误；按路径段边界判断，`/__pageforge2` 不算冲突。
- 保留前缀名称可通过系统配置修改（如部署环境中已被其他系统占用），但同一软件实例内全局唯一。

验收：

- 尝试创建 `path_prefix="/__pageforge/foo"` 的 binding 被拒绝。
- 请求 `/__pageforge/healthz` 返回 200，不匹配任何 binding。
- 请求 `/url1/__pageforge/healthz` 中 `__pageforge/healthz` 会先被 binding `/url1` 吸收为 remainder，但 handler 在 remainder 层面按路径段边界再次拦截保留段，返回 404 `RESERVED_PATH`，防止路径穿越；`/__pageforge2` 这类非路径段边界不视为保留前缀。

### FR-17 Web 管理界面（已实现）

- 提供 Web 前端，覆盖 alias、project、version、binding、auth policy、deployment 的增删改查。
- 前端由控制面同进程提供（内嵌静态资源），与 VS-17 控制面 API 同源，复用管理 Bearer Token。
- 支持版本发布、回滚、指针切换、binding 固定版本。
- 支持在「访问鉴权」页维护 Basic Auth 策略与用户，并在「访问绑定」中为 URL 选择鉴权策略（默认公开访问）。
- 支持查看控制面访问日志与请求指标快照。

架构预留：控制面 API 全部 JSON 且无状态；前端静态资源随控制面二进制分发，无需 CORS。

### FR-18 分享链接（已实现）

- 为任意 project + 可选版本创建分享链接，独立于 URL binding。
- 分享链接路由：`/__pageforge/s/{share_token}/*`。
- 分享链接属性：
  - `project_id`；
  - `version_id`，可选，空则跟随 project 指针；
  - `expires_at`，长期或指定过期；
  - `password`，可选，访问时需输入；编辑链接时传空字符串表示**清除密码保护**，不传该字段表示不修改；
  - `max_views`，可选；
  - 分享链接自己的缓存策略。
- 分享链接独立鉴权逻辑：密码、过期、访问计数。
- 密码输入方式：浏览器导航访问未携带密码时返回只含密码输入框的 HTML 页面（不弹带用户名的 Basic 认证框）；校验通过后设置 `HttpOnly` 会话 Cookie（`Path` 限定在该分享链接前缀下），后续页面与静态资源自动复用。API 客户端可用 `X-Share-Password` 请求头或 Basic 认证（密码即密码部分，用户名忽略）。非 HTML 请求仍返回 JSON 401。
- 密码表单的 POST 不消耗 `max_views`，重定向后的页面 GET 计一次访问。
- 分享链接到期后，token 进入 `expired` 状态但可重新启用。
- 预留数据库表 `share_links`；数据面 handler 在保留前缀分支中独立路由到 share handler。

架构预留：share handler 需要能读取 manifest、执行请求解析（与 binding 复用 try_files 流程），但鉴权走独立逻辑；share link 的请求不生成 binding 记录，也不进入 binding 路由表。

### FR-19 任意版本直链（已实现）

- 任意 project 的任意已发布版本，可通过 `/__pageforge/v/{project_id}/{version_id}/*` 直接访问，不依赖 binding。
- 路径语义：`/__pageforge/v/prj_01/ver_02/docs/intro.html` 直接返回该版本的该文件。
- 鉴权：默认走 project 级（可配置允许匿名 / 需要 admin），与 binding 鉴权无关。
- version_id 不存在、未 ready 或已删除时返回 404 `VERSION_NOT_AVAILABLE`。
- 预留数据面在保留前缀分支中独立路由到 version-direct handler。

架构预留：version-direct handler 需要 alias/bucket/project/version 都能从路径中解析并加载 manifest；请求处理流程可复用 try_files 但不走 binding 匹配。

### FR-20 原包下载（已实现）

- 管理后台和 API 可下载某个版本的原始 zip/tar 包。
- 预留 API：`GET /api/v1/projects/{project_id}/versions/{version_id}/package`。
- 只有 `package_retention=true` 且归档存在的版本可下载；GC 删除归档后返回 404。
- 下载走控制面（不经过数据面代理），使用 alias 凭证直接从 `_system/packages/` 读取。

架构预留：对象存储接口已支持任意 bucket + key 的 Get，控制面加一个专用 handler 即可；数据面不参与此能力。

### FR-21 响应头（add_header）（已实现）

- 响应头规则以 **binding 级为主**（对应 nginx 的 `location` 级 `add_header`），project 级作为默认。
- 生效范围：命中该 binding 的所有响应（200、301、404、SPA fallback 等）。同一 project 挂多个 binding 时，各 binding 可有不同的头。
- 合并规则：先应用 project 级规则，再应用 binding 级规则；同名头按 `replace` 语义覆盖或追加（不采用 nginx "location 一旦定义 add_header 就完全丢弃 server 级" 的反直觉规则）。
- 规则列表：每条包含 `name`、`value`、`replace`（默认 true，覆盖同名头；false 为追加）。
- 安全约束：禁止覆盖 hop-by-hop 和长度类头部，禁止列表：`Content-Length`、`Connection`、`Transfer-Encoding`、`Trailer`、`Host`；尝试设置返回配置校验错误。其余头部（含 `Content-Type`）允许覆盖。
- 支持在 value 中使用变量（二期可选）：`$project_id`、`$version_id`、`$binding_id`、`$request_id`。
- 数据面在响应写出前统一应用规则；与代理自身设置的头部（Content-Type、Cache-Control、ETag）冲突时，自定义规则优先（禁止列表除外）。
- 配置变更走 snapshot，10 秒内生效。

架构预留：binding 和 project 配置中各新增 `response_headers` 字段并进 snapshot；数据面在响应写出前统一调用 header 应用函数（project 默认 + binding 覆盖）。本期只落字段和结构，不实现应用逻辑。

### FR-07 Manifest 自动生成

- manifest 只能由服务端生成和管理，上传者不得提交文件清单。
- manifest 不可变，随版本创建、随版本删除。
- manifest 位置在版本目录之外的保留前缀下：
  - `_system/manifests/{project_id}/{version_id}/root.json`
  - `_system/manifests/{project_id}/{version_id}/shards/*.json`
- manifest 支持分片：
  - 默认按一级路径段分片，根文件进 `_root`；
  - 单 shard 超过阈值（默认 1-2 万文件或 1-2MB）时按更深前缀继续拆分；
  - `root.json` 维护 `prefix -> shard` 映射；
  - 运行时最长前缀匹配加载 shard。
- root manifest 包含索引文档、错误页、文件总数、总字节、shard 映射。
- shard 包含 `files` 和 `dirs`，`dirs` 包含所有父目录。
- manifest 生成后计算 sha256，写入数据库，节点加载时可校验。
- 上传者可以在包根提供 `pageforge.json` 作为发布配置，只允许 `index_doc`、`error_doc`、`content_types`（扩展名 → Content-Type 覆盖）三个字段；该文件在解包时读取并被排除，不进入 manifest、不可通过 URL 访问。
- `spa`、`cache` 等策略字段不允许由发布包覆盖（安全与路由策略必须由管理员控制），`pageforge.json` 中出现时忽略并记录 warning。
- 优先级：project 显式配置 > `pageforge.json` > 系统默认；最终生效的 `index_doc`、`error_doc` 写入 manifest root，`content_types` 在发布时固化进 manifest 条目的 `ctype`。
- 运行时解析以 project 的**实时配置**为准：`error_doc` 非空、`index_doc` 显式配置（不等于默认 `index.html`）时立即生效，无需重新发布；project 未显式配置时才回退到 manifest root 中固化的值（可能来自 `pageforge.json`），再回退系统默认。因此把 `error_doc` 改为不存在的文件后，未命中的请求直接返回 JSON 404，而不是包内旧的错误页。

验收：

- 上传者未提供 manifest 时发布成功。
- 上传者伪造 manifest 不会影响服务端生成结果。
- 单版本 20 万文件（默认上限 `publish.max_files`）时，发布成功且节点启动不加载全部 shard，只按访问加载；把上限调至 100 万时行为不变。

### FR-08 鉴权

- S3 层：bucket 私有，数据面使用 alias 凭证访问。
- URL 层：binding 级鉴权，`auth_policy_id` 为空表示 `none`（公开访问，默认）。
- 支持模式：本期仅 `basic`；`none` 由“不绑定策略”表达，不创建 `none` 策略。
- Basic：
  - 策略名称同时作为 realm，`401` 返回 `WWW-Authenticate: Basic realm="<policy name>"`；
  - 密码以 bcrypt 哈希存储，只写不读，任何 API 响应都不返回哈希或明文；
  - 用户名与密码校验使用常量时间比较；校验成功结果可按短 TTL 缓存；
  - 同一策略可被多个 binding 复用；binding 未选择策略即公开访问。
- 鉴权配置变更走 snapshot，数据面轮询后生效（默认 2 秒，最迟 10 秒内）。
- auth policy 通过管理 API 维护：
  - `GET/POST /api/v1/auth-policies`；
  - `GET/PATCH/DELETE /api/v1/auth-policies/{id}`；
  - `PUT/DELETE /api/v1/auth-policies/{id}/users/{username}`，密码只写不读；
  - 查询策略详情只返回 `username` 与 `password_hash_set`；
  - 仍被 binding 引用时删除策略返回 `AUTH_POLICY_IN_USE`。
- snapshot 只包含策略的 `id`、`realm`、`mode`、`secret_version`，不含用户与哈希；用户哈希通过内部接口单独下发。任何用户变更都递增 `secret_version`，snapshot 随之刷新。
- 数据面尚未加载到密码目录、或 `secret_version` 不一致时失败关闭，返回 `503 AUTH_UNAVAILABLE`，不返回受保护内容。
- 不得把 S3 access key/secret、Basic 密码明文写入 snapshot 或日志。

验收：

- 公开 binding（`auth_policy_id` 为空）无需登录即可访问。
- 受保护 binding 未携带凭证返回 401，并带 `WWW-Authenticate: Basic realm="<policy name>"`。
- Basic 正确凭证返回 200，错误密码或未知用户返回 401。
- 删除用户后，数据面下一次轮询即拒绝旧凭证。
- 删除仍被 binding 引用的策略返回 `AUTH_POLICY_IN_USE`。
- IP 白名单与 `none/basic` 之外的模式不在本期范围。

### FR-09 对象缓存

**目标**：减少回源对象存储，提高本软件吞吐量，降低请求延时。

**缓存对象**：对象存储中属于 project 的文件（S3 对象）。只缓存对象内容，不缓存 HTTP 响应、鉴权结果或 manifest。

**缓存粒度**：判断是否命中缓存的最小单元是具体的一个个文件。同一个文件无论从哪种入口访问，都使用同一条缓存记录。

**缓存目录**：缓存目录层级与对象存储目录结构保持一致，便于人工定位与清理：

```text
{cache_dir}/{alias_id}/{bucket}/{project_id}/{version_id}/{object_dir}/{object_key}
```

缓存 key 由 `alias_id`、`bucket`、`project_id`、`version_id`、`object_key` 五部分组成，不包含 host、URL 前缀、binding 或分享 token。

**建立缓存**：

1. client 请求某个文件未命中缓存时，回源获取文件返回客户端，并自动建立缓存。
2. 已缓存文件丢失（例如被人为删除了部分已缓存文件）时，视为未命中缓存，仍然回源获取文件返回客户端，并自动重建缓存。

**跳过缓存**：

1. client 请求未命中缓存，并且缓存目录空间已到上限（`cache_max_bytes`）时，直接回源获取文件返回客户端，不建立缓存。

**缓存使用场景**：

1. 通过 URL 绑定访问；
2. 通过分享链接访问；
3. 通过 version 直链访问。

以上场景统一走同一套对象缓存逻辑与同一个缓存 key。

**缓存管理**：

1. `cache_ttl`（默认 24h）表示文件“未被访问”的时长，不是缓存建立时间。文件超过 `cache_ttl` 未被访问时，清理该缓存文件。
2. 缓存目录空间达到 `cache_max_bytes` 上限时，忽略 `cache_ttl`，按 LRU 强制淘汰缓存，把空间使用率降低到 `cache_evict_ratio`（默认 60%）。淘汰后仍有新文件放不下时，按“跳过缓存”处理。
3. 版本不可变是对象缓存长期有效的前提。若修改 alias 的 endpoint/bucket 等存储配置，应清理对应缓存目录。

**响应标识**：数据面在响应头 `X-Cache` 返回 `HIT`、`MISS`、`BYPASS` 或 `NEGATIVE`，便于排障与统计。

验收：

- 首次请求返回 `X-Cache: MISS`，第二次相同文件返回 `X-Cache: HIT`，且第二次不再回源。
- 手动删除已缓存文件后再次请求，返回 `X-Cache: MISS`，回源成功并重新生成缓存文件。
- 缓存空间已满时新文件返回 `X-Cache: BYPASS`，响应正常但缓存目录不新增文件。
- 文件超过 `cache_ttl` 未访问后被清理；期间若被访问，清理计时从最后一次访问重新开始。
- 缓存使用率达到 `cache_max_bytes` 后，按 LRU 淘汰到 60%，最近访问的文件保留。
- URL 绑定、分享链接、version 直链访问同一文件时共享同一条缓存记录。

### FR-10 多节点与快照

- 控制面维护路由表、project 指针、鉴权策略，并生成 snapshot。
- 数据节点每 2 秒轮询 snapshot，使用 generation/ETag 判断变化。
- snapshot 变化后原子替换内存路由表，最坏 10 秒内生效。
- 拉取失败保留最后一份可用 snapshot，继续服务。
- snapshot 不包含任何密钥明文。
- 数据节点启动时若无 snapshot，返回 503，直到加载成功。
- 支持手动 `POST /internal/v1/reload` 立即刷新。

验收：

- 两个节点在切版本后 10 秒内都返回新版本。
- 控制面停止后，节点继续用旧 snapshot 服务。
- snapshot generation 回退时节点拒绝加载。

### FR-11 管理与可观测性

- 管理 API 使用 Bearer token 或 mTLS。
- 所有写操作记录审计日志：操作者、动作、对象、时间、结果。
- 提供：
  - `GET /__pageforge/healthz` 存活；
  - `GET /__pageforge/readyz` 就绪（snapshot 已加载、S3 可用）；
  - Prometheus 指标；
  - JSON 结构化访问日志。
- 数据面日志分两个文件输出：客户端请求访问日志 `logs/access.log` 与 Caddy 自身错误日志 `logs/error.log`，按天轮换（本地时间 00:00），保留天数可通过 `PAGEFORGE_LOG_KEEP` 设置，日志目录可通过 `PAGEFORGE_LOG_DIR` 设置。
- 不提供根路径 `/healthz`、`/readyz`，避免与 binding 路径冲突；如需供 LB 使用，由外部反代到保留前缀路径。
- 访问日志字段至少包含：host、path、binding_id、project_id、version_id、status、bytes、cache_status、s3_latency、request_id。

验收：

- 每次请求都能从日志定位到 binding、project、version。
- 指标能区分缓存命中/未命中、S3 错误、404。
- 访问日志与错误日志分别落在独立文件，并按天轮换，超过保留天数的旧文件自动清理。

### FR-12 清理与保留

- 定期清理：
  - 失败/中断发布的 staging 对象；
  - 超过保留期的旧版本对象和 manifest；
  - 未被引用且超过宽限期的包归档；
  - 本地缓存按容量淘汰。
- 被 binding 固定、被标记 protected 的版本不得清理；保护标记通过 FR-03 的 protect API 设置。
- 保留窗口按 project 解析：优先使用 project 的 `keep_versions` / `keep_days`，未设置时继承全局 `gc.keep_versions` / `gc.keep_days`；`keep_versions` 与 `keep_days` 为「或」关系，满足任一即保留，二者都为 0 时不删除任何版本。
- 清理任务必须幂等、可重试、有审计。

### FR-13 安全

- 路径规范化和 key 构造必须集中在一个函数中，禁止各处自行拼接。
- 拒绝路径穿越、编码绕过、双重编码、超长路径。
- alias endpoint 只能由管理员配置，避免 SSRF。
- S3 secret、Basic 密码、内部 token 加密存储，不进日志、不进 snapshot。
- 管理 API 与访问面分离；内部 API 需要独立 token/mTLS。
- 支持请求体大小限制、超时、并发上限。
- 对异常请求返回统一错误结构，不泄露内部 key 或 bucket 信息。

### FR-14 S3 兼容性

必须支持并测试：

- MinIO；
- Ceph RGW；
- Cloudflare R2；
- 阿里云 OSS / 腾讯云 COS（S3 兼容模式）；
- AWS S3。

兼容点：

- SigV4；
- path-style 与 virtual-host-style；
- ListObjectsV2 分页；
- multipart upload；
- Range GET；
- 自定义 endpoint 与自签证书。

> **跨后端兼容性原则（强制）**
>
> 不同 S3 兼容实现在协议细节与 SDK 行为上存在差异，代码实现**必须只依赖 S3 协议本身的行为，不得依赖某一厂商或 SDK 的隐式行为**，否则会在换后端时出现难排查的问题。
>
> - **不依赖 SDK 的隐式转换。** 对象内容编码统一记录在**用户元数据**（`pageforge-encoding`），`Put` 时不设置 S3 `Content-Encoding` 属性，存储始终保存原始字节；读取端仅对历史遗留对象按 gzip magic（`0x1f 0x8b`）兜底判断。这样无论后端是否自动解压 gzip，行为都一致。
> - **不依赖厂商特有的响应头、错误码或分页顺序。** 错误识别以标准 S3 错误码为准（`NoSuchKey`/`NoSuchBucket`/404 等），并容忍厂商自定义错误。
> - **Range 与压缩的交互需显式处理。** 对可能被压缩的对象，Range 语义与整体读取分开实现，不得假设 SDK 对两者一致。
> - **所有差异点应有测试覆盖。** 兼容性关键路径（如 gzip 透明解压与否两种输入）须有单元/集成测试，避免回归。

## 6. 非功能需求

| 类别 | 要求 |
|---|---|
| 性能 | 缓存命中对象 P95 < 10ms（不含网络）；manifest 查找 < 1ms；冷对象额外延迟以 S3 为准 |
| 吞吐 | 单节点目标 5k QPS 小文件缓存命中，可水平加节点 |
| 可用性 | 控制面故障不影响数据面继续服务；S3 故障时缓存命中仍可服务 |
| 一致性 | 切版本 10 秒内生效；允许短暂新旧混合 |
| 扩展性 | 项目数、binding 数、单版本文件数可扩展；manifest 必须分片 |
| 安全 | 私有 bucket；密钥加密；路径安全；管理面隔离 |
| 兼容性 | 见 FR-14 |
| 可维护性 | 控制面/数据面/存储层接口清晰，可单测 |
| 可观测性 | 指标、日志、审计、健康检查 |

## 7. 关键验收场景

1. 创建 alias → 创建 project → 发布 v1 → 绑定 `/url1` → 访问 `/url1` 返回 `index.html`。
2. 发布 v2 → 10 秒内 `/url1` 返回 v2；回滚 v1 → 10 秒内恢复。
3. 绑定 `/url1` 和 `/url1/r1`，访问 `/url1/r1/app.js` 命中 project2。
4. 请求 `/docs`，存在 `docs/index.html`，返回 301 到 `/docs/`，随后 200。
5. SPA 项目访问 `/user/123` 返回 index.html；访问 `/missing.js` 返回 404。
6. 受 Basic 保护的 binding，无凭证 401，正确凭证 200。
7. 两个数据节点同时服务，切版本后均生效。
8. 10 万文件项目发布成功，manifest 多 shard，冷访问只加载对应 shard。
9. 上传含路径穿越的包被拒绝，线上不受影响。
10. 控制面停止，数据节点继续服务。

## 8. 约束与假设

- 数据面基于 Caddy v2 自定义 handler，不采用 per-binding Caddy route。
- 语言为 Go。
- 默认监听 HTTP `:80`（数据面）、`127.0.0.1:8080`（控制面管理 API）、`127.0.0.1:8081`（控制面内部 API），均可配置；不自动签发 HTTPS 证书。
- 软件全局保留前缀 `/__pageforge`，禁止 binding path_prefix 占用。
- 单租户，不做租户隔离。
- bucket 默认私有，代理持有凭证。
- manifest 由服务端生成。
- 发布产物解包为独立对象，zip 仅归档。
- latest 为最近成功发布版本。
- `try_files_enabled` 默认关闭，project 级开启。
- binding 可 `pinned_version_id` 固定到任意版本。

## 9. 可配置默认值

| 配置 | 默认值 |
|---|---|
| snapshot 轮询间隔 | 2s |
| 切版本生效上限 | 10s |
| manifest shard 文件数阈值 | 20000 |
| manifest shard 大小阈值 | 2MB |
| 单版本最大文件数 | 200000（可调） |
| 单文件最大大小 | 2GB（可调） |
| 单版本最大总字节 | 50GB（可调） |
| 发布包最大字节 | 4GiB（`publish.max_package_bytes`） |
| 幂等键有效期 | 24h（`publish.idempotency_ttl`） |
| 版本保留数量 | 20（`gc.keep_versions`，可被 project 级 `keep_versions` 覆盖） |
| 版本保留天数 | 0（`gc.keep_days`，0 表示只按数量保留；可被 project 级 `keep_days` 覆盖） |
| 本地缓存默认容量 | 10GB（`cache_max_bytes`） |
| 对象缓存空闲 TTL | 24h（`cache_ttl`，按最后访问时间计算） |
| 缓存淘汰目标水位 | 60%（`cache_evict_ratio`，达到容量上限时按 LRU 淘汰至此水位） |
| 404 负缓存 | 30s |
| HTML Cache-Control | `no-cache` |
| 静态资源 Cache-Control | `public, max-age=300` |
| 静态资源 immutable 匹配 | 空（`cache.assets_immutable_glob` 不启用） |
| SPA 默认关闭 | 是 |
| try_files_enabled 默认关闭 | 是 |
| package_retention 默认开启 | 是 |
| binding 默认鉴权 | `none` |
| 保留 URL 前缀 | `/__pageforge`（可配置） |

## 10. 二期扩展需求（预留）

以下功能需要在本期设计中预留扩展点，但代码不在本期交付：

### FR-22 发布模式 B（presigned 直传）

- 模式 B：控制面返回 staging 前缀与 presigned PUT URL，客户端直传对象后调用 `/complete`；服务端通过 ListObjectsV2 校验数量/大小，并流式回读计算 sha256，再生成 manifest。
- 需要对象存储能力：`PresignPut`、`Copy`（staging → 正式版本前缀）。
- 架构预留：`ObjectStore` 增加 `PresignPut`、`Copy` 方法；发布流水线抽象出 `ArchiveReader`（zip/tar 两个实现）；staging 前缀沿用 `_system/staging/`。

### FR-23 鉴权扩展模式

- `forward_auth`：向配置 URL 发起子请求，透传 `X-Original-URI`、`X-Original-Host`；2xx 放行，401/403 原样返回，其他视为拒绝。
- `signed_url`：`HMAC-SHA256(secret, path + expires)`，校验 `expires` 未过期且签名匹配，否则 403。
- 两种模式复用现有 binding 级 `auth_policy_id`，不新增路由；secret 不进 snapshot，由内部接口下发。

## 11. 二期扩展点清单

| 扩展 | 预留位置 |
|---|---|
| 直传发布 / tar 归档 | `ObjectStore.PresignPut/Copy`、`ArchiveReader` 抽象、`_system/staging/`、`_system/packages/` |
| forward_auth / signed_url | binding `auth_policy_id`、内部策略接口、`internal/auth` 扩展点 |

