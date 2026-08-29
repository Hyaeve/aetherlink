# AetherLink 以太链接

[![ci](https://github.com/Hyaeve/aetherlink/actions/workflows/ci.yml/badge.svg)](https://github.com/Hyaeve/aetherlink/actions/workflows/ci.yml) [![docker](https://github.com/Hyaeve/aetherlink/actions/workflows/docker.yml/badge.svg)](https://github.com/Hyaeve/aetherlink/actions/workflows/docker.yml)

反代 Audiobookshelf / Emby 的 x86 Docker 服务：拦截播放请求，读取库里的 `.strm` 指针，把播放器直接 **302** 到真实媒体地址，音频与视频字节不再经过媒体服务器。

前端 Vue 3 + Vite，后端 Go，最终产物是单个静态二进制（前端资源用 `go:embed` 打进二进制）。

**所有配置都在网页上完成。** compose 只需要一个 `./config` 卷：管理账号、上游地址与 API 密钥、反代端口、302 策略、路径映射全部在管理界面填写，保存即生效、无需重启容器。唯一需要动 compose 的地方是把上游的反代端口映射出去。

## 工作原理

```
播放器 ──▶ AetherLink:5152 ──▶ (非媒体请求) ──▶ Audiobookshelf:13378
                    │
                    ├─ 命中媒体接口 ──▶ 用 API 密钥问上游「这个文件在哪」
                    │                  ↓
                    │              路径映射 → 读取 .strm → 归一化 URL
                    │                  ↓
                    └───────────── 302 Location: http://10.0.0.31:19527/d/xxx.m4a
```

**一个上游一个端口。** 每个上游在 AetherLink 上独占一个反代端口，路径与上游完全一致——
播放端只需要把地址里的端口从媒体服务器端口换成 AetherLink 的反代端口，别的什么都不用改。
管理界面自己占 5151，且只服务管理页与管理 API，不做反代。

- **拿到账号**：使用上游项目自带的 API 密钥（Audiobookshelf 的 Key 就是一枚 JWT，可作 Bearer 使用；Emby 用 `X-Emby-Token`/`api_key`），因此 AetherLink 能以该账号权限查询书库、条目和文件真实路径。
- **只拦截字节投递接口**，其余请求（Web UI、封面、元数据、进度同步、HLS 转码）原样反代，客户端行为不变。
  - Audiobookshelf：`/api/items/:id/file/:ino`、`/api/items/:id/file/:ino/download`、`/public/session/:id/track/:index`
  - Emby：`/Videos|Audio/:id/stream(.ext)`、`/Items/:id/Download`
- **非 `.strm` 文件自动透传**给上游，不影响普通有声书和普通影片。
- **URL 归一化**是关键：STRM 生成器写进去的往往不是合法 URL。AetherLink 会补齐百分号编码，同时保留已编码序列，因此下面三种主流形态都能直接 302：
  - 115 pick code：`http://10.0.0.31:19527/d/bi6jeznun2rvu88v6.m4a?/001.总序.m4a`（查询串里的显示文件名会被识别为 `filename`）
  - openlist 中文路径带空格：`http://10.0.0.31:25244/d/移动云盘/.../白色巨塔 (2003) S01E01.再读.mkv`
  - openlist 已编码路径含 `%26`、`%20`：保持原样不二次编码

## 快速开始

```bash
mkdir -p config
docker compose up -d
```

浏览器打开 `http://宿主机:15151`（会自动跳到管理界面，不用手敲 `/aetherlink/`）：

1. 用默认账号 **admin / password** 登录。
2. 进入「反代上游」，点虚线的「添加上游」卡片：选服务端类型（Audiobookshelf 或 Emby），填上游地址、反代端口和对应的 API 密钥——Audiobookshelf 填「设置 → 用户 → 该用户的 API Token」，Emby 填「控制台 → 高级 → API 密钥」。保存前可以点「试连」当场确认密钥是否可用。
3. 到「设置 → 管理账号」改掉默认账号密码。密码只以 PBKDF2-SHA256 的派生值存进 `config/config.yaml`，明文不落盘。
4. 「反代端口」表单里已预填一个空闲端口（默认 5152），把它加到 compose 的 `ports` 里再 `docker compose up -d`。
5. 播放端只改端口：`http://宿主机:13378` → `http://宿主机:5152`，路径不用动。

> 端口暴露在局域网时请务必先改掉默认账号：任何能访问这个端口的人都能用 admin / password 登录并读到上游 API 密钥。登录后界面顶部与设置页会一直提示，直到你改掉为止。

配置文件不存在时容器会自动创建，不需要事先准备任何文件（入口脚本以 root 启动，会先把 `/config` 的属主改成服务用户 10001，再降权运行主进程）。`deploy/config.example.yaml` 只是字段说明。

### 挂载说明

```yaml
volumes:
  - ./config:/config
```

默认只需要这一个卷。`.strm` 里写的是 `http://...` 直链时（115 pick code、openlist 都是这种），AetherLink 只需要读到 `.strm` 文件本体——而文件路径是通过上游 API 拿到的，因此**只有当 AetherLink 与上游不在同一台机器上、或 `.strm` 指向的是容器内本地文件路径**时，才需要额外挂载媒体目录，例如：

```yaml
  - /vol1/1000/NetDisk/115-Strm/Set/Read:/audiobooks:ro
  - /vol1/1000/NetDisk:/NetDisk:ro
```

挂载时让**容器内路径与上游保持同名**，这样可以省掉路径映射。确实无法同名时（例如上游是 `/audiobooks`，AetherLink 只挂了 `/NetDisk/115-Strm`），在「反代上游」里右键卡片 → 详细编辑，在表单里补一条路径映射做前缀重写。

容器以非 root 用户（uid 10001）运行，被挂载的目录需对该用户可读。

关于 Audiobookshelf 那份 compose 里的 `./podcasts:/podcasts`：那是 Audiobookshelf 的播客库目录，用来存它自己下载的播客音频。AetherLink 不需要它——播客是真实文件，走透传交给上游处理。

本地开发：

```bash
cd web && npm install && npm run build   # 产物写入 internal/web/dist
cd .. && go run ./cmd/aetherlink -config ./config.yaml
```

前端热更新：`cd web && npm run dev`（Vite 会把 `/aetherlink/api` 代理到 `127.0.0.1:5151`）。

## 容器起不来怎么排查

先看日志，`restart: always` 会把启动失败伪装成「一直重启」：

```bash
docker logs AetherLink
docker inspect -f "{{.State.ExitCode}} {{.State.Error}}" AetherLink
```

| 日志里的现象 | 原因与处置 |
| --- | --- |
| `加载配置失败: ... permission denied` | 入口脚本会自动把 `/config` 的属主改好，用当前镜像不该再出现。仍出现说明你在 compose 里加了 `user:`（脚本没有改属主的权限），在宿主执行 `chown -R <那个uid>:<那个gid> ./config` 即可。 |
| `[entrypoint] 错误：/config 必须可写` | 卷被挂成了只读。检查 compose 的 `volumes` 有没有多写结尾的 `:ro`。 |
| `解析 /config/config.yaml 失败` | 手改过配置文件且 YAML 写坏了，或写了程序不认识的字段（配置是严格解析的）。改回去，或直接删掉该文件让程序重建。旧版的 `prefix` 字段会被自动升级成 `listen_port`，不会再报这个错。 |
| `redirect.mode "xxx" 必须是 always、private 或 never` | 手改配置或环境变量 `AETHERLINK_REDIRECT_MODE` 填了非法值。 |
| `listen tcp :5151: bind: address already in use` | 管理端口被占，通常是某个上游的反代端口和它撞了。管理端口固定 5151，把上游端口改成别的。 |
| `端口 xxxx 无法监听（可能已被其他程序占用）` | 该反代端口在容器内已被占用（多半是两个上游撞了端口，或与管理端口 5151 冲突）。在界面上改成别的端口保存即可，原有上游不受影响。 |
| 播放端连不上反代端口 | 端口没在 compose 的 `ports` 里映射出去。加一条 `- 5152:5152` 再 `docker compose up -d`。 |
| 没有任何日志、`ExitCode` 是 127 或 `exec format error` | 镜像架构不对。本项目只发 `linux/amd64`，ARM 设备（树莓派、某些 NAS）跑不了。 |

### 配置目录的属主

默认不需要管：容器以 root 进入入口脚本，脚本把 `/config` 的属主改成服务用户（uid 10001）后降权运行主进程。属主实在改不动时（部分 NAS 的 SMB/NFS 挂载、带强制 ACL 的存储池）脚本会退回以 root 运行，宁可让你进得去界面，也不陷入重启循环。

想让配置文件归某个特定用户，加两个环境变量即可，仍由脚本自动完成：

```yaml
    environment:
      - PUID=1000
      - PGID=1000
```

只有当你坚持用 compose 的 `user:` 时，脚本才没有改属主的权限，需要自己在宿主准备好目录：

```bash
mkdir -p config && sudo chown -R 1000:1000 config
```

## 管理界面

| 页面 | 作用 |
| --- | --- |
| 反代上游 | 每个上游一张卡片，卡片直接显示名称、服务端类型、`地址:端口`、`上游端口 → 反代端口`与状态（缺密钥 / 已停用 / 端口未监听）。左键卡片打开详细编辑窗口，右键弹出菜单（详细编辑 / 测试连接 / 停用启用 / 删除）。保存立即生效。 |
| 日志 | 内存环形缓冲的运行日志，按级别过滤。 |
| 设置 | 302 跳转策略、缓存、日志级别、修改管理账号。 |

界面是莫兰迪低饱和浅色主题，左侧一列图标做页面切换，点顶部的按钮可把侧栏展开成图标 + 文字（状态记在浏览器本地，刷新后保持）。

### 配置项说明

| 项 | 说明 |
| --- | --- |
| 跳转模式 | `always` 全部 302；`private` 仅内网目标 302，公网目标中继；`never` 全部中继。 |
| 先跟随上游 302 | 播放前把跳转链走完，把最终直链交给播放器。适合 115 这类二次跳转后端，代价是首次播放多一次预检。 |
| 转发播放器 User-Agent | 部分网盘直链与 UA 绑定时必须开启；播放器未带 UA 时使用回落 UA。 |
| 解析缓存 TTL | 播放器 seek 会反复请求同一轨道，缓存能大幅减少上游 API 调用。 |
| 反代端口 | 该上游在 AetherLink 上独占的端口。播放端把地址里的上游端口换成它，路径保持原样。改动后要在 compose 的 `ports` 里同步映射。 |
| 服务端类型 | 选 Audiobookshelf 就填 Audiobookshelf 的 API Token（设置 → 用户 → 该用户的 API Token）；选 Emby 就填 Emby 的 API 密钥（控制台 → 高级 → API 密钥）。 |
| 路径映射 | 上游看到的媒体路径 → AetherLink 容器内路径。两侧挂载一致时留空。 |
| STRM 允许根目录 | `.strm` 指向本地文件时允许读取的目录白名单，防止指针被用作任意文件读取入口。 |

### 环境变量

正常使用不需要任何环境变量。仅保留几个容器级开关：

| 变量 | 用途 |
| --- | --- |
| `TZ` | 时区，影响日志时间戳。 |
| `AETHERLINK_CONFIG` | 配置文件路径，镜像内已设为 `/config/config.yaml`。 |
| `AETHERLINK_ADMIN_TOKEN` | **应急令牌**，可绕过账号登录。仅在忘记管理密码时临时加上，排障后请移除。 |
| `AETHERLINK_LISTEN`、`AETHERLINK_LOG_LEVEL`、`AETHERLINK_REDIRECT_MODE`、`AETHERLINK_FOLLOW_REDIRECTS` | 启动期覆盖，用于排障。 |

忘记密码的另一种恢复方式：删掉 `config/config.yaml` 里的整个 `auth:` 段并重启容器，AetherLink 会重新写入内置的 admin / password，上游配置不受影响。

## 安全说明

- 管理界面有独立账号，与上游账号无关。首次启动写入内置的 admin / password，请尽快在设置页改掉。未登录访问管理 API 一律返回 401；只有 `/aetherlink/api/health` 与 `/aetherlink/api/bootstrap` 免鉴权，后者只回版本号与密码长度下限——不回显账号、凭据，也不透露是否仍在用默认账号，登录页因此不会给扫端口的人任何线索。
- 会话令牌只存在内存里，容器重启后全部失效；修改账号或密码会立即注销所有会话。
- 配置接口只回报 `hasApiKey` 布尔值，不回显上游密钥，也不回显任何密码材料。`config.yaml` 以 0600 权限原子写入。
- `.strm` 指向容器本地文件时，目标必须落在「STRM 允许根目录」内且必须是普通文件。
- 播放侧的鉴权仍由被反代的 Audiobookshelf / Emby 负责。不要把 AetherLink 直接暴露到公网而只依赖上游鉴权，建议放在内网或前置反代 + HTTPS。
- 302 的 Location 是可直接访问的媒体直链，等同于把内网 STRM 后端地址交给客户端；不希望暴露内网地址时把跳转模式设为 `never`。

## 已知取值边界

- Audiobookshelf 的 `/public/session/:id/track/:index` 走的是「打开会话」接口，只有会话属主或管理员可读。若配置的 API 密钥不属于管理员，该路径会退化为普通反代（普通播放走 `/api/items/:id/file/:ino`，不受影响）。
- Emby 的 HLS/转码分段路由不拦截：转码必须由 Emby 自己读取媒体完成。
- 中继模式转发 `Range`、`If-Range`、`Content-Range`，seek 行为与直连一致。
- 管理端口无法热改：在配置里改了 `server.listen` 之后界面会提示需要重启容器。上游的反代端口可以热改热增删，但新端口要在 compose 里映射出来才能从外部访问。

## 测试

```bash
go test ./...     # URL 归一化、路径映射、指针解析、302 行为、口令与会话、配置热重载、管理 API 鉴权
cd web && npm run build
```

## 镜像构建

镜像由 GitHub Actions 构建，只出 **linux/amd64（x86_64）** 一个平台：

| 工作流 | 触发 | 产物 |
| --- | --- | --- |
| `.github/workflows/ci.yml` | push 到 main、PR、手动 | `gofmt` / `go vet` / `go test`，以及 `npm run build` |
| `.github/workflows/docker.yml` | push 到 main、打 `v*` 标签、手动 | 推送镜像到 `ghcr.io/hyaeve/aetherlink`，并起容器跑一次健康检查冒烟 |

标签规则：main 分支出 `latest` 与 `main`；打 `v1.2.3` 标签时额外出 `1.2.3`、`1.2`、`1`；每次构建都带一个 `sha-xxxxxxx`。二进制里的版本号来自 `VERSION` 构建参数（`-X main.version=`），容器内 `/aetherlink --version` 可查。

首次推送后需要在仓库 Settings → Actions → General 里确认 workflow 有 `packages: write` 权限（工作流已声明），GHCR 包默认为私有，公开使用请在包页面改成 public。

本地构建（需要 Docker）：

```bash
docker build --platform linux/amd64 --build-arg VERSION=local -t aetherlink:local .
```
