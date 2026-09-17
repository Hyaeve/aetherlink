# AetherLink 架构说明

## 包职责

| 包 | 职责 |
| --- | --- |
| `internal/config` | YAML 配置加载、默认值合并、校验与归一化（反代端口、路径、去重），以及原子写回。空闲端口推荐（`SuggestPort`）也在这里。文件缺失时自动创建默认配置；未知字段会报错，避免拼写错误静默失效。 |
| `internal/auth` | 管理账号（用户名 + 口令）的 PBKDF2-SHA256 派生与恒定时间校验、内置默认账号的生成，以及内存会话令牌存储（只存令牌哈希）。 |
| `internal/runtime` | 配置与运行期组件的解耦层：`atomic.Pointer` 持有一组彼此匹配的 provider / resolver / proxy 快照，管理接口改配置后整体替换，播放请求读路径无锁。同时管理每个上游的反代端口监听器，支持热增删。 |
| `internal/urlx` | STRM 原始内容的 URL 归一化与分类。保留已有 `%XX` 转义，识别 115 pick code / openlist 形态，判断私有网段。 |
| `internal/pathmap` | 上游媒体路径 → 容器路径的前缀重写（最长前缀优先）、本地目标的根目录白名单校验，以及 `Locate`：翻译后的路径不存在时，把上游路径逐段剥前缀，在配置根目录、映射目标与常见挂载点下 stat 定位指针，两侧挂载点不同名也不必手写映射。 |
| `internal/strm` | 读取 `.strm` 指针（跳过注释行、限制读取长度），区分远程 URL 与容器本地文件，推导显示文件名。 |
| `internal/upstream` | Audiobookshelf、Emby 与飞牛影视的 API 客户端：识别需要拦截的媒体请求、回答「这个媒体是什么」（`MediaTarget`：ABS 给指针路径，Emby 系给已解析的直链）、书库浏览，以及两种鉴权方式（静态密钥与账号密码登录换令牌 —— 飞牛还必须把令牌挂在完整的 `X-Emby-Authorization` 身份头上，`X-Emby-Token` 简写形式会被它 400 拒掉）。Emby 与飞牛影视（`fnos.go`）还会改写客户端的 `PlaybackInfo`，只把已经通过客户端兼容性判断的 STRM 接到 302 路由。 |
| `internal/resolver` | 解析流水线：问上游 → 直链直接用 / 定位并读指针 →（可选）走完跳转链。带 TTL+LRU 缓存与并发去重。读不到指针时返回 `ErrPointerUnavailable`，由调用方退回透传。 |
| `internal/proxy` | 反向代理与拦截决策：302、中继转发、本地直读、透传四条出口。一个 `Server` 只服务一个上游，因此不需要路径匹配。 |
| `internal/stats` | 内存计数与最近事件环形缓冲，供日志与排障使用。 |
| `internal/adminapi` | `/aetherlink/api` 管理接口：登录页自举、账号登录、账号修改、设置与上游 CRUD、连通性测试与日志。 |
| `internal/web` | `go:embed` 承载编译后的 Vue SPA，未构建前端时回退到占位页。 |

## 前端结构

`web/` 是 Vue 3 + Vite 单页应用，`npm run build` 产物落在 `internal/web/dist`，由 `go:embed` 打进二进制。

