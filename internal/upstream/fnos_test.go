package upstream

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/aetherlink/aetherlink/internal/config"
)

func newFnosProvider(t *testing.T, baseURL string) *fnosProvider {
	t.Helper()
	provider, err := New(config.Upstream{
		Name:       "fnos",
		Type:       config.UpstreamFnos,
		BaseURL:    baseURL,
		APIKey:     "token",
		ListenPort: 5154,
	})
	if err != nil {
		t.Fatalf("New(fnos) returned error: %v", err)
	}
	fnos, ok := provider.(*fnosProvider)
	if !ok {
		t.Fatalf("New(fnos) returned %T, want *fnosProvider", provider)
	}
	return fnos
}

func mustURL(t *testing.T, rawPath string) *url.URL {
	t.Helper()
	parsed, err := url.Parse(rawPath)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}

func TestFnosProviderIsEmbyFamily(t *testing.T) {
	provider := newFnosProvider(t, "http://10.0.0.33:8005")
	if provider.Type() != config.UpstreamFnos {
		t.Fatalf("Type() = %q, want %q", provider.Type(), config.UpstreamFnos)
	}
	if !provider.Type().IsEmbyFamily() {
		t.Fatal("IsEmbyFamily() = false for fnos, want true")
	}
	// 播放路径按飞牛的 /emby 前缀生成，客户端下一跳才不会落到 SPA 上。
	if got, want := provider.PlaybackPath("42", "src-1"), "/emby/Items/42/Download?MediaSourceId=src-1"; got != want {
		t.Fatalf("PlaybackPath() = %q, want %q", got, want)
	}
}

func TestFnosAPIPrefix(t *testing.T) {
	cases := []struct {
		baseURL string
		want    string
	}{
		{"http://10.0.0.33:8005", "/emby"},
		{"http://10.0.0.33:8005/", "/emby"},
		{"http://10.0.0.33:8005/emby", ""},
		{"http://10.0.0.33:8005/EMBY", ""},
		{"http://nas.local/fnos", "/emby"},
	}
	for _, testCase := range cases {
		provider := newFnosProvider(t, testCase.baseURL)
		if got := provider.client.apiPrefix; got != testCase.want {
			t.Errorf("apiPrefix for %q = %q, want %q", testCase.baseURL, got, testCase.want)
		}
	}
}

// 前缀最终要落到实际请求上：少了它飞牛只会返回 SPA 的 HTML。
func TestFnosAPICallsCarryEmbyPrefix(t *testing.T) {
	for _, suffix := range []string{"", "/emby"} {
		var gotPath string
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			gotPath = request.URL.Path
			writer.WriteHeader(http.StatusOK)
			_, _ = writer.Write([]byte(`{"Items":[]}`))
		}))
		provider := newFnosProvider(t, server.URL+suffix)
		var out map[string]any
		if err := provider.client.getJSON(context.Background(), "/Items", nil, &out); err != nil {
			server.Close()
			t.Fatalf("getJSON(base=%q) returned error: %v", suffix, err)
		}
		server.Close()
		if gotPath != "/emby/Items" {
			t.Fatalf("base=%q 时请求路径为 %q，want /emby/Items", suffix, gotPath)
		}
	}
}

func TestFnosWantsResponseRewrite(t *testing.T) {
	provider := newFnosProvider(t, "http://10.0.0.33:8005")
	cases := []struct {
		method string
		path   string
		want   bool
	}{
		{http.MethodPost, "/emby/Items/42/PlaybackInfo", true},
		{http.MethodGet, "/Items/42/PlaybackInfo", true},
		{http.MethodGet, "/emby/web/modules/htmlvideoplayer/basehtmlplayer.js", true},
		{http.MethodGet, "/web/modules/htmlvideoplayer/basehtmlplayer.js", true},
		{http.MethodGet, "/emby/System/Info", false},
		{http.MethodGet, "/emby/Videos/42/stream", false},
		{http.MethodDelete, "/emby/Items/42/PlaybackInfo", false},
	}
	for _, testCase := range cases {
		request := &http.Request{Method: testCase.method, URL: mustURL(t, testCase.path)}
		if got := provider.WantsResponseRewrite(request); got != testCase.want {
			t.Errorf("WantsResponseRewrite(%s %s) = %v, want %v", testCase.method, testCase.path, got, testCase.want)
		}
	}
}

