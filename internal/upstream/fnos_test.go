package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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

// fakeFnosClientAuthServer 复刻飞牛实测出来的鉴权口吻：所有 /emby 接口都要求
// 完整的 X-Emby-Authorization 客户端身份头，只发 X-Emby-Token 会被直接
// 400「X-Emby-Authorization is missing」（这正是用户试连时看到的报错）。
// 登录接口发令牌，其余接口校验令牌；withPublicInfo 为 false 时模拟没有
// /System/Info/Public 这条路由的版本。
func fakeFnosClientAuthServer(t *testing.T, withPublicInfo bool) *httptest.Server {
	t.Helper()
	const (
		token   = "tok-1"
		missing = `{"error":"X-Emby-Authorization is missing"}`
	)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/emby/Users/AuthenticateByName" {
			writer.Header().Set("Content-Type", "application/json")
			_, _ = writer.Write([]byte(`{"AccessToken":"` + token + `","User":{"Id":"u1","Name":"kiro"}}`))
			return
		}
		if request.URL.Path == "/emby/System/Info/Public" && !withPublicInfo {
			writer.WriteHeader(http.StatusNotFound)
			return
		}
		// 只认完整身份头：缺了它，哪怕 X-Emby-Token 和 api_key 都在也照样 400。
		if !strings.Contains(request.Header.Get("X-Emby-Authorization"), `Token="`+token+`"`) {
			writer.WriteHeader(http.StatusBadRequest)
			_, _ = writer.Write([]byte(missing))
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"ServerName":"飞牛NAS","Version":"1.0.1","Items":[]}`))
	}))
	t.Cleanup(server.Close)
	return server
}

// 飞牛只认完整的 X-Emby-Authorization 身份头，X-Emby-Token 那个简写形态它不认。
// 少了这枚头，媒体库查询会 400、试连会失败 —— 而且看起来像是「没配置鉴权」，
// 极难定位。这条断言把请求头形态钉死。
func TestFnosSendsEmbyAuthorizationHeader(t *testing.T) {
	var gotHeader, gotTokenHeader string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/emby/Users/AuthenticateByName" {
			writer.Header().Set("Content-Type", "application/json")
			_, _ = writer.Write([]byte(`{"AccessToken":"tok-1","User":{"Id":"u1"}}`))
			return
		}
		gotHeader = request.Header.Get("X-Emby-Authorization")
		gotTokenHeader = request.Header.Get("X-Emby-Token")
		writer.WriteHeader(http.StatusOK)
		_, _ = writer.Write([]byte(`{}`))
	}))
	defer server.Close()

	provider := newFnosProviderWithLogin(t, server.URL, "kiro", "s3cret")
	var out map[string]any
	if err := provider.client.getJSON(context.Background(), "/Items", nil, &out); err != nil {
		t.Fatalf("getJSON returned error: %v", err)
	}
	if !strings.HasPrefix(gotHeader, "MediaBrowser ") {
		t.Fatalf("X-Emby-Authorization = %q, want 以 MediaBrowser 开头", gotHeader)
	}
	if !strings.Contains(gotHeader, `Token="tok-1"`) {
		t.Fatalf("X-Emby-Authorization = %q, want 带上登录令牌", gotHeader)
	}
	// 简写形态继续保留：Emby 本身认它，两枚并存最兼容。
	if gotTokenHeader != "tok-1" {
		t.Fatalf("X-Emby-Token = %q, want tok-1", gotTokenHeader)
	}
}

// 试连的完整链路：登录拿令牌 → 探 /System/Info/Public（有的版本没有这条路由）
// → 回退 /System/Info。飞牛要求带完整身份头，之前只发 X-Emby-Token 会 400，
// 用户看到的就是「GET /System/Info returned 400: X-Emby-Authorization is missing」。
func TestFnosPingWorksWithClientAuthDialect(t *testing.T) {
	server := fakeFnosClientAuthServer(t, false)
	provider := newFnosProviderWithLogin(t, server.URL, "kiro", "s3cret")
	label, err := provider.Ping(context.Background())
	if err != nil {
		t.Fatalf("Ping returned error: %v", err)
	}
	if label != "飞牛影视 飞牛NAS v1.0.1" {
		t.Fatalf("Ping = %q", label)
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

// 两条探测都失败、而且这个上游压根没配账号时，飞牛那句英文拒绝语
// （X-Emby-Authorization is missing）原样丢给用户完全看不出该做什么，
// 必须换成「要填账号密码」。
func TestFnosPingHintWhenNoCredentials(t *testing.T) {
	server := fakeFnosClientAuthServer(t, false)
	provider := newKeylessFnosProvider(t, server.URL)
	if _, err := provider.Ping(context.Background()); err == nil {
		t.Fatal("没有账号密码时 Ping 应当报错")
	} else if !strings.Contains(err.Error(), "登录账号与密码") {
		t.Fatalf("错误信息应当提示要填账号密码，实际: %v", err)
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
	} else if !strings.Contains(err.Error(), "登录账号与密码") {
		t.Fatalf("错误信息应当提示要填账号密码，实际: %v", err)
	}
}

// 管理员账号读得到 /Library/VirtualFolders 时，就该只用它，不该多打一次接口。
func TestFnosLibrariesPrefersVirtualFolders(t *testing.T) {
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/emby/Users/AuthenticateByName" {
			writer.Header().Set("Content-Type", "application/json")
			_, _ = writer.Write([]byte(`{"AccessToken":"tok-1","User":{"Id":"u1"}}`))
			return
		}
		paths = append(paths, request.URL.Path)
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`[{"Name":"电影","ItemId":"lib-1","CollectionType":"movies"}]`))
	}))
	defer server.Close()

	provider := newFnosProviderWithLogin(t, server.URL, "kiro", "s3cret")
	libraries, err := provider.Libraries(context.Background())
	if err != nil {
		t.Fatalf("Libraries returned error: %v", err)
	}
	if len(libraries) != 1 || libraries[0].Name != "电影" || libraries[0].ID != "lib-1" {
		t.Fatalf("libraries = %#v", libraries)
	}
	if len(paths) != 1 || paths[0] != "/emby/Library/VirtualFolders" {
		t.Fatalf("请求路径 = %v，want 只问 /emby/Library/VirtualFolders", paths)
	}
}

// 飞牛的 /Library/VirtualFolders 是管理员接口，普通账号会被 403 挡掉；这时必须
// 退回用户级的 /Library/SelectableMediaFolders，否则「填了账号也读不到媒体库」，
// 而账号密码的意义就没了。响应是一个扁平数组，没有 Id 的占位项要丢掉。
func TestFnosLibrariesFallsBackToSelectableFolders(t *testing.T) {
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/emby/Users/AuthenticateByName" {
			writer.Header().Set("Content-Type", "application/json")
			_, _ = writer.Write([]byte(`{"AccessToken":"tok-1","User":{"Id":"u1"}}`))
			return
		}
		paths = append(paths, request.URL.Path)
		if request.URL.Path != "/emby/Library/SelectableMediaFolders" {
			writer.WriteHeader(http.StatusForbidden)
			_, _ = writer.Write([]byte(`{"error":"admin only"}`))
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`[{"Id":"lib-1","Name":"电影"},{"Id":"","Name":"占位"},{"Id":"lib-2","Name":"剧集","CollectionType":"tvshows"}]`))
	}))
	defer server.Close()

	provider := newFnosProviderWithLogin(t, server.URL, "kiro", "s3cret")
	libraries, err := provider.Libraries(context.Background())
	if err != nil {
		t.Fatalf("普通账号应当能在用户级接口上读到媒体库，实际: %v", err)
	}
	if len(libraries) != 2 {
		t.Fatalf("libraries = %#v，want 2 条（没有 Id 的占位项要丢掉）", libraries)
	}
	if libraries[0].Name != "电影" || libraries[1].Name != "剧集" || libraries[1].MediaType != "tvshows" {
		t.Fatalf("libraries = %#v", libraries)
	}
	joined := strings.Join(paths, " · ")
	if !strings.Contains(joined, "/emby/Library/VirtualFolders") || !strings.Contains(joined, "/emby/Library/SelectableMediaFolders") {
		t.Fatalf("请求路径 = %v", paths)
	}
	if paths[len(paths)-1] != "/emby/Library/SelectableMediaFolders" {
		t.Fatalf("最后一条请求应当落在用户级接口，实际 = %v", paths)
	}
}

// fnosSPAHTML 复刻飞牛对「不认识的路径」的回应：HTTP 200 加一整页单页应用。
// 这正是用户看到的那条报错的来源 —— json 解析在第一个 '<' 上就失败了。
const fnosSPAHTML = `<!doctype html><html lang="zh-CN"><head><title>飞牛影视</title></head>` +
	`<body><div id="app"></div></body></html>`

// fnosStrmSourceJSON 是一条展开后的媒体源：Path 已经是 .strm 里的直链。
const fnosStrmSourceJSON = `{"Id":"src-1","Path":"https://cdn.example.test/movie.mkv",` +
	`"Protocol":"Http","Container":"strm"}`

// fnosStrmItemJSON 是一条真正的条目详情：Path 是 .strm 指针，媒体源上是展开后的直链。
const fnosStrmItemJSON = `{"Id":"42","Name":"电影","Path":"/media/movie.strm",` +
	`"MediaSources":[` + fnosStrmSourceJSON + `]}`

// fakeFnosItemServer 搭一个「只实现单项路由」的假飞牛：byIDs 给集合路由
// /Items?Ids= 的响应，byID 给单项路由 /Items/{id} 的响应。返回被请求过的路径，
// 便于断言到底走了哪条路由。
func fakeFnosItemServer(t *testing.T, byID, byIDs string) (*httptest.Server, *[]string) {
	t.Helper()
	paths := &[]string{}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		*paths = append(*paths, request.URL.Path)
		switch {
		case request.URL.Path == "/emby/Items":
			writer.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = writer.Write([]byte(byIDs))
		case strings.Count(request.URL.Path, "/") == 3 && strings.HasPrefix(request.URL.Path, "/emby/Items/"):
			writer.Header().Set("Content-Type", "application/json")
			_, _ = writer.Write([]byte(byID))
		default:
			// /emby/Users 之类：给个空对象就够了，取用户 ID 失败不影响断言。
			writer.Header().Set("Content-Type", "application/json")
			_, _ = writer.Write([]byte(`{}`))
		}
	}))
	t.Cleanup(server.Close)
	return server, paths
}

// fakeFnosPlaybackServer 复刻用户在飞牛上实测到的真实情况：两种条目路由都回单页
// 应用的 HTML，只有播放协商 /Items/{id}/PlaybackInfo 是真的 JSON 接口 —— 客户端
// 的播放流程一直在用它，所以它必定可用。
func fakeFnosPlaybackServer(t *testing.T, playback string) (*httptest.Server, *[]string) {
	t.Helper()
	paths := &[]string{}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		*paths = append(*paths, request.URL.Path)
		if strings.HasSuffix(request.URL.Path, "/PlaybackInfo") {
			writer.Header().Set("Content-Type", "application/json")
			_, _ = writer.Write([]byte(playback))
			return
		}
		// 条目路由与 /Users 一概落到单页应用上。
		writer.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = writer.Write([]byte(fnosSPAHTML))
	}))
	t.Cleanup(server.Close)
	return server, paths
}

// blockedStrmSourceJSON 模拟飞牛对外网客户端的判定：SupportsDirectPlay=false
// 且带转码原因（码率超限最常见）。
const blockedStrmSourceJSON = `{"Id":"src-1","Path":"https://cdn.example.test/movie.mkv",` +
	`"Protocol":"Http","Container":"strm","SupportsDirectPlay":false,` +
	`"TranscodeReasons":"ContainerBitrateExceedsLimit"}`

func fnosProviderWithMode(t *testing.T, baseURL string, mode config.RedirectMode) *fnosProvider {
	t.Helper()
	provider, err := New(config.Upstream{
		Name:         "fnos",
		Type:         config.UpstreamFnos,
		BaseURL:      baseURL,
		ListenPort:   5154,
		RedirectMode: mode,
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

func rewriteFnosPlaybackInfo(t *testing.T, provider *fnosProvider, sourceJSON string) (int, map[string]any) {
	t.Helper()
	body := `{"MediaSources":[` + sourceJSON + `]}`
	response := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
	changed, err := provider.RewriteResponse("/emby/Items/42/PlaybackInfo", response)
	if err != nil {
		t.Fatalf("RewriteResponse returned error: %v", err)
	}
	rewritten, readErr := io.ReadAll(response.Body)
	if readErr != nil {
		t.Fatalf("读改写后的响应体失败: %v", readErr)
	}
	var envelope map[string]any
	if err := json.Unmarshal(rewritten, &envelope); err != nil {
		t.Fatalf("改写后的响应不是 JSON: %v", err)
	}
	return changed, envelope
}

// 外网播放走中继的根因：飞牛对外网客户端常在 PlaybackInfo 里判
// SupportsDirectPlay=false（码率限制等），客户端于是转投转码 HLS，
// 转码流量全走飞牛自己。卡片是「始终跳转」时必须无视该判定：
// 强制补 DirectStreamUrl 并记住可直放，/stream 请求才会被 302。
func TestFnosForcesDirectPlayWhenRedirectAlways(t *testing.T) {
	provider := fnosProviderWithMode(t, "http://127.0.0.1:1", config.RedirectAlways)

	changed, envelope := rewriteFnosPlaybackInfo(t, provider, blockedStrmSourceJSON)
	if changed != 1 {
		t.Fatalf("changed = %d，want 1（强制接入 302）", changed)
	}
	sources := envelope["MediaSources"].([]any)
	source := sources[0].(map[string]any)
	// 强制的是 DirectStream 而不是 DirectPlay：DirectPlay 会让客户端绕开
	// AetherLink 直连媒体源的 Path（内网地址或 UA 绑定的网盘直链），
	// 既不会有 302 也多半播不出来。
	if source["SupportsDirectPlay"] != false {
		t.Fatalf("SupportsDirectPlay = %v，want false（客户端不许绕开 AetherLink 直连）", source["SupportsDirectPlay"])
	}
	if source["SupportsDirectStream"] != true {
		t.Fatalf("SupportsDirectStream = %v，want true（客户端应走改写的 /stream 路由）", source["SupportsDirectStream"])
	}
	if directURL, _ := source["DirectStreamUrl"].(string); !strings.Contains(directURL, "/Videos/42/stream") {
		t.Fatalf("DirectStreamUrl = %q，want 含 /Videos/42/stream", directURL)
	}

	target, err := provider.MediaTarget(context.Background(), MediaRef{Kind: RefStream, ItemID: "42", MediaSourceID: "src-1"})
	if err != nil {
		t.Fatalf("MediaTarget returned error: %v", err)
	}
	if target.URL != "https://cdn.example.test/movie.mkv" {
		t.Fatalf("target.URL = %q，want 直链", target.URL)
	}
}

// 跳转模式不是 always 的卡片维持原行为：尊重上游的不可直放判定，
// 媒体源保留给上游转码，/stream 请求也交回上游。
func TestFnosKeepsUpstreamDirectPlayVerdictWithoutAlways(t *testing.T) {
	provider := fnosProviderWithMode(t, "http://127.0.0.1:1", config.RedirectPublic)

	changed, envelope := rewriteFnosPlaybackInfo(t, provider, blockedStrmSourceJSON)
	if changed != 0 {
		t.Fatalf("changed = %d，want 0（保留上游转码）", changed)
	}
	sources := envelope["MediaSources"].([]any)
	source := sources[0].(map[string]any)
	if _, has := source["DirectStreamUrl"]; has {
		t.Fatal("不该给被判定不可直放的源补 DirectStreamUrl")
	}

	_, err := provider.MediaTarget(context.Background(), MediaRef{Kind: RefStream, ItemID: "42", MediaSourceID: "src-1"})
	if !errors.Is(err, ErrDirectPlayUnsupported) {
		t.Fatalf("MediaTarget error = %v，want ErrDirectPlayUnsupported", err)
	}
}

// 飞牛实测：/emby/Items 与 /emby/Items/{id} 都回单页应用的 HTML，只有播放协商
// 那条是真的。这时唯一的出路是把 PlaybackInfo 的媒体源当条目用 —— 否则解析永远
// 断在 HTML 上，播放只能退化成透传。
func TestFnosResolvesMediaViaPlaybackInfoWhenItemRoutesReturnHTML(t *testing.T) {
	server, paths := fakeFnosPlaybackServer(t, `{"MediaSources":[`+fnosStrmSourceJSON+`]}`)
	provider := newKeylessFnosProvider(t, server.URL)

	target, err := provider.MediaTarget(context.Background(), MediaRef{Kind: RefStream, ItemID: "42"})
	if err != nil {
		t.Fatalf("MediaTarget returned error: %v", err)
	}
	if target.URL != "https://cdn.example.test/movie.mkv" {
		t.Fatalf("target.URL = %q，want 直链", target.URL)
	}
	if !containsPath(*paths, "/emby/Items/42/PlaybackInfo") {
		t.Fatalf("请求路径 = %v，want 含 /emby/Items/42/PlaybackInfo", *paths)
	}
	// 它排在最前：既然给出了媒体源，就不该再去问注定回 HTML 的条目路由。
	for _, requestPath := range *paths {
		if requestPath == "/emby/Items" || requestPath == "/emby/Items/42" {
			t.Fatalf("PlaybackInfo 已经给出媒体源，不该再问条目路由，实际请求 = %v", *paths)
		}
	}
}

// 播放器的令牌要能一路送到 API 调用上：飞牛不填账号密码也能解析媒体。
func TestFnosBorrowsPlayerTokenWhenNothingIsConfigured(t *testing.T) {
	var gotToken, gotQuery, gotAuth string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		gotToken = request.Header.Get("X-Emby-Token")
		gotQuery = request.URL.Query().Get("api_key")
		gotAuth = request.Header.Get("X-Emby-Authorization")
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{}`))
	}))
	defer server.Close()

	provider := newKeylessFnosProvider(t, server.URL)
	ctx := WithClientCredentials(context.Background(), playerRequest(t))

	var out map[string]any
	if err := provider.client.getJSON(ctx, "/Items", nil, &out); err != nil {
		t.Fatalf("getJSON returned error: %v", err)
	}
	if gotToken != "player-token" || gotQuery != "player-token" {
		t.Fatalf("X-Emby-Token=%q api_key=%q，want 都是 player-token", gotToken, gotQuery)
	}
	if !strings.Contains(gotAuth, `Token="player-token"`) {
		t.Fatalf("X-Emby-Authorization = %q，want 含播放器令牌", gotAuth)
	}
}

