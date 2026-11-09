# 数据面对象缓存设计（FR-09）

> 状态：定稿，作为实现依据。
> 范围：数据面本地对象缓存。控制面 snapshot、manifest 缓存不在本文范围。
> 目标：减少回源对象存储，提高数据面吞吐，降低请求延时。

## 1. 结论摘要

数据面缓存的是**对象存储中的单个文件**，不是 HTTP 响应。缓存目录直接镜像对象存储路径，缓存命中与回源都围绕同一条文件记录展开：

1. 未命中回源，回源内容同时写缓存并返回客户端；
2. 已缓存文件被外部删除时视为未命中，回源并自动重建；
3. 空间达到上限时直接回源、不落缓存；
4. 空闲超过 `cache_ttl`（默认 24h，按最后访问时间）后清理；
5. 空间达到 `cache_max_bytes` 时忽略 TTL，按 LRU 淘汰到 `cache_evict_ratio`（默认 60%）；
6. URL 绑定、分享链接、version 直链共用同一个缓存 key。

不做 revalidate、不做 stale、不缓存 HTTP 状态与响应头。正确性依赖“已发布版本内对象不可变”这一前提（见 [requirements.md FR-01/FR-03](requirements.md)）。

## 2. 缓存模型

### 2.1 缓存对象与粒度

- 缓存对象：某个 project 在 S3 中的文件内容（object body）。
- 缓存粒度：单个文件。命中判断、填充、淘汰都以一个 object 为单位。
- 不缓存：HTTP 响应头、状态码、鉴权结果、manifest、发布包。
- 例外：404 负缓存只记录“该对象不存在”的短时标记（默认 30s），不保存内容。

### 2.2 缓存 key

```text
alias_id / bucket / project_id / version_id / object_key
```

- 不包含 host、URL 前缀、binding、分享 token、`Accept-Encoding`。
- 同一个物理对象从不同入口访问时共享同一条缓存记录。
- 版本不可变，因此 key 一旦写入内容就不会变化。
- 修改 alias 的 endpoint/bucket 等存储配置后，需要清理对应缓存目录，避免命中旧存储内容。

### 2.3 缓存目录

```text
{cache_dir}/{alias_id}/{bucket}/{project_id}/{version_id}/{object_dir}/{object_key}
```

示例（`alias_id`/`project_id`/`version_id` 是 `internal/ids` 生成的 `als_`/`prj_`/`ver_` 前缀 ULID，`bucket` 是项目实际使用的 bucket 名）：

```text
./cache/als_<ulid>/static/prj_<ulid>/ver_<ulid>/assets/app.js
./cache/als_<ulid>/static/prj_<ulid>/ver_<ulid>/index.html
```

`object_dir`/`object_key` 直接来自对象的相对 key，不额外插入 `objects/` 之类的层级；根目录对象没有 `object_dir`。目录结构与对象存储一致，运维可以直接按业务维度查看、统计和手动清理。缓存包不维护单独的元数据文件，索引在内存中，文件本身就是缓存条目。

### 2.4 路径安全

对象 key 来自对象存储，落盘前逐段校验：

- 拒绝空段、`.`、`..`；
- 拒绝 `\`、`:`、`*`、`?`、`"`、`<`、`>`、`|` 与控制字符；
- 拒绝 Windows 设备名（`CON`、`NUL`、`COM1` 等）与以点/空格结尾的段；
- 单段长度超过 255 字节时拒绝。

校验失败不影响请求：对象照常回源并返回客户端，只是标记为 `BYPASS`、不写缓存。

## 3. 请求流程

所有数据面入口（URL 绑定、分享链接、version 直链）最终都进入同一个 `serveEntry`，因此缓存逻辑只有一份。

### 3.1 命中

1. 按 key 查内存索引；
2. 索引存在且未超过空闲 TTL；
3. 打开对应文件。打开失败（文件被删除、权限变化）时删除索引项并按未命中处理；
4. 更新 LRU 位置与最后访问时间，返回文件内容。

命中路径只做一次 map 查找、一次链表移动和一次 `open`，不回源。

### 3.2 未命中并建立缓存

