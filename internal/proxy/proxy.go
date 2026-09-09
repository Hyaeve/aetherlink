// Package proxy implements the reverse proxy that fronts each upstream media
// server and rewrites STRM media deliveries into 302 redirects.
package proxy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/aetherlink/aetherlink/internal/config"
	"github.com/aetherlink/aetherlink/internal/logx"
	"github.com/aetherlink/aetherlink/internal/resolver"
	"github.com/aetherlink/aetherlink/internal/stats"
	"github.com/aetherlink/aetherlink/internal/strm"
	"github.com/aetherlink/aetherlink/internal/upstream"
)

// hopHeaders are per-connection headers that must not be forwarded.
var hopHeaders = []string{
	"Connection",
	"Proxy-Connection",
	"Keep-Alive",
	"Proxy-Authenticate",
	"Proxy-Authorization",
	"Te",
	"Trailer",
	"Transfer-Encoding",
	"Upgrade",
}

// Server reverse proxies exactly one upstream media server on its own port.
//
// Each upstream owns a port rather than a path prefix, so a player only swaps
// the port number: every path the media server understands arrives unchanged.
type Server struct {
	provider upstream.Provider
	proxy    *httputil.ReverseProxy
	resolver *resolver.Resolver
	stats    *stats.Collector
	redirect config.Redirect

	// streamClient relays bytes when a 302 is not appropriate.
	streamClient *http.Client
	ffmpegPath   string
	audioCache   *audioCache
}

type responseRewriteContextKey struct{}

// New builds the proxy serving one upstream.
func New(provider upstream.Provider, mediaResolver *resolver.Resolver, collector *stats.Collector, redirectCfg config.Redirect) *Server {
	ffmpegPath, _ := exec.LookPath("ffmpeg")
	if ffmpegPath == "" {
		logx.Warnf("[%s] 未找到 FFmpeg，Apple 客户端播放 WMA/AAC 时将继续使用原有 302 或中继逻辑", provider.Name())
	}
	server := &Server{
		provider: provider,
		proxy:    newReverseProxy(provider, mediaResolver),
		resolver: mediaResolver,
		stats:    collector,
		redirect: redirectCfg,
		streamClient: &http.Client{
			// No client timeout: media responses are long lived. Cancellation is
			// driven by the incoming request context instead.
			Transport: &http.Transport{
				Proxy:                 http.ProxyFromEnvironment,
				DialContext:           (&net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
				MaxIdleConns:          128,
				MaxIdleConnsPerHost:   32,
				IdleConnTimeout:       90 * time.Second,
				TLSHandshakeTimeout:   15 * time.Second,
				ExpectContinueTimeout: 1 * time.Second,
			},
		},
		ffmpegPath: ffmpegPath,
		audioCache: newAudioCache(),
	}
	return server
}

// Provider exposes the upstream this proxy serves.
func (s *Server) Provider() upstream.Provider { return s.provider }

// newReverseProxy builds the pass-through proxy for non-media requests.
func newReverseProxy(provider upstream.Provider, mediaResolver *resolver.Resolver) *httputil.ReverseProxy {
	target := provider.BaseURL()
	rewriter, canRewrite := provider.(upstream.ResponseRewriter)

	return &httputil.ReverseProxy{
		Transport: provider.Transport(),
		Rewrite: func(request *httputil.ProxyRequest) {
			userAgent := mediaResolver.EffectiveUserAgentForUpstream(provider.Type(), provider.Name(), request.In.UserAgent())
			request.Out.Header.Set("User-Agent", userAgent)
			if canRewrite && rewriter.WantsResponseRewrite(request.In) {
				// PlaybackInfo 必须以明文 JSON 返回，才能在交给 Emby 客户端前改写。
				request.Out.Header.Set("Accept-Encoding", "identity")
				ctx := context.WithValue(request.Out.Context(), responseRewriteContextKey{}, request.In.URL.Path)
				request.Out = request.Out.WithContext(ctx)
			}
			request.Out.URL.Scheme = target.Scheme
			request.Out.URL.Host = target.Host
			request.Out.Host = target.Host
			request.Out.URL.Path = joinPath(target.Path, request.In.URL.Path)
			request.SetXForwarded()
		},
		ErrorHandler: func(writer http.ResponseWriter, request *http.Request, err error) {
			if errors.Is(err, context.Canceled) {
				return
			}
			logx.Warnf("[proxy] upstream %s error for %s: %v", provider.Name(), request.URL.Path, err)
			writer.WriteHeader(http.StatusBadGateway)
		},
		ModifyResponse: func(response *http.Response) error {
			if !canRewrite {
				return nil
			}
			originalPath, _ := response.Request.Context().Value(responseRewriteContextKey{}).(string)
			if originalPath == "" {
				return nil
			}
			if _, err := rewriter.RewriteResponse(originalPath, response); err != nil {
				// 上游响应异常时不能让播放协商失败；RewriteResponse 已恢复原始响应体。
				logx.Warnf("[%s] PlaybackInfo 改写失败，已返回上游原响应：%v", provider.Name(), err)
			}
			return nil
		},
		// FlushInterval -1 streams responses without buffering, which matters for
		// progressive audio and SSE-style endpoints.
		FlushInterval: -1,
	}
}

// ServeHTTP proxies a request to the upstream, intercepting media deliveries.
func (s *Server) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if s.redirect.IsBlockedClientUserAgentForUpstream(s.provider.Type(), s.provider.Name(), request.UserAgent()) {
		logx.Warnf("[%s] UA 屏蔽：拒绝 UA %q 的请求 %s %s（403）", s.provider.Name(), request.UserAgent(), request.Method, request.URL.Path)
		writer.Header().Set("Cache-Control", "no-store")
		http.Error(writer, "User-Agent blocked", http.StatusForbidden)
		return
	}
	ref, isMedia := s.provider.Match(request)
	if !isMedia {
		// 没被拦截的请求量很大（界面、封面、进度同步），所以只在 debug 级别
		// 记一行。排查「明明在播放却完全不 302」时，这一行是唯一能回答
		// 「播放请求到底长什么样、为什么没被识别成媒体」的证据。
		if rewriter, ok := s.provider.(upstream.ResponseRewriter); ok && rewriter.WantsResponseRewrite(request) {
			logx.Infof("[%s] 拦截播放协商 %s %s（将检查并改写 STRM 媒体源的直放能力）", s.provider.Name(), request.Method, request.URL.RequestURI())
		} else if isEmbyHLSRequest(s.provider, request.URL.Path) {
			logx.Infof("[%s] 检测到 Emby HLS 转码请求 %s %s（HLS 清单或分片本身不能 302；若前一条 PlaybackInfo 日志提示保留转码，这是客户端兼容性回退）", s.provider.Name(), request.Method, request.URL.RequestURI())
		} else if looksLikeMedia(request.URL.Path) {
			logx.Infof("[%s] 未识别的疑似播放请求 %s %s（没有匹配到媒体路由，已原样转发）", s.provider.Name(), request.Method, request.URL.RequestURI())
		} else {
			logx.Debugf("[%s] 透传 %s %s", s.provider.Name(), request.Method, request.URL.RequestURI())
		}
		s.proxy.ServeHTTP(writer, request)
		return
	}
	logx.Infof("[%s] 拦截播放请求 %s %s（类型 %s，item=%s file=%s）", s.provider.Name(), request.Method, request.URL.RequestURI(), ref.Kind, ref.ItemID, ref.FileID)
	s.serveMedia(writer, request, ref)
}

