package upstream

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// playerRequest 造一个播放器请求：Emby 客户端标准的身份头形态。
func playerRequest(t *testing.T) *http.Request {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, "/emby/videos/42/stream.mkv", nil)
	request.Header.Set("X-Emby-Authorization",
		`MediaBrowser Client="AfuseKt", Device="Android", DeviceId="abc", Version="1.0", Token="player-token"`)
	return request
}

// 播放器带令牌的方式不止一种，三种都要能摘出来，否则「复用播放器身份」这件事
// 就只在部分客户端上成立。
func TestEmbyTokenFromRequest(t *testing.T) {
	cases := []struct {
		name   string
		header map[string]string
		target string
		want   string
	}{
		{
			name:   "标准身份头",
			header: map[string]string{"X-Emby-Authorization": `MediaBrowser Client="AfuseKt", Device="Android", DeviceId="abc", Version="1.0", Token="tok-a"`},
			want:   "tok-a",
		},
		{
			name:   "Token 不在最后一位",
			header: map[string]string{"X-Emby-Authorization": `MediaBrowser Token="tok-b", Client="AetherLink"`},
			want:   "tok-b",
		},
		{
			name:   "身份头里没有 Token",
			header: map[string]string{"X-Emby-Authorization": `MediaBrowser Client="AetherLink"`},
			want:   "",
		},
		{
			name:   "简写头",
			header: map[string]string{"X-Emby-Token": "tok-c"},
			want:   "tok-c",
		},
		{
			name:   "只有查询参数",
			target: "/emby/videos/42/stream.mkv?api_key=tok-d",
			want:   "tok-d",
		},
		{
			name: "什么都没有",
			want: "",
		},
	}
	for _, testCase := range cases {
		target := testCase.target
		if target == "" {
			target = "/emby/videos/42/stream.mkv"
		}
		request := httptest.NewRequest(http.MethodGet, target, nil)
		for key, value := range testCase.header {
			request.Header.Set(key, value)
		}
		if got := embyTokenFromRequest(request); got != testCase.want {
			t.Errorf("%s: embyTokenFromRequest() = %q, want %q", testCase.name, got, testCase.want)
		}
	}
}

// 配了密钥就还是用配置的：那是管理员身份，权限比播放器更完整
// （Emby 的 /Users 只有管理员读得到），不能被播放器的令牌顶掉。
func TestConfiguredKeyWinsOverClientToken(t *testing.T) {
	var gotQuery string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		gotQuery = request.URL.Query().Get("api_key")
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{}`))
	}))
	defer server.Close()

	base, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	client := &apiClient{
		base:        base,
		apiKey:      "admin-key",
		authHeader:  "X-Emby-Token",
		authQuery:   "api_key",
		embyDialect: true,
		http:        server.Client(),
	}
	ctx := WithClientCredentials(context.Background(), playerRequest(t))

	var out map[string]any
	if err := client.getJSON(ctx, "/Items", nil, &out); err != nil {
		t.Fatalf("getJSON returned error: %v", err)
	}
	if gotQuery != "admin-key" {
		t.Fatalf("api_key = %q，want admin-key", gotQuery)
	}
}

// 配置的账号密码失效（改过密码、账号被停用）时，播放器自己带的令牌仍然可用：
// 播放解析不该因为一份过期的配置而整个失败。参考 LitePan 取条目时直接复制
// 播放器请求的凭据，正是同一个道理。
func TestStaleLoginFallsBackToClientToken(t *testing.T) {
	var gotQuery string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if strings.HasSuffix(request.URL.Path, "/Users/AuthenticateByName") {
			writer.WriteHeader(http.StatusUnauthorized)
			_, _ = writer.Write([]byte(`{"error":"bad password"}`))
			return
		}
		gotQuery = request.URL.Query().Get("api_key")
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{}`))
	}))
	defer server.Close()

	provider := newFnosProviderWithLogin(t, server.URL, "kiro", "stale-password")
	ctx := WithClientCredentials(context.Background(), playerRequest(t))

	var out map[string]any
	if err := provider.client.getJSON(ctx, "/Items", nil, &out); err != nil {
		t.Fatalf("配置失效时应当改用播放器的令牌，实际返回错误: %v", err)
	}
	if gotQuery != "player-token" {
		t.Fatalf("api_key = %q，want player-token", gotQuery)
	}
}

func TestAPIClientUsesContextUserAgent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if got := request.Header.Get("User-Agent"); got != "Player/1.2" {
			t.Fatalf("User-Agent = %q, want Player/1.2", got)
		}
		writer.WriteHeader(http.StatusOK)
		_, _ = writer.Write([]byte(`{}`))
	}))
	defer server.Close()

	base, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	client := &apiClient{base: base, apiKey: "test-key", http: server.Client()}
	if err := client.getJSON(WithUserAgent(context.Background(), "Player/1.2"), "/probe", nil, nil); err != nil {
		t.Fatal(err)
	}
}

func TestWithUserAgentNeverReturnsEmpty(t *testing.T) {
	if got := contextUserAgent(WithUserAgent(context.Background(), "")); got == "" {
		t.Fatal("context User-Agent is empty")
	}
}