1. 查负缓存；命中则直接 404；
2. 判断准入：对象大小未知、对象大于 `cache_max_bytes`、或 `已用空间 + 对象大小 > cache_max_bytes` 时走 3.4 跳过缓存；
3. 同一 key 只允许一个 leader 回源（singleflight）。leader 从 S3 拉取 body；
4. body 一边写给客户端，一边写入同目录的临时文件（`.pf-tmp-*`）；
5. 读完后校验字节数与 manifest 一致，关闭临时文件并 `rename` 到最终路径，登记索引，计入 LRU 头部；
6. 客户端中途断开、读取出错、长度不一致或 rename 失败时不提交缓存，临时文件删除。

响应头 `X-Cache: MISS` 表示本次回源并尝试建立缓存；`X-Cache: HIT` 表示命中。

### 3.3 已缓存文件丢失

文件系统扫描与实际读取是两件事，索引可能领先于磁盘：

- 命中打开文件返回 `ENOENT` 时，删除该索引项；
- 继续按未命中流程回源；
- 回源成功后自动写回同一路径，重建缓存。

因此“被人为删除部分缓存文件”不需要额外修复工具，下一次访问即自愈。

### 3.4 跳过缓存

以下情况不写缓存，但请求正常返回：

- `已用空间 + 对象大小 > cache_max_bytes`；
- 对象大小大于整个预算；
- 对象大小未知（manifest 缺失 size）；
- key 无法安全映射为路径；
- 临时文件创建或 rename 失败。

响应头 `X-Cache: BYPASS`。跳过时会异步触发一次 LRU 淘汰到低水位，为后续请求腾出空间。

### 3.5 HEAD

启用缓存时，HEAD 不再下载对象：

- 命中：`X-Cache: HIT`，`Content-Length` 取缓存文件大小；
- 未命中：`X-Cache: MISS`，`Content-Length` 取 manifest 中记录的大小，不建立缓存。

未启用缓存时保持原有行为。

### 3.6 Range

- 命中：从本地缓存文件读取指定区间，`206`；
- 未命中：直接带 Range 回源，不写缓存，`X-Cache` 不参与该路径。

不对部分内容做“半个对象”缓存，避免碎片和长度不可信。完整 GET 会在下一次请求时建立缓存。

### 3.7 404 负缓存

- S3 返回 `ErrNotFound` 时，记录 key 到负缓存，默认 30s；
- 负缓存命中时直接 404，不回源；
- 到期自动失效，不做主动清理；
- 负缓存只存在于内存，重启后清空。

## 4. 缓存管理

### 4.1 空闲 TTL

- 配置：`cache_ttl`，默认 24h。
- 计时起点是**最后一次访问时间**，不是缓存建立时间。
- 命中会刷新最后访问时间。
- 后台每 5 分钟扫描一次，清理超过 TTL 未访问的条目及其文件；命中时也会惰性判断一次，避免读到过期条目。

### 4.2 内存索引与最后访问时间

- 索引：`map[key]entry` + LRU 双向链表，`entry` 记录路径、大小、最后访问时间。
- 启动时扫描缓存目录，按目录层级还原 key；文件 mtime 作为最后访问时间。
- 运行期命中时，内存中的最后访问时间立即更新；文件 mtime 最多每 5 分钟刷新一次，避免每请求一次写操作。进程重启后，最后访问时间误差不超过该刷新间隔。

### 4.3 容量与 LRU 淘汰

- 配置：`cache_max_bytes`（默认 10GB）、`cache_evict_ratio`（默认 0.6）。
- 正常准入保证 `已用空间 + 对象大小 <= cache_max_bytes`。
- 达到上限时忽略 TTL，从 LRU 尾部开始删除，直到使用率降到 `cache_max_bytes * cache_evict_ratio`。
- 触发点：
  - 未命中且空间不足（跳过缓存时异步触发）；
  - 后台扫描发现使用量超过上限；
  - 启动扫描后使用量超过上限。
- 最近访问的条目保留；被淘汰的文件立即从索引移除并尽力删除，删除失败时文件成为孤儿，由后续扫描按空闲 TTL 清理。

### 4.4 并发与单飞