// 客户端播放时 AetherLink 会自己回头查一次条目。飞牛没有实现集合路由
// /Items?Ids=（回的是单页应用的 HTML），所以要换别的路由取：PlaybackInfo 优先
// （见 TestFnosResolvesMediaViaPlaybackInfoWhenItemRoutesReturnHTML），它没给出
// 媒体源时再退回单项路由。少了这一步，解析在 HTML 上就断了，用户看到的是
// 「能进库、能浏览，一播放就失败」。
func TestFnosResolvesMediaViaItemByIDRoute(t *testing.T) {
	server, paths := fakeFnosItemServer(t, fnosStrmItemJSON, fnosSPAHTML)
	provider := newKeylessFnosProvider(t, server.URL)

	target, err := provider.MediaTarget(context.Background(), MediaRef{Kind: RefStream, ItemID: "42"})
	if err != nil {
		t.Fatalf("MediaTarget returned error: %v", err)
	}
	if target.URL != "https://cdn.example.test/movie.mkv" {
		t.Fatalf("target.URL = %q，want 直链", target.URL)
	}
	for _, requestPath := range *paths {
		if requestPath == "/emby/Items" {
			t.Fatalf("不该去问回 HTML 的集合路由，实际请求 = %v", *paths)
		}
	}
	if !containsPath(*paths, "/emby/Items/42") {
		t.Fatalf("请求路径 = %v，want 含 /emby/Items/42", *paths)
	}
}

