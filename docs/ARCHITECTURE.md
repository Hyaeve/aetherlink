# AetherLink 架构说明

## 包职责

| 包 | 职责 |
| --- | --- |
| `internal/config` | YAML 配置加载、默认值合并、校验与归一化（前缀、路径、去重），以及原子写回。文件缺失时自动创建默认配置；未知字段会报错，避免拼写错误静默失效。 |
| `internal/auth` | 管理口令的 PBKDF2-SHA256 派生与恒定时间校验，以及内存会话令牌存储（只存令牌哈希）。 |
| `internal/runtime` | 配置与运行期组件的解耦层：`atomic.Pointer` 持有一组彼此匹配的 provider / resolver / proxy 快照，管理接口改配置后整体替换，播放请求读路径无锁。 |
| `internal/urlx` | STRM 原始内容的 URL 归一化与分类。保留已有 `%XX` 转义，识别 115 pick code / openlist 形态，判断私有网段。 |
| `internal/pathmap` | 上游媒体路径 → 容器路径的前缀重写（最长前缀优先），以及本地目标的根目录白名单校验。 |
| `internal/strm` | 读取 `.strm` 指针（跳过注释行、限制读取长度），区分远程 URL 与容器本地文件，推导显示文件名。 |
| `internal/upstream` | Audiobookshelf 与 Emby 的 API 客户端：识别需要拦截的媒体请求、查询媒体真实路径、书库浏览。 |
| `internal/resolver` | 解析流水线：查上游路径 → 路径映射 → 读指针 →（可选）走完跳转链。带 TTL+LRU 缓存与并发去重。 |
| `internal/proxy` | 反向代理与拦截决策：302、中继转发、本地直读、透传四条出口。 |
| `internal/stats` | 内存计数与最近事件环形缓冲，供概览页使用。 |
| `internal/adminapi` | `/aetherlink/api` 管理接口：初始化向导、口令登录、设置与上游 CRUD、书库浏览与 STRM 调试。 |
| `internal/web` | `go:embed` 承载编译后的 Vue SPA，未构建前端时回退到占位页。 |

## 前端结构

`web/` 是 Vue 3 + Vite 单页应用，`npm run build` 产物落在 `internal/web/dist`，由 `go:embed` 打进二进制。

| 文件 | 作用 |
| --- | --- |
| `web/src/App.vue` | 左侧图标栏 + 主区布局，以及 loading / setup / login / app 四态的首屏闸门。 |
| `web/src/styles.css` | 莫兰迪低饱和浅色主题的全部样式，配色集中在 `:root` 的 CSS 变量里。 |
| `web/src/palette.js` | 上游名称哈希到固定的莫兰迪渐变，保证同名上游的卡片配色恒定。 |
| `web/src/components/UpstreamsView.vue` | 卡片网格：一个上游一张方卡，左键开编辑弹窗，右键出上下文菜单。 |
| `web/src/components/ContextMenu.vue` | 通用右键菜单，含视口边缘回折与点击外部关闭。 |
| `web/src/components/UpstreamForm.vue` | 上游详细编辑弹窗，按「基本 / 路径 / 安全」分组。 |
## 请求判定顺序

1. 按 `prefix` 最长匹配选出上游；`/` 作为兜底。
2. 交给该上游的 `Match` 判断是否为媒体字节接口，不是则直接反代。
3. 上游没有 API 密钥 → 记为 `passthrough` 并反代（无法查询路径）。
4. 解析失败且原因是「不是 `.strm`」→ 记为 `passthrough` 并反代。
5. 本地目标 → `http.ServeContent` 直读（自动支持 Range/HEAD）。
6. 远程目标 → 按 `redirect.mode` 决定 302 还是中继。

## 配置变更流程

网页上的每次保存都走 `runtime.Apply`，顺序固定：

1. 深拷贝当前生效配置得到草稿（`Config.Clone`）。
2. 在草稿上执行修改闭包（改设置 / 增删改上游 / 设置口令）。
3. `Validate()` 归一化并校验草稿。
4. 用草稿构建一整套新的 provider、resolver 与 proxy。
5. 原子写入 `config.yaml`（临时文件 + 0600 + rename）。
6. 全部成功后才 `atomic.Store` 切换。

任何一步失败都直接返回错误，运行中的服务与磁盘上的文件都保持原样——所以一个写错的上游地址不会把正在播放的库弄下线。落盘放在切换之前，是为了让 `/config` 不可写这种问题立刻暴露，而不是给用户一个「重启后就消失的已保存」。

## 关键设计取舍

- **不缓存指针内容按 mtime**：文件系统 mtime 精度不足，同一 tick 内两次写入无法区分。缓存键是媒体引用（上游 + 条目 + 文件），TTL 到期后重新读取指针，路径白名单校验永远在缓存之外无条件执行。
- **并发去重**：播放器 seek 时会并发发起多个 Range 请求，`resolver` 用 inflight map 让同一轨道只打一次上游 API。
- **不设 HTTP 读写超时**：媒体流是长连接，只设 `ReadHeaderTimeout` 与 `IdleTimeout`，取消由请求上下文驱动。
- **跳转预检失败不致命**：`follow_upstream_redirects` 预检失败时退回原始 URL，让播放器自行协商，而不是让播放直接失败。
- **UA 透传**：部分网盘直链与 User-Agent 绑定，默认转发客户端 UA，仅在客户端未提供时使用 `fallback_user_agent`。
- **可选布尔用指针**：`forward_user_agent` 与 `allow_public_targets` 默认为真，若用普通 `bool`，一份省略该键的手写配置会被静默当成 `false`。用 `*bool` 才能区分「没写」与「写了 false」。
- **配置即唯一真相**：不做「环境变量优先于文件」的双轨制。除了少数排障用的启动期覆盖，配置只有一个来源，网页所见即磁盘所存。
- **会话只在内存**：令牌不写配置文件，重启即失效。自托管面板用这个取舍换来「配置文件被复制走也拿不到登录态」。
- **统计跨重载保留**：`stats.Collector` 由 runtime 持有并在重建时复用，改一次配置不会把概览页的计数清零。
- **监听地址不热改**：套接字无法在不中断连接的前提下重绑，`RestartRequired()` 把这个事实如实报给界面，而不是假装生效。

## 与 Audiobookshelf 侧的对应关系

被反代项目（`C:\Develop\AudioBookShelf`，只读参考）自身已支持 `.strm`，把指针目标在服务端代理出去。AetherLink 做的是把这层代理前移：

| 关注点 | Audiobookshelf 内部实现 | AetherLink |
| --- | --- | --- |
| 指针解析 | `server/utils/strmUtils.js` 的 `resolveStrmTarget` | `internal/strm` + `internal/urlx` |
| 字节投递 | `proxyStrm` 服务端流式转发 | 默认 302，必要时才中继 |
| 根目录限制 | 库目录 + 固定 `/NetDisk` | 界面配置的 `strm_roots` |
| 私网放行 | STRM 链路绕过 SSRF 过滤 | `redirect.mode: private` / `allow_public_targets` |

因此上游继续负责扫库、元数据、章节与进度，AetherLink 只接管播放字节这一段。