func isEmbyHLSRequest(provider upstream.Provider, requestPath string) bool {
	if provider.Type() != config.UpstreamEmby {
		return false
	}
	lowered := strings.ToLower(requestPath)
	return strings.Contains(lowered, "/videos/") &&
		(strings.Contains(lowered, "/hls") || strings.Contains(lowered, "/master")) &&
		(strings.HasSuffix(lowered, ".ts") || strings.HasSuffix(lowered, ".m3u8"))
}

// mediaExtensions 是播放请求路径里常见的媒体扩展名，用来在「没被拦截」时判断
// 这条请求是否值得升级成 info 日志。
var mediaExtensions = []string{
	".m4b", ".m4a", ".mp3", ".flac", ".opus", ".ogg", ".oga", ".aac", ".wav",
	".wma", ".webm", ".webma", ".mka", ".mkv", ".mp4", ".avi", ".ts", ".strm",
}

// looksLikeMedia 判断一条未匹配的请求是否很可能是播放请求。
// 命中说明拦截规则漏了这条路由，这正是「反代通了但不 302」的典型原因。
func looksLikeMedia(requestPath string) bool {
	lowered := strings.ToLower(requestPath)
	for _, keyword := range []string{"/stream", "/original", "/universal", "/download", "/track/", "/file/", "/playbackinfo"} {
		if strings.Contains(lowered, keyword) {
			return true
		}
	}
	for _, extension := range mediaExtensions {
		if strings.HasSuffix(lowered, extension) {
			return true
		}
	}
	return false
}

