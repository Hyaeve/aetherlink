package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/aetherlink/aetherlink/internal/web"
)

// 管理端口上裸访问 / 必须直接把用户送进管理界面，这样浏览器里输
// http://主机:15151 就够了，不用记 /aetherlink/ 后缀。
func TestRootRedirectsToAdminUI(t *testing.T) {
	recorder := httptest.NewRecorder()
	adminRootHandler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/", nil))

	if recorder.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302", recorder.Code)
	}
	if location := recorder.Header().Get("Location"); location != web.MountPath {
		t.Fatalf("location = %q, want %q", location, web.MountPath)
	}
}

// 反代请求应该发到上游自己的端口。打到管理端口上说明端口填错了，
// 这里直接 404 比静默转发更容易排查。
func TestAdminPortDoesNotProxyMediaPaths(t *testing.T) {
	recorder := httptest.NewRecorder()
	adminRootHandler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/items/x/file/1", nil))

	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 on the admin port", recorder.Code)
	}
}
