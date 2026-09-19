package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aetherlink/aetherlink/internal/config"
	"github.com/aetherlink/aetherlink/internal/logx"
	"github.com/aetherlink/aetherlink/internal/pathmap"
	"github.com/aetherlink/aetherlink/internal/resolver"
	"github.com/aetherlink/aetherlink/internal/stats"
	"github.com/aetherlink/aetherlink/internal/strm"
	"github.com/aetherlink/aetherlink/internal/upstream"
)

func TestAppleAudioTranscodeDetection(t *testing.T) {
	if !isAppleAudioClient("Mozilla/5.0 (iPhone; CPU iPhone OS 18_0 like Mac OS X) AppleWebKit/605.1") {
		t.Fatal("iPhone User-Agent should be recognized")
	}
	if !isAppleAudioClient("Player/1.0 iOS") {
		t.Fatal("iOS User-Agent should be recognized")
	}
	if isAppleAudioClient("Mozilla/5.0 (X11; Linux x86_64) Chrome/130") {
		t.Fatal("Linux User-Agent should not be recognized as Apple")
	}

	remote := &resolver.Resolution{Target: &strm.Target{
		Type:     strm.TargetRemote,
		URL:      "https://example.test/audio/track.bin?filename=book.wma",
		Filename: "book.wma",
	}}
	if !isIncompatibleAudio(remote) {
		t.Fatal("WMA remote target should require transcode")
	}

	local := &resolver.Resolution{Target: &strm.Target{
		Type: strm.TargetLocal,
		Path: "/media/chapter.aac",
	}}
	if !isIncompatibleAudio(local) {
		t.Fatal("AAC local target should require transcode")
	}

	compatible := &resolver.Resolution{Target: &strm.Target{
		Type:     strm.TargetRemote,
		URL:      "https://example.test/audio/chapter.m4a",
		Filename: "chapter.m4a",
	}}
	if isIncompatibleAudio(compatible) {
		t.Fatal("M4A target should keep the normal 302 path")
	}

	if got := audioCacheKey("https://example.test/a.aac", "aac-remux", "Komic-iOS"); got == "" {
		t.Fatal("audio cache key should not be empty")
	}
	if note := audioAdaptationNote(false); !strings.Contains(note, "首次") {
		t.Fatalf("first audio cache note = %q", note)
	}
	if note := audioAdaptationNote(true); !strings.Contains(note, "命中") {
		t.Fatalf("hit audio cache note = %q", note)
	}
}

func TestAudioCachePersistsByBookAndRefreshesIdleTime(t *testing.T) {
	directory := t.TempDir()
	cache := &audioCache{dir: directory, entries: make(map[string]*audioCacheEntry)}
	key := audioCacheKey("https://example.test/book.aac", "aac-remux", "Komic-iOS")

	filename, hit, err := cache.getOrCreate(context.Background(), "测试书", key, "第001集.m4a", func(destination string) error {
		return os.WriteFile(destination, []byte("m4a"), 0o600)
	})
	if err != nil || hit {
		t.Fatalf("first cache request = filename %q hit %v err %v", filename, hit, err)
	}
	if filepath.Base(filepath.Dir(filepath.Dir(filename))) != "测试书" {
		t.Fatalf("cache directory = %q, want book name", filepath.Base(filepath.Dir(filepath.Dir(filename))))
	}
	if filepath.Base(filename) != "第001集.m4a" {
		t.Fatalf("cache filename = %q, want original filename", filepath.Base(filename))
	}

	restarted := &audioCache{dir: directory, entries: make(map[string]*audioCacheEntry)}
	restored, hit, err := restarted.getOrCreate(context.Background(), "测试书", key, "第001集.m4a", func(string) error {
		t.Fatal("cache rebuild should not run after restart")
		return nil
	})
	if err != nil || !hit || restored != filename {
		t.Fatalf("restored cache = filename %q hit %v err %v", restored, hit, err)
	}
}

// fakeABS stands in for an Audiobookshelf server. It serves the item metadata
// used to locate media on disk plus a plain UI route for pass-through checks.
type fakeABS struct {
	server      *httptest.Server
	strmPath    string
	regularPath string
	bearerSeen  string
	hits        int
}

func newFakeABS(t *testing.T, strmPath, regularPath string) *fakeABS {
	t.Helper()
	fake := &fakeABS{strmPath: strmPath, regularPath: regularPath}
	mux := http.NewServeMux()

	mux.HandleFunc("/api/items/book-1", func(writer http.ResponseWriter, request *http.Request) {
		fake.hits++
		fake.bearerSeen = request.Header.Get("Authorization")
		writeJSON(t, writer, map[string]any{
			"id":        "book-1",
			"libraryId": "lib-1",
			"mediaType": "book",
			"libraryFiles": []map[string]any{
				{"ino": "ino-strm", "metadata": map[string]any{"filename": "001.strm", "ext": ".strm", "path": strmPath}},
				{"ino": "ino-plain", "metadata": map[string]any{"filename": "002.m4a", "ext": ".m4a", "path": regularPath}},
			},
			"media": map[string]any{
				"metadata":   map[string]any{"title": "测试有声书", "authorName": "作者"},
				"audioFiles": []map[string]any{{"index": 1, "ino": "ino-strm", "duration": 0, "metadata": map[string]any{"filename": "001.strm", "ext": ".strm", "path": strmPath}}},
			},
		})
	})
	mux.HandleFunc("/api/items/book-1/file/ino-plain", func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "audio/mp4")
		writer.Write([]byte("upstream-served-bytes"))
	})
	// 会话响应刻意不带 audioTracks：真实的 Audiobookshelf 就是这样，
	// PlaybackSession 模型不持久化音轨，只有 libraryItemId 可用。
	mux.HandleFunc("/api/session/sess-1", func(writer http.ResponseWriter, request *http.Request) {
		writeJSON(t, writer, map[string]any{"id": "sess-1", "libraryItemId": "book-1"})
	})
	// 刚开始播放的会话还没落库，这条接口就是 404，只能去活跃会话列表里找。
	mux.HandleFunc("/api/session/sess-new", func(writer http.ResponseWriter, request *http.Request) {
		http.Error(writer, "not found", http.StatusNotFound)
	})
	mux.HandleFunc("/api/sessions/open", func(writer http.ResponseWriter, request *http.Request) {
		writeJSON(t, writer, map[string]any{
			"sessions": []map[string]any{{"id": "sess-new", "libraryItemId": "book-1"}},
		})
	})
	mux.HandleFunc("/api/libraries", func(writer http.ResponseWriter, request *http.Request) {
		writeJSON(t, writer, map[string]any{"libraries": []map[string]any{{"id": "lib-1", "name": "有声书", "mediaType": "book"}}})
	})
	mux.HandleFunc("/", func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("X-Fake-Upstream", "1")
		writer.Write([]byte("upstream-ui:" + request.URL.Path))
	})

	fake.server = httptest.NewServer(mux)
	t.Cleanup(fake.server.Close)
	return fake
}

func writeJSON(t *testing.T, writer http.ResponseWriter, payload any) {
	t.Helper()
	writer.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(writer).Encode(payload); err != nil {
		t.Fatalf("encode fake response: %v", err)
	}
}

// newTestServer wires a proxy in front of the fake upstream.
func newTestServer(t *testing.T, absURL, strmRoot string, redirectCfg config.Redirect) (*Server, *stats.Collector) {
	t.Helper()
	return newTestServerWithRelayExempt(t, absURL, strmRoot, redirectCfg, nil)
}

// newTestServerWithRelayExempt 与 newTestServer 相同，额外挂上卡片上的
// 「不中继的客户端」名单（UA 片段）。
func newTestServerWithRelayExempt(t *testing.T, absURL, strmRoot string, redirectCfg config.Redirect, relayExempt []string) (*Server, *stats.Collector) {
	t.Helper()
	provider, err := upstream.New(config.Upstream{
		Name:       "abs",
		Type:       config.UpstreamAudiobookshelf,
		BaseURL:    absURL,
		APIKey:     "test-api-key",
		ListenPort: 13378,
		StrmRoots:  []string{pathmap.Normalize(strmRoot)},
	})
	if err != nil {
		t.Fatalf("build provider: %v", err)
	}
	collector := stats.New(50)
	mediaResolver := resolver.New(config.Cache{TTL: time.Minute, MaxSize: 32}, redirectCfg)
	return New(provider, mediaResolver, collector, redirectCfg, relayExempt), collector
}

func TestBlockedUserAgentIsRejectedBeforeProxying(t *testing.T) {
	root, _, _ := writeStrm(t, "http://10.0.0.31:19527/d/blocked.m4a")
	redirectCfg := defaultRedirect()
	redirectCfg.BlockClientUserAgentAudiobookshelf = config.Bool(true)
	redirectCfg.BlockedUserAgentsAudiobookshelf = []string{"Filmly"}
	redirectCfg.BlockedUserAgentsAudiobookshelfUpstreams = []string{"abs"}
	server, _ := newTestServer(t, "http://127.0.0.1:1", root, redirectCfg)

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/library/audiobooks", nil)
	request.Header.Set("User-Agent", "Filmly/99.0.0-217")
	server.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", recorder.Code)
	}
}

func defaultRedirect() config.Redirect {
	return config.Redirect{
		Mode:               config.RedirectAlways,
		MaxFollowHops:      3,
		ForwardUserAgent:   config.Bool(true),
		FallbackUserAgent:  "AetherLink",
		ProbeTimeout:       5 * time.Second,
		AllowPublicTargets: config.Bool(true),
	}
}

// 卡片上的「不中继的客户端」名单：命中者即使选「始终中继」也要拿到直链。
//
// 依据来自用户实例：AfuseKt 那一系播放器只在拿着直链自己取流时能播，走 AetherLink
// 的中继会读几 KB 就撒手，而中继的字节与响应头已逐项证实与直链等价——这是客户端
// 自己的选路差异，中继侧没有可改的东西，所以出路只能是让这些客户端绕开中继。
func TestRelayExemptClientGetsTheDirectLinkEvenUnderAlwaysRelay(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Write([]byte("origin-stream-bytes"))
	}))
	defer origin.Close()

	root, strmPath, regularPath := writeStrm(t, origin.URL+"/d/relay-exempt.m4a")
	fake := newFakeABS(t, strmPath, regularPath)
	redirectCfg := defaultRedirect()
	redirectCfg.Mode = config.RedirectNever
	server, collector := newTestServerWithRelayExempt(t, fake.server.URL, root, redirectCfg, []string{"AfuseKt"})

	exemptRecorder := httptest.NewRecorder()
	exemptRequest := httptest.NewRequest(http.MethodGet, "/api/items/book-1/file/ino-strm", nil)
	exemptRequest.Header.Set("User-Agent", "AfuseKt%2F%28Linux%3BAndroid+Release%29Player")
	server.ServeHTTP(exemptRecorder, exemptRequest)
	if exemptRecorder.Code != http.StatusFound {
		t.Fatalf("命中名单的客户端状态 = %d, want 302；body = %q", exemptRecorder.Code, exemptRecorder.Body.String())
	}
	if location := exemptRecorder.Header().Get("Location"); location != origin.URL+"/d/relay-exempt.m4a" {
		t.Fatalf("Location = %q, want 直链（名单命中就该把直链交出去）", location)
	}
	// 302 行是这件事唯一的输出口：用户得能从日志看出「这次不是模式选错，是名单命中」。
	if !logContainsAll("302 /api/items/book-1/file/ino-strm", "不中继的客户端") {
		t.Fatalf("302 行没写明是名单命中的：%q", findLogEntry("302 /api/items/book-1/file/ino-strm"))
	}

	plainRecorder := httptest.NewRecorder()
	plainRequest := httptest.NewRequest(http.MethodGet, "/api/items/book-1/file/ino-strm", nil)
	plainRequest.Header.Set("User-Agent", "Infuse/8.0")
	server.ServeHTTP(plainRecorder, plainRequest)
	if plainRecorder.Code != http.StatusOK || plainRecorder.Body.String() != "origin-stream-bytes" {
		t.Fatalf("未命中名单的客户端应当继续中继，得到 %d / %q", plainRecorder.Code, plainRecorder.Body.String())
	}

	if snapshot := collector.Snapshot(10); snapshot.Redirects != 1 || snapshot.ProxyStreams != 1 {
		t.Fatalf("跳转/中继 = %d/%d, want 1/1", snapshot.Redirects, snapshot.ProxyStreams)
	}
}

