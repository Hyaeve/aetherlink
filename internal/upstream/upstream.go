// Package upstream wraps the media servers AetherLink reverse proxies.
//
// A provider does three things: it recognises which proxied requests are media
// deliveries worth intercepting, it resolves the upstream-side filesystem path
// of the media behind such a request using the upstream API key, and it exposes
// enough library browsing for the AetherLink UI to show a STRM library.
package upstream

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/aetherlink/aetherlink/internal/config"
	"github.com/aetherlink/aetherlink/internal/logx"
	"github.com/aetherlink/aetherlink/internal/pathmap"
)

// RefKind labels the shape of an intercepted media request.
type RefKind string

const (
	RefLibraryFile  RefKind = "library-file"
	RefSessionTrack RefKind = "session-track"
	RefStream       RefKind = "stream"
)

// MediaRef identifies the media behind an intercepted request.
type MediaRef struct {
	Kind          RefKind
	ItemID        string
	FileID        string
	MediaSourceID string
	SessionID     string
	TrackIndex    string
}

// CacheKey is a stable identity for the resolved-target cache.
func (r MediaRef) CacheKey(upstreamName string) string {
	return strings.Join([]string{upstreamName, string(r.Kind), r.ItemID, r.FileID, r.MediaSourceID, r.SessionID, r.TrackIndex}, "|")
}

// Library is a browsable upstream library.
type Library struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	MediaType string `json:"mediaType"`
	Provider  string `json:"provider"`
}

// Item is one book, audiobook or video in a library.
type Item struct {
	ID        string  `json:"id"`
	Title     string  `json:"title"`
	Author    string  `json:"author,omitempty"`
	LibraryID string  `json:"libraryId,omitempty"`
	MediaType string  `json:"mediaType,omitempty"`
	NumFiles  int     `json:"numFiles"`
	NumStrm   int     `json:"numStrm"`
	Duration  float64 `json:"duration,omitempty"`
	CoverURL  string  `json:"coverUrl,omitempty"`
}

// File is one playable file inside an item.
type File struct {
	ID       string  `json:"id"`
	Index    int     `json:"index"`
	Filename string  `json:"filename"`
	Path     string  `json:"path"`
	Ext      string  `json:"ext"`
	Size     int64   `json:"size"`
	Duration float64 `json:"duration,omitempty"`
	IsStrm   bool    `json:"isStrm"`
}

// MediaTarget 是上游对「这个播放请求背后是什么」的回答。
//
// 两种媒体服务器给出的答案形态不同，必须区分开：
//   - Audiobookshelf 只报告 .strm 指针文件在它那边的路径，指针内容要由
//     AetherLink 自己去文件系统里读，因此必须挂载媒体目录。
//   - Emby 在扫库阶段就把 .strm 读掉了，MediaSources[].Path 直接是指针里的
//     那条 http 直链（Protocol 为 Http）。这种情况下 AetherLink 不需要看到
//     任何文件，拿到 URL 就能 302。
type MediaTarget struct {
	// Path 是上游侧的文件系统路径，可能是一个 .strm 指针。
	Path string
	// URL 在上游已经把指针解析成直链时给出，此时 Path 只用于日志。
	URL string
	// Container 是上游报告的容器格式（Emby 对 strm 会给 "strm"），
	// 用来在路径看不出扩展名时仍能判断这是不是指针媒体。
	Container string
}

// IsDirectURL 报告上游是否已经给出了可直接 302 的地址。
func (t MediaTarget) IsDirectURL() bool { return strings.TrimSpace(t.URL) != "" }

// Describe 给日志用：优先显示直链，其次显示上游路径。
func (t MediaTarget) Describe() string {
	if t.IsDirectURL() {
		return t.URL
	}
	return t.Path
}

