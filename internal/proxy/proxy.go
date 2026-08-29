// Package proxy implements the reverse proxy that fronts each upstream media
// server and rewrites STRM media deliveries into 302 redirects.
package proxy

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path"
	"strings"
	"time"

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
}

// New builds the proxy serving one upstream.
func New(provider upstream.Provider, mediaResolver *resolver.Resolver, collector *stats.Collector, redirectCfg config.Redirect) *Server {
	server := &Server{
		provider: provider,
		proxy:    newReverseProxy(provider),
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
	}
	return server
}

// Provider exposes the upstream this proxy serves.
func (s *Server) Provider() upstream.Provider { return s.provider }

// newReverseProxy builds the pass-through proxy for non-media requests.
func newReverseProxy(provider upstream.Provider) *httputil.ReverseProxy {
	target := provider.BaseURL()

	return &httputil.ReverseProxy{
		Transport: provider.Transport(),
		Rewrite: func(request *httputil.ProxyRequest) {
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
		// FlushInterval -1 streams responses without buffering, which matters for
		// progressive audio and SSE-style endpoints.
		FlushInterval: -1,
	}
}

// ServeHTTP proxies a request to the upstream, intercepting media deliveries.
func (s *Server) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	ref, isMedia := s.provider.Match(request)
	if !isMedia {
		s.proxy.ServeHTTP(writer, request)
		return
	}
	s.serveMedia(writer, request, ref)
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

	if !s.provider.HasCredentials() {
		// Without an API key AetherLink cannot ask the upstream where the media
		// lives, so behave like a plain reverse proxy.
		event.Outcome = stats.OutcomePassthrough
		event.Error = upstream.ErrNoAPIKey.Error()
		event.Duration = time.Since(started)
		s.stats.Record(event)
		s.proxy.ServeHTTP(writer, request)
		return
	}

	resolution, cacheHit, err := s.resolver.Resolve(request.Context(), s.provider, ref, request.UserAgent())
	event.CacheHit = cacheHit
	if err != nil {
		event.Duration = time.Since(started)
		if errors.Is(err, resolver.ErrNotStrm) {
			// Regular media file: let the upstream serve it.
			event.Outcome = stats.OutcomePassthrough
			s.stats.Record(event)
			s.proxy.ServeHTTP(writer, request)
			return
		}
		if errors.Is(err, resolver.ErrPointerUnavailable) {
			// 指针确实是 strm，但这个容器读不到它（媒体目录没挂进来）。
			// 上游自己能读，退回透传让播放继续，同时把原因记进日志与事件，
			// 避免用户只看到「能播但没有 302」而查不出为什么。
			event.Outcome = stats.OutcomePassthrough
			event.Error = err.Error()
			s.stats.Record(event)
			logx.Warnf("[proxy] %s 读不到 strm 指针，本次退回透传（挂载媒体目录后即可 302）: %v", s.provider.Name(), err)
			s.proxy.ServeHTTP(writer, request)
			return
		}
		event.Outcome = stats.OutcomeError
		event.Error = err.Error()
		event.StatusCode = http.StatusBadGateway
		s.stats.Record(event)
		logx.Errorf("[proxy] resolve failed for %s %s: %v", s.provider.Name(), request.URL.Path, err)
		http.Error(writer, "AetherLink could not resolve the strm target", http.StatusBadGateway)
		return
	}

	event.MediaPath = resolution.ContainerPath
	if resolution.Target != nil {
		event.Kind = string(resolution.Target.Kind)
	}

	if resolution.Target != nil && resolution.Target.Type == strm.TargetLocal {
		event.Target = resolution.Target.Path
		event.Outcome = stats.OutcomeLocalFile
		event.StatusCode = http.StatusOK
		event.Duration = time.Since(started)
		s.stats.Record(event)
		s.serveLocalFile(writer, request, resolution.Target.Path)
		return
	}

	playURL := resolution.PlayURL()
	event.Target = playURL

	if s.resolver.ShouldRedirect(resolution) {
		event.Outcome = stats.OutcomeRedirect
		event.StatusCode = http.StatusFound
		event.Duration = time.Since(started)
		s.stats.Record(event)
		// 302 keeps the request method for GET/HEAD and is what media players
		// (Emby clients, ABS apps, browsers) handle most reliably.
		writer.Header().Set("Location", playURL)
		writer.Header().Set("Cache-Control", "no-store")
		writer.WriteHeader(http.StatusFound)
		return
	}

	status, err := s.relayRemote(writer, request, playURL)
	event.Outcome = stats.OutcomeProxyStream
	event.StatusCode = status
	event.Duration = time.Since(started)
	if err != nil {
		event.Outcome = stats.OutcomeError
		event.Error = err.Error()
	}
	s.stats.Record(event)
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
	outbound.Header.Set("User-Agent", s.resolver.EffectiveUserAgent(request.UserAgent()))
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