// 安全网优先于名单：直链是内网地址而客户端在外网时，302 出去也连不上，名单这一次
// 没有出路，只能中继——但日志必须写明白，否则用户会以为名单没生效。
func TestRelayExemptListGivesWayToTheIntranetTargetSafeNet(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Write([]byte("intranet-bytes"))
	}))
	defer origin.Close()

	root, strmPath, regularPath := writeStrm(t, origin.URL+"/d/intranet-exempt.m4a")
	fake := newFakeABS(t, strmPath, regularPath)
	redirectCfg := defaultRedirect()
	redirectCfg.Mode = config.RedirectNever
	redirectCfg.FollowUpstreamRedirects = true
	server, collector := newTestServerWithRelayExempt(t, fake.server.URL, root, redirectCfg, []string{"AfuseKt"})

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/items/book-1/file/ino-strm", nil)
	request.Header.Set("User-Agent", "AfuseKt/1.0")
	server.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || recorder.Body.String() != "intranet-bytes" {
		t.Fatalf("内网直链 + 外网客户端应当中继，得到 %d / %q", recorder.Code, recorder.Body.String())
	}
	if event := collector.Snapshot(1).RecentEvents[0]; event.Outcome != stats.OutcomeProxyStream {
		t.Fatalf("outcome = %q, want proxy", event.Outcome)
	}
	if !logContainsAll("中继 /api/items/book-1/file/ino-strm", "名单这一次没有出路") {
		t.Fatalf("中继行应当写明名单这次没有出路：%q", findLogEntry("中继 /api/items/book-1/file/ino-strm"))
	}
}

// 安全网：跟随上游重定向开启、直链跟到底仍是没有跳转的内网地址（OpenList
// 本地代理形态）而客户端在外网时，「始终跳转」也不能把连不上的地址 302 出去，
// 改由 AetherLink 中继保证能播。
func TestIntranetLocalProxyTargetRelaysForPublicClient(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Write([]byte("origin-stream-bytes"))
	}))
	defer origin.Close()

	root, strmPath, regularPath := writeStrm(t, origin.URL+"/d/local-proxy.m4a")
	fake := newFakeABS(t, strmPath, regularPath)
	redirectCfg := defaultRedirect()
	redirectCfg.FollowUpstreamRedirects = true
	server, collector := newTestServer(t, fake.server.URL, root, redirectCfg)

	// 外网客户端（httptest 默认 192.0.2.1 属公网段）：内网直链 302 出去连不上，
	// 必须中继出字节。
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/items/book-1/file/ino-strm", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("public client status = %d, want 200 (relay); body=%s", recorder.Code, recorder.Body.String())
	}
	if recorder.Body.String() != "origin-stream-bytes" {
		t.Fatalf("body = %q, want origin bytes", recorder.Body.String())
	}
	if event := collector.Snapshot(1).RecentEvents[0]; event.Outcome != stats.OutcomeProxyStream {
		t.Fatalf("outcome = %q, want proxy", event.Outcome)
	}
	// 这一行的文案只写「结果 + 原因」：补救办法（关掉 OpenList 的本地代理、把它发布到
	// 公网）属于 README 的故障对照表，日志里不写。用户 2026-09-18 明确要求过这一条，
	// 所以除了钉住该有的那句，还要钉住那句被删掉的不再回来。
	if !logContainsAll("中继 /api/items/book-1/file/ino-strm", "直链是内网地址而客户端在外网，本次由 AetherLink 中继") {
		t.Fatalf("中继行应当写明安全网原因：%q", findLogEntry("中继 /api/items/book-1/file/ino-strm"))
	}
	if line := findLogEntry("中继 /api/items/book-1/file/ino-strm"); strings.Contains(line, "发布到公网") {
		t.Fatalf("日志只写结果，不写补救办法：%q", line)
	}

	// 内网客户端连内网直链没有障碍，照常 302。
	privateRecorder := httptest.NewRecorder()
	privateRequest := httptest.NewRequest(http.MethodGet, "/api/items/book-1/file/ino-strm", nil)
	privateRequest.RemoteAddr = "192.168.1.20:5000"
	server.ServeHTTP(privateRecorder, privateRequest)
	if privateRecorder.Code != http.StatusFound {
		t.Fatalf("private client status = %d, want 302", privateRecorder.Code)
	}
	if snapshot := collector.Snapshot(10); snapshot.Redirects != 1 || snapshot.ProxyStreams != 1 {
		t.Fatalf("redirects = %d, proxy = %d, want 1/1", snapshot.Redirects, snapshot.ProxyStreams)
	}
}

// 安全网的条件是「最终地址仍是内网」，不是「一步都没跳」：OpenList 也可能
// 先回一跳再落到它自己的内网地址（Hops>0）。这种目标 302 给外网客户端一样
// 连不上，必须中继。
func TestFollowUpstreamRedirectToIntranetRelaysForPublicClient(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Write([]byte("intranet-hop-bytes"))
	}))
	defer origin.Close()

	// 上游先回一跳，落点仍是内网地址（httptest 只能监听 loopback）。
	hop := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		http.Redirect(writer, request, origin.URL+"/d/after-hop.m4a", http.StatusFound)
	}))
	defer hop.Close()

	root, strmPath, regularPath := writeStrm(t, hop.URL+"/d/before-hop.m4a")
	fake := newFakeABS(t, strmPath, regularPath)
	redirectCfg := defaultRedirect()
	redirectCfg.FollowUpstreamRedirects = true
	server, collector := newTestServer(t, fake.server.URL, root, redirectCfg)

	// 外网客户端（httptest 默认 192.0.2.1 属公网段）：落点是内网地址，
	// 302 出去连不上，必须中继出字节。
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/items/book-1/file/ino-strm", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (relay); body=%s", recorder.Code, recorder.Body.String())
	}
	if recorder.Body.String() != "intranet-hop-bytes" {
		t.Fatalf("body = %q, want origin bytes", recorder.Body.String())
	}
	if event := collector.Snapshot(1).RecentEvents[0]; event.Outcome != stats.OutcomeProxyStream {
		t.Fatalf("outcome = %q, want proxy", event.Outcome)
	}
}

// 中继日志要能区分「上游断流」与「写回客户端失败」，靠的就是读端单独记错：
// 只有非 EOF 的读错误才算上游断流，写端失败不能被算到媒体源头上。
func TestReadErrorRecorderKeepsReadFailuresApartFromEOF(t *testing.T) {
	// 读到正常结束（EOF）：不是失败。
	complete := &readErrorRecorder{reader: strings.NewReader("0123456789")}
	if _, err := io.Copy(io.Discard, complete); err != nil {
		t.Fatalf("copy returned error: %v", err)
	}
	if complete.err != nil {
		t.Fatalf("err = %v，want nil（EOF 不算上游断流）", complete.err)
	}

	// 读到一半断流：必须留下证据。
	broken := errors.New("connection reset by peer")
	partial := &readErrorRecorder{reader: io.MultiReader(strings.NewReader("012"), stubFailingReader{err: broken})}
	written, err := io.Copy(io.Discard, partial)
	if !errors.Is(err, broken) {
		t.Fatalf("copy error = %v，want %v", err, broken)
	}
	if written != 3 {
		t.Fatalf("written = %d，want 3（首段读出的 3 字节已经写出去了）", written)
	}
	if !errors.Is(partial.err, broken) {
		t.Fatalf("err = %v，want %v（上游断流要能归因）", partial.err, broken)
	}

	// 写端失败：读端保持干净，日志才会去怪播放器而不是媒体源。
	writeFail := &readErrorRecorder{reader: strings.NewReader("0123456789")}
	if _, err := io.Copy(stubFailingWriter{}, writeFail); err == nil {
		t.Fatal("copy to a failing writer returned nil error")
	}
	if writeFail.err != nil {
		t.Fatalf("err = %v，want nil（写失败不该记成上游断流）", writeFail.err)
	}
}

type stubFailingReader struct{ err error }

func (r stubFailingReader) Read([]byte) (int, error) { return 0, r.err }

type stubFailingWriter struct{}

func (stubFailingWriter) Write([]byte) (int, error) { return 0, errors.New("client went away") }

func TestFormatCacheTTLUsesMinutesAndHoursWithoutSeconds(t *testing.T) {
	tests := []struct {
		seconds int64
		want    string
	}{
		{seconds: 45, want: "1min"},
		{seconds: 3599, want: "60min"},
		{seconds: 3600, want: "1h"},
		{seconds: 3661, want: "1h 2min"},
	}
	for _, test := range tests {
		if got := formatCacheTTL(test.seconds); got != test.want {
			t.Fatalf("formatCacheTTL(%d) = %q, want %q", test.seconds, got, test.want)
		}
	}
}

func TestFormatBytesReadsLikeALogLine(t *testing.T) {
	tests := []struct {
		value int64
		want  string
	}{
		{value: -1, want: "未知"},
		{value: 0, want: "0B"},
		{value: 512, want: "512B"},
		{value: 2048, want: "2.0KiB"},
		{value: 5 * 1024 * 1024, want: "5.0MiB"},
		{value: 3 * 1024 * 1024 * 1024, want: "3.00GiB"},
	}
	for _, test := range tests {
		if got := formatBytes(test.value); got != test.want {
			t.Fatalf("formatBytes(%d) = %q, want %q", test.value, got, test.want)
		}
	}
}

// 中继不改上游给的类型，只有上游压根没给时才轮到这张表（本地读盘那一路也用它），
// 因此它仍直接决定播放器在那些情况下看到的类型。曾把 .mp4/.webm 也归进 audio/*，
// 播放器读到「这是音轨」后立刻断开，日志里只剩一行「状态 206」。
func TestMimeTypeForURLKeepsVideoAndAudioApart(t *testing.T) {
	tests := []struct {
		target string
		want   string
	}{
		{target: "https://cdnfhnfile.115cdn.net/6a9d/%E4%BA%A4%E9%94%8B.S01E01.2160p.mp4?t=1790000978&k=abc", want: "video/mp4"},
		{target: "/vol1/1000/NetDisk/交锋.S01E01.m4v", want: "video/mp4"},
		{target: "https://cdn.example.com/clip.webm", want: "video/webm"},
		{target: "https://cdn.example.com/movie.mkv", want: "video/x-matroska"},
		{target: "https://cdn.example.com/book.m4b", want: "audio/mp4"},
		{target: "https://cdn.example.com/song.m4a", want: "audio/mp4"},
		{target: "https://cdn.example.com/speech.webma", want: "audio/webm"},
		// 认不出的扩展名不猜：宁可把上游自己的类型透给客户端。
		{target: "https://cdn.example.com/api/items/ino-strm", want: ""},
	}
	for _, test := range tests {
		if got := mimeTypeForURL(test.target); got != test.want {
			t.Fatalf("mimeTypeForURL(%q) = %q, want %q", test.target, got, test.want)
		}
	}
}

// writeStrm creates a .strm pointer and a sibling regular audio file.
func writeStrm(t *testing.T, contents string) (root, strmPath, regularPath string) {
	t.Helper()
	root = t.TempDir()
	strmPath = filepath.Join(root, "001.strm")
	if err := os.WriteFile(strmPath, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
	regularPath = filepath.Join(root, "002.m4a")
	if err := os.WriteFile(regularPath, []byte("local-audio"), 0o644); err != nil {
		t.Fatal(err)
	}
	return root, pathmap.Normalize(strmPath), pathmap.Normalize(regularPath)
}

func TestStrmRequestAnswersWith302(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		t.Errorf("strm backend must not be contacted when redirecting, got %s", request.URL.Path)
	}))
	defer backend.Close()

	root, strmPath, regularPath := writeStrm(t, backend.URL+"/d/bi6jeznun2rvu88v6.m4a?/001.总序.m4a")
	fake := newFakeABS(t, strmPath, regularPath)
	server, collector := newTestServer(t, fake.server.URL, root, defaultRedirect())

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/items/book-1/file/ino-strm", nil)
	request.Header.Set("User-Agent", "Emby/4.8")
	server.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302; body=%s", recorder.Code, recorder.Body.String())
	}
	location := recorder.Header().Get("Location")
	if !strings.Contains(location, "/d/bi6jeznun2rvu88v6.m4a?/001.%E6%80%BB%E5%BA%8F.m4a") {
		t.Fatalf("Location = %q, want percent-encoded pick-code URL", location)
	}
	if _, err := url.Parse(location); err != nil {
		t.Fatalf("Location is not a valid URL: %v", err)
	}
	if fake.bearerSeen != "Bearer test-api-key" {
		t.Fatalf("upstream saw Authorization %q, want bearer api key", fake.bearerSeen)
	}

	snapshot := collector.Snapshot(10)
	if snapshot.Redirects != 1 {
		t.Fatalf("redirect count = %d, want 1", snapshot.Redirects)
	}
	if snapshot.RecentEvents[0].Kind != "pickcode115" {
		t.Fatalf("event kind = %q, want pickcode115", snapshot.RecentEvents[0].Kind)
	}
}

