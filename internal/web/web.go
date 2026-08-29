// Package web serves the compiled Vue admin UI from the binary.
//
// The dist directory is embedded so the Docker image is a single static binary
// with no runtime asset mount. When the frontend has not been built, a small
// placeholder page is served instead so the API stays usable.
package web

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

//go:embed all:dist
var distFS embed.FS

// MountPath is where the UI is served from.
const MountPath = "/aetherlink/"

// Handler returns the static file handler for the admin UI.
func Handler() http.Handler {
	sub, err := fs.Sub(distFS, "dist")
	if err != nil {
		return http.HandlerFunc(placeholder)
	}
	if _, err := fs.Stat(sub, "index.html"); err != nil {
		return http.HandlerFunc(placeholder)
	}

	fileServer := http.FileServer(http.FS(sub))
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		trimmed := strings.TrimPrefix(request.URL.Path, MountPath)
		if trimmed == "" || trimmed == "/" {
			serveIndex(writer, request, sub)
			return
		}
		if _, err := fs.Stat(sub, trimmed); err != nil {
			// Unknown paths fall back to index.html so the SPA router can handle them.
			serveIndex(writer, request, sub)
			return
		}
		stripped := http.StripPrefix(MountPath, fileServer)
		stripped.ServeHTTP(writer, request)
	})
}

func serveIndex(writer http.ResponseWriter, request *http.Request, sub fs.FS) {
	data, err := fs.ReadFile(sub, "index.html")
	if err != nil {
		placeholder(writer, request)
		return
	}
	writer.Header().Set("Content-Type", "text/html; charset=utf-8")
	writer.Header().Set("Cache-Control", "no-cache")
	writer.Write(data)
}

const placeholderHTML = `<!doctype html>
<html lang="zh-CN">
<head><meta charset="utf-8"><title>AetherLink</title>
<style>body{font-family:system-ui,sans-serif;background:#0f172a;color:#e2e8f0;margin:0;display:grid;place-items:center;height:100vh}code{background:#1e293b;padding:2px 6px;border-radius:4px}</style>
</head>
<body><div><h1>AetherLink 以太链接</h1>
<p>前端尚未构建。执行 <code>cd web &amp;&amp; npm install &amp;&amp; npm run build</code> 后重新编译。</p>
<p>管理 API 仍可用：<code>/aetherlink/api/status</code></p></div></body></html>`

func placeholder(writer http.ResponseWriter, request *http.Request) {
	writer.Header().Set("Content-Type", "text/html; charset=utf-8")
	writer.WriteHeader(http.StatusOK)
	writer.Write([]byte(placeholderHTML))
}