// Provider is implemented by each supported media server dialect.
type Provider interface {
	Name() string
	Type() config.UpstreamType
	// ListenPort is the AetherLink container port this upstream is served on.
	ListenPort() int
	BaseURL() *url.URL
	Mapper() *pathmap.Mapper
	Transport() http.RoundTripper
	HasCredentials() bool

	// Match reports whether the request should be intercepted for STRM
	// resolution rather than proxied verbatim.
	Match(request *http.Request) (MediaRef, bool)
	// MediaTarget resolves what the referenced media actually is: either an
	// upstream-side path (Audiobookshelf) or an already resolved direct URL
	// (Emby, which reads .strm files itself at scan time).
	MediaTarget(ctx context.Context, ref MediaRef) (MediaTarget, error)

	Ping(ctx context.Context) (string, error)
	Libraries(ctx context.Context) ([]Library, error)
	Items(ctx context.Context, libraryID string, limit, page int, search string) ([]Item, int, error)
	ItemFiles(ctx context.Context, itemID string) (Item, []File, error)
	// PlaybackPath returns the upstream path used to stream a specific file, so
	// the UI can build a direct AetherLink play URL.
	PlaybackPath(itemID, fileID string) string
}

type userAgentContextKey struct{}

// WithUserAgent 将本次播放使用的 UA 放入上下文，供上游 API 请求复用。
func WithUserAgent(ctx context.Context, userAgent string) context.Context {
	userAgent = strings.TrimSpace(userAgent)
	if userAgent == "" {
		userAgent = "AetherLink"
	}
	return context.WithValue(ctx, userAgentContextKey{}, userAgent)
}

func contextUserAgent(ctx context.Context) string {
	userAgent, _ := ctx.Value(userAgentContextKey{}).(string)
	userAgent = strings.TrimSpace(userAgent)
	if userAgent == "" {
		return "AetherLink"
	}
	return userAgent
}

type clientCredentialsContextKey struct{}

// clientIdentity 是播放器请求里带上的 Emby 身份：令牌，以及它所属的用户 ID。
// 两者通常都有（Emby 客户端标准形态），也可能只有其一，所以分开存。
type clientIdentity struct {
	Token  string
	UserID string
}

// WithClientCredentials 把「发起这次请求的播放器自己带的 Emby 身份」放进上下文。
//
// 播放器本来就已经向上游登录过了，它手上的令牌与 UserId 必定是该上游认可的。
// AetherLink 回头查条目时优先复用它，于是不必另配账号密码，也省掉一次登录往返，
// 还省掉一次 /Users —— 参考项目 LitePan 取条目时正是把原请求的
// X-Emby-Authorization / api_key 复制过去的。
//
// 上游不认这枚令牌（被吊销、换了密码）时并不会卡死：配置里的静态密钥与账号密码
// 仍然照旧生效，见 apiClient.credentials。
func WithClientCredentials(ctx context.Context, request *http.Request) context.Context {
	if request == nil {
		return ctx
	}
	identity := clientIdentity{
		Token:  embyTokenFromRequest(request),
		UserID: clientUserIDFromRequest(request),
	}
	if identity.Token == "" && identity.UserID == "" {
		return ctx
	}
	return context.WithValue(ctx, clientCredentialsContextKey{}, identity)
}

// contextClientIdentity 返回本次请求带上来的播放器身份，没有则为零值。
func contextClientIdentity(ctx context.Context) clientIdentity {
	identity, _ := ctx.Value(clientCredentialsContextKey{}).(clientIdentity)
	identity.Token = strings.TrimSpace(identity.Token)
	identity.UserID = strings.TrimSpace(identity.UserID)
	return identity
}

// contextClientToken 返回本次请求带上来的播放器令牌，没有则返回空串。
func contextClientToken(ctx context.Context) string {
	return contextClientIdentity(ctx).Token
}

// embyAuthTokenRe 从 X-Emby-Authorization 里摘出 Token="…"。
// 该请求头是 MediaBrowser 的结构化身份声明，Token 未必排在最后一位。
var embyAuthTokenRe = regexp.MustCompile(`(?i)\bToken\s*=\s*"([^"]*)"`)