func TestSecondRequestUsesResolutionCache(t *testing.T) {
	root, strmPath, regularPath := writeStrm(t, "http://10.0.0.31:19527/d/bi6jeznun2rvu88v6.m4a")
	fake := newFakeABS(t, strmPath, regularPath)
	server, collector := newTestServer(t, fake.server.URL, root, defaultRedirect())

	for i := 0; i < 3; i++ {
		recorder := httptest.NewRecorder()
		server.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/items/book-1/file/ino-strm", nil))
		if recorder.Code != http.StatusFound {
			t.Fatalf("request %d status = %d", i, recorder.Code)
		}
	}
	if fake.hits != 1 {
		t.Fatalf("upstream metadata hits = %d, want 1 (cache should absorb repeats)", fake.hits)
	}
	if snapshot := collector.Snapshot(10); snapshot.CacheHits != 2 {
		t.Fatalf("cache hits = %d, want 2", snapshot.CacheHits)
	}
}

func TestRegularFileIsProxiedToUpstream(t *testing.T) {
	root, strmPath, regularPath := writeStrm(t, "http://10.0.0.31:19527/d/abc.m4a")
	fake := newFakeABS(t, strmPath, regularPath)
	server, collector := newTestServer(t, fake.server.URL, root, defaultRedirect())

	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/items/book-1/file/ino-plain", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	if body := recorder.Body.String(); body != "upstream-served-bytes" {
		t.Fatalf("body = %q, want upstream bytes", body)
	}
	if snapshot := collector.Snapshot(10); snapshot.Passthroughs != 1 {
		t.Fatalf("passthrough count = %d, want 1", snapshot.Passthroughs)
	}
}

func TestNonMediaRequestIsProxiedUntouched(t *testing.T) {
	root, strmPath, regularPath := writeStrm(t, "http://10.0.0.31:19527/d/abc.m4a")
	fake := newFakeABS(t, strmPath, regularPath)
	server, _ := newTestServer(t, fake.server.URL, root, defaultRedirect())

	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/library/audiobooks", nil))

	if recorder.Header().Get("X-Fake-Upstream") != "1" {
		t.Fatalf("non-media request was not proxied: headers=%v", recorder.Header())
	}
	if body := recorder.Body.String(); body != "upstream-ui:/library/audiobooks" {
		t.Fatalf("body = %q", body)
	}
}

func TestRedirectNeverRelaysBytesWithRange(t *testing.T) {
	var seenRange, seenUserAgent string
	backend := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		seenRange = request.Header.Get("Range")
		seenUserAgent = request.Header.Get("User-Agent")
		writer.Header().Set("Content-Range", "bytes 2-5/11")
		writer.Header().Set("Accept-Ranges", "bytes")
		writer.WriteHeader(http.StatusPartialContent)
		writer.Write([]byte("cdef"))
	}))
	defer backend.Close()

	root, strmPath, regularPath := writeStrm(t, backend.URL+"/d/abc.m4a")
	fake := newFakeABS(t, strmPath, regularPath)
	redirectCfg := defaultRedirect()
	redirectCfg.Mode = config.RedirectNever
	server, collector := newTestServer(t, fake.server.URL, root, redirectCfg)

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/items/book-1/file/ino-strm", nil)
	request.Header.Set("Range", "bytes=2-5")
	request.Header.Set("User-Agent", "Emby/4.8")
	server.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusPartialContent {
		t.Fatalf("status = %d, want 206", recorder.Code)
	}
	if recorder.Body.String() != "cdef" {
		t.Fatalf("body = %q, want relayed range", recorder.Body.String())
	}
	if seenRange != "bytes=2-5" {
		t.Fatalf("backend saw Range %q", seenRange)
	}
	if seenUserAgent != "Emby/4.8" {
		t.Fatalf("backend saw User-Agent %q, want the client value forwarded", seenUserAgent)
	}
	if recorder.Header().Get("Content-Range") != "bytes 2-5/11" {
		t.Fatalf("Content-Range not copied: %v", recorder.Header())
	}
	if snapshot := collector.Snapshot(10); snapshot.ProxyStreams != 1 {
		t.Fatalf("proxy stream count = %d, want 1", snapshot.ProxyStreams)
	}
}

// 中继回给播放器的响应必须与 CDN 直连等价，尤其是 Content-Length：少了它，Go 会
// 把响应改写成 chunked，播放器拿不到总长度——moov 在文件尾部的 MP4 就再也无法
// seek，读完头部即断开，而日志里只剩一行「客户端中断」，排查方向全错。
//
// 这里走真实 TCP（httptest.NewServer），因为传输编码由 Go 服务端在写出时才决定，
// ResponseRecorder 看不到。上游照着线上那条中国移动 EOS 直链的真实形态搭：206、
// Accept-Ranges、ETag、路径无扩展名、Content-Disposition 挂着 .iso 文件名，而
// 字节开头是 MP4 的 ftyp——「文件名说 iso、内容其实是 mp4」正是线上发生的事。
func TestRelayWireResponseMatchesTheDirectLink(t *testing.T) {
	const total = 1 << 20
	body := make([]byte, total)
	copy(body, []byte{0x00, 0x00, 0x00, 0x20, 'f', 't', 'y', 'p', 'i', 's', 'o', 'm', 0x00, 0x00, 0x02, 0x00})

	cdn := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		start, end := 0, total-1
		partial := false
		if raw := request.Header.Get("Range"); strings.HasPrefix(raw, "bytes=") {
			partial = true
			parts := strings.SplitN(strings.TrimPrefix(raw, "bytes="), "-", 2)
			if parsed, err := strconv.Atoi(parts[0]); err == nil {
				start = parsed
			}
			if len(parts) == 2 && parts[1] != "" {
				if parsed, err := strconv.Atoi(parts[1]); err == nil {
					end = parsed
				}
			}
		}
		writer.Header().Set("Content-Type", "application/octet-stream")
		writer.Header().Set("Accept-Ranges", "bytes")
		writer.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, total))
		writer.Header().Set("Content-Length", strconv.Itoa(end-start+1))
		writer.Header().Set("ETag", `"f13d7b17792e412feafbb91e18fe745a-337"`)
		writer.Header().Set("Last-Modified", "Thu, 03 Sep 2026 13:27:26 GMT")
		writer.Header().Set("Content-Disposition", "attachment; filename*=UTF-8''01-4K.%E9%AB%98%E7%A0%81%E7%8E%87.iso")
		if partial {
			writer.WriteHeader(http.StatusPartialContent)
		}
		_, _ = writer.Write(body[start : end+1])
	}))
	defer cdn.Close()

	// 线上那条链的路径只有一串 ID，没有扩展名——按扩展名猜类型必然落空。
	root, strmPath, regularPath := writeStrm(t, cdn.URL+"/50f351580c084c969c0c84f4e500ed39086")
	fake := newFakeABS(t, strmPath, regularPath)
	redirectCfg := defaultRedirect()
	redirectCfg.Mode = config.RedirectNever
	server, _ := newTestServer(t, fake.server.URL, root, redirectCfg)

	front := httptest.NewServer(server)
	defer front.Close()

	request, err := http.NewRequest(http.MethodGet, front.URL+"/api/items/book-1/file/ino-strm", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Range", "bytes=0-")
	response, err := front.Client().Do(request)
	if err != nil {
		t.Fatalf("relay request: %v", err)
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read relayed body: %v", err)
	}

	relayed := map[string]string{}
	for _, header := range []string{"Content-Type", "Content-Length", "Content-Range", "Accept-Ranges", "ETag", "Last-Modified", "Content-Disposition"} {
		relayed[header] = response.Header.Get(header)
		t.Logf("中继 %-20s = %q", header, relayed[header])
	}
	t.Logf("status=%d transferEncoding=%v contentLength=%d bodyBytes=%d",
		response.StatusCode, response.TransferEncoding, response.ContentLength, len(payload))

	if response.StatusCode != http.StatusPartialContent {
		t.Fatalf("status = %d, want 206", response.StatusCode)
	}
	if len(response.TransferEncoding) != 0 {
		t.Fatalf("transferEncoding = %v, want 空（回给播放器必须是定长响应，chunked 会让它拿不到总长度、无法 seek）", response.TransferEncoding)
	}
	if response.ContentLength != total {
		t.Fatalf("Content-Length = %d, want %d", response.ContentLength, total)
	}
	if got, want := response.Header.Get("Content-Range"), fmt.Sprintf("bytes 0-%d/%d", total-1, total); got != want {
		t.Fatalf("Content-Range = %q, want %q", got, want)
	}
	if got := response.Header.Get("Accept-Ranges"); got != "bytes" {
		t.Fatalf("Accept-Ranges = %q, want bytes（播放器据此判断能否 seek）", got)
	}
	if len(payload) != total {
		t.Fatalf("bodyBytes = %d, want %d", len(payload), total)
	}

	// 同一条直链再直连一次，两边逐项对比。中继的全部意义就是「与直连一模一样」：
	// 只要有一个头不同，播放器就可能因为中继而表现异常，而那种故障在日志里只剩一行
	// 「读了少量字节就断开」。这条直链上游给的是中性类型 application/octet-stream
	// （线上实测），中继**不许**替它改写成 video/mp4——302 时播放器拿到的就是中性值，
	// 改写了就等于凭空制造一个差异。
	directRequest, err := http.NewRequest(http.MethodGet, cdn.URL+"/50f351580c084c969c0c84f4e500ed39086", nil)
	if err != nil {
		t.Fatal(err)
	}
	directRequest.Header.Set("Range", "bytes=0-")
	direct, err := cdn.Client().Do(directRequest)
	if err != nil {
		t.Fatalf("direct request: %v", err)
	}
	defer direct.Body.Close()
	if _, err := io.Copy(io.Discard, direct.Body); err != nil {
		t.Fatalf("drain direct body: %v", err)
	}
	for header, relayedValue := range relayed {
		t.Logf("直连 %-20s = %q", header, direct.Header.Get(header))
		if want := direct.Header.Get(header); relayedValue != want {
			t.Fatalf("%s：中继 = %q，直连 = %q，两边必须一致", header, relayedValue, want)
		}
	}
}

// 直链路径没有扩展名时，字节是唯一可信的容器线索。这份表把支持的魔数与「认不出来
// 就别猜」一起钉住——真 ISO 光盘镜像就是认不出来的那类，硬塞一个类型只会误导播放器。
func TestSniffContentTypeReadsTheContainerFromBytes(t *testing.T) {
	cases := []struct {
		name string
		head []byte
		want string
	}{
		{"mp4", []byte("\x00\x00\x00\x20ftypisom\x00\x00\x02\x00mp41"), "video/mp4"},
		{"m4b 有声书", []byte("\x00\x00\x00\x20ftypM4B \x00\x00\x02\x00isom"), "audio/mp4"},
		{"m4a", []byte("\x00\x00\x00\x18ftypM4A mp42isom"), "audio/mp4"},
		{"matroska", []byte("\x1a\x45\xdf\xa3\x01\x00\x00\x00\x00\x00\x00\x1fB\x86\x81\x01matroska"), "video/x-matroska"},
		{"webm", []byte("\x1a\x45\xdf\xa3\x01\x00\x00\x00\x00\x00\x00\x1fB\x86\x81\x01webm"), "video/webm"},
		{"avi", []byte("RIFF\x00\x00\x00\x00AVI LIST"), "video/x-msvideo"},
		{"wav", []byte("RIFF\x00\x00\x00\x00WAVEfmt "), "audio/wav"},
		{"ogg", []byte("OggS\x00\x02\x00\x00\x00\x00\x00\x00\x00\x00"), "application/ogg"},
		{"flac", []byte("fLaC\x00\x00\x00\x22\x00\x00\x00\x00\x00\x00"), "audio/flac"},
		{"asf", []byte("\x30\x26\xb2\x75\x8e\x66\xcf\x11\xa6\xd9\x00\xaa\x00\x62\xce\x6c"), "video/x-ms-asf"},
		{"认不出来就别猜（真 ISO 镜像）", []byte("\x00\x01\x02\x03\x04\x05\x06\x07\x08\x09\x0a\x0b"), ""},
		{"空响应体", nil, ""},
	}
	for _, testCase := range cases {
		if got := sniffContentType(testCase.head); got != testCase.want {
			t.Fatalf("%s: sniffContentType = %q, want %q", testCase.name, got, testCase.want)
		}
	}

	// 只有从文件开头开始的单段请求才谈得上看魔数。
	ranges := []struct {
		raw  string
		want bool
	}{
		{"", true},
		{"bytes=0-", true},
		{"bytes=0-1023", true},
		{"bytes=100-", false},
		{"bytes=-65536", false},
		{"bytes=0-10,20-30", false},
	}
	for _, testCase := range ranges {
		if got := rangeStartsAtZero(testCase.raw); got != testCase.want {
			t.Fatalf("rangeStartsAtZero(%q) = %v, want %v", testCase.raw, got, testCase.want)
		}
	}
}

