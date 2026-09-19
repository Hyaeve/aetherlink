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
	"sort"
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
	"github.com/aetherlink/aetherlink/internal/urlx"
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

	// relayExempt 是卡片上的「不中继的客户端」名单：命中的 UA 即使卡片选的是
	// 「始终中继」也要把直链交出去，因为这类播放器只在直链上能播。
	relayExempt []string
}

type responseRewriteContextKey struct{}

// New builds the proxy serving one upstream. relayExempt 是这张卡片「不中继的
// 客户端」名单（UA 片段，大小写不敏感），为空表示所有客户端一视同仁。
func New(provider upstream.Provider, mediaResolver *resolver.Resolver, collector *stats.Collector, redirectCfg config.Redirect, relayExempt []string) *Server {
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
		ffmpegPath:  ffmpegPath,
		audioCache:  newAudioCache(),
		relayExempt: append([]string(nil), relayExempt...),
	}
	return server
}

// Provider exposes the upstream this proxy serves.
func (s *Server) Provider() upstream.Provider { return s.provider }

// newReverseProxy builds the pass-through proxy for non-media requests.
func newReverseProxy(provider upstream.Provider, mediaResolver *resolver.Resolver) *httputil.ReverseProxy {
	target := provider.BaseURL()
	rewriter, canRewrite := provider.(upstream.ResponseRewriter)
	pathRewriter, canRewritePath := provider.(upstream.RequestPathRewriter)

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
			requestPath := request.In.URL.Path
			if canRewritePath {
				// 方言可以要求换一条上游路径（飞牛的 API 都在 /emby 下）。
				// 上下文里记的始终是客户端原本那条路径，响应改写按它分流。
				requestPath = pathRewriter.RewriteRequestPath(request.In)
			}
			request.Out.URL.Scheme = target.Scheme
			request.Out.URL.Host = target.Host
			request.Out.Host = target.Host
			request.Out.URL.Path = joinPath(target.Path, requestPath)
			if requestPath != request.In.URL.Path {
				// 路径被改过之后，转义形式必须重新推导，不能沿用客户端的 RawPath。
				request.Out.URL.RawPath = ""
			}
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
			// 不止播放协商：飞牛的网页播放器脚本也要改写，所以这句话不能说成
			// 只拦 PlaybackInfo，否则排查时会对不上日志。
			logx.Infof("[%s] 拦截待改写响应 %s %s（将检查并改写其中的 STRM 直放能力）", s.provider.Name(), request.Method, request.URL.RequestURI())
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
	logx.Infof("[%s] 拦截播放请求 %s %s（类型 %s，item=%s file=%s，客户端 IP=%s）", s.provider.Name(), request.Method, request.URL.RequestURI(), ref.Kind, ref.ItemID, ref.FileID, clientIP(request, s.redirect.TrustedProxyCIDRs...))
	s.serveMedia(writer, request, ref)
}