// embyTokenFromRequest 按播放器的几种习惯取令牌：Emby 客户端标准的
// X-Emby-Authorization、简写的 X-Emby-Token，以及部分播放器只带的 ?api_key=。
// 这三种都是 Emby 系独有的，其它上游的请求不会误命中。
func embyTokenFromRequest(request *http.Request) string {
	if value := strings.TrimSpace(request.Header.Get("X-Emby-Authorization")); value != "" {
		if matches := embyAuthTokenRe.FindStringSubmatch(value); matches != nil {
			if token := strings.TrimSpace(matches[1]); token != "" {
				return token
			}
		}
	}
	if token := strings.TrimSpace(request.Header.Get("X-Emby-Token")); token != "" {
		return token
	}
	return strings.TrimSpace(request.URL.Query().Get("api_key"))
}

// clientUserIDFromRequest 取播放器请求里的 UserId。Emby 客户端多放在查询串里，
// 也有实现放在 X-Emby-UserId 头里。
func clientUserIDFromRequest(request *http.Request) string {
	if userID := strings.TrimSpace(request.URL.Query().Get("UserId")); userID != "" {
		return userID
	}
	return strings.TrimSpace(request.Header.Get("X-Emby-UserId"))
}

// ResponseRewriter 允许上游方言改写指定的反代响应。Emby 用它处理
// PlaybackInfo：STRM 必须被标成可直放，否则客户端会选择 HLS 转码，之后再也
// 不会请求能够跳转到指针目标的媒体路由。
type ResponseRewriter interface {
	WantsResponseRewrite(request *http.Request) bool
	RewriteResponse(originalPath string, response *http.Response) (int, error)
}

// RequestPathRewriter 允许方言在转发前调整请求路径。绝大多数上游不需要它：
// 播放器换的只是端口，路径原样送达。飞牛影视是例外——它的 API 只挂在 /emby
// 前缀下，客户端若没带这个前缀，请求会落到单页应用的 HTML 上，PlaybackInfo
// 拿不到 JSON，于是「反代通了却永远不 302」。
type RequestPathRewriter interface {
	// RewriteRequestPath 返回实际发给上游的请求路径（BaseURL 的路径部分
	// 由代理另外拼接，这里不必也不应包含它）。返回原路径表示不做调整。
	RewriteRequestPath(request *http.Request) string
}

// New builds the provider matching the configured upstream type.
func New(cfg config.Upstream) (Provider, error) {
	base, err := url.Parse(cfg.BaseURL)
	if err != nil {
		return nil, fmt.Errorf("upstream %s: invalid base_url: %w", cfg.Name, err)
	}
	rules := make([]pathmap.Rule, 0, len(cfg.PathMappings))
	for _, mapping := range cfg.PathMappings {
		rules = append(rules, pathmap.Rule{From: mapping.From, To: mapping.To})
	}
	client := &apiClient{
		base:     base,
		apiKey:   strings.TrimSpace(cfg.APIKey),
		username: strings.TrimSpace(cfg.Username),
		password: cfg.Password,
		http: &http.Client{
			Timeout:   30 * time.Second,
			Transport: newTransport(cfg.Insecure),
		},
	}
	shared := providerBase{
		name:   cfg.Name,
		kind:   cfg.Type,
		port:   cfg.ListenPort,
		base:   base,
		mapper: pathmap.New(rules, cfg.StrmRoots),
		client: client,
	}
	// 「始终跳转」与「始终中继」都要忽略上游的不可直放判定，见 forcesDirectPlay。
	forcePlay := forcesDirectPlay(cfg.RedirectMode)
	delivery := forcedDelivery(cfg.RedirectMode)
	switch cfg.Type {
	case config.UpstreamAudiobookshelf:
		client.authHeader = "Authorization"
		return &absProvider{providerBase: shared}, nil
	case config.UpstreamEmby:
		client.authHeader = "X-Emby-Token"
		client.authQuery = "api_key"
		client.embyDialect = true
		return &embyProvider{
			providerBase:    shared,
			forceDirectPlay: forcePlay,
			forceDelivery:   delivery,
		}, nil
	case config.UpstreamFnos:
		// 飞牛影视没有 Emby 控制台里那种静态 API 密钥：它的接口只认客户端
		// 登录换来的令牌。所以两种鉴权都留着 —— 地址里配了账号密码就用登录
		// 令牌，什么都没配就裸调（放行空密钥），后者不影响 302 主链路，
		// 只是拿不到媒体库列表。
		client.apiKeyOptional = true
		client.embyClientAuth = true
		client.embyDialect = true
		client.authHeader = "X-Emby-Token"
		client.authQuery = "api_key"
		client.apiPrefix = fnosAPIPrefix(base.Path)
		provider := &fnosProvider{embyProvider: embyProvider{
			providerBase:    shared,
			forceDirectPlay: forcePlay,
			forceDelivery:   delivery,
		}}
		// 飞牛没有实现集合路由 /Items?Ids=：请求会落到单页应用上回一整页 HTML，
		// 解析必然失败。单项路由 /Items/{id} 它有，所以优先用它。
		provider.preferItemByID = true
		provider.normalizeSource = normalizeFnosMediaStreams
		return provider, nil
	default:
		return nil, fmt.Errorf("upstream %s: unsupported type %q", cfg.Name, cfg.Type)
	}
}