// serveMedia resolves a media request and answers with a redirect, a relayed
// stream, a local file, or a plain upstream pass-through.
func (s *Server) serveMedia(writer http.ResponseWriter, request *http.Request, ref upstream.MediaRef) {
	started := time.Now()
	event := stats.Event{
		Upstream:  s.provider.Name(),
		Path:      request.URL.Path,
		ItemID:    ref.ItemID,
		FileID:    ref.FileID,
		Client:    clientIP(request),
		UserAgent: request.UserAgent(),
	}
	event.EffectiveUserAgent = s.resolver.EffectiveUserAgentForUpstream(s.provider.Type(), s.provider.Name(), request.UserAgent())

	// finish 是所有出口的唯一收尾：记录事件之后必定打一行日志。
	// 之前只有 s.stats.Record，成功的 302 与透传一行日志都没有，
	// 于是「不能 302」这个问题在容器日志与界面日志里完全不可观测。
	finish := func(outcome stats.Outcome, note string) {
		event.Outcome = outcome
		event.Duration = time.Since(started)
		s.stats.Record(event)
		s.logOutcome(event, note)
	}

	if !s.provider.HasCredentials() {
		// Without an API key AetherLink cannot ask the upstream where the media
		// lives, so behave like a plain reverse proxy.
		event.Error = upstream.ErrNoAPIKey.Error()
		finish(stats.OutcomePassthrough, "未配置 API 密钥，无法向上游查询媒体位置")
		s.proxy.ServeHTTP(writer, request)
		return
	}

	resolution, cacheSource, cacheTTL, err := s.resolver.ResolveWithSource(request.Context(), s.provider, ref, request.UserAgent())
	event.CacheSource = string(cacheSource)
	event.CacheHit = cacheSource != resolver.CacheSourceMiss
	event.CacheTTLSeconds = cacheTTLSeconds(cacheTTL)
	if err != nil {
		if errors.Is(err, upstream.ErrDirectPlayUnsupported) {
			event.Error = err.Error()
			finish(stats.OutcomePassthrough, "Emby 判定当前客户端不支持原始文件，本次交回上游直流或转码")
			s.proxy.ServeHTTP(writer, request)
			return
		}
		if errors.Is(err, resolver.ErrNotStrm) {
			// Regular media file: let the upstream serve it.
			finish(stats.OutcomePassthrough, "不是 strm 指针，交给上游自己播")
			s.proxy.ServeHTTP(writer, request)
			return
		}
		if errors.Is(err, resolver.ErrPointerUnavailable) {
			// 指针确实是 strm，但这个容器读不到它（媒体目录没挂进来）。
			// 上游自己能读，退回透传让播放继续，同时把原因记进日志与事件，
			// 避免用户只看到「能播但没有 302」而查不出为什么。
			event.Error = err.Error()
			finish(stats.OutcomePassthrough, "读不到 strm 指针文件，本次退回透传；把上游的媒体目录也挂进 AetherLink 后即可 302")
			s.proxy.ServeHTTP(writer, request)
			return
		}
		// 解析失败也不能让播放中断。上游本来就能自己处理这个请求，
		// 502 只会把「AetherLink 装上之后反而播不了」变成事实。
		// 失败原因照样记进事件与日志，方便在界面上直接看到。
		event.Error = err.Error()
		finish(stats.OutcomeError, "解析 strm 目标失败，本次退回透传")
		s.proxy.ServeHTTP(writer, request)
		return
	}

	event.MediaPath = resolution.ContainerPath
	if resolution.Target != nil {
		event.Kind = string(resolution.Target.Kind)
	}

	if resolution.Target != nil && resolution.Target.Type == strm.TargetLocal {
		if s.shouldTranscode(request, resolution) {
			event.Target = resolution.Target.Path
			status, cacheHit, err := s.relayTranscoded(writer, request, resolution.Target.Path, resolution)
			event.StatusCode = status
			if err != nil {
				event.Error = err.Error()
				finish(stats.OutcomeError, "Apple 客户端音频格式不兼容，FFmpeg 音频适配失败")
				return
			}
			finish(stats.OutcomeTranscode, audioAdaptationNote(cacheHit))
			return
		}
		event.Target = resolution.Target.Path
		event.StatusCode = http.StatusOK
		finish(stats.OutcomeLocalFile, cacheNote(event)+"；指针指向本容器内的文件，直接读盘返回")
		s.serveLocalFile(writer, request, resolution.Target.Path)
		return
	}

	playURL := resolution.PlayURL()
	event.Target = playURL
	if s.shouldTranscode(request, resolution) {
		status, cacheHit, err := s.relayTranscoded(writer, request, playURL, resolution)
		event.StatusCode = status
		if err != nil {
			event.Error = err.Error()
			finish(stats.OutcomeError, "Apple 客户端音频格式不兼容，FFmpeg 音频适配失败")
			return
		}
		finish(stats.OutcomeTranscode, audioAdaptationNote(cacheHit))
		return
	}

	if s.resolver.ShouldRedirectWith(resolution, s.redirect) {
		event.StatusCode = http.StatusFound
		finish(stats.OutcomeRedirect, cacheNote(event)+"；已 302 到真实地址")
		// 302 keeps the request method for GET/HEAD and is what media players
		// (Emby clients, ABS apps, browsers) handle most reliably.
		writer.Header().Set("Location", playURL)
		writer.Header().Set("Cache-Control", "no-store")
		writer.WriteHeader(http.StatusFound)
		return
	}

	// 走到这里说明目标解析出来了，但当前的 302 策略不允许跳转，
	// 只能由 AetherLink 中继字节。把具体原因写清楚，否则用户会以为 302 坏了。
	status, err := s.relayRemote(writer, request, playURL)
	event.StatusCode = status
	if err != nil {
		event.Error = err.Error()
		finish(stats.OutcomeError, "中继 strm 目标失败")
		return
	}
	finish(stats.OutcomeProxyStream, cacheNote(event)+"；按 302 策略不跳转，改由 AetherLink 中继："+s.noRedirectReason(resolution))
}

// noRedirectReason 说明为什么一个已经解析成功的目标没有被 302 出去。
func (s *Server) noRedirectReason(resolution *resolver.Resolution) string {
	if resolution == nil || resolution.Target == nil {
		return "没有解析出可跳转的地址"
	}
	if resolution.Target.Type != strm.TargetRemote {
		return "目标不是 http 地址"
	}
	switch s.redirect.Mode {
	case config.RedirectNever:
		return "302 模式为 never"
	case config.RedirectPublic:
		return "302 模式为 public，而目标是内网地址"
	case config.RedirectPrivate:
		return "302 模式为 private，而目标不是内网地址"
	default:
		return "302 模式未启用"
	}
}

