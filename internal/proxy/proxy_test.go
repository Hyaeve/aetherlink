package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aetherlink/aetherlink/internal/config"
	"github.com/aetherlink/aetherlink/internal/pathmap"
	"github.com/aetherlink/aetherlink/internal/resolver"
	"github.com/aetherlink/aetherlink/internal/stats"
	"github.com/aetherlink/aetherlink/internal/upstream"
)

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
	return New(provider, mediaResolver, collector, redirectCfg), collector
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

func TestRedirectPrivateOnlyRedirectsPrivateHosts(t *testing.T) {
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
	server.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/items/book-1/file/ino-strm", nil))
	if recorder.Code != http.StatusFound {
		t.Fatalf("private host status = %d, want 302", recorder.Code)
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

	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/items/book-1/file/ino-strm", nil))

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
		Name:       "emby",
		Type:       config.UpstreamEmby,
		BaseURL:    embyURL,
		APIKey:     "test-api-key",
		ListenPort: 8096,
	})
	if err != nil {
		t.Fatalf("build emby provider: %v", err)
	}
	collector := stats.New(50)
	mediaResolver := resolver.New(config.Cache{TTL: time.Minute, MaxSize: 32}, redirectCfg)
	return New(provider, mediaResolver, collector, redirectCfg), collector
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