// fnosAPIPrefix 决定飞牛影视的 API 调用要在 base 路径后补什么前缀。
// 地址里已经写了 /emby 的用户直接沿用，避免拼成 /emby/emby。
func fnosAPIPrefix(basePath string) string {
	if strings.HasSuffix(strings.ToLower(strings.TrimRight(basePath, "/")), "/emby") {
		return ""
	}
	return "/emby"
}

func newTransport(insecure bool) *http.Transport {
	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConns:          128,
		MaxIdleConnsPerHost:   32,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		// Response header timeouts are intentionally unset: cloud drive backends
		// can take tens of seconds to sign a large media URL.
	}
	if insecure {
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	}
	return transport
}

// providerBase holds the fields shared by every provider implementation.
type providerBase struct {
	name   string
	kind   config.UpstreamType
	port   int
	base   *url.URL
	mapper *pathmap.Mapper
	client *apiClient
}

func (b *providerBase) Name() string                 { return b.name }
func (b *providerBase) Type() config.UpstreamType    { return b.kind }
func (b *providerBase) ListenPort() int              { return b.port }
func (b *providerBase) BaseURL() *url.URL            { return b.base }
func (b *providerBase) Mapper() *pathmap.Mapper      { return b.mapper }
func (b *providerBase) Transport() http.RoundTripper { return b.client.http.Transport }

// HasCredentials 表示这个上游是否具备向上游 API 查询媒体位置的条件。
// 飞牛影视没有静态密钥，配了账号密码算具备，什么都没配也算 —— 它的 302 主链路
// 只靠 PlaybackInfo 改写与缓存，不需要 API；若这里返回 false，代理层会退化成
// 纯反代，播放请求将永远拿不到 302。
func (b *providerBase) HasCredentials() bool {
	return b.client.apiKeyOptional || b.client.authenticated()
}

