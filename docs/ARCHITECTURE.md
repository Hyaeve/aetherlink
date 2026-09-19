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
- **登录页零信息**：免鉴权的 `GET /aetherlink/api/bootstrap` 只返回版本号与密码长度下限，用来确认后端可达。它不回显账号名与凭据，也不回报 `defaultCredentials`——否则扫到端口的人等于被告知「这里 admin/password 就能进」。登录页的账号框因此是空的，要使用者自己输。「仍在用默认账号」的提醒与**当前账号名**都只在登录之后由 `/status` 与 `/config` 提供，设置页「登录账号」卡片用后者带出账号框。
- **账号修改**：`POST /aetherlink/api/account` 接收当前密码 + 新用户名 + 新密码（新密码留空表示只改用户名），成功后清掉 `default_credentials` 并 `RevokeAll()` 注销全部会话，前端随即回到登录页。
- **用户名比较宽松，密码严格**：`auth.VerifyLogin` 对用户名去空白且不区分大小写；用户名不匹配时仍走一次 PBKDF2 派生，使耗时与密码错误一致，不暴露「用户名是否存在」。老配置里没有 `username` 时按 `admin` 兼容。
- **根路径直达**：`adminRootHandler` 让 `GET /` 302 到 `/aetherlink/`，用户不必手敲后缀。管理端口不做反代，其余路径一律 404——端口填错时宁可立刻报错，也不要静默转发出一个难查的问题。

## 请求判定顺序

1. 请求落在哪个反代端口上，就是哪个上游（`runtime.handlerFor`，一端口一上游，路径不参与选择）。
2. Emby 系（`emby` / `fnos`）的 `/Items/:id/PlaybackInfo` 先原样请求上游，再把 STRM 媒体源压成 `SupportsDirectPlay=false` + `SupportsDirectStream=true` 并补 `DirectStreamUrl`，同时把 `TranscodeReasons` / `TranscodingReasons` 清成空数组；其余能力字段与 `TranscodingUrl` 全部保留（客户端必要时仍可回退转码）。压成 DirectStream 是无条件的：DirectPlay 会让客户端直连 `Path`（网盘 / OpenList 直链）绕开 AetherLink，既没有播放记录，也不受内网直链安全网保护，四档跳转模式会一起作废。上游那句「这个客户端不能直放」同样被推翻——它曾经由一个卡片开关决定是否听信（默认开），开关现已整个删掉，四档一视同仁；为什么删、代价是什么，见下方「Emby 的 302 起点是 PlaybackInfo」。飞牛影视同一个响应里还会顺手给 `MediaStreams` 补上非空字段，并在转发前把这条请求钉到 `/emby` 前缀下（`upstream.RequestPathRewriter`）——少了前缀飞牛返回的是单页应用 HTML，拿不到 `MediaSources` 就永远不 302。普通媒体源始终不修改。
3. 交给该上游的 `Match` 判断是否为媒体字节接口，不是则直接反代。未命中但路径看起来像播放请求（含 `/stream`、`/track/`、媒体扩展名等）时记一条 info 日志；Emby HLS 清单或分片会单独说明「分片本身不能 302，应重新播放以重新协商直放」。
4. 上游没有可用凭据、且该类型必需凭据（Audiobookshelf / Emby）→ 记为 `passthrough` 并反代（无法查询媒体信息）。飞牛影视属于「凭据可选」：它没有静态密钥，什么都没配也照常查询，不会落到这一支（见「飞牛的凭据是可选的」）。
5. 问上游 `MediaTarget`，按回答分三条路：
   - **已是直链**（Emby：`Protocol: Http` 或 `Path` 以 `http(s)://` 开头）→ 归一化后直接进入第 7 步，不读任何文件。
   - **是指针文件**（路径以 `.strm` 结尾，或 Emby 报的 `Container` 是 `strm`）→ `pathmap.Locate` 定位到容器内路径再读取。定位不到或读不到（`ErrPointerUnavailable`）→ 记为 `passthrough` 并反代，同时把原因写进日志与事件。
   - **普通文件**（`ErrNotStrm`）→ 记为 `passthrough` 并反代。
6. 指针指向容器本地文件 → `http.ServeContent` 直读（自动支持 Range/HEAD）。
7. 远程目标 → 按 `redirect.mode` 决定 302 还是中继；卡片「不中继的客户端」名单命中时一律优先 302（`never` 档也照发直链），例外只有内网直链 + 外网客户端那条安全网——302 发出去也没人接得住，只能中继，日志会写明名单这次没有出路。