// 兜底不能只是「换个顺序」：单项路由万一也失效，另一条仍要能救回来。
func TestFnosFallsBackWhenItemByIDReturnsHTML(t *testing.T) {
	server, paths := fakeFnosItemServer(t, fnosSPAHTML, `{"Items":[`+fnosStrmItemJSON+`],"TotalRecordCount":1}`)
	provider := newKeylessFnosProvider(t, server.URL)

	target, err := provider.MediaTarget(context.Background(), MediaRef{Kind: RefStream, ItemID: "42"})
	if err != nil {
		t.Fatalf("MediaTarget returned error: %v", err)
	}
	if target.URL != "https://cdn.example.test/movie.mkv" {
		t.Fatalf("target.URL = %q，want 直链", target.URL)
	}
	if !containsPath(*paths, "/emby/Items/42") || !containsPath(*paths, "/emby/Items") {
		t.Fatalf("两条路由都该试过，实际请求 = %v", *paths)
	}
}

// Emby 的路由支持与飞牛不同：集合路由能用就不该多打一次单项路由。
//
// 基地址带上 /emby —— 只有飞牛那条分支会自动补这个前缀（fnosAPIPrefix），
// Emby 的 apiPrefix 是空的，接口根路径得由用户填的地址给出。
func TestEmbyPrefersItemsByIDsRoute(t *testing.T) {
	server, paths := fakeFnosItemServer(t, fnosStrmItemJSON, `{"Items":[`+fnosStrmItemJSON+`],"TotalRecordCount":1}`)
	provider, err := New(config.Upstream{
		Name:       "emby",
		Type:       config.UpstreamEmby,
		BaseURL:    server.URL + "/emby",
		APIKey:     "key",
		ListenPort: 5153,
	})
	if err != nil {
		t.Fatalf("New(emby) returned error: %v", err)
	}

	target, err := provider.MediaTarget(context.Background(), MediaRef{Kind: RefStream, ItemID: "42"})
	if err != nil {
		t.Fatalf("MediaTarget returned error: %v", err)
	}
	if target.URL != "https://cdn.example.test/movie.mkv" {
		t.Fatalf("target.URL = %q，want 直链", target.URL)
	}
	if !containsPath(*paths, "/emby/Items") {
		t.Fatalf("请求路径 = %v，want 走 /emby/Items", *paths)
	}
	if containsPath(*paths, "/emby/Items/42") || containsPath(*paths, "/emby/Items/42/PlaybackInfo") {
		t.Fatalf("集合路由已经够用，不该再打单项路由或播放协商，实际请求 = %v", *paths)
	}
}