// apiClient performs authenticated JSON calls against the upstream API.
type apiClient struct {
	base   *url.URL
	apiKey string
	http   *http.Client
	// authHeader and authQuery let the same client speak both the
	// Audiobookshelf (bearer token) and Emby (api_key) dialects.
	authHeader string
	authQuery  string
	// apiPrefix 是拼在 base 路径与接口路径之间的固定前缀。飞牛影视的接口全都
	// 挂在 /emby 下，少了这一段会落到 SPA 的 HTML 上而不是 JSON 接口。
	apiPrefix string
	// apiKeyOptional 表示这个上游不配置密钥也能直接调 API。飞牛影视没有静态
	// 密钥，必须放行空的 apiKey，否则它会退化成纯反代，永远解析不到媒体源。
	apiKeyOptional bool
	// embyDialect 表示这个上游说 Emby 方言。只有 Emby 系才认识 X-Emby-Authorization
	// 这类请求头，因此也只有它们可以复用播放器请求里带上来的令牌。
	embyDialect bool
	// embyClientAuth 表示 API 调用要带上完整的 X-Emby-Authorization 客户端
	// 身份头（MediaBrowser Client="…", Device="…", Token="…"）。飞牛影视只认
	// 这个形态：只发 X-Emby-Token 时它直接 400「X-Emby-Authorization is
	// missing」，媒体库查询与试连全部失败，而且那句话看起来像「没配鉴权」，
	// 极难定位。Emby 本身两种都认，所以只有飞牛打开它。
	embyClientAuth bool

	// username / password 是账号密码登录用的凭据（见 config.Upstream 的说明）。
	username string
	password string
	// loginMu 保护登录令牌。媒体库查询与播放解析会并发触发登录，必须串行化，
	// 否则一次播放风暴会向飞牛连发几十次登录请求。
	loginMu sync.Mutex
	// loginToken 是登录换来的访问令牌，loginAt 记录它的取得时间。
	loginToken string
	loginAt    time.Time
	// loginUserID 是登录响应里带回的当前用户 ID。调用 /Items/{id}/PlaybackInfo
	// 这类需要 UserId 的接口时优先用它，比管理员 ID 更贴合登录账号的权限。
	loginUserID string
}

// apiLoginTokenTTL 是登录令牌的复用时长。飞牛的令牌没有公开的有效期，
// 取一个保守值：期间一直复用，过期后重登，遇到 401/403 也会立刻重登。
const apiLoginTokenTTL = 6 * time.Hour

// embyLoginResponse 是 /Users/AuthenticateByName 的响应。
type embyLoginResponse struct {
	AccessToken string `json:"AccessToken"`
	User        struct {
		ID   string `json:"Id"`
		Name string `json:"Name"`
	} `json:"User"`
}

// ErrNoAPIKey is returned when an upstream has no API key configured, which
// means AetherLink cannot resolve media paths for it.
var ErrNoAPIKey = fmt.Errorf("upstream api key is not configured")

// ErrDirectPlayUnsupported 表示上游在刚才的 PlaybackInfo 中已经判定当前客户端
// 不能直接播放原始文件。这时即使客户端请求了 /stream，也应退回上游自己处理，
// 不能把无法解码的原文件强行 302 出去。
var ErrDirectPlayUnsupported = errors.New("上游判定当前客户端不能直接播放原始文件")

// getJSON issues an authenticated GET and decodes the JSON body into out.
func (c *apiClient) getJSON(ctx context.Context, endpoint string, query url.Values, out any) error {
	token, err := c.credentials(ctx)
	if err != nil {
		return err
	}
	response, err := c.sendJSONGet(ctx, endpoint, query, token)
	if err != nil {
		return err
	}
	// 令牌会被上游吊销（改了密码、在别处登出、服务端重启）。撞上 401/403 就换
	// 一枚再试一次，仍失败才认账 —— 直接把第一次的失败报出去，会让一次令牌过期
	// 表现为「上游坏了」。换哪一枚见 authRetryToken。
	if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
		if retryToken, ok := c.authRetryToken(ctx); ok {
			io.Copy(io.Discard, io.LimitReader(response.Body, 512))
			response.Body.Close()
			if response, err = c.sendJSONGet(ctx, endpoint, query, retryToken); err != nil {
				return err
			}
		}
	}
	defer response.Body.Close()

	requestURL := c.requestURL(endpoint)
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		snippet, _ := io.ReadAll(io.LimitReader(response.Body, 512))
		return fmt.Errorf("GET %s returned %d: %s", requestURL, response.StatusCode, strings.TrimSpace(string(snippet)))
	}
	if out == nil {
		io.Copy(io.Discard, io.LimitReader(response.Body, 1<<16))
		return nil
	}
	return decodeJSONResponse(http.MethodGet, requestURL, token, response, out)
}

// apiResponseLimit 是 API 响应体的读取上限。这些接口都返回小 JSON；读到这么多
// 字节说明上游回的是意料之外的东西（最典型的是整页 HTML），继续读只会白占内存。
const apiResponseLimit = 16 << 20