// 中继不改上游给的 Content-Type。网盘 CDN 对视频回的是中性类型
// application/octet-stream（实测），播放器拿到它会自己按内容嗅探；同一条直链走 302
// 时播放器拿到的也是这一个值。中继若替它改写成具体类型，就等于制造了一个 302 没有的
// 差异——线上就是这么来的：同一片走 302 能播，走中继只读了几 KiB 就断开。
func TestRelayPassesUpstreamContentTypeThroughUnchanged(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/octet-stream")
		writer.Header().Set("Content-Range", "bytes 0-3/6953811142")
		writer.Header().Set("Accept-Ranges", "bytes")
		writer.WriteHeader(http.StatusPartialContent)
		writer.Write([]byte("fLaG"))
	}))
	defer backend.Close()

	root, strmPath, regularPath := writeStrm(t, backend.URL+"/d/video.mp4")
	fake := newFakeABS(t, strmPath, regularPath)
	redirectCfg := defaultRedirect()
	redirectCfg.Mode = config.RedirectNever
	server, collector := newTestServer(t, fake.server.URL, root, redirectCfg)

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/items/book-1/file/ino-strm", nil)
	request.Header.Set("Range", "bytes=0-3")
	server.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusPartialContent {
		t.Fatalf("status = %d, want 206", recorder.Code)
	}
	// 扩展名是 .mp4、字节也不是 MP4，但上游明确给了中性类型：照原样转发。
	if got := recorder.Header().Get("Content-Type"); got != "application/octet-stream" {
		t.Fatalf("Content-Type = %q，want application/octet-stream（中继必须原样转发上游的类型：302 拿到的就是它）", got)
	}
	if recorder.Body.String() != "fLaG" {
		t.Fatalf("body = %q, want relayed bytes", recorder.Body.String())
	}
	if snapshot := collector.Snapshot(10); snapshot.ProxyStreams != 1 {
		t.Fatalf("proxy stream count = %d, want 1", snapshot.ProxyStreams)
	}
}

// 只有上游压根没给 Content-Type 时才轮到我们补，而补的时候字节压过扩展名：直链的
// 名字不可信（网盘上「名字写 .mkv、内容其实是 MP4」很常见），按扩展名回类型会让类型
// 与响应体自相矛盾。这里故意让两边打架——名字是 .mp4、字节是 Matroska——只有字节判断
// 才能推出 video/x-matroska（Go 自己的嗅探认不出 Matroska，只会给 octet-stream，
// 所以这条断言同时钉住了「确实是我们补的」）。
func TestRelaySynthesizesTypeFromBytesOnlyWhenUpstreamGivesNone(t *testing.T) {
	body := append([]byte("\x1a\x45\xdf\xa3\x01\x00\x00\x00\x00\x00\x00\x1fB\x86\x81\x01matroska"), make([]byte, 48)...)

	cdn := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		// 上游一个类型都不给：Go 的服务端只在「Header 里没有这个键」时才放弃自己
		// 嗅探，所以这里必须显式置空而不是什么都不写。
		writer.Header().Set("Content-Type", "")
		writer.Header().Set("Accept-Ranges", "bytes")
		writer.Header().Set("Content-Range", fmt.Sprintf("bytes 0-%d/%d", len(body)-1, len(body)))
		writer.WriteHeader(http.StatusPartialContent)
		_, _ = writer.Write(body)
	}))
	defer cdn.Close()

	root, strmPath, regularPath := writeStrm(t, cdn.URL+"/d/movie.mp4")
	fake := newFakeABS(t, strmPath, regularPath)
	redirectCfg := defaultRedirect()
	redirectCfg.Mode = config.RedirectNever
	server, _ := newTestServer(t, fake.server.URL, root, redirectCfg)

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/items/book-1/file/ino-strm", nil)
	request.Header.Set("Range", "bytes=0-")
	server.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusPartialContent {
		t.Fatalf("status = %d, want 206", recorder.Code)
	}
	if got := recorder.Header().Get("Content-Type"); got != "video/x-matroska" {
		t.Fatalf("Content-Type = %q，want video/x-matroska（上游没给类型，按字节判断：扩展名写着 .mp4 也不能信）", got)
	}
	// 判容器读走的那几个字节必须原样写回，一个都不能少。
	if got := recorder.Body.Bytes(); string(got) != string(body) {
		t.Fatalf("body = %d 字节，want %d 字节（判容器不能吃掉开头的字节）", len(got), len(body))
	}
}

// 纯音频扩展名是补类型时的例外：MP4 家族里有声书与视频共用容器头，品牌字段常是
// isom/mp42，字节分不出「这是音频」，扩展名才是唯一线索。这类不能因为字节像视频就
// 标成视频，否则有声书会被当成影片交给播放器。
func TestRelayKeepsAudioExtensionForAudiobooks(t *testing.T) {
	body := append([]byte("\x00\x00\x00\x20ftypisom\x00\x00\x02\x00isom"), make([]byte, 48)...)

	cdn := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "")
		writer.Header().Set("Content-Range", fmt.Sprintf("bytes 0-%d/%d", len(body)-1, len(body)))
		writer.WriteHeader(http.StatusPartialContent)
		_, _ = writer.Write(body)
	}))
	defer cdn.Close()

	root, strmPath, regularPath := writeStrm(t, cdn.URL+"/d/book.m4b")
	fake := newFakeABS(t, strmPath, regularPath)
	redirectCfg := defaultRedirect()
	redirectCfg.Mode = config.RedirectNever
	server, _ := newTestServer(t, fake.server.URL, root, redirectCfg)

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/items/book-1/file/ino-strm", nil)
	request.Header.Set("Range", "bytes=0-")
	server.ServeHTTP(recorder, request)

	if got := recorder.Header().Get("Content-Type"); got != "audio/mp4" {
		t.Fatalf("Content-Type = %q，want audio/mp4（有声书与视频同容器头，扩展名才是唯一线索）", got)
	}
	if got := recorder.Body.Bytes(); string(got) != string(body) {
		t.Fatalf("body 长度 = %d，want %d", len(got), len(body))
	}
}

// debug 级的「中继交换明细」会把整幅请求头写进日志，凭据因此必须打码：排障要看
// 的是 Range / UA / 内容协商这些，令牌与 Cookie 没有进日志的必要。键要排序，
// 两次日志才能逐行对比。
func TestHeaderNoteMasksCredentialsAndSortsKeys(t *testing.T) {
	header := http.Header{}
	header.Set("X-Emby-Authorization", `MediaBrowser Token="abc123"`)
	header.Set("Cookie", "session=secret")
	header.Set("Range", "bytes=0-")
	header.Set("User-Agent", "AfuseKt/1.0")

	note := headerNote(header)
	if strings.Contains(note, "abc123") || strings.Contains(note, "secret") {
		t.Fatalf("凭据不该进日志：%s", note)
	}
	if !strings.Contains(note, "X-Emby-Authorization=已省略") || !strings.Contains(note, "Cookie=已省略") {
		t.Fatalf("凭据字段应打码：%s", note)
	}
	if !strings.Contains(note, "Range=bytes=0-") || !strings.Contains(note, "User-Agent=AfuseKt/1.0") {
		t.Fatalf("排障要看的字段必须留着：%s", note)
	}
	if strings.Index(note, "Range=") > strings.Index(note, "User-Agent=") || strings.Index(note, "User-Agent=") > strings.Index(note, "X-Emby-Authorization=") {
		t.Fatalf("键应按序排列：%s", note)
	}
	if note := headerNote(http.Header{}); note != "（空）" {
		t.Fatalf("空头 = %q", note)
	}
}

// 中继的完整交换明细只在 debug 级别输出：默认的 info 不能被排障辅助淹掉，而打开
// debug 必须真的能看到双方的头——「同一片 302 能播、中继播不了」这类问题，差别只
// 可能藏在某个头里，光看 info 那四个字段不够定论。
func TestRelayExchangeTraceAppearsOnlyAtDebugLevel(t *testing.T) {
	body := append([]byte("\x00\x00\x00\x20ftypisom\x00\x00\x02\x00mp41"), make([]byte, 48)...)
	cdn := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/octet-stream")
		writer.WriteHeader(http.StatusPartialContent)
		_, _ = writer.Write(body)
	}))
	defer cdn.Close()

	root, strmPath, regularPath := writeStrm(t, cdn.URL+"/d/movie.mkv")
	fake := newFakeABS(t, strmPath, regularPath)
	redirectCfg := defaultRedirect()
	redirectCfg.Mode = config.RedirectNever
	server, _ := newTestServer(t, fake.server.URL, root, redirectCfg)

	relay := func() {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, "/api/items/book-1/file/ino-strm", nil)
		request.Header.Set("Range", "bytes=0-")
		request.Header.Set("X-Emby-Authorization", `MediaBrowser Token="abc123"`)
		server.ServeHTTP(recorder, request)
	}

	logx.SetLevel(logx.LevelInfo)
	relay()
	if trace := findLogEntry("中继交换明细"); trace != "" {
		t.Fatalf("info 级别不该输出交换明细：%s", trace)
	}

	defer logx.SetLevel(logx.LevelInfo)
	logx.SetLevel(logx.LevelDebug)
	relay()
	trace := findLogEntry("中继交换明细")
	if trace == "" {
		t.Fatal("debug 级别必须能看到中继交换明细")
	}
	for _, want := range []string{"客户端请求头", "上游响应头", "回给客户端", "Content-Type=application/octet-stream", "Range=bytes=0-"} {
		if !strings.Contains(trace, want) {
			t.Fatalf("交换明细缺少 %q：%s", want, trace)
		}
	}
	if strings.Contains(trace, "abc123") {
		t.Fatalf("交换明细把令牌写出来了：%s", trace)
	}
}

// findLogEntry 在日志环形缓冲里找一条包含关键字的记录，找不到返回空串。
func findLogEntry(keyword string) string {
	for _, entry := range logx.Recent(0) {
		if strings.Contains(entry.Message, keyword) {
			return entry.Message
		}
	}
	return ""
}

// logContainsAll 判断日志里是否存在一条同时包含全部关键字的记录。
// 环形缓冲是进程级的，用例之间会互相看见，所以「同一个请求路径 + 这句新话」这类
// 断言不能只看第一条命中——那可能是别的用例留下的。
func logContainsAll(keywords ...string) bool {
	for _, entry := range logx.Recent(0) {
		matched := true
		for _, keyword := range keywords {
			if !strings.Contains(entry.Message, keyword) {
				matched = false
				break
			}
		}
		if matched {
			return true
		}
	}
	return false
}

