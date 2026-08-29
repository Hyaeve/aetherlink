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

func TestStrmOutsideAllowedRootsIsRejected(t *testing.T) {
	root, strmPath, regularPath := writeStrm(t, "http://10.0.0.31:19527/d/abc.m4a")
	fake := newFakeABS(t, strmPath, regularPath)
	// Point the allow-list at an unrelated directory so the pointer is out of scope.
	server, collector := newTestServer(t, fake.server.URL, filepath.Join(root, "nested-only"), defaultRedirect())

	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/items/book-1/file/ino-strm", nil))

	if recorder.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", recorder.Code)
	}
	if snapshot := collector.Snapshot(10); snapshot.Errors != 1 {
		t.Fatalf("error count = %d, want 1", snapshot.Errors)
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