func isEmbyHLSRequest(provider upstream.Provider, requestPath string) bool {
	// 飞牛影视同样是 Emby 方言，HLS 回退的表现一模一样。
	if !provider.Type().IsEmbyFamily() {
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
		Client:    clientIP(request, s.redirect.TrustedProxyCIDRs...),
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

	// 把播放器自己带的 Emby 令牌一并送进解析：上游刚认过这枚令牌，用它比另配
	// 账号密码更可靠，也少了登录往返。上游配了凭据时仍以配置的为准（见
	// apiClient.credentials）。
	ctx := upstream.WithClientCredentials(request.Context(), request)
	resolution, cacheSource, cacheTTL, err := s.resolver.ResolveWithSource(ctx, s.provider, ref, request.UserAgent())
	event.CacheSource = string(cacheSource)
	event.CacheHit = cacheSource != resolver.CacheSourceMiss
	event.CacheTTLSeconds = cacheTTLSeconds(cacheTTL)
	if err != nil {
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

	// 跳转前的安全网：直链是内网地址、客户端却在外网——302 出去客户端连不上，
	// 播放必然失败。典型的 OpenList「本地代理」形态：它自己原地出流，跟随也就
	// 无处可去（零跳；就算它回一跳，落点仍是自己的内网地址）。这种目标没有
	// 「真正的直链」可拿，改由 AetherLink 中继保证能播。没开跟随时尊重原语义：
	// 302 照发，日志里的内网提示负责解释。
	wantRedirect := s.resolver.ShouldRedirectForClient(resolution, s.redirect, event.Client)
	// 卡片上的「不中继的客户端」名单：有些播放器只在拿着直链自己取流时能播，
	// 走我们的中继会读几 KB 就撒手（字节与响应头都已证实与直链逐项等价，属
	// 客户端自己的选路差异）。命中名单就把它们送回 302 那条路，哪怕这张卡片
	// 选的是「始终中继」——这是用户唯一能自己掌握的出路。
	relayExempt := !wantRedirect && resolution.Target != nil &&
		resolution.Target.Type == strm.TargetRemote &&
		config.RelayExemptUserAgent(s.relayExempt, event.UserAgent)
	if relayExempt {
		wantRedirect = true
	}
	intranetTargetPublicClient := wantRedirect && s.redirect.FollowUpstreamRedirects &&
		urlx.IsPrivateHost(playURL) &&
		resolver.ScopeOfClient(event.Client, s.redirect.IntranetCIDRs...) == resolver.ClientScopePublic
	if intranetTargetPublicClient {
		// 安全网优先于名单：直链是内网地址而客户端在外网时，302 出去也连不上，
		// 只能中继，名单在这一格上没有出路。
		wantRedirect = false
	}

	if wantRedirect {
		event.StatusCode = http.StatusFound
		// 302 这一行是它唯一的输出口，附加说明必须写进去——不然「命中了不中继
		// 名单」「跟着上游跳了几跳」「直链是内网地址」这几件事用户看不到。
		notes := make([]string, 0, 3)
		if relayExempt {
			notes = append(notes, "命中卡片「不中继的客户端」名单，本卡片要求中继也照发直链")
		}
		if len(resolution.Hops) > 0 {
			notes = append(notes, fmt.Sprintf("直链为跟随上游 %d 次重定向后的最终地址，客户端无需再跳", len(resolution.Hops)))
		}
		if privateTarget := privateTargetNote(playURL); privateTarget != "" {
			notes = append(notes, strings.TrimPrefix(privateTarget, "；"))
		}
		finish(stats.OutcomeRedirect, strings.Join(notes, "；"))
		// 302 keeps the request method for GET/HEAD and is what media players
		// (Emby clients, ABS apps, browsers) handle most reliably.
		writer.Header().Set("Location", playURL)
		writer.Header().Set("Cache-Control", "no-store")
		writer.WriteHeader(http.StatusFound)
		return
	}

	// 走到中继有两种情况：302 策略不允许跳转，或者直链是内网地址而客户端在
	// 外网（302 出去也连不上）。把具体原因写清楚，否则用户会以为 302 坏了。
	status, err := s.relayRemote(writer, request, playURL)
	event.StatusCode = status
	if err != nil {
		event.Error = err.Error()
		finish(stats.OutcomeError, "中继 strm 目标失败")
		return
	}
	if intranetTargetPublicClient {
		exemptNote := ""
		if relayExempt {
			exemptNote = "；该客户端命中卡片「不中继的客户端」名单，但直链是内网地址而客户端在外网，名单这一次没有出路"
		}
		finish(stats.OutcomeProxyStream, cacheNote(event)+"；直链是内网地址而客户端在外网，本次由 AetherLink 中继"+exemptNote)
		return
	}
	finish(stats.OutcomeProxyStream, cacheNote(event)+"；"+s.noRedirectReason(resolution, event.Client)+privateTargetNote(playURL))
}

// privateTargetNote 在直链指向内网服务时提示一个常见误会：302 出去的地址是
// OpenList 这类内网服务时，若它的网盘驱动开了「本地代理」，播放流量会经该服务
// 中转——用户看到中转常以为 AetherLink 没把 302 发出去，实际中转发生在直链侧。
func privateTargetNote(playURL string) string {
	if urlx.IsPrivateHost(playURL) {
		return "；直链是内网地址，若该服务（如 OpenList）的网盘驱动开了本地代理，播放流量会经它中转"
	}
	return ""
}

// noRedirectReason 说明这次为什么没有 302 出去，末尾点明实际走了哪条路 ——
// 先讲「卡片选了什么模式、客户端是谁、它算哪一边」，最后才是结论，用户一眼就能
// 对上自己配的东西。
// 注意：跳转策略按客户端 IP 的内外网归属判定（README「跳转模式」一节），
// 与媒体直链指向哪台服务器无关；这里必须把客户端的情况说清楚，
// 之前写成「而目标是内网地址」让用户对着公网直链排查目标，方向全错。
func (s *Server) noRedirectReason(resolution *resolver.Resolution, client string) string {
	if resolution == nil || resolution.Target == nil {
		return "没有解析出可跳转的地址，改由 AetherLink 中继"
	}
	if resolution.Target.Type != strm.TargetRemote {
		return "目标不是 http 地址，改由 AetherLink 中继"
	}
	// scopeNote 是「为什么算内网」的补充说明，只有自动识别那一层会给出具体网段：
	// 它是从本机地址推算出来的、界面上只看得到一行提示，日志里必须带上来源。
	scope, scopeNote := resolver.ScopeOfClientWithReason(client, s.redirect.IntranetCIDRs...)
	shown := client
	if shown == "" {
		shown = "无法识别"
	}
	switch s.redirect.Mode {
	case config.RedirectNever:
		return fmt.Sprintf("跳转模式为%s，改由 AetherLink 中继", config.RedirectModeName(config.RedirectNever))
	case config.RedirectPublic:
		switch scope {
		case resolver.ClientScopePrivate:
			return fmt.Sprintf("跳转模式为%s，客户端 IP %s%s，改由 AetherLink 中继", config.RedirectModeName(config.RedirectPublic), shown, intranetReason(scopeNote))
		case resolver.ClientScopeUnknown:
			return fmt.Sprintf("跳转模式为%s，客户端 IP 无法识别，改由 AetherLink 中继", config.RedirectModeName(config.RedirectPublic))
		default:
			return fmt.Sprintf("跳转模式为%s，客户端 IP %s 归属未知，改由 AetherLink 中继", config.RedirectModeName(config.RedirectPublic), shown)
		}
	case config.RedirectPrivate:
		switch scope {
		case resolver.ClientScopePublic:
			return fmt.Sprintf("跳转模式为%s，客户端 IP %s 是公网地址，改由 AetherLink 中继", config.RedirectModeName(config.RedirectPrivate), shown)
		case resolver.ClientScopeUnknown:
			return fmt.Sprintf("跳转模式为%s，客户端 IP 无法识别，改由 AetherLink 中继", config.RedirectModeName(config.RedirectPrivate))
		default:
			return fmt.Sprintf("跳转模式为%s，客户端 IP %s 归属未知，改由 AetherLink 中继", config.RedirectModeName(config.RedirectPrivate), shown)
		}
	default:
		return "跳转模式未启用，改由 AetherLink 中继"
	}
}

// intranetReason 补出「这个客户端为什么算内网」。自动识别那一层带来的是具体网段
// （scopeNote 形如「，与本机为同一网段 2409:…::/64」）；内置规则与配置声明的网段
// 没什么可补充的，只说结论。
func intranetReason(scopeNote string) string {
	if scopeNote == "" {
		return " 是内网地址"
	}
	return scopeNote
}

// logOutcome 把一条播放请求的处理结果写进日志。302 与透传都记成 info，
// 因为用户排查的正是这两条路的区别；失败记成 warn/error。
func (s *Server) logOutcome(event stats.Event, note string) {
	milliseconds := event.Duration.Milliseconds()
	switch event.Outcome {
	case stats.OutcomeRedirect:
		// 附加说明只在有内容时追加，普通 302 行的形状保持不变。
		extra := ""
		if note != "" {
			extra = "：" + note
		}
		logx.Infof("[%s] 302 %s -> %s（类型 %s，%dms，%s，%s）%s", event.Upstream, event.Path, event.Target, displayKind(event.Kind), milliseconds, cacheNote(event), userAgentNote(event), extra)
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

// earlyAbortBytes 是「客户端读了一点就走」的判定门槛。拖进度条、切集也会让客户端
// 断开，但那时它已经吃掉了大量字节；几十 KB 就撒手，只可能是它没认这个响应。
// 这类失败的成因分属两边——库里的容器名与真实文件不符，或播放器自己判定这个文件
// 放不了——双方的头是唯一能分辨它们的证据，所以门槛以下的那一支必须把头发全。
const earlyAbortBytes = 64 << 10

// relayRemote streams the remote target through AetherLink, preserving Range
// semantics so seeking keeps working.
func (s *Server) relayRemote(writer http.ResponseWriter, request *http.Request, target string) (int, error) {
	started := time.Now()
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

	// 类型以上游原样为准——**中继不改上游给的 `Content-Type`**，只有上游压根没给
	// 类型时（罕见）才由我们补一个。
	//
	// 这条规则是被线上逼出来的。网盘 CDN 与移动云 EOS 对视频一律回中性类型
	// `application/octet-stream`，播放器拿到它就自己按内容嗅探；我们若替它改写成
	// `video/x-matroska` / `video/mp4` 这类具体类型，等于替播放器做了一次它没要求
	// 的判断。实测就有一例：同一片走 302（播放器拿到 CDN 的中性类型）能播，走中继
	// （拿到我们改写的 `video/x-matroska`）却只读了 7.1KiB、把 MKV 的 `Tracks`
	// （HEVC 4K + 两条 ASS 字幕）读完就断开——两边可观测的差异只剩 host 与这一个头，
	// 而 host 改不了。结论：**302 与中继交给播放器的响应要逐项等价，包括类型**。
	//
	// 补类型时的顺序是「字节 → 扩展名」：直链的名字与上游的类型都不可信——线上既有
	// 「文件名写着 .iso、字节其实是 MP4」的直链，也有「STRM 报 mkv、真实文件是 MP4」
	// 的库。唯一的例外是纯音频扩展名（m4a/m4b/mp3/flac…）：MP4 家族里纯音频与视频
	// 共用容器头，品牌字段常是 isom/mp42，字节分不出有声书，扩展名才是唯一线索，
	// 所以这一类跳过字节判断。
	upstreamType := writer.Header().Get("Content-Type")
	synthesizedType := upstreamType == ""
	// 取一小段响应体开头（16~128 字节，判断完原样回写）：既给「上游没给类型」
	// 兜底，也用来核对客户端请求的容器名与真实文件是否相符。
	//
	// 后者是这次新加的：网盘 CDN 与移动云 EOS 对视频一律回中性类型
	// `application/octet-stream`，而中性类型「没有声明容器」，于是「库里的容器名
	// 与真实文件不符」这一类在日志里完全不可见——可它正是「读了几 KB 就断开」
	// 首先要排除的一条。读了字节就能把这一条钉死或排除掉。
	//
	// 只对从 0 开始的单段 Range 做：换别的偏移时这段字节不是文件开头，不能回写。
	// 纯音频扩展名（m4a/m4b…）跳过：MP4 家族里纯音频与视频共用容器头，字节分不出
	// 有声书，扩展名才是唯一线索（详见 mimeTypeForURL 的注释）。
	var prefix []byte
	observedType := upstreamType
	if request.Method != http.MethodHead &&
		rangeStartsAtZero(request.Header.Get("Range")) &&
		(synthesizedType || isNeutralContentType(upstreamType)) &&
		!strings.HasPrefix(mimeTypeForURL(target), "audio/") {
		prefix = readSniffPrefix(response.Body)
		if sniffed := sniffContentType(prefix); sniffed != "" {
			observedType = sniffed
		}
	}
	if synthesizedType {
		guessed := mimeTypeForURL(target)
		if observedType != "" {
			guessed = observedType
		}
		if guessed != "" {
			writer.Header().Set("Content-Type", guessed)
		}
	}
	// 客户端按上游给的容器名请求（如 `/Videos/xxx/stream.mkv`），而实际类型与它不符时
	// 说一句：播放器可能据此选错解复用器，而这类「库里的容器名与真实文件不符」正是
	// 「读了少量字节就断开」的一个已知成因。
	if note := containerMismatchNote(request.URL.Path, observedType); note != "" {
		source := "上游给的就是这个类型，中继没有改写它"
		switch {
		case synthesizedType:
			source = "上游没给 Content-Type，这个类型是 AetherLink 按响应体字节补的"
		case isNeutralContentType(upstreamType):
			source = fmt.Sprintf("上游回的是中性类型 %q，这个类型是 AetherLink 从响应体字节读出来的、没有改写回给客户端的头；这条直链走 302 时播放器拿到的正是那个中性类型，得自己嗅探", upstreamType)
		}
		logx.Warnf("[%s] 中继 %s：%s；播放器可能据此选错解复用器（%s）", s.provider.Name(), request.URL.Path, note, source)
	}
	// 回给客户端的头必须进日志：中继排障时播放器只看得到这半边，差一个字段的表现
	// 都是「读了少量字节就断开」，不写下来就只能靠猜。
	sentNote := sentHeaderNote(writer.Header())
	traceRelayExchange(s.provider.Name(), request, outbound, response, writer.Header(), prefix)
	writer.WriteHeader(response.StatusCode)
	if request.Method == http.MethodHead {
		return response.StatusCode, nil
	}
	recorder := &readErrorRecorder{reader: response.Body}
	// 判容器读走的那几个字节当作响应体的前缀一起拷，而不是先单独写一次：
	// 先写会多出一个出口——那一次写失败就直接返回，客户端「读了一点就走」的
	// 诊断行（唯一能说明它吃了多少、双方发了什么的那行）就整条丢了。
	var body io.Reader = recorder
	if len(prefix) > 0 {
		body = io.MultiReader(bytes.NewReader(prefix), recorder)
	}
	copied, copyErr := io.Copy(writer, body)
	written := copied
	elapsed := time.Since(started).Round(time.Millisecond)
	// 三条出口分开报，谁也别盖住谁：
	//   · 客户端还在等、上游先读失败 —— 确定是源头的事（网盘断流、直链过期）；
	//   · 客户端断开 —— 拖进度条、切集，或者读了一点就不认这个响应；
	//   · 只写回失败 —— 客户端先走，上游没问题。
	// 关键在上一版把顺序写反了：客户端断开会让上游请求一起被取消，读端必然也报错，
	// 于是「先判客户端」就把每一次拖进度条都写成源头故障。反过来「先判上游」也不对，
	// 会把客户端读了一点就放弃写成「上游断流」。所以上游断流只有在客户端还在等时
	// 才算定论，客户端断开那一支只把读到的错误附带提一句。
	clientGone := request.Context().Err() != nil
	switch {
	case recorder.err != nil && !clientGone:
		logx.Warnf("[%s] 中继上游断流 %s -> %s：上游 %d 声明 %s，只转发出去 %s，客户端还在等剩下的字节，播放必然卡住或失败；客户端 Range=%q，%s，用时 %s，原因：%v", s.provider.Name(), request.URL.Path, target, response.StatusCode, formatBytes(response.ContentLength), formatBytes(written), request.Header.Get("Range"), sentNote, elapsed, recorder.err)
	case clientGone:
		aside := ""
		if recorder.err != nil {
			aside = fmt.Sprintf("；同时读到上游错误：%v（客户端断开会让上游请求一起取消，多为连带现象）", recorder.err)
		}
		// 「读了一点就走」是中继排障里最难缠的一种形态：字节投递与响应头都已证实
		// 与直连逐项等价，却仍只有某些播放器不认。它的两个已知成因分属两边——库里
		// 的容器名与直链真实文件不符，或者播放器自己判定这个文件放不了（编码、位深、
		// 多字幕轨都可能）——而分辨它们要看的正是双方的头，加上播放器随后有没有再发
		// 别的请求（转码播放列表、第二次 Range）。所以这一支把双方的头都写全：原先
		// 只有 debug 级别才有的那部分，不该让人先跑去改日志级别才能拿到。
		if written < earlyAbortBytes {
			logx.Warnf("[%s] 中继被客户端中断 %s：只读了 %s 就断开，像是没认这个响应（上游 %d 声明 %s，客户端 Range=%q，用时 %s）；客户端请求头 %s；回给客户端的头 %s%s；客户端主动断开。对照：同一片切「始终跳转」能播就是播放器自己判定不放（与中继无关），同样不能播则查库里的容器名与直链真实文件是否相符", s.provider.Name(), request.URL.Path, formatBytes(written), response.StatusCode, formatBytes(response.ContentLength), request.Header.Get("Range"), elapsed, headerNote(request.Header), headerNote(writer.Header()), aside)
			return response.StatusCode, nil
		}
		logx.Infof("[%s] 中继被客户端中断 %s：已转发 %s（上游 %d 声明 %s），客户端 Range=%q，%s，用时 %s%s；客户端主动断开，拖进度条与切集是正常形态", s.provider.Name(), request.URL.Path, formatBytes(written), response.StatusCode, formatBytes(response.ContentLength), request.Header.Get("Range"), sentNote, elapsed, aside)
	case copyErr != nil:
		logx.Infof("[%s] 中继写回客户端结束 %s：已转发 %s，客户端 Range=%q，%s，用时 %s，原因：%v", s.provider.Name(), request.URL.Path, formatBytes(written), request.Header.Get("Range"), sentNote, elapsed, copyErr)
	case response.ContentLength > 0 && written < response.ContentLength:
		logx.Warnf("[%s] 中继不完整 %s -> %s：上游 %d 声明 %s，只转发出去 %s，客户端会卡住或播不动；客户端 Range=%q，%s，用时 %s", s.provider.Name(), request.URL.Path, target, response.StatusCode, formatBytes(response.ContentLength), formatBytes(written), request.Header.Get("Range"), sentNote, elapsed)
	default:
		logx.Infof("[%s] 中继完成 %s -> %s：上游 %d，转发 %s（长度 %s），客户端 Range=%q，%s，用时 %s", s.provider.Name(), request.URL.Path, target, response.StatusCode, formatBytes(written), formatBytes(response.ContentLength), request.Header.Get("Range"), sentNote, elapsed)
	}
	return response.StatusCode, nil
}

// sentHeaderNote 把「回给客户端的响应头」摘成一行。中继排障里这半边一直是不可见
// 的，而播放器只认它：类型、长度、区间、可跳转标记差一个，表现都是「读了少量
// 字节就断开」——日志里必须能直接看到我们到底回了什么。
func sentHeaderNote(header http.Header) string {
	return fmt.Sprintf("回给客户端：Content-Type=%q，Content-Length=%q，Content-Range=%q，Accept-Ranges=%q",
		header.Get("Content-Type"), header.Get("Content-Length"), header.Get("Content-Range"), header.Get("Accept-Ranges"))
}

// traceRelayExchange 在 debug 级别把一次中继的完整 HTTP 交换摊开写进日志。
//
// 中继反复出现「同一片走 302 能播、走中继播不了」，而两边的响应在字节与长度上
// 本该等价——真正的差异只可能藏在某个头里。info 级别那行只摘四个字段，够用但不够
// 定论；打开 debug（设置页「日志级别」或 AETHERLINK_LOG_LEVEL=debug）就能一次看全
// 双方到底发了什么，不必再来一轮「把日志发我看看」。
func traceRelayExchange(upstreamName string, request *http.Request, outbound *http.Request, response *http.Response, sent http.Header, prefix []byte) {
	if !logx.Enabled(logx.LevelDebug) {
		return
	}
	logx.Debugf("[%s] 中继交换明细 %s %s -> %s：客户端请求头 %s；上游请求头 %s；上游响应 %d，TransferEncoding=%v，ContentLength=%d，上游响应头 %s；回给客户端 %s",
		upstreamName, request.Method, request.URL.Path, outbound.URL.Host,
		headerNote(request.Header), headerNote(outbound.Header),
		response.StatusCode, response.TransferEncoding, response.ContentLength, headerNote(response.Header),
		headerNote(sent))
	if len(prefix) > 0 {
		shown := prefix
		if len(shown) > 32 {
			shown = shown[:32]
		}
		logx.Debugf("[%s] 中继判容器读了 %d 字节（原样写回，不丢一字节），前 %d 字节 %s",
			upstreamName, len(prefix), len(shown), hex.EncodeToString(shown))
	}
}

// headerNote 把整幅 HTTP 头摘成一行「键=值」列表，按键排序以便两次日志逐行对比。
// 凭据一律打码：排障要看的是类型、长度、区间、UA、内容协商这些，令牌与 Cookie
// 没有进日志的必要。
func headerNote(header http.Header) string {
	if len(header) == 0 {
		return "（空）"
	}
	keys := make([]string, 0, len(header))
	for key := range header {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		value := strings.Join(header.Values(key), "|")
		if secretHeaders[strings.ToLower(key)] {
			value = "已省略"
		}
		parts = append(parts, key+"="+value)
	}
	return strings.Join(parts, "，")
}

// secretHeaders 是 debug 明细里必须打码的头。
var secretHeaders = map[string]bool{
	"authorization":        true,
	"cookie":               true,
	"set-cookie":           true,
	"x-emby-authorization": true,
	"x-emby-token":         true,
	"x-api-key":            true,
	"proxy-authorization":  true,
}

// readErrorRecorder 包住上游响应体，单独记下「读取失败」。io.Copy 只回一个
// 错误，读失败和写失败混在一起；中继排障时两者方向完全相反——上游断流要查
// 媒体源，写失败要查播放器与网络——所以读端必须自己留证据。
// 只包读端不自己搬字节：io.Copy 的写侧优化（ReaderFrom / 缓冲复用）照旧有效。
type readErrorRecorder struct {
	reader io.Reader
	err    error
}

func (r *readErrorRecorder) Read(buffer []byte) (int, error) {
	read, err := r.reader.Read(buffer)
	if err != nil && !errors.Is(err, io.EOF) {
		r.err = err
	}
	return read, err
}

// formatBytes 把字节数写成日志里好读的短字符串；负数代表长度未知。
func formatBytes(value int64) string {
	switch {
	case value < 0:
		return "未知"
	case value < 1024:
		return fmt.Sprintf("%dB", value)
	case value < 1024*1024:
		return fmt.Sprintf("%.1fKiB", float64(value)/(1024))
	case value < 1024*1024*1024:
		return fmt.Sprintf("%.1fMiB", float64(value)/(1024*1024))
	default:
		return fmt.Sprintf("%.2fGiB", float64(value)/(1024*1024*1024))
	}
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
//
// 音频与视频必须分开写：上游不给 `Content-Type` 而只能由我们补时，把 `.mp4` 猜成
// `audio/mp4` 等于告诉播放器「这是音轨」，而真实内容是 2160p 视频，播放器会在读完
// 响应头后立刻断开——日志里只剩一行「状态 206」，看不出原因。
// 容器扩展名相同（mp4/m4v）但语义不同时，按视频处理：audio 侧只保留
// 真正的纯音频容器（m4a/m4b/m4p）。
//
// 注意这只在「上游没给 Content-Type」时才用得上：网盘 CDN 回的中性类型
// `application/octet-stream` 也是「上游给的值」，中继照原样转发，不在这里改写。
func mimeTypeForURL(target string) string {
	candidate := target
	if parsed, err := url.Parse(target); err == nil && parsed.Path != "" {
		candidate = parsed.Path
	}
	extension := strings.ToLower(path.Ext(candidate))
	switch extension {
	case ".m4a", ".m4b", ".m4p":
		return "audio/mp4"
	case ".mp4", ".m4v":
		return "video/mp4"
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
	case ".webma":
		return "audio/webm"
	case ".webm":
		return "video/webm"
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

// containerMismatchNote 报告客户端请求路径暗示的容器与响应里实际的类型是否矛盾。
//
// 例：客户端请求 `/Videos/xxx/stream.mkv`（这个名字来自上游的 `Container`），而实际
// 类型是 `video/mp4`——网盘上「STRM 名写 .mkv、真实文件是 MP4」很常见。播放器拿 mkv
// 去解 MP4 就会读几百 KB 后断开，而这类失败在日志里只剩一行「客户端中断」，所以这里
// 主动写一句，写明是哪两样东西对不上。
//
// 只在两边都是明确的音视频容器时才提示：上游给 `application/octet-stream` 是中性值、
// 不是「声明了别的容器」，`/stream`（不带扩展名）与 `emby` 这类路径本就没有容器语义，
// 硬报只会制造噪声。
func containerMismatchNote(requestPath, actualType string) string {
	implied := mimeTypeForURL(requestPath)
	if implied == "" || actualType == "" || implied == actualType {
		return ""
	}
	if !strings.HasPrefix(implied, "video/") && !strings.HasPrefix(implied, "audio/") {
		return ""
	}
	// 中性类型没有声明容器，谈不上「与请求路径矛盾」。
	if !strings.HasPrefix(actualType, "video/") && !strings.HasPrefix(actualType, "audio/") {
		return ""
	}
	return fmt.Sprintf("请求路径 %q 暗示 %s，实际类型是 %s", path.Ext(requestPath), implied, actualType)
}

// isNeutralContentType 判断这个类型是不是「没有声明容器」：缺失、以及网盘 CDN 与
// 移动云 EOS 惯用的 `application/octet-stream` / `binary/octet-stream` 都属于这一类。
// 中继不改写上游给的类型，但读到这种值时值得读一眼响应体字节，好让日志说得清
// 「客户端按 .mkv 请求，而字节其实是 MP4」。
func isNeutralContentType(contentType string) bool {
	media := strings.ToLower(strings.TrimSpace(strings.SplitN(contentType, ";", 2)[0]))
	switch media {
	case "", "application/octet-stream", "binary/octet-stream", "application/binary":
		return true
	default:
		return false
	}
}

// sniffBytes 是判断真实容器需要读的头部字节数：ftyp、EBML、RIFF、OggS 这些魔数都在
// 前 16 字节内，WebM 的 DocType 也在前 64 字节内。
const sniffBytes = 128

// readSniffPrefix 从响应体开头取一小段用于判断容器。读到够判断（至少 16 字节，最多
// sniffBytes）就停，不为凑满缓冲把首字节卡在网络上；取到的字节由调用方原样写回
// 客户端——判断容器不能吃掉任何一个字节。
func readSniffPrefix(body io.Reader) []byte {
	buffer := make([]byte, sniffBytes)
	filled := 0
	idle := 0
	for filled < 16 {
		read, err := body.Read(buffer[filled:])
		if read > 0 {
			filled += read
			idle = 0
		} else {
			idle++
		}
		if err != nil || idle > 1 {
			break
		}
	}
	if filled == 0 {
		return nil
	}
	return buffer[:filled]
}

// sniffContentType 从响应体开头的魔数判断真实容器。
//
// 网盘 / CDN 直链的路径常常只有一串 ID（没有扩展名可猜），回给客户端的又一律是
// application/octet-stream——这时按扩展名猜必然落空，只能看字节。线上就有这样
// 一条直链：路径是 /50f351580c084c969c0c84f4e500ed39086，Content-Disposition 里
// 的文件名写着 .iso，而前 64 字节是 ftypisom…mp41——**文件名与 Content-Type 都
// 不可信，字节可信**。
func sniffContentType(head []byte) string {
	switch {
	case len(head) >= 12 && string(head[4:8]) == "ftyp":
		// MP4 家族。品牌能分出纯音频容器（m4a/m4b）与视频，别把有声书当视频。
		switch string(head[8:12]) {
		case "M4A ", "M4B ", "M4P ", "F4A ":
			return "audio/mp4"
		}
		return "video/mp4"
	case len(head) >= 4 && bytes.Equal(head[:4], []byte{0x1A, 0x45, 0xDF, 0xA3}):
		// EBML：Matroska 与 WebM 共用容器头，靠 DocType 区分。
		if bytes.Contains(head, []byte("webm")) {
			return "video/webm"
		}
		return "video/x-matroska"
	case len(head) >= 12 && string(head[:4]) == "RIFF" && string(head[8:12]) == "AVI ":
		return "video/x-msvideo"
	case len(head) >= 12 && string(head[:4]) == "RIFF" && string(head[8:12]) == "WAVE":
		return "audio/wav"
	case len(head) >= 4 && string(head[:4]) == "OggS":
		return "application/ogg"
	case len(head) >= 4 && string(head[:4]) == "fLaC":
		return "audio/flac"
	case len(head) >= 4 && bytes.Equal(head[:4], []byte{0x30, 0x26, 0xB2, 0x75}):
		// ASF（wmv/wma）的 16 字节 GUID 头。
		return "video/x-ms-asf"
	}
	return ""
}

// rangeStartsAtZero 判断这次响应是不是从文件开头开始的。只有从头开始的字节才谈得
// 上「看魔数」：seek 到中途的响应，开头就是媒体数据本身，判断不出容器。
func rangeStartsAtZero(raw string) bool {
	if raw == "" {
		return true
	}
	// 多段 Range 的响应体是 multipart 拼出来的，更不能猜。
	return strings.HasPrefix(raw, "bytes=0-") && !strings.Contains(raw, ",")
}
