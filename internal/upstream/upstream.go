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
	switch cfg.Type {
	case config.UpstreamAudiobookshelf:
		client.authHeader = "Authorization"
		return &absProvider{providerBase: shared}, nil
	case config.UpstreamEmby:
		client.authHeader = "X-Emby-Token"
		client.authQuery = "api_key"
		return &embyProvider{providerBase: shared}, nil
	case config.UpstreamFnos:
		// 飞牛影视没有 Emby 控制台里那种静态 API 密钥：它的接口只认客户端
		// 登录换来的令牌。所以两种鉴权都留着 —— 地址里配了账号密码就用登录
		// 令牌，什么都没配就裸调（放行空密钥），后者不影响 302 主链路，
		// 只是拿不到媒体库列表。
		client.apiKeyOptional = true
		client.authHeader = "X-Emby-Token"
		client.authQuery = "api_key"
		client.apiPrefix = fnosAPIPrefix(base.Path)
		provider := &fnosProvider{embyProvider: embyProvider{providerBase: shared}}
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

	// username / password 是账号密码登录用的凭据（见 config.Upstream 的说明）。
	username string
	password string
	// loginMu 保护登录令牌。媒体库查询与播放解析会并发触发登录，必须串行化，
	// 否则一次播放风暴会向飞牛连发几十次登录请求。
	loginMu sync.Mutex
	// loginToken 是登录换来的访问令牌，loginAt 记录它的取得时间。
	loginToken string
	loginAt    time.Time
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
	// 登录换来的令牌会被上游吊销（改了密码、在别处登出、服务端重启）。撞上
	// 401/403 就丢掉缓存重登一次再试，仍失败才认账 —— 直接把第一次的失败报
	// 出去会让一次令牌过期表现为「上游坏了」。
	if c.usesLogin() && (response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden) {
		io.Copy(io.Discard, io.LimitReader(response.Body, 512))
		response.Body.Close()
		refreshed, loginErr := c.login(ctx, true)
		if loginErr != nil {
			return loginErr
		}
		if response, err = c.sendJSONGet(ctx, endpoint, query, refreshed); err != nil {
			return err
		}
	}
	defer response.Body.Close()

	if response.StatusCode < 200 || response.StatusCode >= 300 {
		snippet, _ := io.ReadAll(io.LimitReader(response.Body, 512))
		return fmt.Errorf("GET %s returned %d: %s", endpoint, response.StatusCode, strings.TrimSpace(string(snippet)))
	}
	if out == nil {
		io.Copy(io.Discard, io.LimitReader(response.Body, 1<<16))
		return nil
	}
	return json.NewDecoder(response.Body).Decode(out)
}

// sendJSONGet 发一次 GET。鉴权材料由调用方算好：可能是配置里的静态密钥，
// 可能是账号密码换来的登录令牌，也可能是空（上游不需要鉴权）。
func (c *apiClient) sendJSONGet(ctx context.Context, endpoint string, query url.Values, token string) (*http.Response, error) {
	target := *c.base
	target.Path = strings.TrimRight(target.Path, "/") + c.apiPrefix + endpoint
	if query == nil {
		query = url.Values{}
	}
	if c.authQuery != "" && token != "" {
		query.Set(c.authQuery, token)
	}
	target.RawQuery = query.Encode()

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", contextUserAgent(ctx))
	if c.authHeader != "" && token != "" {
		request.Header.Set(c.authHeader, c.authHeaderValue(token))
	}
	return c.http.Do(request)
}

// credentials 返回本次 API 调用应当携带的令牌。空串表示这个上游不需要鉴权。
func (c *apiClient) credentials(ctx context.Context) (string, error) {
	if c.apiKey != "" {
		return c.apiKey, nil
	}
	if c.usesLogin() {
		return c.login(ctx, false)
	}
	if c.apiKeyOptional {
		return "", nil
	}
	return "", ErrNoAPIKey
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

	target := *c.base
	target.Path = strings.TrimRight(target.Path, "/") + c.apiPrefix + "/Users/AuthenticateByName"
	target.RawQuery = ""
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
		return "", fmt.Errorf("登录上游失败（HTTP %d）: %s", response.StatusCode, strings.TrimSpace(string(snippet)))
	}
	var payload embyLoginResponse
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		return "", fmt.Errorf("解析登录响应: %w", err)
	}
	if strings.TrimSpace(payload.AccessToken) == "" {
		return "", errors.New("上游登录成功但没有返回访问令牌，请确认账号密码是否正确")
	}
	c.loginToken = payload.AccessToken
	c.loginAt = time.Now()
	logx.Infof("[upstream] 已用账号 %s 登录 %s，取得访问令牌", c.username, c.base.Host)
	return c.loginToken, nil
}

// embyAuthorizationHeader 是 Emby 系登录接口要求的客户端身份声明。
// DeviceId 固定成一个常量，避免每次连接都让上游记一条新设备。
const embyAuthorizationHeader = `MediaBrowser Client="AetherLink", Device="AetherLink", DeviceId="aetherlink-server", Version="1.0.0"`

func (c *apiClient) authHeaderValue(token string) string {
	if c.authHeader == "Authorization" {
		return "Bearer " + token
	}
	return token
}