// 客户端请求的容器名来自上游的 Container，而响应里的实际类型来自上游自己或字节判断。
// 两者不符时必须有一条明确的日志：播放器拿错的容器去解复用就会读几百 KB 后断开，而
// 日志里只剩一行「客户端中断」，没有这一行就只能靠猜。中性类型（octet-stream）不算
// 「声明了别的容器」，不能报——网盘 CDN 给的全是它。
// 「客户端读了一点就走」是中继侧唯一看不出差别的失败：字节投递对、响应头逐项等价，
// 播放器却在解析完头部（MKV 的 Tracks、MP4 的 moov）就断开。它的成因分属两边——库里
// 的容器名与真实文件不符，或播放器自己判定这个文件放不了（编码、位深、多字幕轨）——
// 而能分辨它们的只有双方的头。所以这一支必须把客户端请求头与回给客户端的完整头都写
// 出来，不能要求排障的人先去把日志级别改成 debug（这个出口过去只有四个头，两轮排障
// 都卡在这里）。门槛以上的断开（拖进度条、切集）仍保持原来那一行，不刷屏。
func TestRelayEarlyClientAbortPrintsBothSidesHeaders(t *testing.T) {
	payload := make([]byte, 512<<10)
	for index := range payload {
		payload[index] = byte(index)
	}
	cdn := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/octet-stream")
		writer.Header().Set("Content-Range", fmt.Sprintf("bytes 0-%d/%d", len(payload)-1, len(payload)))
		writer.Header().Set("Accept-Ranges", "bytes")
		writer.WriteHeader(http.StatusPartialContent)
		_, _ = writer.Write(payload)
	}))
	defer cdn.Close()

	countEntries := func(keyword string) int {
		count := 0
		for _, entry := range logx.Recent(0) {
			if strings.Contains(entry.Message, keyword) {
				count++
			}
		}
		return count
	}

	// abortAfter 是模拟客户端关连接前吃掉的字节数。
	relayUntilClientLeaves := func(t *testing.T, abortAfter int) {
		t.Helper()
		root, strmPath, regularPath := writeStrm(t, cdn.URL+"/d/movie.mkv")
		fake := newFakeABS(t, strmPath, regularPath)
		redirectCfg := defaultRedirect()
		redirectCfg.Mode = config.RedirectNever
		server, _ := newTestServer(t, fake.server.URL, root, redirectCfg)

		requestCtx, cancel := context.WithCancel(context.Background())
		defer cancel()
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, "/api/items/book-1/file/ino-strm", nil).WithContext(requestCtx)
		request.Header.Set("Range", "bytes=0-")
		request.Header.Set("User-Agent", "AfuseKt%2F%28Linux%3BAndroid+Release%29Player")
		request.Header.Set("X-Emby-Authorization", `MediaBrowser Token="abc123"`)

		server.ServeHTTP(&abortAfterBytesWriter{writer: recorder, cancel: cancel, limit: abortAfter}, request)
		if recorder.Code != http.StatusPartialContent {
			t.Fatalf("status = %d, want 206", recorder.Code)
		}
	}

	// 读了几十 KB 就走：这一行必须自己带上双方的头。
	relayUntilClientLeaves(t, 1)
	entry := findLogEntry("只读了")
	if entry == "" {
		t.Fatal("客户端读了一点就断开必须有一条 warn，写明它到底吃了多少")
	}
	for _, want := range []string{"客户端请求头", "回给客户端", "Range=bytes=0-", "User-Agent=AfuseKt%2F%28Linux%3BAndroid+Release%29Player", "始终跳转"} {
		if !strings.Contains(entry, want) {
			t.Fatalf("这一行缺少 %q：%s", want, entry)
		}
	}
	if strings.Contains(entry, "abc123") {
		t.Fatalf("这一行把令牌写出来了：%s", entry)
	}
	if earlyAborts := countEntries("只读了"); earlyAborts != 1 {
		t.Fatalf("「只读了」应只出现一次，实际 %d 次", earlyAborts)
	}

	// 吃到 80KiB 才断（拖进度条、切集那一类）：保持原来那一行，不再把整幅头写出来。
	relayUntilClientLeaves(t, 80<<10)
	if findLogEntry("拖进度条与切集是正常形态") == "" {
		t.Fatal("门槛以上的断开应保持原来那一行")
	}
	if earlyAborts := countEntries("只读了"); earlyAborts != 1 {
		t.Fatalf("门槛以上的断开不该升级成 warn，实际多出 %d 条", earlyAborts-1)
	}
}

// abortAfterBytesWriter 模拟「客户端吃到一定字节数就把连接关掉」：累计写出的字节数到达
// 门槛后取消请求上下文（真实场景里由客户端断开触发），并返回写错误让 io.Copy 立刻停下。
// 门槛设成 1 就是「读了一点就不认这个响应」，设成几十 KB 以上就是拖进度条那一类。
type abortAfterBytesWriter struct {
	writer  http.ResponseWriter
	cancel  context.CancelFunc
	limit   int
	written int
}

func (w *abortAfterBytesWriter) Header() http.Header { return w.writer.Header() }

func (w *abortAfterBytesWriter) WriteHeader(status int) { w.writer.WriteHeader(status) }

func (w *abortAfterBytesWriter) Write(buffer []byte) (int, error) {
	written, err := w.writer.Write(buffer)
	w.written += written
	if w.written >= w.limit {
		w.cancel()
		if err == nil {
			err = io.ErrClosedPipe
		}
	}
	return written, err
}

func TestContainerMismatchNoteNamesBothSides(t *testing.T) {
	note := containerMismatchNote("/emby/videos/42/stream.mkv", "video/mp4")
	for _, want := range []string{".mkv", "video/x-matroska", "video/mp4"} {
		if !strings.Contains(note, want) {
			t.Fatalf("提示里必须写清两边（缺 %q）：%q", want, note)
		}
	}
	for _, testCase := range []struct{ requestPath, actualType string }{
		{"/emby/videos/42/stream.mkv", "video/x-matroska"},         // 两边一致
		{"/emby/videos/42/stream.mp4", "video/mp4"},                // 两边一致
		{"/emby/videos/42/stream", "video/mp4"},                    // /stream 没有容器语义
		{"/api/items/book-1/file/ino-strm", "video/mp4"},           // ABS 路径没有扩展名
		{"/emby/videos/42/stream.mkv", ""},                         // 上游没给、也没猜出类型
		{"/emby/videos/42/stream.mkv", "application/octet-stream"}, // 中性类型没声明容器
	} {
		if note := containerMismatchNote(testCase.requestPath, testCase.actualType); note != "" {
			t.Fatalf("%s + %q 不该提示：%q", testCase.requestPath, testCase.actualType, note)
		}
	}
}

func TestNoRedirectReasonNamesTheClientNotTheTarget(t *testing.T) {
	resolution := &resolver.Resolution{Target: &strm.Target{Type: strm.TargetRemote, URL: "https://cdn.115.com/video.mkv"}}
	cases := []struct {
		mode   config.RedirectMode
		client string
		want   string
	}{
		{config.RedirectPublic, "192.168.1.3", "跳转模式为公网跳转（public），而客户端 192.168.1.3 是内网地址（只有公网客户端才 302）"},
		{config.RedirectPrivate, "8.8.8.8", "跳转模式为内网跳转（private），而客户端 8.8.8.8 是公网地址（只有内网客户端才 302）"},
		{config.RedirectPublic, "", "跳转模式为公网跳转（public），而客户端 IP 无法识别（若 AetherLink 前面还有反代，请把它加入 trusted_proxy_cidrs）"},
		{config.RedirectNever, "8.8.8.8", "跳转模式为始终中继（never），任何客户端都不 302"},
	}
	for _, test := range cases {
		server := &Server{redirect: config.Redirect{Mode: test.mode}}
		got := server.noRedirectReason(resolution, test.client)
		if got != test.want {
			t.Errorf("mode %s client %q reason = %q, want %q", test.mode, test.client, got, test.want)
		}
	}
}

func TestPrivateTargetNoteMentionsIntranetRelay(t *testing.T) {
	if privateTargetNote("http://10.0.0.31:5244/d/%E7%94%B5%E5%BD%B1.mkv") == "" {
		t.Fatal("private host should produce a note")
	}
	if privateTargetNote("https://cdn.115.com/video.mkv") != "" {
		t.Fatal("public host should not produce a note")
	}
}

func TestRedirectPrivateOnlyRedirectsPrivateClients(t *testing.T) {
	publicBackend := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Write([]byte("public-bytes"))
	}))
	defer publicBackend.Close()

	root, strmPath, regularPath := writeStrm(t, "http://10.0.0.31:19527/d/private.m4a")
	fake := newFakeABS(t, strmPath, regularPath)
	redirectCfg := defaultRedirect()
	redirectCfg.Mode = config.RedirectPrivate
	server, _ := newTestServer(t, fake.server.URL, root, redirectCfg)

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "http://media.example/api/items/book-1/file/ino-strm", nil)
	request.RemoteAddr = "192.168.1.20:5000"
	server.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusFound {
		t.Fatalf("private host status = %d, want 302", recorder.Code)
	}
}

func TestRedirectModeUsesEachPlaybackClientWithSharedCache(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Write([]byte("media-bytes"))
	}))
	defer backend.Close()
	root, strmPath, regularPath := writeStrm(t, backend.URL+"/audio.m4a")
	fake := newFakeABS(t, strmPath, regularPath)
	for _, mode := range []config.RedirectMode{config.RedirectPublic, config.RedirectPrivate} {
		t.Run(string(mode), func(t *testing.T) {
			redirectCfg := defaultRedirect()
			redirectCfg.Mode = mode
			redirectCfg.TrustedProxyCIDRs = []string{"172.18.0.2/32"}
			server, collector := newTestServer(t, fake.server.URL, root, redirectCfg)
			for _, client := range []string{"192.168.1.20", "8.8.8.8", "192.168.1.20"} {
				request := httptest.NewRequest(http.MethodGet, "http://media.example/api/items/book-1/file/ino-strm", nil)
				request.RemoteAddr = "172.18.0.2:4000"
				request.Header.Set("X-Forwarded-For", client)
				recorder := httptest.NewRecorder()
				server.ServeHTTP(recorder, request)
				want := http.StatusOK
				if (mode == config.RedirectPrivate) == (client == "192.168.1.20") {
					want = http.StatusFound
				}
				if recorder.Code != want {
					t.Fatalf("client %s: status %d, want %d", client, recorder.Code, want)
				}
				if event := collector.Snapshot(1).RecentEvents[0]; event.Client != client {
					t.Fatalf("event client = %q, want %q", event.Client, client)
				}
			}
			if snapshot := collector.Snapshot(10); snapshot.CacheHits != 2 {
				t.Fatalf("cache hits = %d, want 2", snapshot.CacheHits)
			}
		})
	}
}

func TestFollowUpstreamRedirectsResolvesFinalURL(t *testing.T) {
	final := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Write([]byte("final-bytes"))
	}))
	defer final.Close()

	hop := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		http.Redirect(writer, request, final.URL+"/signed/abc.m4a?token=xyz", http.StatusFound)
	}))
	defer hop.Close()

	root, strmPath, regularPath := writeStrm(t, hop.URL+"/d/abc.m4a")
	fake := newFakeABS(t, strmPath, regularPath)
	redirectCfg := defaultRedirect()
	redirectCfg.FollowUpstreamRedirects = true
	server, _ := newTestServer(t, fake.server.URL, root, redirectCfg)

	// 本用例只看「跟随重定向是否把最终签名地址 302 出去」，与客户端内外网无关。
	// 但 httptest 只能监听 loopback，落点必然是内网地址，对外网客户端会命中
	// 内网直链安全网改走中继（见 TestFollowUpstreamRedirectToIntranetRelaysForPublicClient），
	// 所以这里用内网客户端，避免两件事互相干扰。
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/items/book-1/file/ino-strm", nil)
	request.RemoteAddr = "192.168.1.20:5000"
	server.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302", recorder.Code)
	}
	if location := recorder.Header().Get("Location"); !strings.HasPrefix(location, final.URL+"/signed/abc.m4a") {
		t.Fatalf("Location = %q, want the final signed URL", location)
	}
}

func TestLocalStrmTargetIsServedFromDisk(t *testing.T) {
	root, strmPath, regularPath := writeStrm(t, "./002.m4a")
	fake := newFakeABS(t, strmPath, regularPath)
	server, collector := newTestServer(t, fake.server.URL, root, defaultRedirect())

	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/items/book-1/file/ino-strm", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	if recorder.Body.String() != "local-audio" {
		t.Fatalf("body = %q, want the local file contents", recorder.Body.String())
	}
	if got := recorder.Header().Get("Content-Type"); got != "audio/mp4" {
		t.Fatalf("Content-Type = %q, want audio/mp4", got)
	}
	if snapshot := collector.Snapshot(10); snapshot.LocalFiles != 1 {
		t.Fatalf("local file count = %d, want 1", snapshot.LocalFiles)
	}
}