// decodeJSONResponse 读取响应体并解析成 out，失败时给出能自解释的错误。
//
// 关键在错误信息。上游对不认识的路径经常回单页应用的 HTML（HTTP 200 + 一整页
// <!doctype html>），此时 encoding/json 只吐一句
// 「invalid character '<' looking for beginning of value」——既看不出是哪一条
// 请求，也看不出上游其实返回了网页，日志里就是这么一句，排查只能靠猜。
// 把真实请求地址、鉴权状态、Content-Type 与响应开头一并写进去，这类问题一眼可定位。
//
// requestURL 必须是实际打出去的完整地址（见 apiClient.requestURL），不能只给
// endpoint：endpoint 不含 apiPrefix，写进日志反而会把人引向「前缀没补」这个
// 错误结论。
func decodeJSONResponse(method, requestURL, token string, response *http.Response, out any) error {
	body, err := io.ReadAll(io.LimitReader(response.Body, apiResponseLimit))
	if err != nil {
		return fmt.Errorf("读取 %s %s 的响应失败: %w", method, requestURL, err)
	}
	// 沿用 Decoder 的宽容度：个别实现会在 JSON 之后补字节，Unmarshal 会因此失败。
	decoder := json.NewDecoder(bytes.NewReader(bytes.TrimSpace(body)))
	if err := decoder.Decode(out); err != nil {
		return jsonDecodeError(method, requestURL, response.Header.Get("Content-Type"), token, body, err)
	}
	return nil
}

// authStateNote 说明这次调用带了什么鉴权，只用于错误消息。
//
// 飞牛对「不认识的路径」和「没带令牌」会给出不同的回应，成因也完全不同：不带
// 令牌是配置问题（该去补账号密码），带了还失败才说明路径或版本有问题。不区分
// 开，用户拿着报错也不知道下一步该做什么。
func authStateNote(token string) string {
	if strings.TrimSpace(token) == "" {
		return "未携带访问令牌"
	}
	return "已携带访问令牌"
}

// jsonDecodeError 把「解析失败」翻译成能直接定位问题的一句话。
func jsonDecodeError(method, requestURL, contentType, token string, body []byte, err error) error {
	snippet := responseSnippet(body)
	note := authStateNote(token)
	if looksLikeHTML(body) {
		return fmt.Errorf("%s %s 返回的是网页而不是 JSON（%s，Content-Type=%q，开头：%s）：上游不认识这条路径，或缺了 /emby 前缀（错误里的地址是实际请求地址，可直接核对）", method, requestURL, note, contentType, snippet)
	}
	if snippet == "" {
		return fmt.Errorf("%s %s 返回了空响应，无法解析为 JSON（%s）: %w", method, requestURL, note, err)
	}
	return fmt.Errorf("解析 %s %s 的 JSON 响应失败（%s，Content-Type=%q，开头：%s）: %w", method, requestURL, note, contentType, snippet, err)
}

// looksLikeHTML 判断响应体是不是一整页网页。只认开头，避免把正文里偶然出现的
// 尖括号当成 HTML。
func looksLikeHTML(body []byte) bool {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return false
	}
	if len(trimmed) > 512 {
		trimmed = trimmed[:512]
	}
	lowered := bytes.ToLower(trimmed)
	return bytes.HasPrefix(lowered, []byte("<!doctype html")) || bytes.HasPrefix(lowered, []byte("<html"))
}

// responseSnippet 把响应开头压成一行短文本，只用于错误消息。
func responseSnippet(body []byte) string {
	const maxSnippet = 120
	text := strings.Join(strings.Fields(string(body)), " ")
	if len(text) > maxSnippet {
		text = text[:maxSnippet] + "…"
	}
	return text
}

// target 拼出这次调用真正要打的地址。endpoint 以 / 开头，apiPrefix 与 base 的
// 路径段都排在它前面 —— 飞牛少了 /emby 这一段就会落到 SPA 的 HTML 上。
func (c *apiClient) target(endpoint string, query url.Values) *url.URL {
	target := *c.base
	target.Path = strings.TrimRight(target.Path, "/") + c.apiPrefix + endpoint
	if query == nil {
		query = url.Values{}
	}
	target.RawQuery = query.Encode()
	return &target
}

