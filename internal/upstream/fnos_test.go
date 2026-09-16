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

// newKeylessFnosProvider 构造一个没有配置密钥的飞牛上游。界面上不再要求填写密钥，
// 后端必须让这种上游照样能调 API。
func newKeylessFnosProvider(t *testing.T, baseURL string) *fnosProvider {
	t.Helper()
	provider, err := New(config.Upstream{
		Name:       "fnos",
		Type:       config.UpstreamFnos,
		BaseURL:    baseURL,
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

// 飞牛影视没有密钥可填：空密钥必须放行，还要照常补 /emby 前缀；否则代理层会
// 退化成纯反代，播放请求永远拿不到 302，而且日志里看不出是缺密钥导致的。
func TestFnosWorksWithoutAPIKey(t *testing.T) {
	var gotPath, gotQuery, gotToken string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		gotPath = request.URL.Path
		gotQuery = request.URL.RawQuery
		gotToken = request.Header.Get("X-Emby-Token")
		writer.WriteHeader(http.StatusOK)
		_, _ = writer.Write([]byte(`{"Items":[]}`))
	}))
	defer server.Close()

	provider := newKeylessFnosProvider(t, server.URL)
	if !provider.HasCredentials() {
		t.Fatal("HasCredentials() = false，飞牛会被当成没凭据而退化成纯反代")
	}

	var out map[string]any
	if err := provider.client.getJSON(context.Background(), "/Items", nil, &out); err != nil {
		t.Fatalf("没有密钥时 getJSON 不该报错，实际: %v", err)
	}
	if gotPath != "/emby/Items" {
		t.Fatalf("请求路径 = %q, want /emby/Items", gotPath)
	}
	if gotToken != "" {
		t.Fatalf("没有密钥时不该带 X-Emby-Token，实际 %q", gotToken)
	}
	if strings.Contains(gotQuery, "api_key") {
		t.Fatalf("没有密钥时不该带 api_key 查询参数，实际 %q", gotQuery)
	}
}

// 放宽只针对飞牛：Emby 没有密钥时仍然要挡住，不能让请求白跑一趟。
func TestEmbyStillRequiresAPIKey(t *testing.T) {
	provider, err := New(config.Upstream{
		Name:       "emby",
		Type:       config.UpstreamEmby,
		BaseURL:    "http://10.0.0.31:8096",
		ListenPort: 5153,
	})
	if err != nil {
		t.Fatalf("New(emby) returned error: %v", err)
	}
	if provider.HasCredentials() {
		t.Fatal("Emby 没有密钥时 HasCredentials() 应为 false")
	}
	emby, ok := provider.(*embyProvider)
	if !ok {
		t.Fatalf("New(emby) returned %T, want *embyProvider", provider)
	}
	var out map[string]any
	if err := emby.client.getJSON(context.Background(), "/Items", nil, &out); err != ErrNoAPIKey {
		t.Fatalf("getJSON error = %v, want ErrNoAPIKey", err)
	}
}

// 配置文件里手工填了密钥时，飞牛依旧要把它带上，兼容需要鉴权的个例。
func TestFnosSendsKeyWhenConfigured(t *testing.T) {
	var gotToken, gotQuery string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		gotToken = request.Header.Get("X-Emby-Token")
		gotQuery = request.URL.RawQuery
		writer.WriteHeader(http.StatusOK)
		_, _ = writer.Write([]byte(`{}`))
	}))
	defer server.Close()

	provider := newFnosProvider(t, server.URL) // 这个 helper 带 APIKey: "token"
	var out map[string]any
	if err := provider.client.getJSON(context.Background(), "/Items", nil, &out); err != nil {
		t.Fatalf("getJSON returned error: %v", err)
	}
	if gotToken != "token" {
		t.Fatalf("X-Emby-Token = %q, want %q", gotToken, "token")
	}
	if !strings.Contains(gotQuery, "api_key=token") {
		t.Fatalf("查询参数 = %q, want 含 api_key=token", gotQuery)
	}
}

// newFnosProviderWithLogin 构造一个用账号密码登录的飞牛上游。
func newFnosProviderWithLogin(t *testing.T, baseURL, username, password string) *fnosProvider {
	t.Helper()
	provider, err := New(config.Upstream{
		Name:       "fnos",
		Type:       config.UpstreamFnos,
		BaseURL:    baseURL,
		Username:   username,
		Password:   password,
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

// fakeFnosAuthServer 搭一个只认令牌的假飞牛：登录接口发令牌，其余接口校验它。
// tokenOf 给出第 n 次登录该发什么令牌，hits 返回该次接口请求应当得到的状态码，
// onLogin 会在每次登录时收到原始请求，便于断言请求体与请求头。
func fakeFnosAuthServer(t *testing.T, tokenOf func(login int) string, hits func(login int, token string) int, onLogin func(request *http.Request)) (*httptest.Server, *int) {
	t.Helper()
	logins := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/emby/Users/AuthenticateByName" {
			status := hits(logins, request.Header.Get("X-Emby-Token"))
			writer.WriteHeader(status)
			if status == http.StatusOK {
				_, _ = writer.Write([]byte(`{"Items":[]}`))
				return
			}
			_, _ = writer.Write([]byte(`{"error":"unauthorized"}`))
			return
		}
		logins++
		if onLogin != nil {
			onLogin(request)
		}
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusOK)
		_, _ = writer.Write([]byte(`{"AccessToken":"` + tokenOf(logins) + `","User":{"Id":"u1"}}`))
	}))
	t.Cleanup(server.Close)
	return server, &logins
}