中继在响应头上是**透明的**：上游的状态码、`Content-Length`、`Content-Range`、`Accept-Ranges`、`ETag`、`Last-Modified`、`Content-Disposition` 全部原样转发（只滤 hop-by-hop），因此回给播放器的一定是定长响应、不会退化成 chunked。这不是细节：`Content-Length` 一旦丢失，moov 在文件尾部的 MP4 就无法 seek（播放器要按总长度去取尾部），表现是「读了几百 KB 就断开」，而日志里只剩一行「客户端中断」。**`Content-Type` 同样原样转发——中继不改上游给的类型。** 这一条是被线上逼出来的：网盘 CDN 与移动云 EOS 对视频一律回中性类型 `application/octet-stream`，播放器拿到它会自己按内容嗅探；替它改写成 `video/x-matroska`、`video/mp4` 这类具体类型，等于凭空造出一个 302 没有的差异。实测那一例是：同一片走 302（播放器拿到 CDN 的中性类型）能播，走中继（拿到被改写的 `video/x-matroska`）只读了 7.1KiB、把 MKV 的 `Tracks`（HEVC 4K + 两条 ASS 字幕）读完就断开，两边可观测的差异只剩 host 与这一个头。只有上游**压根没给** `Content-Type` 时才由 AetherLink 补一个（罕见），顺序是**先字节、后扩展名**：`sniffContentType` 先看响应体开头的魔数（直链路径常常只有一串 ID、`Content-Disposition` 里的文件名也不可信——出现过文件名 `.iso`、字节是 MP4 的直链），认不出来才退回直链扩展名；而补的时候同样不能让扩展名压过字节，因为「文件名写 `.mkv`、内容其实是 MP4」在网盘上一样常见。纯音频扩展名（`.m4a` / `.m4b` / `.mp3` 等）是唯一例外：MP4 家族里有声书与视频共用容器头（品牌常是 `isom` / `mp42`），字节分不出音频，扩展名才是唯一线索，这类直接采信扩展名、连字节都不读。所以 **302 与中继交给播放器的响应必须逐项等价，`Content-Type` 也在内**——`internal/proxy` 的 `TestRelayWireResponseMatchesTheDirectLink` 走真实 TCP，把中继与同一条直链直连的七个响应头逐项对比。两边表现不同时，先把日志级别改成 `debug` 看一行 `中继交换明细`——它把客户端请求头、上游请求头、上游状态与全部响应头、以及回给客户端的头一次列全（凭据已打码），差异只可能藏在那里；确认响应确实等价之后，再怀疑客户端对 URL / 会话的差异。**「客户端读了一点就走」（转发量低于 64KiB）那一支不用开 debug**：它自己就会把客户端请求头与回给客户端的完整头写进日志，因为这种形态的成因分属两边——库里的容器名与真实文件不符，或者播放器自己判定这个文件放不了——而分辨它们唯一的证据就是双方的头。容器名对账这件事不只在上游没给类型时才做：上游回中性类型（`application/octet-stream`，网盘 CDN 的常态）时也会读一眼响应体开头的 16~128 字节（原样写回、不改写任何响应头、不为它补 `Content-Type`），否则「库里的容器名与真实文件不符」在日志里永远不可见。如果只有某一个播放器在中继下播不了、其它播放器与 302 都正常（实测 AfuseKt 就是这一类），那结论是播放器侧的事，中继这半边没有可改的东西——出路是卡片上的「不中继的客户端」名单：命中者单独拿直链，其余客户端继续中继。

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
- **Emby 的 302 起点是 PlaybackInfo，不是 HLS 分片**：客户端先根据 `PlaybackInfo` 决定直放或转码。一旦选中 `/hls1/main/*.ts`，每个请求只代表一段转码数据，不可能 302 到完整媒体文件。所以 PlaybackInfo 是唯一能改变字节去向的关口：四种跳转模式都要求客户端先回到 AetherLink 的 `/stream`——卡片决定的是字节去向（302 出去、由 AetherLink 自己中继，或按客户端来源二选一），客户端不回这条路，档位就等于没设。因此 STRM 源一律压成 DirectStream（`DirectPlay` 绝不能用——客户端会拿它直连 `Path` 绕开我们）。压之前它曾被上游的兼容性判定分成两半：`SupportsDirectPlay=true` 的照压，`false` 的留给上游转码；中间短暂地由卡片开关「无视上游的不可直放判定」来决定要不要连判定一起推翻，后来发现开关默认开＝与没有开关逐字节相同、只是多一个能把自己弄坏的旋钮，于是整个删掉——判定不再被采纳。代价是明确的：真正解不了原文件的客户端（网页端碰上 H.265，或只认上游 HLS 的播放器——实测 AfuseKt 拿到原始文件直链后读几十 KB、正好读完 MKV 的 `Tracks` 就断开）不再有「上游替你转码」这条退路，只能让它绕开 AetherLink 直连上游；而多数客户端既然还来要 `/stream`，说明它自认能播这个文件，此时替它拒绝才是更大的问题（用户实例：飞牛影视 + 公网跳转 + 内网播放，日志只剩一行「透传」，卡片上写的「内网客户端中继」等于没生效）。判定被推翻之后，紧接着那条 `/stream` 请求才会真的去解析直链并按客户端来源 302 或中继；若还沿用「听上游的」行为，它会就地透传，中继链路压根不启动。现在 `RewriteResponse` 是无条件接管，跳转模式只决定日志里那句「字节由 AetherLink …」怎么写（`forcedDelivery`），`fnos_test.go` 里四种模式正反成对钉住，`proxy_test.go` 另有一条走完 PlaybackInfo → `/stream` 的端到端用例（同一张卡片上公网客户端 302、内网客户端中继）。
- **内外网归属要能补自家网段**：`ScopeOfClient` 只按地址类型分类（RFC1918 / 回环 / 链路本地 / IPv6 ULA），而家用路由器下发给每台设备的常是运营商 IPv6 全局地址（`240e::` 这类 GUA）——按类型是公网，可它确实在局域网里：「公网跳转」于是把内网客户端统统 302 出去、「内网跳转」又漏掉同一批人（用户实例 2026-09-19：同一个播放器时好时坏，正是它有时用 IPv4、有时用 IPv6 连进来）。一条 TCP 连接只有一个对端地址，从 IPv6 连接里问不出客户端的 IPv4，**这件事没有可以自动推断的解**，所以留了一条显式配置 `redirect.intranet_cidrs`（**只有配置文件，界面上没有入口**：自动识别那一层已经覆盖了绝大多数部署，再在设置页摆一个网段输入框，只会让人以为这里必须填），`ScopeOfClient(client, intranet...)` 命中即按内网处理，其余判断一字不动。`0.0.0.0/0` 与 `::/0` 在 `Validate()` 里当场拒绝——那等于让内外网判断整体失效，该用跳转模式表达。带 zone 的地址（`fe80::1%eth0`）由 `ClientAddress` 统一去掉 zone 后再判：netip 的 `Prefix.Contains` 对带 zone 的地址**恒为 false**，留着它，配置的网段就匹配不上那台设备；`proxy.clientAddress` 直接复用这一个解析，避免「日志里的地址」与「判归属的地址」各说一套。
  **但它还有一解**：不必从客户端地址反推它的 IPv4——只要客户端与 AetherLink 本机落在同一个网段，它就一定在同一个局域网里。于是再加一层 `resolver/localnet.go`：读本机 up 且非回环网卡的 IPv6 网段（`LocalNetworkPrefixes`），命中的客户端同样按内网处理。它自动跟随运营商换前缀（本机地址变了，网段跟着变），用户什么都不用维护。三处取舍：① **只收 IPv6**——IPv4 私网已由内置规则覆盖，而把公网 IPv4 子网也算进来会在 VPS 上把同一 /29 里的邻居判成内网；② **前缀长度超过 /64 的按所在 /64 计**（Windows 的 SLAAC 地址常报 /128，按单机看待会漏掉整条链路）；③ **按网卡名排除虚拟 / 隧道接口**（`docker0`、`br-<12 位十六进制>`、`tun` / `tap` / `wg` / `tailscale` / `zt` 等）——那些网段里的对端是容器或远端 VPN 节点，算成内网会让「公网跳转」下的远程客户端被无谓地中继；真实的局域网网桥叫 `br0`，不在排除之列。（别高估这一条：Tailscale 那类隧道发的是 ULA，本来就被内置规则判成内网，那是既有行为；它挡住的是用 GUA 网段的隧道与容器网桥。）这一层是推算出来的，所以它必须出声：`ScopeOfClientWithReason` 会把「，与本机为同一网段 2409:…::/64」带进日志，检测结果也随设置接口下发、显示在「网络地址」卡片上（只读的 `localNetworkPrefixes`，请求里带上它不会被采纳）。容器看不到局域网网段（bridge 网络）时那一行为空，正好指向配置文件里的 `intranet_cidrs`——它在界面上没有入口，是留给那种部署的唯一后路。**日志那一句的顺序也是定死的一部分**：先说卡片选了什么模式、客户端 IP 是谁、它为什么算内网，最后才是「改由 AetherLink 中继」。先前写成「按 302 策略不跳转，改由 AetherLink 中继：<一长串解释>」，结论在前、理由在后，用户得倒着读才能对上自己配的东西。