| 文件 | 作用 |
| --- | --- |
| `web/src/App.vue` | 左侧图标栏 + 主区布局（侧栏右边缘的竖线按钮控制展开成图标 + 文字，状态存 localStorage），以及 loading / login / app 三态的首屏闸门。只有以太链接 / 运行日志 / 系统设置三个页面。 |
| `web/src/styles.css` | 莫奈低饱和浅色主题的全部样式，配色集中在 `:root` 的 CSS 变量里。动态部分是 `body::before` 的多点径向渐变漂移与卡片的 `background-position` 位移，两者都在 `prefers-reduced-motion` 下静止。 |
| `web/src/palette.js` | 上游名称哈希到固定的莫奈三段渐变与动画相位，保证同名上游的卡片配色恒定，同时让卡片之间的流动不同步。 |
| `web/src/components/UpstreamsView.vue` | 卡片网格：一个上游一张方卡，左键开编辑弹窗，右键出上下文菜单。 |
| `web/src/components/ContextMenu.vue` | 通用右键菜单，含视口边缘回折与点击外部关闭。 |
| `web/src/components/UpstreamForm.vue` | 上游详细编辑弹窗，按「基本 / 密钥 / 路径」分组；地址提示随服务端类型切换，密钥输入框只对 Audiobookshelf 与 Emby 显示，飞牛影视换成一对其登录账号密码（可留空，且必须成对填写）。已保存的密钥与密码在弹窗打开时向 `GET /upstreams/{name}/credentials` 取回并填进字段，默认遮蔽成圆点，点输入框右侧的小眼睛才看明文；飞牛的「试连」在账号或密码确实缺失时直接拦下、不发请求。 |
| `web/src/components/LogsView.vue` | 播放流水（读 `/stats`：计数 + 逐条事件 + 无 302 时的诊断结论）与运行日志（读 `/logs`，按级别过滤）。 |
| `web/src/components/SettingsView.vue` | 302 策略、缓存与日志、管理账号、运行信息。 |
## 管理账号与入口

- **恒有一个账号**：`cmd/aetherlink/main.go` 在加载配置后检查 `Auth.IsConfigured()`，为空就写入 `auth.Default()`（`admin` / `password`，同时置 `default_credentials: true`）并立刻落盘。首次启动、旧版升级、手工清空 `auth:` 段这三种情形因此都不再有「无账号」状态，管理 API 未登录一律 401，不存在需要免鉴权写入的初始化通道。
- **登录页只多给一个账号名**：免鉴权的 `GET /aetherlink/api/bootstrap` 返回版本号、密码长度下限与**当前账号名**，用来确认后端可达并预填登录页的账号框（改过名也能对上）。账号名不是秘密——要显示在登录页上，就不该装作它是密码；凭据、密码校验材料、以及 `defaultCredentials` 一概不回，否则扫到端口的人等于被告知「这里 admin/password 就能进」。「仍在用默认账号」的提醒只在登录之后由 `/status` 与 `/config` 提供。前端还把这个名字记在 localStorage，后端不可达时用它兜底。
- **账号修改**：`POST /aetherlink/api/account` 接收当前密码 + 新用户名 + 新密码（新密码留空表示只改用户名），成功后清掉 `default_credentials` 并 `RevokeAll()` 注销全部会话，前端随即回到登录页。
- **用户名比较宽松，密码严格**：`auth.VerifyLogin` 对用户名去空白且不区分大小写；用户名不匹配时仍走一次 PBKDF2 派生，使耗时与密码错误一致。注意这里的「不区分」只为了让登录失败的两条路径耗时一致，不代表账号名是秘密——登录页要显示它（见上一条）。老配置里没有 `username` 时按 `admin` 兼容。
- **根路径直达**：`adminRootHandler` 让 `GET /` 302 到 `/aetherlink/`，用户不必手敲后缀。管理端口不做反代，其余路径一律 404——端口填错时宁可立刻报错，也不要静默转发出一个难查的问题。

## 请求判定顺序

