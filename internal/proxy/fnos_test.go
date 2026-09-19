package proxy

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aetherlink/aetherlink/internal/config"
	"github.com/aetherlink/aetherlink/internal/resolver"
	"github.com/aetherlink/aetherlink/internal/stats"
	"github.com/aetherlink/aetherlink/internal/upstream"
)

// fnosPlayerScript 复刻飞牛网页播放器里给 media 元素加 crossorigin 的那段逻辑。
// 302 出去的是跨域直链，带上 anonymous 会让浏览器按 CORS 规则处理请求，
// 直链服务端不返回 CORS 头时视频就播不出来。
const fnosPlayerScript = `function play(mediaSource){var playMethod="DirectPlay";` +
	`el.setAttribute("crossorigin",mediaSource.IsRemote&&"DirectPlay"===playMethod?null:"anonymous");}`

// fakeFnos 只注册 /emby 前缀下的路由，其余路径一律返回单页应用的 HTML —— 这正是
// 飞牛的真实行为，也是「少了前缀就一定拿不到 JSON」的根因。把它做成假的之后，
// 「前缀有没有补上」就从一个假设变成了可断言的事实。
type fakeFnos struct {
	server *httptest.Server

	mu             sync.Mutex
	paths          []string
	playbackInfo   int
	acceptEncoding string
}

func (f *fakeFnos) record(request *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.paths = append(f.paths, request.URL.Path)
}

func (f *fakeFnos) snapshot() (paths []string, playbackInfo int, acceptEncoding string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.paths...), f.playbackInfo, f.acceptEncoding
}

// lastPath 返回上游最后收到的那条路径，用来断言前缀。
func (f *fakeFnos) lastPath() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.paths) == 0 {
		return ""
	}
	return f.paths[len(f.paths)-1]
}

func newFakeFnos(t *testing.T, sources []map[string]any) *fakeFnos {
	t.Helper()
	fake := &fakeFnos{}
	mux := http.NewServeMux()
	mux.HandleFunc("/emby/Users", func(writer http.ResponseWriter, request *http.Request) {
		fake.record(request)
		writeJSON(t, writer, []map[string]any{})
	})
	mux.HandleFunc("/emby/Items", func(writer http.ResponseWriter, request *http.Request) {
		fake.record(request)
		if request.URL.Query().Get("api_key") != "test-api-key" {
			writer.WriteHeader(http.StatusUnauthorized)
			return
		}
		writeJSON(t, writer, map[string]any{
			"Items": []map[string]any{{
				"Id":           "movie-1",
				"Name":         "白色巨塔",
				"Type":         "Episode",
				"Path":         "/media/tv/白色巨塔/S01E01.strm",
				"MediaSources": sources,
			}},
			"TotalRecordCount": 1,
		})
	})
	mux.HandleFunc("/emby/Items/movie-1/PlaybackInfo", func(writer http.ResponseWriter, request *http.Request) {
		fake.record(request)
		fake.mu.Lock()
		fake.playbackInfo++
		fake.acceptEncoding = request.Header.Get("Accept-Encoding")
		fake.mu.Unlock()
		writeJSON(t, writer, map[string]any{"PlaySessionId": "play-1", "MediaSources": sources})
	})
	mux.HandleFunc("/emby/web/modules/htmlvideoplayer/basehtmlplayer.js", func(writer http.ResponseWriter, request *http.Request) {
		fake.record(request)
		writer.Header().Set("Content-Type", "application/javascript")
		_, _ = writer.Write([]byte(fnosPlayerScript))
	})
	// 兜底：不在 /emby 下的路径拿到的是 SPA 的 HTML，不是 404。
	mux.HandleFunc("/", func(writer http.ResponseWriter, request *http.Request) {
		fake.record(request)
		writer.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = writer.Write([]byte("<!doctype html><html><body>fnos-spa</body></html>"))
	})
	fake.server = httptest.NewServer(mux)
	t.Cleanup(fake.server.Close)
	return fake
}