// logOutcome 把一条播放请求的处理结果写进日志。302 与透传都记成 info，
// 因为用户排查的正是这两条路的区别；失败记成 warn/error。
func (s *Server) logOutcome(event stats.Event, note string) {
	milliseconds := event.Duration.Milliseconds()
	switch event.Outcome {
	case stats.OutcomeRedirect:
		logx.Infof("[%s] 302 %s -> %s（类型 %s，%dms，%s，%s）", event.Upstream, event.Path, event.Target, displayKind(event.Kind), milliseconds, cacheNote(event), userAgentNote(event))
	case stats.OutcomeLocalFile:
		logx.Infof("[%s] 本地直读 %s -> %s（%dms）：%s；%s", event.Upstream, event.Path, event.Target, milliseconds, note, userAgentNote(event))
	case stats.OutcomeProxyStream:
		logx.Infof("[%s] 中继 %s -> %s（状态 %d，%dms）：%s；%s", event.Upstream, event.Path, event.Target, event.StatusCode, milliseconds, note, userAgentNote(event))
	case stats.OutcomeTranscode:
		logx.Infof("[%s] 转码中继 %s -> %s（状态 %d，%dms）：%s；%s", event.Upstream, event.Path, event.Target, event.StatusCode, milliseconds, note, userAgentNote(event))
	case stats.OutcomePassthrough:
		if event.Error != "" {
			logx.Warnf("[%s] 透传 %s（%dms）：%s；UA：%s；原因：%s", event.Upstream, event.Path, milliseconds, note, userAgentNote(event), event.Error)
			return
		}
		logx.Infof("[%s] 透传 %s（%dms）：%s；UA：%s", event.Upstream, event.Path, milliseconds, note, userAgentNote(event))
	default:
		logx.Errorf("[%s] 失败 %s（%dms）：%s；UA：%s；原因：%s", event.Upstream, event.Path, milliseconds, note, userAgentNote(event), event.Error)
	}
}

func (s *Server) shouldTranscode(request *http.Request, resolution *resolver.Resolution) bool {
	return s.ffmpegPath != "" && isAppleAudioClient(request.UserAgent()) && isIncompatibleAudio(resolution)
}

func isAppleAudioClient(userAgent string) bool {
	lowered := strings.ToLower(userAgent)
	return strings.Contains(lowered, "iphone") || strings.Contains(lowered, "ipad") ||
		strings.Contains(lowered, "ipod") || strings.Contains(lowered, "applecoremedia") ||
		strings.Contains(lowered, "ios")
}

func isIncompatibleAudio(resolution *resolver.Resolution) bool {
	if resolution == nil || resolution.Target == nil {
		return false
	}
	name := resolution.Target.Filename
	if resolution.Target.Type == strm.TargetLocal {
		name = resolution.Target.Path
	} else if resolution.Target.URL != "" {
		name = resolution.Target.URL + " " + name
	}
	extension := strings.ToLower(path.Ext(name))
	return extension == ".wma" || extension == ".aac"
}

func (s *Server) relayTranscoded(writer http.ResponseWriter, request *http.Request, source string, resolution *resolver.Resolution) (int, bool, error) {
	mode := audioAdaptationMode(resolution, source)
	key := audioCacheKey(source, mode, s.resolver.EffectiveUserAgentForUpstream(s.provider.Type(), s.provider.Name(), request.UserAgent()))
	bookName := audioCacheBookName(resolution, source)
	filename := audioCacheFilename(resolution, source)
	buildContext := context.WithoutCancel(request.Context())
	if s.redirect.StreamTimeout > 0 {
		var cancel context.CancelFunc
		buildContext, cancel = context.WithTimeout(buildContext, s.redirect.StreamTimeout)
		defer cancel()
	}

	cachePath, cacheHit, err := s.audioCache.getOrCreate(buildContext, bookName, key, filename, func(destination string) error {
		return s.transcodeToFile(buildContext, request, source, resolution, destination)
	})
	if err != nil {
		return http.StatusBadGateway, cacheHit, err
	}

	status, err := s.serveCachedAudio(writer, request, cachePath)
	if err != nil {
		return status, cacheHit, err
	}
	if cacheHit {
		logx.Infof("[%s] 音频适配缓存命中 %s：已通过本地 M4A 文件中继", s.provider.Name(), request.URL.Path)
	} else {
		logx.Infof("[%s] 音频适配缓存首次生成 %s：已通过本地 M4A 文件中继", s.provider.Name(), request.URL.Path)
	}
	return status, cacheHit, nil
}

