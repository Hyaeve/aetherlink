package main

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/aetherlink/aetherlink/internal/config"
	"github.com/aetherlink/aetherlink/internal/runtime"
	"github.com/aetherlink/aetherlink/internal/stats"
	"github.com/aetherlink/aetherlink/internal/web"
)

func newRuntime(t *testing.T, upstreams ...config.Upstream) *runtime.Runtime {
	t.Helper()
	cfg, _, err := config.LoadOrCreate(filepath.Join(t.TempDir(), "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Upstreams = upstreams
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	rt, err := runtime.New(cfg, stats.New(10))
	if err != nil {
		t.Fatal(err)
	}
	return rt
}

// 没有上游占用根路径时，裸访问 / 必须直接把用户送进管理界面，
// 这样浏览器里输 http://主机:15151 就够了，不用记 /aetherlink/ 后缀。
func TestRootRedirectsToAdminUI(t *testing.T) {
	recorder := httptest.NewRecorder()
	rootHandler(newRuntime(t)).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/", nil))

	if recorder.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302", recorder.Code)
	}
	if location := recorder.Header().Get("Location"); location != web.MountPath {
		t.Fatalf("location = %q, want %q", location, web.MountPath)
	}
}

// 上游挂在根路径时，/ 属于那个上游（例如 Audiobookshelf 自己的首页），
// 不能被管理界面抢走，否则播放端会被重定向到错误的地方。
func TestRootGoesToUpstreamWhenMountedOnRoot(t *testing.T) {
	upstreamCfg := config.Upstream{
		Name:    "abs",
		Type:    config.UpstreamAudiobookshelf,
		BaseURL: "http://127.0.0.1:13378",
		APIKey:  "key",
		Prefix:  "/",
	}
	recorder := httptest.NewRecorder()
	rootHandler(newRuntime(t, upstreamCfg)).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/", nil))

	// 上游其实并不存在，因此这里只要求「没有被重定向到管理界面」。
	if recorder.Code == http.StatusFound && recorder.Header().Get("Location") == web.MountPath {
		t.Fatal("/ was hijacked by the admin UI even though an upstream owns it")
	}
}

// 非根路径永远交给反向代理，绝不能被跳转逻辑吞掉。
func TestNonRootPathIsProxied(t *testing.T) {
	recorder := httptest.NewRecorder()
	rootHandler(newRuntime(t)).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/items/x/file/1", nil))

	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 from the proxy with no upstreams", recorder.Code)
	}
}