func newFnosTestServer(t *testing.T, fnosURL string, redirectCfg config.Redirect) (*Server, *stats.Collector) {
	t.Helper()
	provider, err := upstream.New(config.Upstream{
		Name:       "飞牛影视",
		Type:       config.UpstreamFnos,
		BaseURL:    fnosURL,
		APIKey:     "test-api-key",
		ListenPort: 5154,
	})
	if err != nil {
		t.Fatalf("build fnos provider: %v", err)
	}
	collector := stats.New(50)
	mediaResolver := resolver.New(config.Cache{TTL: time.Minute, MaxSize: 32}, redirectCfg)
	return New(provider, mediaResolver, collector, redirectCfg), collector
}

// fnosStrmPlaybackSource 是飞牛对一条 .strm 影片给出的媒体源：Path 已经是指针里
// 那条直链，而 MediaStreams 里缺键/为 null（客户端据此会判定不可播放）。
func fnosStrmPlaybackSource() map[string]any {
	return map[string]any{
		"Id":                   "source-strm",
		"Path":                 "http://10.0.0.31:25244/d/移动云盘/白色巨塔 (2003)/S01E01.再读.mkv",
		"Protocol":             "Http",
		"Container":            "strm",
		"MediaType":            "Video",
		"SupportsDirectPlay":   true,
		"SupportsDirectStream": false,
		"SupportsTranscoding":  true,
		"DirectStreamUrl":      "/emby/Videos/movie-1/stream.mkv?PlaySessionId=play-1",
		"TranscodingUrl":       "/emby/Videos/movie-1/master.m3u8?PlaySessionId=play-1",
		"TranscodingContainer": "ts",
		"MediaStreams": []any{
			map[string]any{"Type": "Video", "Language": nil, "Title": nil},
			map[string]any{"Index": float64(1)},
		},
	}
}