func (s *Server) transcodeToFile(ctx context.Context, request *http.Request, source string, resolution *resolver.Resolution, destination string) error {
	inputFormat := transcodeInputFormat(resolution, source)
	mode := audioAdaptationMode(resolution, source)
	codecArgs, label := audioAdaptationArgs(mode)
	args := []string{"-hide_banner", "-loglevel", "error", "-nostdin", "-stats_period", "5", "-progress", "pipe:2", "-analyzeduration", "1M", "-probesize", "1M", "-y"}
	if strings.HasPrefix(source, "http://") || strings.HasPrefix(source, "https://") {
		args = append(args, "-user_agent", s.resolver.EffectiveUserAgentForUpstream(s.provider.Type(), s.provider.Name(), request.UserAgent()))
	}
	if inputFormat != "" {
		args = append(args, "-f", inputFormat)
	}
	args = append(args, "-i", source, "-map", "0:a:0", "-vn")
	args = append(args, codecArgs...)
	args = append(args, "-f", "mp4", "-movflags", "+faststart", destination)

	command := exec.CommandContext(ctx, s.ffmpegPath, args...)
	progress := newTranscodeProgress(s.provider.Name(), request.URL.Path)
	command.Stderr = progress
	logx.Infof("[%s] FFmpeg 音频适配开始 %s：%s，输入格式 %s；临时文件缓存中；UA：%s", s.provider.Name(), request.URL.Path, label, displayInputFormat(inputFormat), s.resolver.EffectiveUserAgentForUpstream(s.provider.Type(), s.provider.Name(), request.UserAgent()))
	if err := command.Run(); err != nil {
		return progress.errorWith(err)
	}
	info, err := os.Stat(destination)
	if err != nil {
		return fmt.Errorf("ffmpeg output unavailable: %w", err)
	}
	if info.Size() == 0 {
		return errors.New("ffmpeg output is empty")
	}
	progress.finish()
	return nil
}

func (s *Server) serveCachedAudio(writer http.ResponseWriter, request *http.Request, filename string) (int, error) {
	file, err := os.Open(filename)
	if err != nil {
		return http.StatusBadGateway, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || info.IsDir() {
		if err == nil {
			err = errors.New("audio cache path is a directory")
		}
		return http.StatusBadGateway, err
	}
	writer.Header().Set("Content-Type", "audio/mp4")
	writer.Header().Set("Content-Disposition", `inline; filename="aetherlink.m4a"`)
	writer.Header().Set("Cache-Control", "private, max-age=0, must-revalidate")
	http.ServeContent(writer, request, "aetherlink.m4a", info.ModTime(), file)
	return http.StatusOK, nil
}

func audioAdaptationNote(cacheHit bool) string {
	if cacheHit {
		return "Apple 客户端强制中继，命中本地 M4A 缓存"
	}
	return "Apple 客户端强制中继，首次生成本地 M4A 缓存"
}

const audioCacheIdle = 2 * time.Hour
const defaultAudioCacheDir = "/cache"

type audioCache struct {
	mu      sync.Mutex
	dir     string
	entries map[string]*audioCacheEntry
}

type audioCacheEntry struct {
	path     string
	ready    chan struct{}
	err      error
	lastUsed time.Time
}

func newAudioCache() *audioCache {
	directory := strings.TrimSpace(os.Getenv("AETHERLINK_AUDIO_CACHE_DIR"))
	if directory == "" {
		directory = defaultAudioCacheDir
	}
	cache := &audioCache{
		dir:     directory,
		entries: make(map[string]*audioCacheEntry),
	}
	go cache.cleanupLoop()
	return cache
}

func audioCacheKey(source, mode, userAgent string) string {
	sum := sha256.Sum256([]byte(source + "\x00" + mode + "\x00" + userAgent))
	return hex.EncodeToString(sum[:])
}

func audioCacheBookName(resolution *resolver.Resolution, source string) string {
	candidates := make([]string, 0, 4)
	if resolution != nil {
		candidates = append(candidates, resolution.ContainerPath)
		if resolution.Target != nil {
			candidates = append(candidates, resolution.Target.Path, resolution.Target.URL)
		}
	}
	candidates = append(candidates, source)
	for _, candidate := range candidates {
		candidate = strings.TrimSpace(candidate)
		if candidate == "" {
			continue
		}
		if parsed, err := url.Parse(candidate); err == nil && parsed.Path != "" {
			candidate = parsed.Path
		}
		candidate = path.Clean(candidate)
		book := path.Base(path.Dir(candidate))
		if book != "." && book != "/" && book != "" {
			return sanitizeAudioCacheName(book)
		}
	}
	if resolution != nil && resolution.Target != nil && resolution.Target.Filename != "" {
		return sanitizeAudioCacheName(strings.TrimSuffix(resolution.Target.Filename, path.Ext(resolution.Target.Filename)))
	}
	return "未命名"
}

func audioCacheFilename(resolution *resolver.Resolution, source string) string {
	name := ""
	if resolution != nil && resolution.Target != nil {
		name = resolution.Target.Filename
		if resolution.Target.Type == strm.TargetLocal && resolution.Target.Path != "" {
			name = path.Base(resolution.Target.Path)
		}
	}
	if name == "" {
		if parsed, err := url.Parse(source); err == nil {
			name = path.Base(parsed.Path)
		}
	}
	name = strings.TrimSpace(name)
	if name == "" || name == "." || name == "/" {
		name = "audio"
	}
	name = strings.TrimSuffix(name, path.Ext(name))
	name = sanitizeAudioCacheName(name)
	return name + ".m4a"
}

func sanitizeAudioCacheName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" || name == "." || name == ".." {
		return "未命名"
	}
	name = strings.Map(func(character rune) rune {
		if unicode.IsControl(character) || strings.ContainsRune(`<>:"/\\|?*`, character) {
			return '_'
		}
		return character
	}, name)
	name = strings.Trim(name, " .")
	if name == "" || name == "." || name == ".." {
		return "未命名"
	}
	if len([]rune(name)) > 96 {
		name = string([]rune(name)[:96])
	}
	return name
}