// requestURL 返回 endpoint 对应的完整上游地址，不带查询串。
//
// 错误消息与日志都该用它，而不是 endpoint 参数：endpoint 不含 apiPrefix，写出来
// 是「/Items/eb35…」，而实际请求的是「/emby/Items/eb35…」。少一段前缀的地址
// 看上去就像「前缀没补上」，照着它排查会直奔错误结论。查询串一律不打印 ——
// Emby 系的令牌就以 ?api_key= 的形式挂在里面。
func (c *apiClient) requestURL(endpoint string) string {
	return c.target(endpoint, nil).String()
}

// sendJSONGet 发一次 GET。鉴权材料由调用方算好：可能是配置里的静态密钥，
// 可能是账号密码换来的登录令牌，也可能是播放器请求带上来的令牌。
func (c *apiClient) sendJSONGet(ctx context.Context, endpoint string, query url.Values, token string) (*http.Response, error) {
	if query == nil {
		query = url.Values{}
	}
	if c.authQuery != "" && token != "" {
		query.Set(c.authQuery, token)
	}
	target := c.target(endpoint, query)

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", contextUserAgent(ctx))
	if token != "" {
		if c.authHeader != "" {
			request.Header.Set(c.authHeader, c.authHeaderValue(token))
		}
		// 飞牛只认完整的客户端身份头。与上面那枚简写形态并存：服务端各取自己
		// 认识的那一枚，多带一枚不会有副作用。
		if c.embyClientAuth {
			request.Header.Set("X-Emby-Authorization", embyAuthorizationValue(token))
		}
	}
	return c.http.Do(request)
}

// credentials 返回本次 API 调用应当携带的令牌。空串表示这个上游不需要鉴权。
//
// 只在「这个上游什么都没配」时才借用播放器请求带上来的令牌：那枚令牌必定是上游
// 刚刚认可的身份，于是飞牛不填账号密码也能解析媒体。配了就还是用配置里的 ——
// 配置的是管理员身份，权限比播放器更完整（例如 Emby 的 /Users 只有管理员读得到）。
func (c *apiClient) credentials(ctx context.Context) (string, error) {
	if c.apiKey != "" {
		return c.apiKey, nil
	}
	if c.usesLogin() {
		token, err := c.login(ctx, false)
		if err != nil {
			// 配置的账号密码可能已经不对了（改过密码、账号被停用）。播放器自己
			// 带的令牌仍然可用，播放解析不该因为一份过期的配置而失败。
			if clientToken := contextClientToken(ctx); clientToken != "" {
				logx.Debugf("[upstream] 账号密码登录失败，改用播放器带上来的令牌: %v", err)
				return clientToken, nil
			}
			return "", err
		}
		return token, nil
	}
	if c.embyDialect {
		if token := contextClientToken(ctx); token != "" {
			return token, nil
		}
	}
	if c.apiKeyOptional {
		return "", nil
	}
	return "", ErrNoAPIKey
}

// authRetryToken 在收到 401/403 之后给出一枚新令牌，ok 为假表示没有别的令牌可换、
// 不必再打一次。只有「账号密码换来的令牌」能续期：丢掉缓存强制重登即可。
//
// 播放器带上来的令牌不归 AetherLink 管、也无从续期；而且它只在上游没配凭据时
// 才会被借用，那种情况下也拿不出配置凭据来顶替，所以这里不必为它留分支。
func (c *apiClient) authRetryToken(ctx context.Context) (string, bool) {
	if !c.usesLogin() {
		return "", false
	}
	token, err := c.login(ctx, true)
	if err != nil {
		logx.Debugf("[upstream] 令牌被上游拒绝，重新登录也失败: %v", err)
		return "", false
	}
	return token, true
}

// usesLogin 报告这个上游是不是靠「账号密码换令牌」鉴权，而不是配置里的静态密钥。
func (c *apiClient) usesLogin() bool {
	return c.apiKey == "" && c.username != "" && c.password != ""
}