- 同一个 key 的并发未命中只允许一个 leader 回源。
- follower 等待 leader 完成（默认最多 2s），完成后直接打开缓存文件；
- 等待超时或 leader 失败时，follower 自行回源并按 `BYPASS` 返回，避免把请求无限阻塞。
- 不同 key 之间没有全局串行化；索引锁只保护内存结构，不覆盖文件读写与 S3 请求。

### 4.5 启动与关闭

- 启动：创建缓存目录，扫描已有文件，清理遗留 `.pf-tmp-*`，建立内存索引，然后启动后台维护协程。
- 关闭：停止后台协程。索引和文件保留，供下次启动复用。

## 5. 配置

Caddyfile：

```text
s3static {
	cache_dir ./cache
	cache_max_bytes 1073741824
	cache_ttl 24h
	cache_evict_ratio 0.6
}
```

| 配置 | 默认值 | 说明 |
|---|---|---|
| `cache_dir` | 空（不启用缓存） | 缓存根目录 |
| `cache_max_bytes` | 10GB（启用缓存时） | 缓存容量上限 |
| `cache_ttl` | 24h | 未访问多久后清理，按最后访问时间计算 |
| `cache_evict_ratio` | 0.6 | 达到容量上限后 LRU 淘汰的目标水位，取值 `(0,1)` |

`cache_evict_ratio` 只接受 0 到 1 之间的值（不含 0 和 1）；`cache_ttl` 必须为正数。

## 6. 可观测性

### 6.1 响应头

| `X-Cache` | 含义 |
|---|---|
| `HIT` | 命中本地缓存 |
| `MISS` | 未命中，已回源并尝试建立缓存（HEAD 未命中时表示未建立） |
| `BYPASS` | 未命中且未写缓存 |
| `NEGATIVE` | 命中 404 负缓存 |

### 6.2 进程内指标

`cache.Cache.Stats()` 提供：条目数、已用字节、容量、命中、未命中、跳过、填充、填充失败、淘汰、过期。后续可接入 Prometheus 或内部状态接口。

## 7. 代码结构

| 文件 | 职责 |
|---|---|
| [internal/cache/object.go](../internal/cache/object.go) | 缓存对象标识、key、路径段校验 |
| [internal/cache/cache.go](../internal/cache/cache.go) | 索引、LRU、TTL、容量、单飞、填充、扫描、后台维护 |
| [internal/handler/serve.go](../internal/handler/serve.go) | 入口统一接入缓存、`X-Cache`、HEAD/Range/负缓存 |
| [caddy/module.go](../caddy/module.go) | 配置解析、默认值与校验、缓存生命周期 |

核心接口：

```go
func Open(opts Options) (*Cache, error)
func (c *Cache) Get(ctx context.Context, obj Object, size int64, fetch FetchFunc) (io.ReadCloser, int64, Status, error)
func (c *Cache) Open(obj Object) (*os.File, int64, bool)
func (c *Cache) Sweep()
func (c *Cache) Close() error
```

## 8. 已知限制

1. **对象 key 与目录冲突**：对象存储允许同时存在 `a` 与 `a/b`，文件系统不允许。出现冲突时该对象 `BYPASS`，不影响响应。
2. **大小写不敏感文件系统**：极少数仅大小写不同的 key 会互相覆盖；项目内 key 通常不区分大小写使用。
3. **超长路径**：依赖操作系统的长路径支持；无法创建时 `BYPASS`。
4. **alias 配置变更**：缓存 key 使用 `alias_id`，修改 endpoint 后需手工清理对应目录，否则可能命中旧存储内容。
5. **客户端中断**：填充随客户端读取进行，客户端提前断开时本次不落缓存，下一次请求重新建立。

## 9. 测试要点

- 首次 `MISS`、再次 `HIT`，第二次不回源；
- 缓存路径与对象存储层级一致；
- 删除缓存文件后自愈重建；
- 空闲 TTL 按最后访问时间计算，访问会续期；
- 空间不足时 `BYPASS`，不产生缓存文件；
- 容量压力下按 LRU 淘汰到 60%，最近访问条目保留；
- 并发未命中只回源一次；
- 重启后能从目录恢复索引；
- 不安全 key、长度不一致不写缓存；
- 负缓存到期失效。