func TestFnosRewritesBaseHTMLPlayerCrossOrigin(t *testing.T) {
	original := `function play(mediaSource){var playMethod="DirectPlay";` +
		`el.setAttribute("crossorigin",mediaSource.IsRemote&&"DirectPlay"===playMethod?null:"anonymous");}`

	response := &http.Response{
		StatusCode:    http.StatusOK,
		Header:        http.Header{"Content-Type": []string{"application/javascript"}},
		Body:          io.NopCloser(strings.NewReader(original)),
		ContentLength: int64(len(original)),
	}
	changed, err := rewriteFnosBaseHTMLPlayer(response)
	if err != nil {
		t.Fatalf("rewriteFnosBaseHTMLPlayer returned error: %v", err)
	}
	if changed == 0 {
		t.Fatal("rewriteFnosBaseHTMLPlayer reported no change")
	}
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(body, []byte(`?null:"anonymous"`)) {
		t.Fatal("crossorigin 表达式仍留在脚本里")
	}
	if !bytes.HasPrefix(body, fnosCrossOriginGuard) {
		t.Fatal("兜底守卫应当插在脚本最前面")
	}
	if got := response.Header.Get("Content-Length"); got != strconv.Itoa(len(body)) {
		t.Fatalf("Content-Length = %q, want %d", got, len(body))
	}
	if got := response.Header.Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}

	// 二次改写应当幂等：守卫已经在了，不该再插一遍。
	response.Body = io.NopCloser(bytes.NewReader(body))
	response.Header.Set("Content-Length", strconv.Itoa(len(body)))
	if _, err := rewriteFnosBaseHTMLPlayer(response); err != nil {
		t.Fatalf("second rewrite returned error: %v", err)
	}
	again, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Count(again, []byte("HTMLMediaElement.prototype,'crossOrigin'")) != 1 {
		t.Fatal("兜底守卫被重复插入")
	}
}

func TestFnosBaseHTMLPlayerSkipsCompressedBody(t *testing.T) {
	response := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Encoding": []string{"gzip"}},
		Body:       io.NopCloser(strings.NewReader("not really gzip")),
	}
	if _, err := rewriteFnosBaseHTMLPlayer(response); err == nil {
		t.Fatal("压缩响应应当返回错误而不是改坏脚本")
	}
}

func TestFnosFillsMissingMediaStreamFields(t *testing.T) {
	source := map[string]any{
		"MediaStreams": []any{
			map[string]any{"Type": "Video", "Language": nil, "Title": "主视频"},
			map[string]any{"Index": float64(1)},
			"not-a-map",
		},
	}
	if !normalizeFnosMediaStreams(source) {
		t.Fatal("normalizeFnosMediaStreams reported no change")
	}
	streams := source["MediaStreams"].([]any)
	first := streams[0].(map[string]any)
	if first["Language"] != "" {
		t.Fatalf("Language = %#v, want empty string", first["Language"])
	}
	if first["Type"] != "Video" || first["Title"] != "主视频" {
		t.Fatal("已有字段被覆盖")
	}
	second := streams[1].(map[string]any)
	for _, field := range fnosMediaStreamNonNullFields {
		if value, exists := second[field]; !exists || value == nil {
			t.Fatalf("字段 %s 没有被补齐", field)
		}
	}
	if streams[2] != "not-a-map" {
		t.Fatal("非对象元素被改动")
	}
	if normalizeFnosMediaStreams(source) {
		t.Fatal("normalizeFnosMediaStreams 应当幂等")
	}
}