// 指针文件落在白名单之外时绝不读取：AetherLink 不 302，而是安全地退回透传，
// 让上游自己去服务这个文件，同时把原因记进事件里。
func TestStrmOutsideAllowedRootsFallsBackToUpstream(t *testing.T) {
	root, strmPath, regularPath := writeStrm(t, "http://10.0.0.31:19527/d/abc.m4a")
	fake := newFakeABS(t, strmPath, regularPath)
	// Point the allow-list at an unrelated directory so the pointer is out of scope.
	server, collector := newTestServer(t, fake.server.URL, filepath.Join(root, "nested-only"), defaultRedirect())

	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/items/book-1/file/ino-strm", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want the upstream pass-through to succeed", recorder.Code)
	}
	snapshot := collector.Snapshot(10)
	if snapshot.Redirects != 0 {
		t.Fatalf("redirect count = %d, want 0 for an out-of-scope pointer", snapshot.Redirects)
	}
	if snapshot.Passthroughs != 1 {
		t.Fatalf("passthrough count = %d, want 1", snapshot.Passthroughs)
	}
	if snapshot.RecentEvents[0].Error == "" {
		t.Fatal("the pass-through event should record why the pointer was skipped")
	}
}

// newFakeEmby stands in for an Emby server that has already resolved a .strm
// pointer at scan time: the media source keeps the .strm path but reports the
// pointer URL with Protocol "Http".
func newFakeEmby(t *testing.T, sources []map[string]any) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/Items", func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Query().Get("api_key") != "test-api-key" {
			writer.WriteHeader(http.StatusUnauthorized)
			return
		}
		writeJSON(t, writer, map[string]any{
			"Items": []map[string]any{{
				"Id":           "movie-1",
				"Name":         "白色巨塔",
				"Type":         "Episode",
				"Path":         "/media/tv/白色巨塔 (2003)/S01E01.strm",
				"MediaSources": sources,
			}},
			"TotalRecordCount": 1,
		})
	})
	mux.HandleFunc("/", func(writer http.ResponseWriter, request *http.Request) {
		writer.Write([]byte("emby-ui:" + request.URL.Path))
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

// newFakeEmbyWithoutItemSources stands in for the Emby builds that ignore
// Fields=MediaSources on /Items and only report sources through PlaybackInfo.
func newFakeEmbyWithoutItemSources(t *testing.T, sources []map[string]any) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/Items", func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Query().Get("api_key") != "test-api-key" {
			writer.WriteHeader(http.StatusUnauthorized)
			return
		}
		writeJSON(t, writer, map[string]any{
			"Items": []map[string]any{{
				"Id":   "movie-1",
				"Name": "白色巨塔",
				"Type": "Episode",
				"Path": "/media/tv/白色巨塔 (2003)/S01E01.strm",
			}},
			"TotalRecordCount": 1,
		})
	})
	mux.HandleFunc("/Items/movie-1/PlaybackInfo", func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Query().Get("api_key") != "test-api-key" {
			writer.WriteHeader(http.StatusUnauthorized)
			return
		}
		writeJSON(t, writer, map[string]any{"MediaSources": sources})
	})
	mux.HandleFunc("/", func(writer http.ResponseWriter, request *http.Request) {
		writer.Write([]byte("emby-ui:" + request.URL.Path))
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

func newEmbyTestServer(t *testing.T, embyURL string, redirectCfg config.Redirect) (*Server, *stats.Collector) {
	t.Helper()
	provider, err := upstream.New(config.Upstream{
		Name:         "emby",
		Type:         config.UpstreamEmby,
		BaseURL:      embyURL,
		APIKey:       "test-api-key",
		ListenPort:   8096,
		RedirectMode: redirectCfg.Mode,
	})
	if err != nil {
		t.Fatalf("build emby provider: %v", err)
	}
	collector := stats.New(50)
	mediaResolver := resolver.New(config.Cache{TTL: time.Minute, MaxSize: 32}, redirectCfg)
	return New(provider, mediaResolver, collector, redirectCfg, nil), collector
}

// Emby 已判定可直接播放的 STRM 也要压成 DirectStream 引回 AetherLink：DirectPlay
// 会让客户端直连 Path（网盘 / OpenList 直链）绕开我们，卡片上选的「始终跳转」等于
// 没设——四种模式都接管，模式选的只是字节去向。转码回退（`TranscodingUrl` 与
// `TranscodingContainer`）一律保留，客户端必要时仍可回退；普通本地媒体源不碰。
func TestEmbyPlaybackInfoRoutesCompatibleStrmToDirectPlay(t *testing.T) {
	var acceptEncoding string
	var itemRequests int
	remoteSource := map[string]any{
		"Id":                   "source-strm",
		"Path":                 "http://10.0.0.31:25244/d/移动云盘/白色巨塔 (2003)/S01E01.再读.mkv",
		"Protocol":             "Http",
		"Container":            "mkv",
		"MediaType":            "Video",
		"SupportsDirectPlay":   true,
		"SupportsDirectStream": false,
		"SupportsTranscoding":  true,
		"DirectStreamUrl":      "/emby/Videos/movie-1/stream.mkv?PlaySessionId=play-1",
		"TranscodingUrl":       "/emby/Videos/movie-1/master.m3u8?PlaySessionId=play-1",
		"TranscodingContainer": "ts",
	}
	localSource := map[string]any{
		"Id":                   "source-local",
		"Path":                 "/media/movies/普通影片.mkv",
		"Protocol":             "File",
		"Container":            "mkv",
		"SupportsDirectPlay":   false,
		"SupportsDirectStream": false,
		"SupportsTranscoding":  true,
		"TranscodingUrl":       "/emby/Videos/movie-2/master.m3u8",
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/emby/emby/Items/movie-1/PlaybackInfo", func(writer http.ResponseWriter, request *http.Request) {
		acceptEncoding = request.Header.Get("Accept-Encoding")
		writeJSON(t, writer, map[string]any{
			"PlaySessionId": "play-1",
			"MediaSources":  []map[string]any{remoteSource, localSource},
		})
	})
	mux.HandleFunc("/Users", func(writer http.ResponseWriter, request *http.Request) {
		writeJSON(t, writer, []map[string]any{})
	})
	mux.HandleFunc("/Items", func(writer http.ResponseWriter, request *http.Request) {
		itemRequests++
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
				"MediaSources": []map[string]any{remoteSource},
			}},
		})
	})
	mux.HandleFunc("/", func(writer http.ResponseWriter, request *http.Request) {
		writer.Write([]byte("emby-ui:" + request.URL.Path))
	})
	emby := httptest.NewServer(mux)
	t.Cleanup(emby.Close)
	server, collector := newEmbyTestServer(t, emby.URL, defaultRedirect())

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/emby/emby/Items/movie-1/PlaybackInfo?UserId=user-1", strings.NewReader(`{}`))
	request.Header.Set("Accept-Encoding", "gzip")
	request.Header.Set("Content-Type", "application/json")
	server.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %q", recorder.Code, recorder.Body.String())
	}
	if acceptEncoding != "identity" {
		t.Fatalf("upstream Accept-Encoding = %q, want identity", acceptEncoding)
	}
	var playbackInfo struct {
		MediaSources []map[string]any `json:"MediaSources"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &playbackInfo); err != nil {
		t.Fatalf("decode rewritten PlaybackInfo: %v", err)
	}
	if len(playbackInfo.MediaSources) != 2 {
		t.Fatalf("media source count = %d, want 2", len(playbackInfo.MediaSources))
	}
	rewritten := playbackInfo.MediaSources[0]
	if rewritten["SupportsDirectPlay"] != false || rewritten["SupportsDirectStream"] != true || rewritten["SupportsTranscoding"] != true {
		t.Fatalf("STRM 源应压成 DirectStream（引回 AetherLink）且保留下转码能力: %#v", rewritten)
	}
	if rewritten["TranscodingUrl"] == nil || rewritten["TranscodingContainer"] != "ts" {
		t.Fatalf("STRM transcoding fallback should stay available: %#v", rewritten)
	}
	directURL, _ := rewritten["DirectStreamUrl"].(string)
	parsedDirectURL, err := url.Parse(directURL)
	if err != nil {
		t.Fatalf("parse DirectStreamUrl %q: %v", directURL, err)
	}
	if parsedDirectURL.Path != "/emby/Videos/movie-1/stream.mkv" {
		t.Fatalf("DirectStreamUrl path = %q", parsedDirectURL.Path)
	}
	if parsedDirectURL.Query().Get("MediaSourceId") != "source-strm" || parsedDirectURL.Query().Get("Static") != "true" || parsedDirectURL.Query().Get("PlaySessionId") != "play-1" {
		t.Fatalf("DirectStreamUrl query = %q", parsedDirectURL.RawQuery)
	}
	if playbackInfo.MediaSources[1]["SupportsTranscoding"] != true || playbackInfo.MediaSources[1]["TranscodingUrl"] == nil {
		t.Fatalf("普通本地媒体源不应被修改: %#v", playbackInfo.MediaSources[1])
	}

	redirectRecorder := httptest.NewRecorder()
	server.ServeHTTP(redirectRecorder, httptest.NewRequest(http.MethodGet, directURL, nil))
	if redirectRecorder.Code != http.StatusFound {
		t.Fatalf("direct stream status = %d, want 302; body = %q", redirectRecorder.Code, redirectRecorder.Body.String())
	}
	if snapshot := collector.Snapshot(10); snapshot.Redirects != 1 {
		t.Fatalf("redirect count = %d, want 1", snapshot.Redirects)
	}
	if itemRequests != 0 {
		t.Fatalf("direct stream queried /Items %d times; want the just-rewritten PlaybackInfo source to be reused", itemRequests)
	}
}

// 上游判「当前客户端不能直接播放原始文件」同样不再被采纳。这条判定以前会把客户端
// 交回上游转码——要靠卡片上一个开关才能推翻，而那意味着卡片上写的档位落不到这台
// 客户端身上。现在四种模式都与判定无关地压成 DirectStream 引回 AetherLink。
// 转码回退本身仍留在响应里（`TranscodingUrl` / `TranscodingContainer` 不动），
// 客户端真要回退时还能自己走；HLS 分片路由也照旧不拦截。
func TestEmbyPlaybackInfoClaimsIncompatibleStrmToo(t *testing.T) {
	remoteSource := map[string]any{
		"Id":                   "source-h265",
		"Path":                 "https://cdn.example/episode.h265.mkv",
		"Protocol":             "Http",
		"Container":            "mkv",
		"MediaType":            "Video",
		"SupportsDirectPlay":   false,
		"SupportsDirectStream": false,
		"SupportsTranscoding":  true,
		"DirectStreamUrl":      "/emby/Videos/movie-h265/stream.mkv?PlaySessionId=play-h265",
		"TranscodingUrl":       "/emby/Videos/movie-h265/master.m3u8?PlaySessionId=play-h265&TranscodeReasons=VideoCodecNotSupported",
		"TranscodingContainer": "ts",
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/emby/Items/movie-h265/PlaybackInfo", func(writer http.ResponseWriter, request *http.Request) {
		writeJSON(t, writer, map[string]any{"MediaSources": []map[string]any{remoteSource}})
	})
	mux.HandleFunc("/", func(writer http.ResponseWriter, request *http.Request) {
		writer.Write([]byte("emby-transcode:" + request.URL.Path))
	})
	emby := httptest.NewServer(mux)
	t.Cleanup(emby.Close)
	server, collector := newEmbyTestServer(t, emby.URL, defaultRedirect())

	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/emby/Items/movie-h265/PlaybackInfo", strings.NewReader(`{}`)))
	if recorder.Code != http.StatusOK {
		t.Fatalf("PlaybackInfo status = %d, want 200", recorder.Code)
	}
	var playbackInfo struct {
		MediaSources []map[string]any `json:"MediaSources"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &playbackInfo); err != nil {
		t.Fatal(err)
	}
	source := playbackInfo.MediaSources[0]
	if source["SupportsDirectPlay"] != false || source["SupportsDirectStream"] != true || source["SupportsTranscoding"] != true {
		t.Fatalf("判定被推翻后应压成 DirectStream 且保留下转码能力: %#v", source)
	}
	if reasons, ok := source["TranscodeReasons"].([]any); !ok || len(reasons) != 0 {
		t.Fatalf("TranscodeReasons = %v，want 空（不再与「引回 AetherLink」自相矛盾）", source["TranscodeReasons"])
	}
	if source["TranscodingUrl"] != remoteSource["TranscodingUrl"] {
		t.Fatalf("转码回退不该被抹掉: %#v", source)
	}
	directURL, _ := source["DirectStreamUrl"].(string)
	if !strings.Contains(directURL, "/Videos/movie-h265/stream") {
		t.Fatalf("DirectStreamUrl = %q，want 指回 AetherLink", directURL)
	}

	// 客户端拿着改写后的地址来要原文件：这一次必须真的 302 出去，而不是原样透传。
	directRecorder := httptest.NewRecorder()
	server.ServeHTTP(directRecorder, httptest.NewRequest(http.MethodGet, directURL, nil))
	if directRecorder.Code != http.StatusFound {
		t.Fatalf("direct stream status = %d, want 302; body = %q", directRecorder.Code, directRecorder.Body.String())
	}
	if location := directRecorder.Header().Get("Location"); !strings.Contains(location, "cdn.example/episode.h265.mkv") {
		t.Fatalf("Location = %q，want 直链", location)
	}

	// HLS 分片路由仍然不拦截：客户端真要回退转码时照旧原样交给上游。
	hlsRecorder := httptest.NewRecorder()
	server.ServeHTTP(hlsRecorder, httptest.NewRequest(http.MethodGet, "/emby/videos/movie-h265/hls1/main/10.ts", nil))
	if hlsRecorder.Code != http.StatusOK || !strings.HasPrefix(hlsRecorder.Body.String(), "emby-transcode:") {
		t.Fatalf("HLS fallback status=%d body=%q", hlsRecorder.Code, hlsRecorder.Body.String())
	}
	// 只有一个被拦截的媒体请求（那条 /stream），它这次走的是 302；HLS 分片不属于
	// 被拦截的媒体路由，不计入透传计数。
	if snapshot := collector.Snapshot(10); snapshot.Redirects != 1 || snapshot.Passthroughs != 0 {
		t.Fatalf("redirects=%d passthroughs=%d, want 1 and 0", snapshot.Redirects, snapshot.Passthroughs)
	}
}