// 两条路由都失败时，报错必须自己说清「哪条路由、上游回了网页」。
// 以前这里只有一句 invalid character '<' looking for beginning of value，
// 既看不出是哪条请求，也看不出上游其实返回了 HTML。
func TestFnosItemLookupErrorNamesRouteAndHTML(t *testing.T) {
	server, _ := fakeFnosItemServer(t, fnosSPAHTML, fnosSPAHTML)
	provider := newKeylessFnosProvider(t, server.URL)

	_, err := provider.MediaTarget(context.Background(), MediaRef{Kind: RefStream, ItemID: "42"})
	if err == nil {
		t.Fatal("两条路由都返回 HTML 时 MediaTarget 应当报错")
	}
	message := err.Error()
	for _, want := range []string{"/Items/{id}", "/Items?Ids=", "返回的是网页", "text/html", "/Items/42"} {
		if !strings.Contains(message, want) {
			t.Fatalf("错误信息缺少 %q，实际: %s", want, message)
		}
	}
}

// looksLikeHTML 只在响应确实是一整页网页时才成立，别把 JSON 里的尖括号也算进来。
func TestLooksLikeHTML(t *testing.T) {
	cases := []struct {
		body string
		want bool
	}{
		{fnosSPAHTML, true},
		{"  \n\t<html><body>spa</body></html>", true},
		{"<!DOCTYPE HTML>\n<html></html>", true},
		{`{"Items":[],"Note":"a < b"}`, false},
		{"", false},
		{"  ", false},
	}
	for _, testCase := range cases {
		if got := looksLikeHTML([]byte(testCase.body)); got != testCase.want {
			t.Errorf("looksLikeHTML(%.40q) = %v, want %v", testCase.body, got, testCase.want)
		}
	}
}

func containsPath(paths []string, want string) bool {
	for _, candidate := range paths {
		if candidate == want {
			return true
		}
	}
	return false
}