1. 请求落在哪个反代端口上，就是哪个上游（`runtime.handlerFor`，一端口一上游，路径不参与选择）。
2. Emby 系（`emby` / `fnos`）的 `/Items/:id/PlaybackInfo` 先原样请求上游。STRM 源只有在 `SupportsDirectPlay=true` 时才重写 `DirectStreamUrl`，三个 `Supports*` 能力字段与 `TranscodingUrl` 全部保留；若上游判断客户端不兼容，则响应逐字节不改并继续 HLS 转码。例外：该上游的跳转模式为 always（卡片「始终跳转」）时，STRM 源无条件压成 `SupportsDirectPlay=false` + `SupportsDirectStream=true` 并补 `DirectStreamUrl`，同时把 `TranscodeReasons` / `TranscodingReasons` 清成空数组。无条件是必须的：既忽略上游的不可直放判定（飞牛对外网客户端的码率限制判定会让转码流量全部走上游自己），也堵掉上游判可直放时的 DirectPlay——那种情况下客户端会直连 `Path`（网盘 / OpenList 直链）绕开 AetherLink，既没有 302 记录，也不受内网直链安全网保护。普通媒体源始终不修改。飞牛影视同一个响应里还会顺手给 `MediaStreams` 补上非空字段，并在转发前把这条请求钉到 `/emby` 前缀下（`upstream.RequestPathRewriter`）——少了前缀飞牛返回的是单页应用 HTML，拿不到 `MediaSources` 就永远不 302。
3. 交给该上游的 `Match` 判断是否为媒体字节接口，不是则直接反代。未命中但路径看起来像播放请求（含 `/stream`、`/track/`、媒体扩展名等）时记一条 info 日志；Emby HLS 清单或分片会单独说明「分片本身不能 302，应重新播放以重新协商直放」。
4. 上游没有可用凭据、且该类型必需凭据（Audiobookshelf / Emby）→ 记为 `passthrough` 并反代（无法查询媒体信息）。飞牛影视属于「凭据可选」：它没有静态密钥，什么都没配也照常查询，不会落到这一支（见「飞牛的凭据是可选的」）。
5. 问上游 `MediaTarget`，按回答分三条路：
   - **已是直链**（Emby：`Protocol: Http` 或 `Path` 以 `http(s)://` 开头）→ 归一化后直接进入第 7 步，不读任何文件。
   - **是指针文件**（路径以 `.strm` 结尾，或 Emby 报的 `Container` 是 `strm`）→ `pathmap.Locate` 定位到容器内路径再读取。定位不到或读不到（`ErrPointerUnavailable`）→ 记为 `passthrough` 并反代，同时把原因写进日志与事件。
   - **普通文件**（`ErrNotStrm`）→ 记为 `passthrough` 并反代。
6. 指针指向容器本地文件 → `http.ServeContent` 直读（自动支持 Range/HEAD）。
7. 远程目标 → 按 `redirect.mode` 决定 302 还是中继。

## 配置变更流程

网页上的每次保存都走 `runtime.Apply`，顺序固定：

1. 深拷贝当前生效配置得到草稿（`Config.Clone`）。
2. 在草稿上执行修改闭包（改设置 / 增删改上游 / 改管理账号）。
3. `Validate()` 归一化并校验草稿（含反代端口的范围、重复与管理端口冲突）。
4. 用草稿构建一整套新的 provider、resolver 与 proxy。
5. 先把草稿里新增的反代端口绑定下来（`acquirePorts`），端口被占就整体失败。
6. 原子写入 `config.yaml`（临时文件 + 0600 + rename），失败则释放刚拿到的端口。
7. 全部成功后才 `atomic.Store` 切换，并关掉不再需要的旧端口。

任何一步失败都直接返回错误，运行中的服务与磁盘上的文件都保持原样——所以一个写错的上游地址不会把正在播放的库弄下线。落盘放在切换之前，是为了让 `/config` 不可写这种问题立刻暴露，而不是给用户一个「重启后就消失的已保存」。

## 关键设计取舍