- **基础路径不能重复拼接**：配置的 Emby / 飞牛影视地址可能已经带 `/emby`，客户端请求也可能以 `/emby` 开头。反代 `joinPath` 会先判断请求是否已含基础路径，直放 URL 也会优先复用 Emby 原本的路由前缀并折叠相邻重复段，避免生成 `/emby/emby/Videos/...`；`fnosAPIPrefix` 同样在地址已以 `/emby` 结尾时不再补前缀。
- **条目查询三种路由要排序，不能只写一条**：`/Items?Ids=a,b`（集合）、`/Items/{id}`（单项）与 `/Items/{id}/PlaybackInfo`（播放协商）都能给出条目的媒体源，但各家实现的支持面差得很远——Emby 三条都有，飞牛**只**有 PlaybackInfo 那条：`/Items` 与 `/Items/{id}` 都回整页单页应用的 HTML 并带上 HTTP 200，**不返回 404**。这时 JSON 解码在第一个 `<` 上失败，抛出来的是 `invalid character '<' looking for beginning of value`：既看不出是哪条请求，也看不出上游其实回了网页。因此 `embyProvider.fetchItem` 按 `itemLookupOrder()` 依次试并汇总各自的失败原因：飞牛由 `preferItemByID = true` 排成「PlaybackInfo → 单项 → 集合」（PlaybackInfo 是它唯一确定可用的——客户端的播放流程一直在走它，AetherLink 也一直在改写它的响应；排在首位既省掉两次注定失败的请求，也保证解析能落地），Emby 保持「集合在前」（一次调用同时给出元数据与媒体源，能用就不多打）。PlaybackInfo 给的是媒体源而非条目元数据，所以 `fetchItemBy` 会把 `MediaSources[0].Path` 一并取为 `item.Path`，让「没有可用媒体源时退回读指针文件」那条兜底不断。配套两件事：`decodeJSONResponse` 解码失败时把**实际请求地址**、`Content-Type`、是否携带令牌与响应开头片段一起写进错误（写 endpoint 参数会漏掉 `apiPrefix`，于是「GET /Items/eb35…」看上去像前缀没补，照着它排查只会走偏），`looksLikeHTML` 负责识别「这其实是网页」。
- **播放器的令牌要能一路送到解析调用上**：播放器本来就向上游登录过，它手上的令牌必定是上游刚认可的身份，而且客户端请求里就带着（`X-Emby-Authorization` 的 `Token="…"`、简写的 `X-Emby-Token`，或 `?api_key=`）。因此 `proxy.serveMedia` 用 `upstream.WithClientCredentials` 把这份身份放进上下文，`apiClient.credentials` 在**上游什么都没配**时借它一用——飞牛不填账号密码也能完成「回头查上游」，参考项目 LitePan 取条目时直接复制原请求凭据，是同一个道理。配了就还是以配置的为准：那是管理员身份，权限比播放器更完整（Emby 的 `/Users` 只有管理员读得到）。还有一处兜底：配置的账号密码失效（改过密码、账号被停用）时登录会失败，此时改用播放器带上来的令牌——播放解析不该因为一份过期的配置而整个失败。
- **前缀改写只给播放协商开一个口子**：飞牛的 API 只挂在 `/emby` 下，而网页控制台（`/web/...`）、封面（`/Items/:id/Images/...`）与字节接口都在根路径。因此 `RequestPathRewriter` 只在 `PlaybackInfo` 上门禁补前缀，其余路径一律原样送达——无差别加前缀会把界面整个打挂，这比「少 302」更难排查。反向断言（其余路由必须不被改写）与正向断言（少前缀时仍能 302）在 `internal/proxy/fnos_test.go` 里成对存在。
- **飞牛的凭据是可选的，令牌只活在内存里**：飞牛影视没有 Emby 控制台里那种静态 API 密钥，它的接口只认客户端登录换来的令牌——而那份令牌由播放器自己登录取得、随请求转发，AetherLink 只是把请求转过去。所以「什么都不填」就能完成 302：`PlaybackInfo` 改写时顺手缓存下来的媒体源足够回答紧接着的 `/stream`。超出这个 10 分钟窗口、或客户端直接请求下载入口时，AetherLink 才需要自己回头查一次上游，那份查询才需要凭据。因此账号密码被设计成**可选项而非必填**：填了就用 `POST /emby/Users/AuthenticateByName` 换令牌（复用 6 小时，只存在内存里，不落盘）；没填也不要紧——播放器自己的令牌会随请求送进来，AetherLink 直接借用（见「播放器的令牌要能一路送到解析调用上」）。撞上 401/403 视为令牌被吊销，丢掉缓存重登一次再重试——不这么做，一次令牌过期会表现成「上游坏了」。`HasCredentials()` 对飞牛恒为真，否则代理层会退化成纯反代，播放请求永远拿不到 302。既然凭据只会用在「回头查上游」这一支上，那么**试连**也必须先填账号密码才成立：没凭据时飞牛连服务信息都不给，失败说明不了地址对不对，等于白试——所以界面在发请求前就拦下并直说原因，后端 `Ping` 的兜底错误同样写成「需要先填写登录账号与密码」，而不是把上游那句 `X-Emby-Authorization is missing` 原样抛出来（它看着像「没配鉴权」，极难定位）。
- **已保存的凭据平时是圆点，看明文点输入框右侧的眼睛**：列表接口 `GET /upstreams` 只回报 `hasApiKey` / `hasPassword` 两个布尔，秘密真正的值只能靠调 `GET /upstreams/{name}/credentials` 取（同样走会话鉴权，并留一行审计日志）。编辑窗口在 `onMounted` 里为「已存过该凭据」的上游调一次，把值填进字段并保持 `type="password"`——所以用户看到的就是一串圆点，而不是一个空框。这个选择是刻意的：空框必须配一句「留空即保留原值」的隐藏规矩，用户得先记住规则才能判断「框里没有」到底是什么意思；圆点没有这个问题，**框里是什么就存什么，清空才等于删除**。与之配套的三件事：1) 加载成功时把内部标记 `keepApiKey` / `keepPassword` 置假，让「清空字段」能真的表达删除；2) 加载期间字段 `disabled`，否则请求返回时会盖掉用户刚敲进去的字；3) 加载失败时标记保持为真、并提示「直接保存会保留原值」，这样一次网络抖动不会把配置里的密钥抹掉。换服务端类型（如 Emby → 飞牛影视）不需要清理字段：`buildPayload` 本来就按类型决定送哪一项，另一个字段的值到不了后端。
- **自动定位而非要求手写映射**：`pathmap.Locate` 把上游路径逐段剥前缀，在配置根目录、映射目标与常见挂载点下 stat。白名单校验从不放宽——白名单外的候选连 stat 都不做。凡能自动化的就不要求用户填表。
- **不缓存指针内容按 mtime**：文件系统 mtime 精度不足，同一 tick 内两次写入无法区分。缓存键是媒体引用（上游 + 条目 + 文件），TTL 到期后重新读取指针，路径白名单校验永远在缓存之外无条件执行。
- **改写缓存没命中时，必须留下一条 info 日志**（`PlaybackInfo 改写缓存未命中（…，播放器请求带来的令牌：有/无）`）：`RewriteResponse` 会把改写过的媒体源按「条目 + 上游给的 Id」缓存 10 分钟，命中时解析**根本不问上游**——这是「点开就能播」的常态。没命中时就全看那次回头查，而它在飞牛上要凭据：卡片没填账号密码时只能借用播放器自己带在请求上的令牌，而我们生成的 `DirectStreamUrl` 里不含令牌，带不带全看播放器。两条路的分叉点以前一声不吭，界面与日志里只剩最后那行「透传」，于是「同一片、什么都没改、重试几次又好了」永远无法解释（写「令牌：无」的那次必然失败，重试之所以会好，是因为客户端重做了一遍播放协商、缓存又被写热）。日志只写有/无，绝不写令牌本身。
- **缓存查询要给「不带 `MediaSourceId` 的请求」留退路**：缓存键含媒体源 Id，但播放器沿用上一次协商结果、或自己拼 `/videos/{id}/stream` 时不带它就是常态。所以除了带 Id 的精确命中，还要有两条退路（带 Id 却对不上 → 退回空 Id 那条；请求完全没带 Id → 该条目下**恰好只缓存了一条**时用它）。**多条候选时绝不猜**：选错媒体源就是选错文件，那种情况宁可回头问上游。少了这条退路，这类客户端会永远命中不了缓存、每次都落到上面那条日志描述的最坏路径上。
- **并发去重**：播放器 seek 时会并发发起多个 Range 请求，`resolver` 用 inflight map 让同一轨道只打一次上游 API。
- **不设 HTTP 读写超时**：媒体流是长连接，只设 `ReadHeaderTimeout` 与 `IdleTimeout`，取消由请求上下文驱动。
- **跳转预检失败不致命**：`follow_upstream_redirects` 预检失败时退回原始 URL，让播放器自行协商，而不是让播放直接失败。
- **UA 透传**：部分网盘直链与 User-Agent 绑定，默认转发客户端 UA，仅在客户端未提供时使用 `fallback_user_agent`；同一次播放的上游 API、直链预检和实际下载始终使用同一个非空 UA。
- **UA 屏蔽**：安全设置中的 UA 关键词列表按大小写不敏感的包含关系匹配；命中后在反代入口直接返回 403，不再转发到上游。关键词按行填写，不再支持 `/xxx/` 包裹语法。
- **「不中继的客户端」名单挂在卡片上，而不是全局**：中继与 302 交给播放器的字节、响应头已逐项等价（见上），但客户端选路仍有差异——实测 AfuseKt 只在拿着直链自己取流时能播，走中继读几 KB 就断开，而同一台设备上的 CapyPlayer、爆米花走中继正常，打开反代工具的「自动反代重定向」（让流量绕回直链）后它也能播。这是客户端自己的差异，服务端没有修法，所以把选择权交回用户：名单里的 UA（大小写不敏感的子串，与屏蔽 UA 同一套匹配）即使卡片选「始终中继」也走 302。挂在卡片上是因为它只在「这张卡片会中继」时才有意义，与跳转模式同处一地；名单为空时行为与从前逐字节一致。优先级上，内网直链 + 外网客户端的安全网压过名单——302 发出去也没人接得住。
- **签名直链按 `t` 缓存**：Emby 系直链若带数字 `t` 参数，解析缓存只保留到该直链到期；没有有效 `t` 时固定缓存 2 小时，不提供自定义入口。缓存命中和首次获取都会把当前有效期写入容器日志与播放流水；Audiobookshelf 固定缓存 15 分钟。
- **直链缓存跨重启保留**：解析结果以原子替换方式写入配置目录下的 `direct-links-cache.json`，启动时只恢复尚未过期的条目；缓存键仍包含上游、媒体引用和实际使用的 UA，因此不同 UA 不会复用同一条签名直链。清空缓存时同步清空磁盘文件。
- **可选布尔用指针**：`forward_user_agent` 与 `allow_public_targets` 默认为真，若用普通 `bool`，一份省略该键的手写配置会被静默当成 `false`。用 `*bool` 才能区分「没写」与「写了 false」。
- **配置即唯一真相**：不做「环境变量优先于文件」的双轨制。除了少数排障用的启动期覆盖，配置只有一个来源，网页所见即磁盘所存。启动期覆盖（`AETHERLINK_PORT` / `AETHERLINK_LISTEN` 的监听端口、`AETHERLINK_ADMIN_TOKEN` 的应急令牌）只作用于本次启动，`Save` 会把文件里原本的值写回去——否则「临时加了个变量」会变成永久改动：删掉端口变量后监听端口仍被改过，compose 映射的 `5151` 就再也进不去；删掉令牌变量后绕过口令还留在文件里。
- **会话只在内存**：令牌不写配置文件，重启即失效。自托管面板用这个取舍换来「配置文件被复制走也拿不到登录态」。
- **401 只在一处判定，且不落到界面上**：前端的 `request()` 把「带着令牌仍收到 401」统一认定为会话失效（容器重启、令牌到期、账号改过），清掉本地令牌并回调 App 回登录页，各页面的错误横栏一律走 `visibleMessage()`（会话失效返回空串）。否则每个页面都要各判一次 401，而后端的 401 文案（「会话无效或已过期，请重新登录」）会先被当成普通错误挂在某个页面上，等到主状态轮询发现才消失。登录口令错误的 401 **不带令牌**，因此不走这条路，仍照常提示「账号或密码不正确」。
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