// 整条链路走一遍：播放协商 → 补齐流信息 → 接到 302 → 真正跳转到指针里的直链。
// 请求刻意不带 /emby 前缀，重现「反代通了却永远不 302」的条件。
func TestFnosPlaybackInfoFillsMediaStreamsAndRoutesStrmToDirectPlay(t *testing.T) {
	fake := newFakeFnos(t, []map[string]any{fnosStrmPlaybackSource()})
	server, collector := newFnosTestServer(t, fake.server.URL, defaultRedirect())

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/Items/movie-1/PlaybackInfo?UserId=user-1", strings.NewReader(`{}`))
	request.Header.Set("Accept-Encoding", "gzip")
	request.Header.Set("Content-Type", "application/json")
	server.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %q", recorder.Code, recorder.Body.String())
	}
	if got := fake.lastPath(); got != "/emby/Items/movie-1/PlaybackInfo" {
		t.Fatalf("上游收到 %q，want /emby/Items/movie-1/PlaybackInfo（前缀没补上就会落到 SPA 的 HTML 上）", got)
	}
	if _, _, acceptEncoding := fake.snapshot(); acceptEncoding != "identity" {
		t.Fatalf("上游 Accept-Encoding = %q, want identity", acceptEncoding)
	}

	var playbackInfo struct {
		MediaSources []map[string]any `json:"MediaSources"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &playbackInfo); err != nil {
		t.Fatalf("解析改写后的 PlaybackInfo: %v（body = %q）", err, recorder.Body.String())
	}
	if len(playbackInfo.MediaSources) != 1 {
		t.Fatalf("媒体源数量 = %d, want 1", len(playbackInfo.MediaSources))
	}
	rewritten := playbackInfo.MediaSources[0]

	// 1) MediaStreams 的必填字段必须存在且非 null。
	streams, ok := rewritten["MediaStreams"].([]any)
	if !ok || len(streams) != 2 {
		t.Fatalf("MediaStreams = %#v", rewritten["MediaStreams"])
	}
	for index, entry := range streams {
		stream, ok := entry.(map[string]any)
		if !ok {
			t.Fatalf("MediaStreams[%d] 不是对象：%#v", index, entry)
		}
		for _, field := range []string{"Type", "Language", "DisplayLanguage", "Title", "DisplayTitle"} {
			value, exists := stream[field]
			if !exists || value == nil {
				t.Fatalf("MediaStreams[%d].%s 仍为 null/缺失：%#v", index, field, stream)
			}
		}
	}
	// 已经填好的值不能被覆盖。
	first := streams[0].(map[string]any)
	if first["Type"] != "Video" {
		t.Fatalf("MediaStreams[0].Type = %#v, want Video", first["Type"])
	}

	// 2) DirectStreamUrl 必须被接到 AetherLink，且保留飞牛给的转码回退能力。
	// AetherLink 在这里比参考实现更保守：不无条件声称可直放，否则客户端会拿到
	// 自己解不开的文件，变成「有跳转却放不出来」。
	if rewritten["TranscodingUrl"] == nil || rewritten["SupportsTranscoding"] != true || rewritten["TranscodingContainer"] != "ts" {
		t.Fatalf("不该动飞牛给的转码回退：%#v", rewritten)
	}
	directURL, _ := rewritten["DirectStreamUrl"].(string)
	parsed, err := url.Parse(directURL)
	if err != nil {
		t.Fatalf("解析 DirectStreamUrl %q: %v", directURL, err)
	}
	if parsed.Path != "/emby/Videos/movie-1/stream.mkv" {
		t.Fatalf("DirectStreamUrl 路径 = %q", parsed.Path)
	}
	if parsed.Query().Get("MediaSourceId") != "source-strm" || parsed.Query().Get("Static") != "true" || parsed.Query().Get("PlaySessionId") != "play-1" {
		t.Fatalf("DirectStreamUrl 查询串 = %q", parsed.RawQuery)
	}

	// 3) 客户端照 DirectStreamUrl 取流，必须 302 到指针里的真实地址。
	redirectRecorder := httptest.NewRecorder()
	server.ServeHTTP(redirectRecorder, httptest.NewRequest(http.MethodGet, directURL, nil))
	if redirectRecorder.Code != http.StatusFound {
		t.Fatalf("取流状态 = %d, want 302; body = %q", redirectRecorder.Code, redirectRecorder.Body.String())
	}
	location := redirectRecorder.Header().Get("Location")
	if !strings.HasPrefix(location, "http://10.0.0.31:25244/d/%E7%A7%BB%E5%8A%A8%E4%BA%91%E7%9B%98/") {
		t.Fatalf("Location = %q", location)
	}
	if snapshot := collector.Snapshot(10); snapshot.Redirects != 1 {
		t.Fatalf("302 计数 = %d, want 1", snapshot.Redirects)
	}
	if _, playbackInfoCalls, _ := fake.snapshot(); playbackInfoCalls != 1 {
		t.Fatalf("PlaybackInfo 上游调用 = %d, want 1（取流应复用刚缓存的媒体源）", playbackInfoCalls)
	}
}

// 前缀只能加在播放协商上。飞牛的界面与字节接口都在根路径下，无差别加前缀会把
// 网页控制台整个打挂，所以这条反向断言必须锁住。
func TestFnosRequestPathRewriteLeavesOtherRoutesAlone(t *testing.T) {
	fake := newFakeFnos(t, []map[string]any{fnosStrmPlaybackSource()})
	server, collector := newFnosTestServer(t, fake.server.URL, defaultRedirect())

	cases := []struct {
		method string
		path   string
	}{
		// 网页控制台。
		{http.MethodGet, "/web/index.html"},
		// 封面图。
		{http.MethodGet, "/Items/movie-1/Images/Primary"},
		// 根路径下的 API。
		{http.MethodGet, "/System/Info/Public"},
		// 已经带前缀的播放协商不该被补成 /emby/emby。
		{http.MethodPost, "/emby/Items/movie-1/PlaybackInfo"},
	}
	for _, testCase := range cases {
		recorder := httptest.NewRecorder()
		server.ServeHTTP(recorder, httptest.NewRequest(testCase.method, testCase.path, nil))
		if got := fake.lastPath(); got != testCase.path {
			t.Errorf("%s %s 转发到了 %q, want %q", testCase.method, testCase.path, got, testCase.path)
		}
	}

	// 反面对照：媒体路径必须被拦截成 302，而不是原样转发。它正是上面那批
	// 「路径要原样送达」的规则里的唯一例外，也是整条链路存在的意义。
	mediaRecorder := httptest.NewRecorder()
	server.ServeHTTP(mediaRecorder, httptest.NewRequest(http.MethodGet, "/Videos/movie-1/stream.mkv?MediaSourceId=source-strm", nil))
	if mediaRecorder.Code != http.StatusFound {
		t.Fatalf("媒体路径状态 = %d, want 302", mediaRecorder.Code)
	}
	if snapshot := collector.Snapshot(10); snapshot.Redirects != 1 {
		t.Fatalf("302 计数 = %d, want 1", snapshot.Redirects)
	}
}

// 地址里已经写了 /emby 的用户，代理层会去掉重复的一段，不能再补一次。
func TestFnosRequestPathRewriteHonoursEmbyInBaseURL(t *testing.T) {
	fake := newFakeFnos(t, []map[string]any{fnosStrmPlaybackSource()})
	server, _ := newFnosTestServer(t, fake.server.URL+"/emby", defaultRedirect())

	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/Items/movie-1/PlaybackInfo", strings.NewReader(`{}`)))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	if got := fake.lastPath(); got != "/emby/Items/movie-1/PlaybackInfo" {
		t.Fatalf("上游收到 %q, want /emby/Items/movie-1/PlaybackInfo", got)
	}
}

// 网页播放器的 crossorigin 必须被摘掉，并插入兜底守卫。
func TestFnosStripsCrossOriginFromWebPlayerScript(t *testing.T) {
	fake := newFakeFnos(t, []map[string]any{fnosStrmPlaybackSource()})
	server, _ := newFnosTestServer(t, fake.server.URL, defaultRedirect())

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/emby/web/modules/htmlvideoplayer/basehtmlplayer.js", nil)
	request.Header.Set("Accept-Encoding", "gzip")
	server.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %q", recorder.Code, recorder.Body.String())
	}
	body := recorder.Body.String()
	if strings.Contains(body, `?null:"anonymous"`) {
		t.Fatalf("crossorigin 表达式仍在脚本里：%q", body)
	}
	if !strings.HasPrefix(body, ";try{") {
		t.Fatalf("兜底守卫应当插在脚本最前面：%q", body[:min(40, len(body))])
	}
	if !strings.Contains(body, "HTMLMediaElement.prototype,'crossOrigin'") {
		t.Fatal("兜底守卫内容不完整")
	}
	if got := recorder.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}
	// 脚本改写完后长度会变，Content-Length 必须跟着更新，否则客户端会截断。
	if got := recorder.Header().Get("Content-Length"); got != "" && got != strconv.Itoa(len(body)) {
		t.Fatalf("Content-Length = %q, want %d", got, len(body))
	}
}

// Emby 系客户端用 WebSocket 维持会话控制通道。升级请求必须真的隧道成 101，
// 普通转发会把 Upgrade 头吃掉、握手直接失败——参考项目为此单独写了一条升级
// 代理，所以这条链路要实测，而不是假定 Go 的默认行为就够用。
func TestFnosTunnelsWebSocketUpgrade(t *testing.T) {
	handshake := make(chan string, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		handshake <- request.Header.Get("Upgrade")
		hijacker, ok := writer.(http.Hijacker)
		if !ok {
			t.Error("上游无法接管连接")
			return
		}
		connection, buffer, err := hijacker.Hijack()
		if err != nil {
			t.Errorf("接管上游连接: %v", err)
			return
		}
		defer connection.Close()
		_, _ = buffer.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
		if err := buffer.Flush(); err != nil {
			return
		}
		// 握手之后的双向隧道：回显一个字节即可证明隧道是通的。
		payload := make([]byte, 1)
		if _, err := io.ReadFull(buffer, payload); err != nil {
			return
		}
		_, _ = buffer.Write(payload)
		_ = buffer.Flush()
	}))
	t.Cleanup(upstream.Close)

	server, _ := newFnosTestServer(t, upstream.URL, defaultRedirect())
	frontend := httptest.NewServer(server)
	t.Cleanup(frontend.Close)

	connection, err := net.DialTimeout("tcp", frontend.Listener.Addr().String(), 5*time.Second)
	if err != nil {
		t.Fatalf("连接反代端口: %v", err)
	}
	defer connection.Close()
	if err := connection.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	request := "GET /emby/Socket HTTP/1.1\r\n" +
		"Host: example.com\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n" +
		"Sec-WebSocket-Version: 13\r\n\r\n"
	if _, err := connection.Write([]byte(request)); err != nil {
		t.Fatalf("发送升级请求: %v", err)
	}

	reader := bufio.NewReader(connection)
	statusLine, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("读取响应状态行: %v", err)
	}
	if !strings.Contains(statusLine, "101") {
		t.Fatalf("状态行 = %q，want 101 Switching Protocols", strings.TrimSpace(statusLine))
	}
	if got := <-handshake; !strings.EqualFold(got, "websocket") {
		t.Fatalf("上游收到的 Upgrade = %q，want websocket（被吃掉就说明升级头没透传）", got)
	}
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("读取握手响应头: %v", err)
		}
		if strings.TrimSpace(line) == "" {
			break
		}
	}

	if _, err := connection.Write([]byte("Z")); err != nil {
		t.Fatalf("隧道写入: %v", err)
	}
	echoed, err := reader.ReadByte()
	if err != nil {
		t.Fatalf("隧道读取: %v", err)
	}
	if echoed != 'Z' {
		t.Fatalf("隧道回显 = %q, want 'Z'", echoed)
	}
}

// 飞牛上游的直链缓存与 Emby 共用同一套：第一次取流回头问一次上游并把直链存下，
// 第二次直接命中，而且缓存有效期按直链上的 t 参数动态计算，不是回落到固定 2 小时。
//
// 这里刻意不先走 PlaybackInfo 改写：那条路会把媒体源记进 provider 的 10 分钟内存
// 缓存，命中它根本到不了解析缓存，两条缓存就验混了。绕过它正好也是真实场景之一
// ——客户端自己缓存了播放协商、或 AetherLink 中途重启后直接请求 /stream。
// 上游回中性类型时（网盘 CDN 与移动云 EOS 的常态），「库里的容器名与真实文件
// 不符」也必须能看见：中性类型「没有声明容器」，旧逻辑在日志里一个字都不说，而
// 这一类正是「读了几 KB 就断开」首先要排除的一条。这里只报不改写——回给客户端的
// 头必须与直链一致，类型由客户端自己按内容嗅探。
func TestFnosReportsContainerMismatchFromBytesUnderNeutralUpstreamType(t *testing.T) {
	// 字节是 MP4（ftyp/isom），而客户端请求的是 /stream.mkv。
	body := append([]byte("\x00\x00\x00\x20ftypisom\x00\x00\x02\x00isom"), make([]byte, 48)...)
	origin := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/octet-stream")
		writer.Header().Set("Accept-Ranges", "bytes")
		writer.Header().Set("Content-Range", fmt.Sprintf("bytes 0-%d/%d", len(body)-1, len(body)))
		writer.WriteHeader(http.StatusPartialContent)
		_, _ = writer.Write(body)
	}))
	defer origin.Close()

	source := fnosStrmPlaybackSource()
	source["Path"] = origin.URL + "/d/白色巨塔/S01E01.mkv"
	fake := newFakeFnos(t, []map[string]any{source})
	redirectCfg := defaultRedirect()
	redirectCfg.Mode = config.RedirectNever
	server, _ := newFnosTestServer(t, fake.server.URL, redirectCfg)

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/Videos/movie-1/stream.mkv?MediaSourceId=source-strm", nil)
	request.Header.Set("Range", "bytes=0-")
	request.Header.Set("User-Agent", "AfuseKt/1.0")
	server.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusPartialContent {
		t.Fatalf("状态 = %d, want 206（never 档应当中继）；body = %q", recorder.Code, recorder.Body.String())
	}
	if got := recorder.Header().Get("Content-Type"); got != "application/octet-stream" {
		t.Fatalf("Content-Type = %q，中性类型必须原样转发，不能被我们改写", got)
	}
	if got := recorder.Body.Bytes(); string(got) != string(body) {
		t.Fatalf("body = %d 字节，want %d 字节（判容器不能吃掉开头的字节）", len(got), len(body))
	}
	if !logContainsAll(`请求路径 ".mkv" 暗示 video/x-matroska，实际类型是 video/mp4`, "从响应体字节读出来") {
		t.Fatalf("缺少「容器名与真实字节不符」的告警：%q", findLogEntry("暗示"))
	}
}

func TestFnosReusesCachedDirectLinkForRepeatedPlayback(t *testing.T) {
	expiry := time.Now().Add(30 * time.Minute).Unix()
	source := fnosStrmPlaybackSource()
	source["Path"] = fmt.Sprintf("http://10.0.0.31:25244/d/移动云盘/白色巨塔 (2003)/S01E01.再读.mkv?t=%d", expiry)
	fake := newFakeFnos(t, []map[string]any{source})
	server, collector := newFnosTestServer(t, fake.server.URL, defaultRedirect())

	play := func() *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, "/Videos/movie-1/stream.mkv?MediaSourceId=source-strm", nil)
		request.Header.Set("User-Agent", "AfuseKt/1.0")
		server.ServeHTTP(recorder, request)
		return recorder
	}

	first := play()
	if first.Code != http.StatusFound {
		t.Fatalf("首次取流状态 = %d, want 302; body = %q", first.Code, first.Body.String())
	}
	if _, calls, _ := fake.snapshot(); calls != 1 {
		t.Fatalf("首次取流的 PlaybackInfo 调用 = %d, want 1（媒体源缓存为空，必然回头问一次）", calls)
	}

	second := play()
	if second.Code != http.StatusFound {
		t.Fatalf("第二次取流状态 = %d, want 302; body = %q", second.Code, second.Body.String())
	}
	if got, want := second.Header().Get("Location"), first.Header().Get("Location"); got != want {
		t.Fatalf("两次 302 目标不一致：%q → %q", want, got)
	}
	if _, calls, _ := fake.snapshot(); calls != 1 {
		t.Fatalf("第二次取流的 PlaybackInfo 调用 = %d, want 1（应命中直链缓存，不再问上游）", calls)
	}

	snapshot := collector.Snapshot(10)
	if snapshot.CacheMisses != 1 || snapshot.CacheHits != 1 {
		t.Fatalf("缓存未命中/命中 = %d/%d, want 1/1", snapshot.CacheMisses, snapshot.CacheHits)
	}
	if len(snapshot.RecentEvents) != 2 {
		t.Fatalf("播放流水条数 = %d, want 2", len(snapshot.RecentEvents))
	}
	// RecentEvents 是倒序的：第 0 条就是第二次取流。
	hit := snapshot.RecentEvents[0]
	if hit.CacheSource != string(resolver.CacheSourceHit) {
		t.Fatalf("第二次取流的缓存状态 = %q, want %q", hit.CacheSource, resolver.CacheSourceHit)
	}
	if hit.CacheTTLSeconds < 1700 || hit.CacheTTLSeconds > 1800 {
		t.Fatalf("命中时的缓存有效期 = %ds, want 约 1800s（按直链的 t 算，而不是回落 2 小时）", hit.CacheTTLSeconds)
	}
}
