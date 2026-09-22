// Package resolver turns an intercepted upstream media request into a final
// playable target: it asks the upstream API where the media lives, maps that
// path into the container, reads the .strm pointer and optionally follows
// redirect hops so the client receives a directly playable URL.
package resolver

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aetherlink/aetherlink/internal/config"
	"github.com/aetherlink/aetherlink/internal/logx"
	"github.com/aetherlink/aetherlink/internal/strm"
	"github.com/aetherlink/aetherlink/internal/upstream"
	"github.com/aetherlink/aetherlink/internal/urlx"
)

// ErrNotStrm signals that the media behind a request is a regular file, so the
// request should be proxied to the upstream untouched.
var ErrNotStrm = errors.New("media is not a strm pointer")

// ErrPointerUnavailable signals that the media *is* a .strm pointer but
// AetherLink could not read it, almost always because the media directory is
// not mounted into this container. The upstream can still serve the file itself,
// so callers fall back to plain proxying instead of failing the playback.
var ErrPointerUnavailable = errors.New("strm pointer file is not readable inside the AetherLink container")

const (
	audiobookshelfCacheTTL = 15 * time.Minute
	embyFallbackCacheTTL   = 2 * time.Hour
	// cmecloudCacheTTL 是直链落在移动云盘（cmecloud.cn）时的固定缓存值。
	// 这类直链的有效期远短于其它网盘，沿用 Emby 那条 2 小时的回退会让客户端
	// 在缓存命中之后拿到一条已经失效的地址 —— 现象是「突然有一批播不了」。
	// 它是**固定值**：关键词命中就是 15 分钟，不看直链自带的 `t`（原因见 cacheTTLFor）。
	cmecloudCacheTTL = 15 * time.Minute
	// resolverCacheMax 是解析结果缓存寿命的全站上限，任何一档都不能超过它
	// （用户 2026-09-22 要求「最多缓存 24h，超过则缓存 24h」）。它同时兜住两件事：
	// 直链自带的 `t` 报出很远的时间（有的网盘给的是「一个月后」），以及配置里
	// 把 Cache.TTL 调得过大 —— 两者都按 24 小时截断。
	resolverCacheMax = 24 * time.Hour
)

// cmecloudCacheKeyword 是识别移动云盘直链的关键词。按子串匹配而不是比对主机名
// 后缀：这个词可能出现在路径或查询参数里（直链本身又是从上游给的地址解析来的，
// 形态不受我们控制），只要出现就按移动云盘处理。
const cmecloudCacheKeyword = "cmecloud.cn"

// Resolution is the outcome of resolving one media reference.
type Resolution struct {
	// UpstreamPath is the path reported by the upstream media server.
	UpstreamPath string `json:"upstreamPath"`
	// ContainerPath is UpstreamPath after path mapping. It is empty when the
	// upstream handed us a direct URL and no pointer file was read.
	ContainerPath string `json:"containerPath,omitempty"`
	// FromUpstreamAPI records that the target came from the upstream API rather
	// than from a pointer file on disk (this is how Emby reports strm sources).
	FromUpstreamAPI bool `json:"fromUpstreamApi,omitempty"`
	// Target is the parsed .strm pointer.
	Target *strm.Target `json:"target"`
	// FinalURL is the URL handed to the client. It equals Target.URL unless
	// redirect following was enabled and the backend answered with a 3xx.
	FinalURL string `json:"finalUrl,omitempty"`
	// Hops records the redirect chain that was followed.
	Hops []string `json:"hops,omitempty"`
	// ResolvedAt is when the resolution was computed.
	ResolvedAt time.Time `json:"resolvedAt"`
}

type CacheSource string

const (
	CacheSourceMiss     CacheSource = "miss"
	CacheSourceHit      CacheSource = "hit"
	CacheSourceRestored CacheSource = "restored"
)

// IsRemote reports whether the resolution points at an HTTP target.
func (r *Resolution) IsRemote() bool {
	return r.Target != nil && r.Target.Type == strm.TargetRemote
}