// 条件跳转模式下，「该中继的那一半客户端」必须真的被中继。用户实例：飞牛影视
// 卡片选「公网跳转」、客户端在内网、上游又在 PlaybackInfo 里判它不能直放原始
// 文件，结果只留下一行「透传 … 上游判定当前客户端不支持原始文件」——卡片上写的
// 「内网客户端中继」等于没生效（把模式改成「始终中继」就正常）。
//
// 成因是条件模式原先不接管 STRM 源，于是客户端压根不会被引回 AetherLink，而
// 即使它自己来了 /stream，解析也会读到上游那句判定就地透传。现在四种模式都
// 接管：内网客户端回到 /stream 由我们中继，公网客户端照旧拿 302。
func TestPublicRedirectRelaysIntranetClientEvenWhenUpstreamBlocksDirectPlay(t *testing.T) {
	cdn := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Write([]byte("cdn-bytes"))
	}))
	defer cdn.Close()

	remoteSource := map[string]any{
		"Id":                   "source-strm",
		"Path":                 cdn.URL + "/movie.mkv",
		"Protocol":             "Http",
		"Container":            "strm",
		"MediaType":            "Video",
		"SupportsDirectPlay":   false,
		"SupportsDirectStream": false,
		"SupportsTranscoding":  true,
		"TranscodeReasons":     "ContainerBitrateExceedsLimit",
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/emby/Items/movie-1/PlaybackInfo", func(writer http.ResponseWriter, request *http.Request) {
		writeJSON(t, writer, map[string]any{"MediaSources": []map[string]any{remoteSource}})
	})
	mux.HandleFunc("/", func(writer http.ResponseWriter, request *http.Request) {
		writer.Write([]byte("upstream-transcode:" + request.URL.Path))
	})
	emby := httptest.NewServer(mux)
	t.Cleanup(emby.Close)

	redirectCfg := defaultRedirect()
	redirectCfg.Mode = config.RedirectPublic
	server, collector := newEmbyTestServer(t, emby.URL, redirectCfg)

	playbackRecorder := httptest.NewRecorder()
	server.ServeHTTP(playbackRecorder, httptest.NewRequest(http.MethodPost, "/emby/Items/movie-1/PlaybackInfo", strings.NewReader(`{}`)))
	if playbackRecorder.Code != http.StatusOK {
		t.Fatalf("PlaybackInfo status = %d, want 200", playbackRecorder.Code)
	}
	var playbackInfo struct {
		MediaSources []map[string]any `json:"MediaSources"`
	}
	if err := json.Unmarshal(playbackRecorder.Body.Bytes(), &playbackInfo); err != nil {
		t.Fatal(err)
	}
	if len(playbackInfo.MediaSources) != 1 {
		t.Fatalf("media source count = %d, want 1", len(playbackInfo.MediaSources))
	}
	claimed := playbackInfo.MediaSources[0]
	if claimed["SupportsDirectPlay"] != false || claimed["SupportsDirectStream"] != true {
		t.Fatalf("条件模式也要把 STRM 源引回 AetherLink: %#v", claimed)
	}
	if directURL, _ := claimed["DirectStreamUrl"].(string); !strings.Contains(directURL, "/Videos/movie-1/stream") {
		t.Fatalf("DirectStreamUrl = %q，want 含 /Videos/movie-1/stream", directURL)
	}

	streamPath := "/emby/Videos/movie-1/stream.mkv?MediaSourceId=source-strm"

	intranetRecorder := httptest.NewRecorder()
	intranetRequest := httptest.NewRequest(http.MethodGet, streamPath, nil)
	intranetRequest.RemoteAddr = "192.168.1.20:5000"
	server.ServeHTTP(intranetRecorder, intranetRequest)
	if intranetRecorder.Code != http.StatusOK || intranetRecorder.Body.String() != "cdn-bytes" {
		t.Fatalf("内网客户端应被中继：状态 %d，body %q", intranetRecorder.Code, intranetRecorder.Body.String())
	}

	publicRecorder := httptest.NewRecorder()
	publicRequest := httptest.NewRequest(http.MethodGet, streamPath, nil)
	publicRequest.RemoteAddr = "8.8.8.8:5000"
	server.ServeHTTP(publicRecorder, publicRequest)
	if publicRecorder.Code != http.StatusFound {
		t.Fatalf("公网客户端状态 = %d，want 302", publicRecorder.Code)
	}
	if location := publicRecorder.Header().Get("Location"); location != cdn.URL+"/movie.mkv" {
		t.Fatalf("Location = %q，want 直链 %q", location, cdn.URL+"/movie.mkv")
	}

	if snapshot := collector.Snapshot(10); snapshot.ProxyStreams != 1 || snapshot.Redirects != 1 {
		t.Fatalf("中继 %d 次、302 %d 次，want 各 1（同一张卡片按客户端来源分流）", snapshot.ProxyStreams, snapshot.Redirects)
	}
}

// 用户实例（2026-09-19）：家里每台设备从路由器拿到的都是运营商下发的 IPv6 全局
// 地址（240e:: 这类 GUA），而判定只按地址类型走 —— GUA 算公网，于是「公网跳转」
// 把这些明明在内网的客户端统统 302 出去，播放器拿到的是一个它连不上的地址。
// 一条 TCP 连接只有一个对端地址，从 IPv6 连接里问不出客户端的 IPv4，所以能做的
// 只有把自家网段明确告诉 AetherLink（设置页「内网网段」）。
func TestPublicRedirectRelaysIntranetIPv6WhenPrefixConfigured(t *testing.T) {
	cdn := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Write([]byte("cdn-bytes"))
	}))
	defer cdn.Close()

	remoteSource := map[string]any{
		"Id":        "source-strm",
		"Path":      cdn.URL + "/movie.mkv",
		"Protocol":  "Http",
		"Container": "strm",
		"MediaType": "Video",
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/emby/Items/movie-1/PlaybackInfo", func(writer http.ResponseWriter, request *http.Request) {
		writeJSON(t, writer, map[string]any{"MediaSources": []map[string]any{remoteSource}})
	})
	mux.HandleFunc("/", func(writer http.ResponseWriter, request *http.Request) {
		writer.Write([]byte("upstream:" + request.URL.Path))
	})
	emby := httptest.NewServer(mux)
	t.Cleanup(emby.Close)

	streamPath := "/emby/Videos/movie-1/stream.mkv?MediaSourceId=source-strm"
	// 先走一遍 PlaybackInfo 把改写缓存写热：/stream 命中缓存才会直接用上那份
	// 媒体源，不然会掉进「回头问上游」的冷路径，那是另一条故事线。
	warm := func(server *Server) {
		recorder := httptest.NewRecorder()
		server.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/emby/Items/movie-1/PlaybackInfo", strings.NewReader(`{}`)))
		if recorder.Code != http.StatusOK {
			t.Fatalf("PlaybackInfo status = %d", recorder.Code)
		}
	}

	// 未配置网段：这就是用户报的现象，内网的 IPv6 客户端被判成公网拿到 302。
	// 样本用 2001:db8::/32（文档保留段）：判定还会参考「本机网段」，用真前缀当
	// 「应当算公网」的样本时，跑测试那台机器万一就有那个网段，断言会翻车。
	plain := defaultRedirect()
	plain.Mode = config.RedirectPublic
	plainServer, _ := newEmbyTestServer(t, emby.URL, plain)
	warm(plainServer)
	plainRecorder := httptest.NewRecorder()
	plainRequest := httptest.NewRequest(http.MethodGet, streamPath, nil)
	plainRequest.RemoteAddr = "[2001:db8:1a2b:3c4d::12]:5000"
	plainServer.ServeHTTP(plainRecorder, plainRequest)
	if plainRecorder.Code != http.StatusFound {
		t.Fatalf("没配置内网网段时应当保持原行为（302），得到 %d", plainRecorder.Code)
	}

	// 配置了自家网段：同一个客户端改为中继。
	configured := defaultRedirect()
	configured.Mode = config.RedirectPublic
	configured.IntranetCIDRs = []string{"2001:db8:1a2b:3c4d::/64"}
	server, collector := newEmbyTestServer(t, emby.URL, configured)
	warm(server)

	intranetRecorder := httptest.NewRecorder()
	intranetRequest := httptest.NewRequest(http.MethodGet, streamPath, nil)
	intranetRequest.RemoteAddr = "[2001:db8:1a2b:3c4d::12]:5000"
	server.ServeHTTP(intranetRecorder, intranetRequest)
	if intranetRecorder.Code != http.StatusOK || intranetRecorder.Body.String() != "cdn-bytes" {
		t.Fatalf("配置内网网段后应当中继：状态 %d，body %q", intranetRecorder.Code, intranetRecorder.Body.String())
	}

	// 网段之外的同族地址照旧 302：补充网段不能把公网客户端一起吞进来。
	outsideRecorder := httptest.NewRecorder()
	outsideRequest := httptest.NewRequest(http.MethodGet, streamPath, nil)
	outsideRequest.RemoteAddr = "[2001:db8:1a2b:3c4e::12]:5000"
	server.ServeHTTP(outsideRecorder, outsideRequest)
	if outsideRecorder.Code != http.StatusFound {
		t.Fatalf("网段之外的 IPv6 客户端状态 = %d，want 302", outsideRecorder.Code)
	}
	if snapshot := collector.Snapshot(10); snapshot.ProxyStreams != 1 || snapshot.Redirects != 1 {
		t.Fatalf("中继 %d 次、302 %d 次，want 各 1", snapshot.ProxyStreams, snapshot.Redirects)
	}

	// 连配置都不用填的那一层：与 AetherLink 本机同网段的客户端同样按内网处理
	// （运营商换前缀时它自动跟随）。拿真实网卡取样本，没有全局 IPv6 的机器跳过 ——
	// 这一段的日志说明也只有这一层会写，正好一并钉住。
	prefixes := resolver.LocalNetworkPrefixes()
	if len(prefixes) == 0 {
		t.Log("这台机器没有全局 IPv6 网段（容器不是 host 网络时属正常），跳过自动识别那一段")
		return
	}
	ownClient := prefixes[0].Addr().Next().String()
	ownRecorder := httptest.NewRecorder()
	ownRequest := httptest.NewRequest(http.MethodGet, streamPath, nil)
	ownRequest.RemoteAddr = "[" + ownClient + "]:5000"
	plainServer.ServeHTTP(ownRecorder, ownRequest)
	if ownRecorder.Code != http.StatusOK || ownRecorder.Body.String() != "cdn-bytes" {
		t.Fatalf("与本机同网段的客户端（%s，本机网段 %s）应当被中继：状态 %d，body %q",
			ownClient, prefixes[0], ownRecorder.Code, ownRecorder.Body.String())
	}
	// 日志里的路径是客户端原始请求路径，不含查询串（stats.Event.Path = URL.Path）。
	// 只有 ULA 的机器（fd00::/8）本来就落进内置规则、说明为空，那种机器不查这一条。
	if !resolver.ClientAddress(ownClient).IsPrivate() &&
		!logContainsAll("中继 /emby/Videos/movie-1/stream.mkv", "是内网地址，与本机在同一网段 "+prefixes[0].String()) {
		t.Fatalf("自动识别那一次没有留下说明（本机网段 %s）", prefixes[0])
	}
}