func (c *audioCache) cacheDirectory(bookName string) string {
	return filepath.Join(c.dir, sanitizeAudioCacheName(bookName))
}

func (c *audioCache) cachePath(bookName, key, filename string) string {
	return filepath.Join(c.cacheDirectory(bookName), key, filename)
}

func (c *audioCache) getOrCreate(ctx context.Context, bookName, key, filename string, build func(string) error) (string, bool, error) {
	now := time.Now()
	bookDirectory := c.cacheDirectory(bookName)
	variantDirectory := filepath.Join(bookDirectory, key)
	if err := os.MkdirAll(variantDirectory, 0o755); err != nil {
		return "", false, fmt.Errorf("create audio cache directory: %w", err)
	}

	c.mu.Lock()
	c.cleanupLocked(now)
	if entry, ok := c.entries[key]; ok {
		ready := entry.ready
		c.mu.Unlock()
		select {
		case <-ready:
			if entry.err != nil {
				return "", true, entry.err
			}
			c.mu.Lock()
			entry.lastUsed = time.Now()
			c.mu.Unlock()
			_ = touchAudioCacheFile(entry.path)
			return entry.path, true, nil
		case <-ctx.Done():
			return "", true, ctx.Err()
		}
	}

	finalPath := c.cachePath(bookName, key, filename)
	if info, err := os.Stat(finalPath); err == nil && !info.IsDir() && info.Size() > 0 {
		entry := &audioCacheEntry{path: finalPath, ready: make(chan struct{}), lastUsed: now}
		close(entry.ready)
		c.entries[key] = entry
		c.mu.Unlock()
		_ = touchAudioCacheFile(finalPath)
		return finalPath, true, nil
	}

	temporary, err := os.CreateTemp(variantDirectory, ".aetherlink-*.part")
	if err != nil {
		c.mu.Unlock()
		return "", false, fmt.Errorf("create audio cache file: %w", err)
	}
	temporaryPath := temporary.Name()
	if err := temporary.Close(); err != nil {
		c.mu.Unlock()
		_ = os.Remove(temporaryPath)
		return "", false, fmt.Errorf("close audio cache file: %w", err)
	}
	entry := &audioCacheEntry{path: finalPath, ready: make(chan struct{}), lastUsed: now}
	c.entries[key] = entry
	c.mu.Unlock()

	buildErr := build(temporaryPath)
	if buildErr == nil {
		buildErr = os.Rename(temporaryPath, finalPath)
	}
	if buildErr != nil {
		_ = os.Remove(temporaryPath)
		_ = os.Remove(finalPath)
	}

	c.mu.Lock()
	entry.err = buildErr
	if buildErr != nil {
		delete(c.entries, key)
	}
	close(entry.ready)
	c.mu.Unlock()
	if buildErr != nil {
		return "", false, buildErr
	}
	_ = touchAudioCacheFile(finalPath)
	return finalPath, false, nil
}

func touchAudioCacheFile(filename string) error {
	now := time.Now()
	return os.Chtimes(filename, now, now)
}

func (c *audioCache) cleanupLoop() {
	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		c.cleanup()
	}
}

func (c *audioCache) cleanup() {
	now := time.Now()
	c.mu.Lock()
	c.cleanupLocked(now)
	c.mu.Unlock()

	cutoff := now.Add(-audioCacheIdle)
	_ = filepath.WalkDir(c.dir, func(filename string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return nil
		}
		if !strings.HasSuffix(entry.Name(), ".m4a") && !strings.HasSuffix(entry.Name(), ".part") {
			return nil
		}
		info, infoErr := entry.Info()
		if infoErr == nil && info.ModTime().Before(cutoff) {
			_ = os.Remove(filename)
		}
		return nil
	})
}

func (c *audioCache) cleanupLocked(now time.Time) {
	cutoff := now.Add(-audioCacheIdle)
	for key, entry := range c.entries {
		select {
		case <-entry.ready:
			if entry.err == nil && entry.lastUsed.Before(cutoff) {
				_ = os.Remove(entry.path)
				delete(c.entries, key)
			}
		default:
		}
	}
}

type transcodeProgress struct {
	mu          sync.Mutex
	diagnostics bytes.Buffer
	pending     string
	upstream    string
	path        string
	started     time.Time
	lastLog     time.Time
	outTime     time.Duration
	speed       string
}

func newTranscodeProgress(upstreamName, requestPath string) *transcodeProgress {
	return &transcodeProgress{
		upstream: upstreamName,
		path:     requestPath,
		started:  time.Now(),
	}
}

func (p *transcodeProgress) Write(data []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.pending += string(data)
	for {
		index := strings.IndexByte(p.pending, '\n')
		if index < 0 {
			break
		}
		line := strings.TrimSpace(p.pending[:index])
		p.pending = p.pending[index+1:]
		p.consume(line)
	}
	return len(data), nil
}