// PlayURL returns the URL a client should be redirected to.
func (r *Resolution) PlayURL() string {
	if r.FinalURL != "" {
		return r.FinalURL
	}
	if r.Target != nil {
		return r.Target.URL
	}
	return ""
}

// Resolver resolves media references, with caching and per-key deduplication.
type Resolver struct {
	cache  *lruCache
	config config.Redirect

	inflightMu sync.Mutex
	inflight   map[string]*inflightCall

	// client follows redirect hops. Redirects are handled manually so each hop
	// can be recorded and capped.
	client *http.Client
}

type inflightCall struct {
	done       chan struct{}
	resolution *Resolution
	err        error
}

// New builds a resolver.
func New(cacheCfg config.Cache, redirectCfg config.Redirect) *Resolver {
	return NewWithPersistence(cacheCfg, redirectCfg, "")
}

// NewWithPersistence builds a resolver whose direct-link cache can survive
// process restarts when persistencePath is configured.
func NewWithPersistence(cacheCfg config.Cache, redirectCfg config.Redirect, persistencePath string) *Resolver {
	return &Resolver{
		cache:    newPersistentLRUCache(cacheCfg.TTL, cacheCfg.MaxSize, persistencePath),
		config:   redirectCfg,
		inflight: make(map[string]*inflightCall),
		client: &http.Client{
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
}

// CacheSize reports how many resolutions are currently cached.
func (r *Resolver) CacheSize() int { return r.cache.size() }

// PurgeCache drops all cached resolutions and returns how many were removed.
func (r *Resolver) PurgeCache() int { return r.cache.purge() }

// Resolve returns the target for ref, using the cache when possible. cacheHit
// reports whether the result came from the cache.
func (r *Resolver) Resolve(ctx context.Context, provider upstream.Provider, ref upstream.MediaRef, userAgent string) (resolution *Resolution, cacheHit bool, cacheTTL time.Duration, err error) {
	resolution, source, cacheTTL, err := r.ResolveWithSource(ctx, provider, ref, userAgent)
	return resolution, source != CacheSourceMiss, cacheTTL, err
}

func (r *Resolver) ResolveWithSource(ctx context.Context, provider upstream.Provider, ref upstream.MediaRef, userAgent string) (resolution *Resolution, source CacheSource, cacheTTL time.Duration, err error) {
	effectiveUserAgent := r.effectiveUserAgentFor(provider.Type(), provider.Name(), userAgent)
	ctx = upstream.WithUserAgent(ctx, effectiveUserAgent)
	// 两个键。共享键里只有「哪个文件 + 哪台上游」，不带 UA 与播放器身份 —— 它只对
	// 移动云盘那类直链开放，见 lookup 与 putResolution。
	sharedKey := ref.CacheKey(provider.Name()) + "\x00server=" + cacheNamespaceOf(provider)
	key := sharedKey + "\x00ua=" + effectiveUserAgent + "\x00scope=" + upstream.PlaybackCacheScope(ctx)
	if cached, source, remaining, ok := r.lookup(key, sharedKey); ok {
		return cached, source, remaining, nil
	}
	ttl := r.cacheTTL(provider)

	// Collapse concurrent requests for the same track. Players routinely open
	// several ranged requests at once when seeking.
	r.inflightMu.Lock()
	if cached, source, remaining, ok := r.lookup(key, sharedKey); ok {
		r.inflightMu.Unlock()
		return cached, source, remaining, nil
	}
	if call, ok := r.inflight[key]; ok {
		r.inflightMu.Unlock()
		select {
		case <-call.done:
			if call.err != nil {
				return nil, CacheSourceMiss, 0, call.err
			}
			return call.resolution, CacheSourceMiss, r.cacheTTLFor(provider, call.resolution, ttl), nil
		case <-ctx.Done():
			return nil, CacheSourceMiss, 0, ctx.Err()
		}
	}
	call := &inflightCall{done: make(chan struct{})}
	r.inflight[key] = call
	r.inflightMu.Unlock()

	call.resolution, call.err = r.resolveUncached(ctx, provider, ref, userAgent)
	resolvedTTL := time.Duration(0)
	if call.err == nil {
		resolvedTTL = r.cacheTTLFor(provider, call.resolution, ttl)
		r.putResolution(key, sharedKey, call.resolution, resolvedTTL)
	}
	r.inflightMu.Lock()
	close(call.done)
	delete(r.inflight, key)
	r.inflightMu.Unlock()
	return call.resolution, CacheSourceMiss, resolvedTTL, call.err
}

// lookup 先按完整键（带 UA 与播放器身份）找，再退回共享键。
//
// 共享键只对移动云盘那类直链开放，而且**必须回读到内容再判一次**：其它网盘的直链
// 可能与 UA 绑定，跨 UA 复用会把一条只有某个播放器才播得动的地址发给别人。
func (r *Resolver) lookup(scopedKey, sharedKey string) (*Resolution, CacheSource, time.Duration, bool) {
	if cached, remaining, restored, ok := r.cache.getWithSource(scopedKey); ok {
		return cached, cacheSourceFor(restored), remaining, true
	}
	if cached, remaining, restored, ok := r.cache.getWithSource(sharedKey); ok && cmecloudDirectLink(cached) {
		return cached, cacheSourceFor(restored), remaining, true
	}
	return nil, CacheSourceMiss, 0, false
}

// putResolution 决定这条结果记在哪个键下：移动云盘的直链记共享键（同一个文件不同
// UA / 不同播放器共用一条），其余记带 UA 与播放器身份的完整键。
func (r *Resolver) putResolution(scopedKey, sharedKey string, resolution *Resolution, ttl time.Duration) {
	if cmecloudDirectLink(resolution) {
		r.cache.put(sharedKey, resolution, ttl)
		return
	}
	r.cache.put(scopedKey, resolution, ttl)
}

func cacheSourceFor(restored bool) CacheSource {
	if restored {
		return CacheSourceRestored
	}
	return CacheSourceHit
}

// cacheNamespaceOf 取「哪台上游」这一段摘要：优先用 provider 自己算好的配置摘要
// （服务器地址、类型、凭据、路径规则都算在里面），取不到再退回类型 + 地址。
func cacheNamespaceOf(provider upstream.Provider) string {
	if namespaced, ok := provider.(interface{ CacheNamespace() string }); ok {
		return namespaced.CacheNamespace()
	}
	return string(provider.Type()) + ":" + provider.BaseURL().String()
}

// Close 停掉直链缓存的后台写盘，并把未落盘的改动补写一次。重建 resolver（保存
// 配置）与进程退出时调用；不调用只会丢掉最后一次写盘，不影响正确性。
func (r *Resolver) Close() { r.cache.close() }

func (r *Resolver) cacheTTLFor(provider upstream.Provider, resolution *Resolution, fallback time.Duration) time.Duration {
	ttl := fallback
	if resolution != nil {
		// 移动云盘直链（含 cmecloud.cn）**固定**缓存 cmecloudCacheTTL，不读它自带的 `t`：
		// 关键词命中就是 15 分钟。这类直链上的 `t` 多半不是到期时间戳（移动云盘给的就是
		// 一个像签名的值），拿它当寿命会把这一档整个抵消掉 —— 旧实现按「在 15 分钟的基础上
		// 往下压」写，`t` 取不到有效值时压成 0，而 0 在 put() 里等于不入缓存，界面上就
		// 显示成「不缓存」。这一支也必须**先于**下面 Emby 系按 `t` 的分支判定。
		switch {
		case cmecloudDirectLink(resolution):
			ttl = cmecloudCacheTTL
		case provider != nil && provider.Type().IsEmbyFamily():
			if fromURL, ok := directURLTTL(resolution); ok {
				ttl = fromURL
			}
		}
	}
	// 全站统一封顶 resolverCacheMax（24 小时），放在最后一步、统一收口：不管上面走的是
	// 移动云盘的固定值、Emby 系按 `t` 算出的寿命，还是配置里那一档回退值，超过 24 小时
	// 都截成 24 小时。已经过期的直链这里仍是 0（0 不大于上限），依旧不入缓存。
	if ttl > resolverCacheMax {
		ttl = resolverCacheMax
	}
	return ttl
}

// directLinkCandidates 列出这次解析结果里客户端会真正请求到的直链：FinalURL 是
// 最终交给客户端的那一条，Target.URL 是它的解析来源（未跟随跳转时两者相同）。
//
// 只收这两条、不收 Hops：跳转链的中间跳是我们自己在预检时走掉的，客户端根本不会
// 去请求它，它过不过期都不影响播放，拿它当判据只会误伤。
func directLinkCandidates(resolution *Resolution) []string {
	if resolution == nil {
		return nil
	}
	candidates := make([]string, 0, 2)
	if resolution.FinalURL != "" {
		candidates = append(candidates, resolution.FinalURL)
	}
	if resolution.Target != nil && resolution.Target.URL != "" {
		candidates = append(candidates, resolution.Target.URL)
	}
	return candidates
}

// cmecloudDirectLink 判断解析出来的直链是不是走移动云盘（cmecloud.cn）。
func cmecloudDirectLink(resolution *Resolution) bool {
	for _, candidate := range directLinkCandidates(resolution) {
		// 主机名大小写不敏感，直链里的域名可能是全大写写法。
		if strings.Contains(strings.ToLower(candidate), cmecloudCacheKeyword) {
			return true
		}
	}
	return false
}

func directURLTTL(resolution *Resolution) (time.Duration, bool) {
	var earliest time.Time
	foundValid := false
	now := time.Now()
	for _, candidate := range directLinkCandidates(resolution) {
		parsed, err := urlx.Parse(candidate)
		if err != nil {
			continue
		}
		value := strings.TrimSpace(parsed.Query().Get("t"))
		if value == "" {
			continue
		}
		seconds, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			continue
		}
		foundValid = true
		if seconds > 1_000_000_000_000 {
			seconds /= 1000
		}
		expiry := time.Unix(seconds, 0)
		if expiry.After(now) && (earliest.IsZero() || expiry.Before(earliest)) {
			earliest = expiry
		}
	}
	if !foundValid {
		return 0, false
	}
	if earliest.IsZero() {
		return 0, true
	}
	return time.Until(earliest), true
}

func (r *Resolver) cacheTTL(provider upstream.Provider) time.Duration {
	if provider != nil {
		switch provider.Type() {
		case config.UpstreamAudiobookshelf:
			return audiobookshelfCacheTTL
		case config.UpstreamEmby, config.UpstreamFnos:
			return embyFallbackCacheTTL
		}
	}
	return r.cache.ttl
}

func (r *Resolver) resolveUncached(ctx context.Context, provider upstream.Provider, ref upstream.MediaRef, userAgent string) (*Resolution, error) {
	mediaTarget, err := provider.MediaTarget(ctx, ref)
	if err != nil {
		return nil, err
	}

	resolution := &Resolution{
		UpstreamPath: mediaTarget.Path,
		ResolvedAt:   time.Now(),
	}

	switch {
	case mediaTarget.IsDirectURL():
		// 上游（Emby）已经把 .strm 读掉了，直接给出了指针里的直链。
		// 这条路径不需要看到任何文件，因此也不需要挂载媒体目录。
		target, err := strm.ParseURL(mediaTarget.URL)
		if err != nil {
			return nil, fmt.Errorf("上游给出的直链无法解析 %q: %w", mediaTarget.URL, err)
		}
		resolution.FromUpstreamAPI = true
		resolution.Target = target

	case isStrmMedia(mediaTarget):
		// 上游（Audiobookshelf）只报告指针文件的位置，内容要自己读。
		// Locate 会先按路径映射找，找不到再尝试常见挂载点，这样两侧挂载点
		// 不同名时也不必手写映射。
		mapper := provider.Mapper()
		containerPath, found, err := mapper.Locate(mediaTarget.Path)
		if err != nil {
			return nil, err
		}
		if !found {
			return nil, fmt.Errorf("%w: 上游路径 %s 在本容器里对应不到 %s", ErrPointerUnavailable, mediaTarget.Path, containerPath)
		}
		target, err := strm.Read(containerPath, mapper)
		if err != nil {
			// 指针文件读不到，几乎总是「媒体目录没挂进来」。这不该让播放
			// 直接失败：上游自己能读到这个文件，退回透传即可。
			if errors.Is(err, os.ErrNotExist) || errors.Is(err, os.ErrPermission) {
				return nil, fmt.Errorf("%w: %s: %v", ErrPointerUnavailable, containerPath, err)
			}
			return nil, fmt.Errorf("read strm %s: %w", containerPath, err)
		}
		resolution.ContainerPath = containerPath
		resolution.Target = target

	default:
		return nil, fmt.Errorf("%w: %s", ErrNotStrm, mediaTarget.Describe())
	}

	if resolution.Target.Type == strm.TargetRemote {
		resolution.FinalURL = resolution.Target.URL
		if r.config.FollowUpstreamRedirects {
			finalURL, hops, err := r.followRedirects(ctx, resolution.Target.URL, upstream.EffectiveContextUserAgent(ctx))
			if err != nil {
				// A failed pre-flight is not fatal: hand the original URL to the
				// client and let the player negotiate directly.
				logx.Warnf("[resolver] follow redirects failed for %s: %v", resolution.Target.URL, err)
			} else {
				resolution.FinalURL = finalURL
				resolution.Hops = hops
			}
		}
	}
	return resolution, nil
}

// isStrmMedia 判断上游报告的这个媒体是不是 .strm 指针。
// 优先看扩展名；Emby 有些库会把扩展名藏在 Container 字段里。
func isStrmMedia(target upstream.MediaTarget) bool {
	return strm.IsStrmPath(target.Path) || strings.EqualFold(target.Container, "strm")
}

// followRedirects walks the redirect chain with HEAD (falling back to a ranged
// GET for backends that reject HEAD) so that clients which do not follow 302
// still receive a directly playable URL.
func (r *Resolver) followRedirects(ctx context.Context, startURL, userAgent string) (string, []string, error) {
	timeout := r.config.ProbeTimeout
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	current := startURL
	hops := make([]string, 0, r.config.MaxFollowHops)
	for hop := 0; hop < r.config.MaxFollowHops; hop++ {
		location, err := r.probeOnce(ctx, http.MethodHead, current, userAgent)
		if err != nil {
			location, err = r.probeOnce(ctx, http.MethodGet, current, userAgent)
			if err != nil {
				return "", hops, err
			}
		}
		if location == "" {
			return current, hops, nil
		}
		resolved, err := resolveRelative(current, location)
		if err != nil {
			return "", hops, err
		}
		hops = append(hops, resolved)
		current = resolved
	}
	return current, hops, fmt.Errorf("exceeded %d redirect hops", r.config.MaxFollowHops)
}

// probeOnce issues one request and returns the Location header when the
// response is a redirect, or an empty string when the URL is already final.
func (r *Resolver) probeOnce(ctx context.Context, method, target, userAgent string) (string, error) {
	request, err := http.NewRequestWithContext(ctx, method, target, nil)
	if err != nil {
		return "", err
	}
	request.Header.Set("User-Agent", r.effectiveUserAgent(userAgent))
	if method == http.MethodGet {
		// Ask for a single byte so backends that ignore HEAD do not start
		// streaming the whole file during the probe.
		request.Header.Set("Range", "bytes=0-0")
	}

	response, err := r.client.Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()

	if response.StatusCode >= 300 && response.StatusCode < 400 {
		location := response.Header.Get("Location")
		if location == "" {
			return "", fmt.Errorf("status %d without Location header", response.StatusCode)
		}
		return location, nil
	}
	if response.StatusCode >= 400 {
		return "", fmt.Errorf("probe %s returned %d", method, response.StatusCode)
	}
	return "", nil
}

// EffectiveUserAgent returns the User-Agent AetherLink should use when talking
// to a STRM backend on behalf of a client.
func (r *Resolver) EffectiveUserAgent(clientUserAgent string) string {
	return r.effectiveUserAgent(clientUserAgent)
}

// EffectiveUserAgentFor applies the provider-specific block list before
// forwarding the client UA to the media backend.
func (r *Resolver) EffectiveUserAgentFor(providerType config.UpstreamType, clientUserAgent string) string {
	return r.effectiveUserAgentFor(providerType, "", clientUserAgent)
}

// EffectiveUserAgentForUpstream applies the selected provider's UA policy to
// one named upstream.
func (r *Resolver) EffectiveUserAgentForUpstream(providerType config.UpstreamType, upstreamName, clientUserAgent string) string {
	return r.effectiveUserAgentFor(providerType, upstreamName, clientUserAgent)
}

func (r *Resolver) effectiveUserAgent(clientUserAgent string) string {
	return r.effectiveUserAgentFor("", "", clientUserAgent)
}

func (r *Resolver) effectiveUserAgentFor(providerType config.UpstreamType, upstreamName, clientUserAgent string) string {
	clientUserAgent = strings.TrimSpace(clientUserAgent)
	if r.config.IsBlockedClientUserAgentForUpstream(providerType, upstreamName, clientUserAgent) {
		clientUserAgent = ""
	}
	if r.config.ShouldForwardUserAgent() && clientUserAgent != "" {
		return clientUserAgent
	}
	if fallback := strings.TrimSpace(r.config.FallbackUserAgent); fallback != "" {
		return fallback
	}
	return "AetherLink"
}

// ShouldRedirect decides between answering with a 302 and relaying the bytes.
func (r *Resolver) ShouldRedirect(resolution *Resolution) bool {
	return r.ShouldRedirectWith(resolution, r.config)
}

// ShouldRedirectWith applies the policy selected for one upstream.
func (r *Resolver) ShouldRedirectWith(resolution *Resolution, redirectCfg config.Redirect) bool {
	return r.ShouldRedirectForClient(resolution, redirectCfg, "")
}

func (r *Resolver) ShouldRedirectForClient(resolution *Resolution, redirectCfg config.Redirect, client string) bool {
	if resolution == nil || !resolution.IsRemote() {
		return false
	}
	playURL := resolution.PlayURL()
	if playURL == "" {
		return false
	}
	switch redirectCfg.Mode {
	case config.RedirectNever:
		return false
	case config.RedirectPublic:
		return ScopeOfClient(client, redirectCfg.IntranetCIDRs...) == ClientScopePublic
	case config.RedirectPrivate:
		return ScopeOfClient(client, redirectCfg.IntranetCIDRs...) == ClientScopePrivate
	case config.RedirectAlways:
		return true
	default:
		return false
	}
}

// ClientScope 是一个客户端 IP 的内外网归属，用于跳转策略与日志展示。
// 302 与否的判定依据是客户端所在的网络位置（README「跳转模式」一节），
// 与媒体直链指向哪台服务器无关。
type ClientScope string

const (
	// ClientScopePublic 是可全球路由的公网地址。
	ClientScopePublic ClientScope = "公网"
	// ClientScopePrivate 表示客户端与 AetherLink 在同一个网络里，三条来路：
	// 内置规则（RFC1918 / 回环 / 链路本地 / IPv6 ULA）、配置文件里声明的网段
	// （config.Redirect.IntranetCIDRs，界面上没有入口，只能手改 YAML）、
	// 以及与 AetherLink 本机同一网段（自动的那一层，见 localnet.go）。
	ClientScopePrivate ClientScope = "内网"
	// ClientScopeUnknown 表示地址缺失或无法解析。条件跳转模式对它一律中继。
	ClientScopeUnknown ClientScope = "无法识别"
)

// ScopeOfClient 把一条客户端地址（裸 IP 或 ip:port）归类为公网、内网或无法识别。
//
// intranet 是配置里补充的内网网段（config.Redirect.IntranetCIDRs）。内置规则
// 只认 RFC1918、回环、链路本地与 IPv6 ULA(fc00::/7)；而家里的设备拿到的常常是
// 运营商下发的 IPv6 全局地址（240e:: 这类 GUA），按地址类型它是公网，可它确实
// 在局域网里 —— 于是「公网跳转」会错误地 302 给它、「内网跳转」又会漏掉它。
// 命中所列网段的一律按内网处理；此外**与本机处于同一网段的客户端**也算内网
// （不需要配置的那一层，见 localnet.go）。
func ScopeOfClient(client string, intranet ...string) ClientScope {
	scope, _ := ScopeOfClientWithReason(client, intranet...)
	return scope
}

// ScopeOfClientWithReason 同 ScopeOfClient，并额外返回一句可以直接接进日志的说明
// （空串表示没什么可说的）。只有「与本机同一网段」这一层会出声：它是从本机地址
// 推算出来的、界面上看不到，不留线索用户就只看到一句「客户端是内网地址」，
// 无从判断该不该信。
func ScopeOfClientWithReason(client string, intranet ...string) (ClientScope, string) {
	return scopeOfClient(ClientAddress(client), intranet, LocalNetworkPrefixes())
}

// scopeOfClient 是内外网判定的全部逻辑，纯函数：declared 是用户声明的网段
// （config.Redirect.IntranetCIDRs），local 是本机自己的网段。拆成纯函数是为了
// 能拿固定网段直接验证，不必依赖跑测试那台机器上恰好插着什么网卡。
func scopeOfClient(address netip.Addr, declared []string, local []netip.Prefix) (ClientScope, string) {
	if !address.IsValid() || address.IsUnspecified() || address.IsMulticast() {
		return ClientScopeUnknown, ""
	}
	if address.IsPrivate() || address.IsLoopback() || address.IsLinkLocalUnicast() {
		return ClientScopePrivate, ""
	}
	for _, value := range declared {
		prefix, err := netip.ParsePrefix(strings.TrimSpace(value))
		if err == nil && prefix.Contains(address) {
			return ClientScopePrivate, ""
		}
	}
	for _, prefix := range local {
		if prefix.Contains(address) {
			return ClientScopePrivate, "，与本机为同一网段 " + prefix.String()
		}
	}
	return ClientScopePublic, ""
}

// ClientAddress 解析一条客户端地址，接受裸 IP（含带 zone 的 `fe80::1%eth0`）
// 与 `ip:port`（含 `[fe80::1%eth0]:5000`）。解析结果一律去掉 zone：内外网判断
// 只看地址本身，而 netip 的 `Prefix.Contains` 对带 zone 的地址**恒为 false** ——
// 留着它，配置里声明的网段就匹配不上这台设备。返回零值表示无法识别。
func ClientAddress(value string) netip.Addr {
	trimmed := strings.TrimSpace(value)
	address, err := netip.ParseAddr(trimmed)
	if err != nil {
		if endpoint, endpointErr := netip.ParseAddrPort(trimmed); endpointErr == nil {
			address = endpoint.Addr()
		}
	}
	address = address.Unmap().WithZone("")
	if address.IsUnspecified() || address.IsMulticast() {
		return netip.Addr{}
	}
	return address
}

func resolveRelative(base, location string) (string, error) {
	parsedBase, err := urlx.Parse(base)
	if err != nil {
		return "", err
	}
	normalizedLocation := location
	if urlx.HasScheme(location) {
		normalized, err := urlx.Normalize(location)
		if err != nil {
			return "", err
		}
		normalizedLocation = normalized
	}
	reference, err := parsedBase.Parse(normalizedLocation)
	if err != nil {
		return "", err
	}
	return reference.String(), nil
}