// authenticated 报告这个上游是否具备任何能用于 API 调用的鉴权材料。
func (c *apiClient) authenticated() bool {
	return c.apiKey != "" || c.usesLogin()
}

// login 用账号密码向上游换取一枚访问令牌，成功后在 TTL 内复用。
//
// 走的就是客户端登录用的那条 /Users/AuthenticateByName：飞牛影视没有 Emby
// 控制台里那种静态 API 密钥，只有这条登录路径能拿到令牌。force 为真时忽略
// 缓存强制重登，用于令牌被吊销后的补救。
func (c *apiClient) login(ctx context.Context, force bool) (string, error) {
	c.loginMu.Lock()
	defer c.loginMu.Unlock()
	if !force && c.loginToken != "" && time.Since(c.loginAt) < apiLoginTokenTTL {
		return c.loginToken, nil
	}

	target := c.target("/Users/AuthenticateByName", nil)
	body, err := json.Marshal(map[string]string{"Username": c.username, "Pw": c.password})
	if err != nil {
		return "", err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, target.String(), bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", contextUserAgent(ctx))
	// Emby 与飞牛都要求登录请求带上客户端身份头，缺了部分版本直接 400。
	request.Header.Set("X-Emby-Authorization", embyAuthorizationHeader)

	response, err := c.http.Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		snippet, _ := io.ReadAll(io.LimitReader(response.Body, 512))
		c.loginToken = ""
		c.loginUserID = ""
		return "", fmt.Errorf("登录上游失败（HTTP %d）: %s", response.StatusCode, strings.TrimSpace(string(snippet)))
	}
	var payload embyLoginResponse
	// 登录本身还没拿到令牌，鉴权状态固定报「未携带」。
	if err := decodeJSONResponse(http.MethodPost, c.requestURL("/Users/AuthenticateByName"), "", response, &payload); err != nil {
		return "", fmt.Errorf("解析登录响应: %w", err)
	}
	if strings.TrimSpace(payload.AccessToken) == "" {
		return "", errors.New("上游登录成功但没有返回访问令牌，请确认账号密码是否正确")
	}
	c.loginToken = payload.AccessToken
	c.loginUserID = strings.TrimSpace(payload.User.ID)
	c.loginAt = time.Now()
	logx.Infof("[upstream] 已用账号 %s 登录 %s，取得访问令牌", c.username, c.base.Host)
	return c.loginToken, nil
}

// loggedInUserID 返回登录时上游告知的用户 ID，没登录过则返回空串。
func (c *apiClient) loggedInUserID() string {
	c.loginMu.Lock()
	defer c.loginMu.Unlock()
	return c.loginUserID
}

// embyAuthorizationHeader 是 Emby 系登录接口要求的客户端身份声明。
// DeviceId 固定成一个常量，避免每次连接都让上游记一条新设备。
const embyAuthorizationHeader = `MediaBrowser Client="AetherLink", Device="AetherLink", DeviceId="aetherlink-server", Version="1.0.0"`

// embyAuthorizationValue 在客户端身份声明后追加令牌，拼出
// X-Emby-Authorization 的完整形态（登录时还没有令牌，所以那里只用声明本身）。
func embyAuthorizationValue(token string) string {
	return embyAuthorizationHeader + `, Token="` + sanitizeAuthToken(token) + `"`
}

// sanitizeAuthToken 摘掉会破坏这个结构化请求头的字符。令牌来自上游响应，
// 正常是十六进制串；引号与反斜杠会让后面的 Token 值提前收尾，控制字符则会
// 让 Go 的传输层直接拒发请求 —— 两者都只能摘掉，不能拼进去。
func sanitizeAuthToken(token string) string {
	return strings.Map(func(character rune) rune {
		switch character {
		case '"', '\\', '\r', '\n', '\t':
			return -1
		}
		return character
	}, token)
}

func (c *apiClient) authHeaderValue(token string) string {
	if c.authHeader == "Authorization" {
		return "Bearer " + token
	}
	return token
}