// 飞牛没有静态密钥，接口只认登录令牌：配了账号密码就必须先登录，而且要用同
// 一张令牌干活，不能每调一个接口就登录一次。
func TestFnosLogsInAndReusesToken(t *testing.T) {
	var loginBody, loginAuthorization string
	var seenTokens []string
	server, logins := fakeFnosAuthServer(t, func(int) string { return "tok-1" },
		func(_ int, token string) int {
			seenTokens = append(seenTokens, token)
			return http.StatusOK
		},
		func(request *http.Request) {
			loginAuthorization = request.Header.Get("X-Emby-Authorization")
			raw, _ := io.ReadAll(request.Body)
			loginBody = string(raw)
		})

	provider := newFnosProviderWithLogin(t, server.URL, "kiro", "s3cret")
	if !provider.HasCredentials() {
		t.Fatal("配了账号密码的飞牛应当被视为具备凭据")
	}
	for attempt := 0; attempt < 2; attempt++ {
		var out map[string]any
		if err := provider.client.getJSON(context.Background(), "/Items", nil, &out); err != nil {
			t.Fatalf("第 %d 次 getJSON 返回错误: %v", attempt+1, err)
		}
	}

	if *logins != 1 {
		t.Fatalf("登录次数 = %d, want 1（令牌应当在 TTL 内复用）", *logins)
	}
	if len(seenTokens) != 2 || seenTokens[0] != "tok-1" || seenTokens[1] != "tok-1" {
		t.Fatalf("接口请求携带的令牌 = %v, want 两次都是 tok-1", seenTokens)
	}
	if loginAuthorization == "" {
		t.Fatal("登录请求缺少 X-Emby-Authorization，部分版本会直接 400")
	}
	if !strings.Contains(loginBody, `"Username":"kiro"`) || !strings.Contains(loginBody, `"Pw":"s3cret"`) {
		t.Fatalf("登录请求体 = %s, want 含 Username 与 Pw", loginBody)
	}
}

// 令牌会被上游吊销（改密码、别处登出、服务端重启）。撞上 401 必须丢掉缓存
// 重登一次再试，否则一次令牌过期会表现成「上游坏了」。
func TestFnosReloginsWhenTokenRejected(t *testing.T) {
	var seen []string
	server, logins := fakeFnosAuthServer(t, func(login int) string { return "tok-" + strconv.Itoa(login) },
		func(_ int, token string) int {
			seen = append(seen, token)
			if token == "tok-2" {
				return http.StatusOK
			}
			return http.StatusUnauthorized
		}, nil)

	provider := newFnosProviderWithLogin(t, server.URL, "kiro", "s3cret")
	var out map[string]any
	if err := provider.client.getJSON(context.Background(), "/Items", nil, &out); err != nil {
		t.Fatalf("令牌被拒后应当重登并成功，实际: %v", err)
	}
	if *logins != 2 {
		t.Fatalf("登录次数 = %d, want 2", *logins)
	}
	if len(seen) != 2 || seen[0] != "tok-1" || seen[1] != "tok-2" {
		t.Fatalf("两次请求携带的令牌 = %v, want [tok-1 tok-2]", seen)
	}
}

// 飞牛在没配账号时不接受 /System/Info，试连必须去问不需要鉴权的
// /System/Info/Public，否则用户看到的会是「连接失败」而不是「连上了」。
func TestFnosPingUsesPublicSystemInfo(t *testing.T) {
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		paths = append(paths, request.URL.Path)
		if request.URL.Path != "/emby/System/Info/Public" {
			writer.WriteHeader(http.StatusUnauthorized)
			return
		}
		writer.WriteHeader(http.StatusOK)
		_, _ = writer.Write([]byte(`{"ServerName":"飞牛NAS","Version":"1.0.1"}`))
	}))
	defer server.Close()

	provider := newKeylessFnosProvider(t, server.URL)
	label, err := provider.Ping(context.Background())
	if err != nil {
		t.Fatalf("Ping returned error: %v", err)
	}
	if label != "飞牛影视 飞牛NAS v1.0.1" {
		t.Fatalf("Ping = %q", label)
	}
	if len(paths) != 1 || paths[0] != "/emby/System/Info/Public" {
		t.Fatalf("探测路径 = %v，want 只问 /emby/System/Info/Public", paths)
	}
}

// 没配账号密码时读媒体库必然 401，错误里要写清「该填什么」，
// 而不是把上游的状态码原样丢给用户。
func TestFnosLibrariesHintWhenNoCredentials(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.WriteHeader(http.StatusUnauthorized)
		_, _ = writer.Write([]byte(`{"error":"unauthorized"}`))
	}))
	defer server.Close()

	provider := newKeylessFnosProvider(t, server.URL)
	if _, err := provider.Libraries(context.Background()); err == nil {
		t.Fatal("没有账号密码时读媒体库应当报错")
	} else if !strings.Contains(err.Error(), "未配置登录账号") {
		t.Fatalf("错误信息应当提示要填账号，实际: %v", err)
	}
}