func TestJoinPathAvoidsDuplicateBasePrefix(t *testing.T) {
	tests := []struct {
		basePath    string
		requestPath string
		want        string
	}{
		{basePath: "/emby", requestPath: "/emby/Items/1", want: "/emby/Items/1"},
		{basePath: "/emby", requestPath: "/Items/1", want: "/emby/Items/1"},
		{basePath: "/emby/", requestPath: "/Emby/Videos/1/stream.mkv", want: "/Emby/Videos/1/stream.mkv"},
	}
	for _, test := range tests {
		if got := joinPath(test.basePath, test.requestPath); got != test.want {
			t.Fatalf("joinPath(%q, %q) = %q, want %q", test.basePath, test.requestPath, got, test.want)
		}
	}
}

func TestEmbyHLSRequestDetection(t *testing.T) {
	provider, err := upstream.New(config.Upstream{
		Name:       "emby",
		Type:       config.UpstreamEmby,
		BaseURL:    "http://127.0.0.1:8096",
		APIKey:     "test-api-key",
		ListenPort: 8097,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, requestPath := range []string{
		"/emby/videos/162227/hls1/main/10.ts",
		"/emby/videos/162227/master.m3u8",
	} {
		if !isEmbyHLSRequest(provider, requestPath) {
			t.Fatalf("%q should be diagnosed as Emby HLS", requestPath)
		}
	}
	if isEmbyHLSRequest(provider, "/emby/Videos/162227/stream.mkv") {
		t.Fatal("direct stream route must not be diagnosed as HLS")
	}
}

// Emby 的 strm 走的是 API 直链，不需要挂载任何媒体目录，这条链路是 Emby 能
// 302 的唯一原因，因此必须有测试锁住。
func TestEmbyHTTPMediaSourceRedirectsWithoutMountedMedia(t *testing.T) {
	emby := newFakeEmby(t, []map[string]any{{
		"Id":        "source-1",
		"Path":      "http://10.0.0.31:25244/d/移动云盘/白色巨塔 (2003)/S01E01.再读.mkv",
		"Protocol":  "Http",
		"Container": "strm",
	}})
	server, collector := newEmbyTestServer(t, emby.URL, defaultRedirect())

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/Videos/movie-1/stream.mkv?MediaSourceId=source-1", nil)
	server.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302; body = %q", recorder.Code, recorder.Body.String())
	}
	location := recorder.Header().Get("Location")
	// 中文与空格必须被百分号编码，否则播放器拿到的是非法 URL。
	if !strings.HasPrefix(location, "http://10.0.0.31:25244/d/%E7%A7%BB%E5%8A%A8%E4%BA%91%E7%9B%98/") {
		t.Fatalf("Location = %q, want a percent-encoded openlist URL", location)
	}
	if strings.Contains(location, " ") {
		t.Fatalf("Location still contains a raw space: %q", location)
	}
	snapshot := collector.Snapshot(10)
	if snapshot.Redirects != 1 {
		t.Fatalf("redirect count = %d, want 1", snapshot.Redirects)
	}
	if snapshot.ByKind["openlist"] != 1 {
		t.Fatalf("kind counts = %v, want one openlist entry", snapshot.ByKind)
	}
}

// Emby 上普通影片的 MediaSource 是本地文件路径，必须原样透传给 Emby 自己播。
func TestEmbyLocalMediaSourceIsProxied(t *testing.T) {
	emby := newFakeEmby(t, []map[string]any{{
		"Id":        "source-1",
		"Path":      "/media/movies/普通影片.mkv",
		"Protocol":  "File",
		"Container": "mkv",
	}})
	server, collector := newEmbyTestServer(t, emby.URL, defaultRedirect())

	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/Videos/movie-1/stream.mkv?MediaSourceId=source-1", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 from the upstream", recorder.Code)
	}
	if !strings.HasPrefix(recorder.Body.String(), "emby-ui:") {
		t.Fatalf("body = %q, want the upstream response", recorder.Body.String())
	}
	if snapshot := collector.Snapshot(10); snapshot.Passthroughs != 1 {
		t.Fatalf("passthrough count = %d, want 1", snapshot.Passthroughs)
	}
}

// Emby 的 pick code 直链同样要能 302，且查询串里的显示文件名不能破坏 URL。
func TestEmbyPickCodeSourceRedirects(t *testing.T) {
	emby := newFakeEmby(t, []map[string]any{{
		"Id":       "source-1",
		"Path":     "http://10.0.0.31:19527/d/bi6jeznun2rvu88v6.m4a?/001.总序.m4a",
		"Protocol": "Http",
	}})
	server, collector := newEmbyTestServer(t, emby.URL, defaultRedirect())

	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/emby/Items/movie-1/Download?MediaSourceId=source-1", nil))

	if recorder.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302; body = %q", recorder.Code, recorder.Body.String())
	}
	if location := recorder.Header().Get("Location"); !strings.HasPrefix(location, "http://10.0.0.31:19527/d/bi6jeznun2rvu88v6.m4a?") {
		t.Fatalf("Location = %q", location)
	}
	if snapshot := collector.Snapshot(10); snapshot.ByKind["pickcode115"] != 1 {
		t.Fatalf("kind counts = %v, want one pickcode115 entry", snapshot.ByKind)
	}
}

// 有些 Emby 版本在 /Items 上忽略 Fields=MediaSources，只有 PlaybackInfo 才给
// 媒体源。少了这个回退，AetherLink 就看不到直链，Emby 侧会静默退化成纯透传
// ——也就是用户看到的「反代成功了但不 302」。
func TestEmbyMediaSourceFromPlaybackInfoRedirects(t *testing.T) {
	emby := newFakeEmbyWithoutItemSources(t, []map[string]any{{
		"Id":        "source-1",
		"Path":      "http://10.0.0.31:19527/d/bi6jeznun2rvu88v6.m4a?/001.总序.m4a",
		"Protocol":  "Http",
		"Container": "strm",
	}})
	server, collector := newEmbyTestServer(t, emby.URL, defaultRedirect())

	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/Videos/movie-1/stream.m4a", nil))

	if recorder.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302; body = %q", recorder.Code, recorder.Body.String())
	}
	if snapshot := collector.Snapshot(10); snapshot.ByKind["pickcode115"] != 1 {
		t.Fatalf("kind counts = %v, want one pickcode115 entry", snapshot.ByKind)
	}
}

// 指针文件没挂进容器时不能让播放失败：退回透传，让上游自己代理。
func TestUnreadableStrmPointerFallsBackToUpstream(t *testing.T) {
	root := t.TempDir()
	// 上游报告的指针路径在本容器里根本不存在。
	missing := pathmap.Normalize(filepath.Join(root, "not-mounted", "001.strm"))
	regular := pathmap.Normalize(filepath.Join(root, "002.m4a"))
	if err := os.WriteFile(filepath.FromSlash(regular), []byte("local-audio"), 0o644); err != nil {
		t.Fatal(err)
	}
	fake := newFakeABS(t, missing, regular)
	server, collector := newTestServer(t, fake.server.URL, root, defaultRedirect())

	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/items/book-1/file/ino-strm", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want the upstream pass-through to succeed", recorder.Code)
	}
	snapshot := collector.Snapshot(10)
	if snapshot.Passthroughs != 1 || snapshot.Errors != 0 {
		t.Fatalf("passthrough = %d, errors = %d; want 1 and 0", snapshot.Passthroughs, snapshot.Errors)
	}
	if snapshot.RecentEvents[0].Error == "" {
		t.Fatal("the pass-through event should record why the pointer was unreadable")
	}
}

func TestHeadRequestGetsRedirectWithoutBody(t *testing.T) {
	root, strmPath, regularPath := writeStrm(t, "http://10.0.0.31:19527/d/abc.m4a")
	fake := newFakeABS(t, strmPath, regularPath)
	server, _ := newTestServer(t, fake.server.URL, root, defaultRedirect())

	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, httptest.NewRequest(http.MethodHead, "/api/items/book-1/file/ino-strm", nil))

	if recorder.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302", recorder.Code)
	}
	if recorder.Body.Len() != 0 {
		t.Fatalf("HEAD response had a body: %q", recorder.Body.String())
	}
}

// Audiobookshelf 可以装在子路径下（ROUTER_BASE_PATH），此时客户端请求的是
// /audiobookshelf/api/items/...。旧正则只认 ^/api/，这类部署会整条漏掉拦截，
// 对外表现就是「反代通了但一次都不 302，日志里也什么都看不到」。
func TestABSRequestUnderRouterBasePathIsIntercepted(t *testing.T) {
	root, strmPath, regularPath := writeStrm(t, "http://10.0.0.31:19527/d/bi6jeznun2rvu88v6.m4a")
	fake := newFakeABS(t, strmPath, regularPath)
	server, collector := newTestServer(t, fake.server.URL, root, defaultRedirect())

	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/audiobookshelf/api/items/book-1/file/ino-strm", nil))

	if recorder.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302; body = %q", recorder.Code, recorder.Body.String())
	}
	if snapshot := collector.Snapshot(10); snapshot.Redirects != 1 {
		t.Fatalf("redirect count = %d, want 1", snapshot.Redirects)
	}
}

// 网页端与 App 播放走的是 /public/session/:id/track/:index，而 ABS 的会话接口
// 不返回 audioTracks，必须回到条目里按序号找音频文件，否则这条链路永远不 302。
func TestSessionTrackResolvesThroughLibraryItem(t *testing.T) {
	root, strmPath, regularPath := writeStrm(t, "http://10.0.0.31:19527/d/bi6jeznun2rvu88v6.m4a")
	fake := newFakeABS(t, strmPath, regularPath)
	server, collector := newTestServer(t, fake.server.URL, root, defaultRedirect())

	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/public/session/sess-1/track/1", nil))

	if recorder.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302; body = %q", recorder.Code, recorder.Body.String())
	}
	if location := recorder.Header().Get("Location"); !strings.HasPrefix(location, "http://10.0.0.31:19527/d/") {
		t.Fatalf("Location = %q", location)
	}
	if snapshot := collector.Snapshot(10); snapshot.Redirects != 1 {
		t.Fatalf("redirect count = %d, want 1", snapshot.Redirects)
	}
}

// 会话刚创建时还没写进数据库，/api/session/:id 会 404。必须回退到活跃会话列表，
// 否则播放刚开始的那几秒全部解析失败，用户看到的就是「不 302」。
func TestSessionTrackFallsBackToOpenSessions(t *testing.T) {
	root, strmPath, regularPath := writeStrm(t, "http://10.0.0.31:19527/d/bi6jeznun2rvu88v6.m4a")
	fake := newFakeABS(t, strmPath, regularPath)
	server, collector := newTestServer(t, fake.server.URL, root, defaultRedirect())

	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/public/session/sess-new/track/1", nil))

	if recorder.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302; body = %q", recorder.Code, recorder.Body.String())
	}
	if snapshot := collector.Snapshot(10); snapshot.Redirects != 1 {
		t.Fatalf("redirect count = %d, want 1", snapshot.Redirects)
	}
}