- **飞牛影视复用 Emby 方言，不另写一套链路**：飞牛 NAS 自带的「影视」就是 Emby 方言的服务端——播放路由（`/Videos/:id/stream`、`/Items/:id/Download`）、播放协商（`/Items/:id/PlaybackInfo`）与字段含义都和 Emby 一致。因此 `upstream.UpstreamType.IsEmbyFamily()` 把 `emby` 与 `fnos` 归为一族，拦截、解析、302、HLS 判断、UA 屏蔽名单、直链缓存策略全部共用；`fnosProvider` 只叠加四处差异：API 调用补 `/emby` 前缀（少了它会落到 SPA 的 HTML 上，表现就是「永远不 302」）、条目查询优先走单项路由 `/Items/{id}`（飞牛没实现集合路由 `/Items?Ids=`，那条路径会落到单页应用上返回 HTTP 200 的 HTML，JSON 解析在第一个 `<` 上就断了，报错是一句无从下手的 `invalid character '<' looking for beginning of value`；用户看到的现象是「能进库、能浏览，一播放就失败」。Emby 本身两条路由都支持，所以顺序反过来、集合路由优先，且由 `preferItemByID` 显式区分，见「条目查询两条路由要排序」）、`PlaybackInfo` 的 `MediaStreams` 补齐客户端要求非空的字段、网页播放器 `basehtmlplayer.js` 去掉 `crossorigin="anonymous"`（302 出去的是跨域直链，带着它浏览器会按需要 CORS 放行处理，直链端不返回 CORS 头就播不出来）。
- **Emby 与 ABS 的 strm 形态根本不同**：Emby 扫库时就把指针读掉了，`MediaSources[].Path` 直接是直链，AetherLink 完全不需要挂载媒体目录；Audiobookshelf 保留指针原样、播放时自己代理，AetherLink 必须能读到那个 `.strm` 才能 302。两条链路在 `resolver.resolveUncached` 里分开处理，`upstream.MediaTarget` 就是为了让这个区别显式化而存在的。
- **指针读不到就透传，不报错**：上游自己能读到那个文件，让它继续服务比让播放失败好得多。原因记进日志与事件，用户能查到「为什么没有 302」，而不是听到一段静音。解析报错（上游 API 挂了、返回体变了）同样退回透传而不是回 502——装上 AetherLink 之后反而播不了，是最不可接受的失败模式。
- **每条出口都必须留下日志**：`serveMedia` 里所有分支统一走一个 `finish` 闭包，记事件的同时必定打一行日志。早先只写 `stats.Collector`、成功路径一行日志都不打，结果「不能 302」这个问题在容器日志与界面里完全不可观测——排障能力本身就是功能。
- **拦截规则容忍路径前缀**：Audiobookshelf 支持 `ROUTER_BASE_PATH`，Emby 与飞牛影视常带 `/emby`，两者的媒体路由都可能多一段前缀。ABS 至少容忍一段，Emby 系直放路由容忍任意层级前缀，否则子路径部署会整条漏匹配，现象是完全静默、既不 302 也没有日志。
- **ABS 会话音轨要两步查**：`/public/session/:id/track/:index` 是网页端与 App 真正取字节的入口，但 `/api/session/:id` 从数据库重建会话、**不返回 audioTracks**，而刚开始播放的会话甚至还没落库（404）。因此先查会话、必要时回退 `/api/sessions/open`，拿到 `libraryItemId` 后再查条目按序号定位音频文件。少了这两级回退，ABS 侧永远不会 302。
- **Emby 查媒体源要带 UserId**：不少 Emby 版本只在「以某个用户身份查询」时才展开 `MediaSources`，`PlaybackInfo` 缺 `UserId` 甚至直接 400。`resolveUserID` 取一次管理员 ID 并缓存，`/Items` 与 `PlaybackInfo` 都带上，最后再留一次不带 UserId 的重试。
- **Emby 的 302 起点是 PlaybackInfo，不是 HLS 分片**：客户端先根据 `PlaybackInfo` 决定直放或转码。一旦选中 `/hls1/main/*.ts`，每个请求只代表一段转码数据，不可能 302 到完整媒体文件。但也不能把所有 STRM 强制直放：网页端拿到不支持的 H.265 原文件同样无法播放。因此只在上游给出 `SupportsDirectPlay=true` 时重写 `DirectStreamUrl`，其余能力字段和转码 URL 保持原样；不兼容时宁可不 302，也要让 Emby 正常转码。媒体源和兼容性判断会一起短暂缓存；不兼容的客户端即使又请求 `/stream`，也会退回上游而不会误跳原文件。唯一的例外是卡片「始终跳转」：那种模式下用户要的就是流量全经 AetherLink 再 302，所以 STRM 源不再按上游判定分流，一律压成 DirectStream（`DirectPlay` 绝不能用——客户端会拿它直连 `Path` 绕开我们）。
- **基础路径不能重复拼接**：配置的 Emby / 飞牛影视地址可能已经带 `/emby`，客户端请求也可能以 `/emby` 开头。反代 `joinPath` 会先判断请求是否已含基础路径，直放 URL 也会优先复用 Emby 原本的路由前缀并折叠相邻重复段，避免生成 `/emby/emby/Videos/...`；`fnosAPIPrefix` 同样在地址已以 `/emby` 结尾时不再补前缀。
- **条目查询三种路由要排序，不能只写一条**：`/Items?Ids=a,b`（集合）、`/Items/{id}`（单项）与 `/Items/{id}/PlaybackInfo`（播放协商）都能给出条目的媒体源，但各家实现的支持面差得很远——Emby 三条都有，飞牛**只**有 PlaybackInfo 那条：`/Items` 与 `/Items/{id}` 都回整页单页应用的 HTML 并带上 HTTP 200，**不返回 404**。这时 JSON 解码在第一个 `<` 上失败，抛出来的是 `invalid character '<' looking for beginning of value`：既看不出是哪条请求，也看不出上游其实回了网页。因此 `embyProvider.fetchItem` 按 `itemLookupOrder()` 依次试并汇总各自的失败原因：飞牛由 `preferItemByID = true` 排成「PlaybackInfo → 单项 → 集合」（PlaybackInfo 是它唯一确定可用的——客户端的播放流程一直在走它，AetherLink 也一直在改写它的响应；排在首位既省掉两次注定失败的请求，也保证解析能落地），Emby 保持「集合在前」（一次调用同时给出元数据与媒体源，能用就不多打）。PlaybackInfo 给的是媒体源而非条目元数据，所以 `fetchItemBy` 会把 `MediaSources[0].Path` 一并取为 `item.Path`，让「没有可用媒体源时退回读指针文件」那条兜底不断。配套两件事：`decodeJSONResponse` 解码失败时把**实际请求地址**、`Content-Type`、是否携带令牌与响应开头片段一起写进错误（写 endpoint 参数会漏掉 `apiPrefix`，于是「GET /Items/eb35…」看上去像前缀没补，照着它排查只会走偏），`looksLikeHTML` 负责识别「这其实是网页」。
- **播放器的令牌要能一路送到解析调用上**：播放器本来就向上游登录过，它手上的令牌必定是上游刚认可的身份，而且客户端请求里就带着（`X-Emby-Authorization` 的 `Token="…"`、简写的 `X-Emby-Token`，或 `?api_key=`）。因此 `proxy.serveMedia` 用 `upstream.WithClientCredentials` 把这份身份放进上下文，`apiClient.credentials` 在**上游什么都没配**时借它一用——飞牛不填账号密码也能完成「回头查上游」，参考项目 LitePan 取条目时直接复制原请求凭据，是同一个道理。配了就还是以配置的为准：那是管理员身份，权限比播放器更完整（Emby 的 `/Users` 只有管理员读得到）。还有一处兜底：配置的账号密码失效（改过密码、账号被停用）时登录会失败，此时改用播放器带上来的令牌——播放解析不该因为一份过期的配置而整个失败。
- **前缀改写只给播放协商开一个口子**：飞牛的 API 只挂在 `/emby` 下，而网页控制台（`/web/...`）、封面（`/Items/:id/Images/...`）与字节接口都在根路径。因此 `RequestPathRewriter` 只在 `PlaybackInfo` 上门禁补前缀，其余路径一律原样送达——无差别加前缀会把界面整个打挂，这比「少 302」更难排查。反向断言（其余路由必须不被改写）与正向断言（少前缀时仍能 302）在 `internal/proxy/fnos_test.go` 里成对存在。
- **飞牛的凭据是可选的，令牌只活在内存里**：飞牛影视没有 Emby 控制台里那种静态 API 密钥，它的接口只认客户端登录换来的令牌——而那份令牌由播放器自己登录取得、随请求转发，AetherLink 只是把请求转过去。所以「什么都不填」就能完成 302：`PlaybackInfo` 改写时顺手缓存下来的媒体源足够回答紧接着的 `/stream`。超出这个 10 分钟窗口、或客户端直接请求下载入口时，AetherLink 才需要自己回头查一次上游，那份查询才需要凭据。因此账号密码被设计成**可选项而非必填**：填了就用 `POST /emby/Users/AuthenticateByName` 换令牌（复用 6 小时，只存在内存里，不落盘）；没填也不要紧——播放器自己的令牌会随请求送进来，AetherLink 直接借用（见「播放器的令牌要能一路送到解析调用上」）。撞上 401/403 视为令牌被吊销，丢掉缓存重登一次再重试——不这么做，一次令牌过期会表现成「上游坏了」。`HasCredentials()` 对飞牛恒为真，否则代理层会退化成纯反代，播放请求永远拿不到 302。既然凭据只会用在「回头查上游」这一支上，那么**试连**也必须先填账号密码才成立：没凭据时飞牛连服务信息都不给，失败说明不了地址对不对，等于白试——所以界面在发请求前就拦下并直说原因，后端 `Ping` 的兜底错误同样写成「需要先填写登录账号与密码」，而不是把上游那句 `X-Emby-Authorization is missing` 原样抛出来（它看着像「没配鉴权」，极难定位）。
- **已保存的凭据平时是圆点，看明文点输入框右侧的眼睛**：列表接口 `GET /upstreams` 只回报 `hasApiKey` / `hasPassword` 两个布尔，秘密真正的值只能靠调 `GET /upstreams/{name}/credentials` 取（同样走会话鉴权，并留一行审计日志）。编辑窗口在 `onMounted` 里为「已存过该凭据」的上游调一次，把值填进字段并保持 `type="password"`——所以用户看到的就是一串圆点，而不是一个空框。这个选择是刻意的：空框必须配一句「留空即保留原值」的隐藏规矩，用户得先记住规则才能判断「框里没有」到底是什么意思；圆点没有这个问题，**框里是什么就存什么，清空才等于删除**。与之配套的三件事：1) 加载成功时把内部标记 `keepApiKey` / `keepPassword` 置假，让「清空字段」能真的表达删除；2) 加载期间字段 `disabled`，否则请求返回时会盖掉用户刚敲进去的字；3) 加载失败时标记保持为真、并提示「直接保存会保留原值」，这样一次网络抖动不会把配置里的密钥抹掉。换服务端类型（如 Emby → 飞牛影视）不需要清理字段：`buildPayload` 本来就按类型决定送哪一项，另一个字段的值到不了后端。
- **自动定位而非要求手写映射**：`pathmap.Locate` 把上游路径逐段剥前缀，在配置根目录、映射目标与常见挂载点下 stat。白名单校验从不放宽——白名单外的候选连 stat 都不做。凡能自动化的就不要求用户填表。
- **不缓存指针内容按 mtime**：文件系统 mtime 精度不足，同一 tick 内两次写入无法区分。缓存键是媒体引用（上游 + 条目 + 文件），TTL 到期后重新读取指针，路径白名单校验永远在缓存之外无条件执行。
- **并发去重**：播放器 seek 时会并发发起多个 Range 请求，`resolver` 用 inflight map 让同一轨道只打一次上游 API。
- **不设 HTTP 读写超时**：媒体流是长连接，只设 `ReadHeaderTimeout` 与 `IdleTimeout`，取消由请求上下文驱动。
- **跳转预检失败不致命**：`follow_upstream_redirects` 预检失败时退回原始 URL，让播放器自行协商，而不是让播放直接失败。
- **UA 透传**：部分网盘直链与 User-Agent 绑定，默认转发客户端 UA，仅在客户端未提供时使用 `fallback_user_agent`；同一次播放的上游 API、直链预检和实际下载始终使用同一个非空 UA。
- **UA 屏蔽**：安全设置中的 UA 关键词列表按大小写不敏感的包含关系匹配；命中后在反代入口直接返回 403，不再转发到上游。关键词按行填写，不再支持 `/xxx/` 包裹语法。
- **签名直链按 `t` 缓存**：Emby 系直链若带数字 `t` 参数，解析缓存只保留到该直链到期；没有有效 `t` 时固定缓存 2 小时，不提供自定义入口。缓存命中和首次获取都会把当前有效期写入容器日志与播放流水；Audiobookshelf 固定缓存 15 分钟。
- **直链缓存跨重启保留**：解析结果以原子替换方式写入配置目录下的 `direct-links-cache.json`，启动时只恢复尚未过期的条目；缓存键仍包含上游、媒体引用和实际使用的 UA，因此不同 UA 不会复用同一条签名直链。清空缓存时同步清空磁盘文件。
- **可选布尔用指针**：`forward_user_agent` 与 `allow_public_targets` 默认为真，若用普通 `bool`，一份省略该键的手写配置会被静默当成 `false`。用 `*bool` 才能区分「没写」与「写了 false」。
- **配置即唯一真相**：不做「环境变量优先于文件」的双轨制。除了少数排障用的启动期覆盖，配置只有一个来源，网页所见即磁盘所存。启动期覆盖（`AETHERLINK_PORT` / `AETHERLINK_LISTEN` 的监听端口、`AETHERLINK_ADMIN_TOKEN` 的应急令牌）只作用于本次启动，`Save` 会把文件里原本的值写回去——否则「临时加了个变量」会变成永久改动：删掉端口变量后监听端口仍被改过，compose 映射的 `5151` 就再也进不去；删掉令牌变量后绕过口令还留在文件里。
- **会话只在内存**：令牌不写配置文件，重启即失效。自托管面板用这个取舍换来「配置文件被复制走也拿不到登录态」。
- **统计跨重载保留**：`stats.Collector` 由 runtime 持有并在重建时复用，改一次配置不会把计数清零。
- **端口而非路径前缀**：反代入口用独立端口而不是 `/前缀`。播放端只要把地址里的端口换掉，路径、客户端配置一律不动；代价是每个上游都要在 compose 里映射一个端口，这个代价换来的是「同一份客户端配置只改一个数字」。
- **旧配置自动升级而不是报错退出**：配置是严格解析的（未知字段直接报错），这在拼写错误上是对的，但换字段名时会让升级镜像的容器卡在重启循环里。`Upstream.Prefix` 因此保留为一个只读的过渡字段，`Config.migrate` 在加载后把它清掉并按管理端口往上分配一个空闲反代端口，`Migrated()` 让 main 把结果落盘，下次启动就是一份干净的新配置。
- **端口热增删**：`handlerFor(port)` 每次请求都从当前快照里取代理，所以增删上游只需要增删监听器，不必重启容器；`serving == false` 时 `acquirePorts` 空转，单元测试因此不会去抢真实端口。
- **管理端口不热改**：套接字无法在不中断连接的前提下重绑，`RestartRequired()` 把这个事实如实报给界面，而不是假装生效。