func (p *transcodeProgress) consume(line string) {
	if !isProgressLine(line) {
		p.diagnostics.WriteString(line)
		p.diagnostics.WriteByte('\n')
	}
	if strings.HasPrefix(line, "out_time_ms=") {
		value, err := strconv.ParseInt(strings.TrimPrefix(line, "out_time_ms="), 10, 64)
		if err == nil {
			p.outTime = time.Duration(value) * time.Microsecond
		}
	}
	if strings.HasPrefix(line, "speed=") {
		p.speed = strings.TrimPrefix(line, "speed=")
	}
	if line == "progress=continue" && time.Since(p.lastLog) >= 5*time.Second {
		p.lastLog = time.Now()
		logx.Infof("[%s] FFmpeg 转码进度 %s：已输出 %s，速度 %s，已运行 %s", p.upstream, p.path, formatDuration(p.outTime), displaySpeed(p.speed), formatDuration(time.Since(p.started)))
	}
}

func (p *transcodeProgress) finish() {
	p.mu.Lock()
	defer p.mu.Unlock()
	logx.Infof("[%s] FFmpeg 转码完成 %s：输出 %s，耗时 %s", p.upstream, p.path, formatDuration(p.outTime), formatDuration(time.Since(p.started)))
}

func (p *transcodeProgress) errorWith(commandErr error) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	message := strings.TrimSpace(p.diagnostics.String())
	if message != "" {
		return fmt.Errorf("ffmpeg: %s: %w", message, commandErr)
	}
	return fmt.Errorf("ffmpeg: %w", commandErr)
}

func isProgressLine(line string) bool {
	for _, prefix := range []string{
		"frame=", "fps=", "stream_", "bitrate=", "total_size=", "out_time_us=",
		"out_time_ms=", "out_time=", "dup_frames=", "drop_frames=", "speed=", "progress=",
	} {
		if strings.HasPrefix(line, prefix) {
			return true
		}
	}
	return false
}

func transcodeInputFormat(resolution *resolver.Resolution, source string) string {
	name := source
	if resolution != nil && resolution.Target != nil {
		if resolution.Target.Filename != "" {
			name = resolution.Target.Filename
		}
		if resolution.Target.Type == strm.TargetLocal && resolution.Target.Path != "" {
			name = resolution.Target.Path
		}
	}
	switch strings.ToLower(path.Ext(name)) {
	case ".aac":
		return "aac"
	case ".wma":
		return "asf"
	default:
		return ""
	}
}

func audioAdaptationMode(resolution *resolver.Resolution, source string) string {
	name := source
	if resolution != nil && resolution.Target != nil {
		if resolution.Target.Filename != "" {
			name = resolution.Target.Filename
		}
		if resolution.Target.Type == strm.TargetLocal && resolution.Target.Path != "" {
			name = resolution.Target.Path
		}
	}
	switch strings.ToLower(path.Ext(name)) {
	case ".aac":
		return "aac-remux"
	case ".wma":
		return "wma-transcode"
	default:
		return ""
	}
}

func audioAdaptationArgs(mode string) ([]string, string) {
	if mode == "aac-remux" {
		return []string{"-c:a", "copy", "-bsf:a", "aac_adtstoasc"}, "AAC 重新封装为 M4A"
	}
	return []string{"-c:a", "aac", "-profile:a", "aac_low", "-b:a", "128k"}, "WMA 转 AAC-LC M4A"
}

func displayInputFormat(format string) string {
	if format == "" {
		return "自动识别"
	}
	return format
}

func displaySpeed(speed string) string {
	if speed == "" || speed == "N/A" {
		return "计算中"
	}
	return speed
}

func formatDuration(value time.Duration) string {
	if value < time.Second {
		return value.Round(time.Millisecond).String()
	}
	minutes := int(value / time.Minute)
	seconds := int((value % time.Minute) / time.Second)
	if minutes > 0 {
		return fmt.Sprintf("%dm%02ds", minutes, seconds)
	}
	return fmt.Sprintf("%ds", seconds)
}

func displayKind(kind string) string {
	if kind == "" {
		return "未知"
	}
	return kind
}

func cacheWord(event stats.Event) string {
	switch event.CacheSource {
	case string(resolver.CacheSourceRestored):
		return "重启恢复命中"
	case string(resolver.CacheSourceHit):
		return "缓存命中"
	default:
		return "首次获取"
	}
}

func cacheTTLSeconds(value time.Duration) int64 {
	if value <= 0 {
		return 0
	}
	return int64((value + time.Second - 1) / time.Second)
}

func formatCacheTTL(seconds int64) string {
	if seconds <= 0 {
		return "不缓存"
	}
	minutesTotal := (seconds + 59) / 60
	if seconds < 3600 {
		return fmt.Sprintf("%dmin", minutesTotal)
	}
	hours := minutesTotal / 60
	minutes := minutesTotal % 60
	if minutes == 0 {
		return fmt.Sprintf("%dh", hours)
	}
	return fmt.Sprintf("%dh %dmin", hours, minutes)
}

func cacheNote(event stats.Event) string {
	return "直链" + cacheWord(event) + "，缓存有效期" + formatCacheTTL(event.CacheTTLSeconds)
}

func userAgentNote(event stats.Event) string {
	clientUserAgent := event.UserAgent
	if clientUserAgent == "" {
		clientUserAgent = "空"
	}
	if event.EffectiveUserAgent == "" || event.EffectiveUserAgent == event.UserAgent {
		return fmt.Sprintf("UA %q", event.EffectiveUserAgent)
	}
	return fmt.Sprintf("客户端 UA %q，实际 UA %q", clientUserAgent, event.EffectiveUserAgent)
}

