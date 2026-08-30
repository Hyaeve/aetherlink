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
	"os"
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
	return &Resolver{
		cache:    newLRUCache(cacheCfg.TTL, cacheCfg.MaxSize),
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
func (r *Resolver) Resolve(ctx context.Context, provider upstream.Provider, ref upstream.MediaRef, userAgent string) (resolution *Resolution, cacheHit bool, err error) {
	key := ref.CacheKey(provider.Name())
	if cached, ok := r.cache.get(key); ok {
		return cached, true, nil
	}

	// Collapse concurrent requests for the same track. Players routinely open
	// several ranged requests at once when seeking.
	r.inflightMu.Lock()
	if call, ok := r.inflight[key]; ok {
		r.inflightMu.Unlock()
		select {
		case <-call.done:
			return call.resolution, false, call.err
		case <-ctx.Done():
			return nil, false, ctx.Err()
		}
	}
	call := &inflightCall{done: make(chan struct{})}
	r.inflight[key] = call
	r.inflightMu.Unlock()

	call.resolution, call.err = r.resolveUncached(ctx, provider, ref, userAgent)
	close(call.done)

	r.inflightMu.Lock()
	delete(r.inflight, key)
	r.inflightMu.Unlock()

	if call.err == nil {
		r.cache.put(key, call.resolution)
	}
	return call.resolution, false, call.err
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
			finalURL, hops, err := r.followRedirects(ctx, resolution.Target.URL, userAgent)
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

func (r *Resolver) effectiveUserAgent(clientUserAgent string) string {
	if r.config.ShouldForwardUserAgent() && strings.TrimSpace(clientUserAgent) != "" {
		return clientUserAgent
	}
	if r.config.FallbackUserAgent != "" {
		return r.config.FallbackUserAgent
	}
	return "AetherLink"
}

// ShouldRedirect decides between answering with a 302 and relaying the bytes.
func (r *Resolver) ShouldRedirect(resolution *Resolution) bool {
	return r.ShouldRedirectWith(resolution, r.config)
}

// ShouldRedirectWith applies the policy selected for one upstream.
func (r *Resolver) ShouldRedirectWith(resolution *Resolution, redirectCfg config.Redirect) bool {
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
	case config.RedirectPrivate:
		return urlx.IsPrivateHost(playURL)
	default:
		if !redirectCfg.PublicTargetsAllowed() && !urlx.IsPrivateHost(playURL) {
			return false
		}
		return true
	}
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