## 与 Audiobookshelf 侧的对应关系

被反代项目（`C:\Develop\AudioBookShelf`，只读参考）自身已支持 `.strm`，把指针目标在服务端代理出去。AetherLink 做的是把这层代理前移：

| 关注点 | Audiobookshelf 内部实现 | AetherLink |
| --- | --- | --- |
| 指针解析 | `server/utils/strmUtils.js` 的 `resolveStrmTarget` | `internal/strm`（读文件）+ `internal/strm.ParseURL`（上游已给直链）+ `internal/urlx` |
| 指针定位 | 指针路径就在本机，直接 `fs.readFile` | `internal/pathmap.Locate`：跨容器，挂载点可能不同名 |
| 音轨定位 | `SessionController.getTrack` 直接从内存里的会话取 `audioTracks[i].metadata.path` | 只能靠 API：`/api/session/:id` 无音轨 → 回退 `/api/sessions/open` → 用 `libraryItemId` 查条目按序号取路径 |
| 字节投递 | `proxyStrm` 服务端流式转发 | 默认 302，必要时才中继 |
| 根目录限制 | 库目录 + 固定 `/NetDisk` | 界面配置的 `strm_roots` |
| 私网放行 | STRM 链路绕过 SSRF 过滤 | `redirect.mode: private` / `allow_public_targets` |

因此上游继续负责扫库、元数据、章节与进度，AetherLink 只接管播放字节这一段。