// relayRemote streams the remote target through AetherLink, preserving Range
// semantics so seeking keeps working.
func (s *Server) relayRemote(writer http.ResponseWriter, request *http.Request, target string) (int, error) {
	ctx := request.Context()
	if s.redirect.StreamTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, s.redirect.StreamTimeout)
		defer cancel()
	}

	outbound, err := http.NewRequestWithContext(ctx, request.Method, target, nil)
	if err != nil {
		http.Error(writer, "invalid strm target", http.StatusBadGateway)
		return http.StatusBadGateway, err
	}
	outbound.Header.Set("User-Agent", s.resolver.EffectiveUserAgentForUpstream(s.provider.Type(), s.provider.Name(), request.UserAgent()))
	for _, header := range []string{"Range", "If-Range", "If-Modified-Since", "If-None-Match", "Accept", "Accept-Encoding"} {
		if value := request.Header.Get(header); value != "" {
			outbound.Header.Set(header, value)
		}
	}

	response, err := s.streamClient.Do(outbound)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return 499, err
		}
		logx.Errorf("[proxy] relay request failed for %s: %v", target, err)
		http.Error(writer, "strm backend unreachable", http.StatusBadGateway)
		return http.StatusBadGateway, err
	}
	defer response.Body.Close()

	copyResponseHeaders(writer.Header(), response.Header)
	if writer.Header().Get("Content-Type") == "" || writer.Header().Get("Content-Type") == "application/octet-stream" {
		if mimeType := mimeTypeForURL(target); mimeType != "" {
			writer.Header().Set("Content-Type", mimeType)
		}
	}
	writer.WriteHeader(response.StatusCode)
	if request.Method == http.MethodHead {
		return response.StatusCode, nil
	}
	if _, err := io.Copy(writer, response.Body); err != nil {
		if !errors.Is(err, context.Canceled) {
			logx.Debugf("[proxy] relay copy ended early for %s: %v", target, err)
		}
		return response.StatusCode, nil
	}
	return response.StatusCode, nil
}

// serveLocalFile serves a container-local strm target directly.
func (s *Server) serveLocalFile(writer http.ResponseWriter, request *http.Request, localPath string) {
	file, err := os.Open(localPath)
	if err != nil {
		logx.Errorf("[proxy] open local strm target %s: %v", localPath, err)
		http.Error(writer, "strm target unavailable", http.StatusBadGateway)
		return
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil || info.IsDir() {
		http.Error(writer, "strm target unavailable", http.StatusBadGateway)
		return
	}
	if mimeType := mimeTypeForURL(localPath); mimeType != "" {
		writer.Header().Set("Content-Type", mimeType)
	}
	// http.ServeContent handles Range, If-Modified-Since and HEAD for us.
	http.ServeContent(writer, request, path.Base(localPath), info.ModTime(), file)
}

func joinPath(basePath, requestPath string) string {
	basePath = strings.TrimRight(basePath, "/")
	if basePath == "" {
		return requestPath
	}
	loweredBase := strings.ToLower(basePath)
	loweredRequest := strings.ToLower(requestPath)
	if loweredRequest == loweredBase || strings.HasPrefix(loweredRequest, loweredBase+"/") {
		return requestPath
	}
	if requestPath == "/" {
		return basePath
	}
	return basePath + requestPath
}

func copyResponseHeaders(destination, source http.Header) {
	for key, values := range source {
		if isHopHeader(key) {
			continue
		}
		for _, value := range values {
			destination.Add(key, value)
		}
	}
}

func isHopHeader(name string) bool {
	for _, header := range hopHeaders {
		if strings.EqualFold(header, name) {
			return true
		}
	}
	return false
}

// mimeTypeForURL guesses a media content type from a URL or path extension.
// Playable extensions are listed explicitly because Go's mime package returns
// nothing useful for m4b and several audiobook containers.
func mimeTypeForURL(target string) string {
	candidate := target
	if parsed, err := url.Parse(target); err == nil && parsed.Path != "" {
		candidate = parsed.Path
	}
	extension := strings.ToLower(path.Ext(candidate))
	switch extension {
	case ".m4b", ".m4a", ".mp4", ".m4p", ".m4v":
		return "audio/mp4"
	case ".mp3":
		return "audio/mpeg"
	case ".flac":
		return "audio/flac"
	case ".opus":
		return "audio/opus"
	case ".ogg", ".oga":
		return "audio/ogg"
	case ".aac":
		return "audio/aac"
	case ".wav":
		return "audio/wav"
	case ".wma":
		return "audio/x-ms-wma"
	case ".webm", ".webma":
		return "audio/webm"
	case ".mka":
		return "audio/x-matroska"
	case ".mkv":
		return "video/x-matroska"
	case ".avi":
		return "video/x-msvideo"
	case ".ts":
		return "video/mp2t"
	default:
		return ""
	}
}

func clientIP(request *http.Request) string {
	host, _, err := net.SplitHostPort(request.RemoteAddr)
	if err != nil {
		return request.RemoteAddr
	}
	return host
}
