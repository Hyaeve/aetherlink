package adminapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aetherlink/aetherlink/internal/auth"
	"github.com/aetherlink/aetherlink/internal/config"
	"github.com/aetherlink/aetherlink/internal/pathmap"
	"github.com/aetherlink/aetherlink/internal/resolver"
	"github.com/aetherlink/aetherlink/internal/runtime"
	"github.com/aetherlink/aetherlink/internal/stats"
)

const (
	testUsername = "admin"
	testPassword = "aetherlink-test-pw"
)

type testEnv struct {
	handler  http.Handler
	rt       *runtime.Runtime
	sessions *auth.Store
	strmPath string
	confPath string
	token    string
}

// newEnv builds an API over a real runtime backed by a temp config file whose
// admin account has already been changed away from the built-in one.
func newEnv(t *testing.T) *testEnv {
	t.Helper()
	env := newFreshEnv(t)
	env.setAccount(t, testUsername, testPassword)
	return env
}

// newFreshEnv builds a just-started container: main.go seeds the built-in
// admin/password account on first boot, so there is never an unauthenticated
// window and no setup wizard.
func newFreshEnv(t *testing.T) *testEnv {
	t.Helper()
	env := newBareEnv(t)
	defaults, err := auth.Default()
	if err != nil {
		t.Fatal(err)
	}
	if err := env.rt.Apply(func(draft *config.Config) error {
		draft.Auth = defaults
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return env
}

// newBareEnv builds an instance with no account at all. Only the seeding logic
// in main.go produces this state transiently; tests use it as a starting point.
func newBareEnv(t *testing.T) *testEnv {
	t.Helper()
	root := t.TempDir()
	strmPath := filepath.Join(root, "001.strm")
	if err := os.WriteFile(strmPath, []byte("http://10.0.0.31:19527/d/bi6jeznun2rvu88v6.m4a?/001.总序.m4a"), 0o600); err != nil {
		t.Fatal(err)
	}
	confPath := filepath.Join(root, "config", "config.yaml")
	cfg, _, err := config.LoadOrCreate(confPath)
	if err != nil {
		t.Fatal(err)
	}
	rt, err := runtime.New(cfg, stats.New(20))
	if err != nil {
		t.Fatal(err)
	}
	sessions := auth.NewStore(time.Hour)
	return &testEnv{
		handler:  New(rt, sessions).Handler(),
		rt:       rt,
		sessions: sessions,
		strmPath: pathmap.Normalize(strmPath),
		confPath: confPath,
	}
}

func (e *testEnv) do(method, target, body, token string) *httptest.ResponseRecorder {
	var request *http.Request
	if body == "" {
		request = httptest.NewRequest(method, target, nil)
	} else {
		request = httptest.NewRequest(method, target, strings.NewReader(body))
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	recorder := httptest.NewRecorder()
	e.handler.ServeHTTP(recorder, request)
	return recorder
}

// setAccount 用内置账号登录后把账号改成给定的账号密码，等价于用户第一次进设置页。
func (e *testEnv) setAccount(t *testing.T, username, password string) {
	t.Helper()
	token := e.login(t, auth.DefaultUsername, auth.DefaultPassword)
	body := `{"currentPassword":"` + auth.DefaultPassword + `","username":"` + username + `","newPassword":"` + password + `"}`
	recorder := e.do(http.MethodPost, BasePath+"/account", body, token)
	if recorder.Code != http.StatusOK {
		t.Fatalf("account status = %d, body=%s", recorder.Code, recorder.Body.String())
	}
}

func (e *testEnv) login(t *testing.T, username, password string) string {
	t.Helper()
	recorder := e.do(http.MethodPost, BasePath+"/login", `{"username":"`+username+`","password":"`+password+`"}`, "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("login status = %d, body=%s", recorder.Code, recorder.Body.String())
	}
	return decodeToken(t, recorder)
}

func decodeToken(t *testing.T, recorder *httptest.ResponseRecorder) string {
	t.Helper()
	var response struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode token: %v (body=%s)", err, recorder.Body.String())
	}
	if response.Token == "" {
		t.Fatalf("empty token in %s", recorder.Body.String())
	}
	return response.Token
}

func TestHealthAndBootstrapNeedNoToken(t *testing.T) {
	env := newFreshEnv(t)
	for _, target := range []string{BasePath + "/health", BasePath + "/bootstrap"} {
		if recorder := env.do(http.MethodGet, target, "", ""); recorder.Code != http.StatusOK {
			t.Errorf("%s status = %d, want 200", target, recorder.Code)
		}
	}
}

// bootstrap 免鉴权，所以既不能回显内置凭据，也不能透露还在用默认账号，更不该给出
// 当前账号名——否则等于告诉扫端口的人「这里 admin/password 就能进」。登录页不需要
// 任何账号信息（账号由使用者自己输），账号名只在登录之后由 /config 提供。
func TestBootstrapNeverMentionsCredentials(t *testing.T) {
	for name, env := range map[string]*testEnv{"fresh": newFreshEnv(t), "changed": newEnv(t)} {
		body := env.do(http.MethodGet, BasePath+"/bootstrap", "", "").Body.String()
		for _, leak := range []string{
			"defaultUsername", "defaultPassword", "defaultCredentials", "default_credentials",
			"password_hash", "PasswordHash", "salt", "iterations", "username", auth.DefaultPassword,
		} {
			if strings.Contains(body, leak) {
				t.Fatalf("%s: bootstrap leaked %q: %s", name, leak, body)
			}
		}
	}
}

// bootstrap 不需要鉴权，因此绝不能泄露上游地址、密钥或配置文件路径。
func TestBootstrapLeaksNothingSensitive(t *testing.T) {
	env := newEnv(t)
	token := env.login(t, testUsername, testPassword)
	payload := `{"name":"abs","type":"audiobookshelf","baseUrl":"http://10.0.0.9:13378","apiKey":"super-secret-key","listenPort":13378}`
	if recorder := env.do(http.MethodPost, BasePath+"/upstreams", payload, token); recorder.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body=%s", recorder.Code, recorder.Body.String())
	}
	body := env.do(http.MethodGet, BasePath+"/bootstrap", "", "").Body.String()
	for _, secret := range []string{"super-secret-key", "10.0.0.9", env.confPath, "password_hash"} {
		if strings.Contains(body, secret) {
			t.Fatalf("bootstrap leaked %q: %s", secret, body)
		}
	}
}

// 首次启动就有内置账号，因此没有「未初始化」状态：受保护路由一律返回 401。
func TestFreshInstanceAcceptsBuiltInAccount(t *testing.T) {
	env := newFreshEnv(t)
	if recorder := env.do(http.MethodGet, BasePath+"/config", "", ""); recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", recorder.Code)
	}
	token := env.login(t, auth.DefaultUsername, auth.DefaultPassword)
	if recorder := env.do(http.MethodGet, BasePath+"/config", "", token); recorder.Code != http.StatusOK {
		t.Fatalf("built-in account could not read config: %d", recorder.Code)
	}
}

func TestAccountUpdatePersistsAndDropsDefaultFlag(t *testing.T) {
	env := newFreshEnv(t)
	env.setAccount(t, "kiro", testPassword)

	// 校验材料必须落盘，否则容器一重启账号就丢了。
	raw, err := os.ReadFile(env.confPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "password_hash") {
		t.Fatalf("config file has no password verifier: %s", raw)
	}
	if strings.Contains(string(raw), testPassword) {
		t.Fatal("the plaintext password must never be written to disk")
	}
	if strings.Contains(string(raw), "default_credentials") {
		t.Fatalf("the default-credentials flag should be cleared once changed: %s", raw)
	}

	// 旧的内置账号必须立刻失效。
	if recorder := env.do(http.MethodPost, BasePath+"/login",
		`{"username":"admin","password":"password"}`, ""); recorder.Code != http.StatusUnauthorized {
		t.Fatalf("built-in account still works after the change: %d", recorder.Code)
	}
	env.login(t, "kiro", testPassword)
}

// 只改用户名时新密码留空，当前密码应当继续可用。
func TestAccountUpdateCanChangeUsernameOnly(t *testing.T) {
	env := newEnv(t)
	token := env.login(t, testUsername, testPassword)
	body := `{"currentPassword":"` + testPassword + `","username":"renamed"}`
	if recorder := env.do(http.MethodPost, BasePath+"/account", body, token); recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", recorder.Code, recorder.Body.String())
	}
	env.login(t, "renamed", testPassword)
}

func TestAccountUpdateWithUsernameAndPasswordOnly(t *testing.T) {
	env := newEnv(t)
	token := env.login(t, testUsername, testPassword)
	body := `{"username":"updated","password":"updated-password"}`
	if recorder := env.do(http.MethodPost, BasePath+"/account", body, token); recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", recorder.Code, recorder.Body.String())
	}
	env.login(t, "updated", "updated-password")
}

func TestAccountUpdateRejectsShortPassword(t *testing.T) {
	env := newEnv(t)
	token := env.login(t, testUsername, testPassword)
	body := `{"currentPassword":"` + testPassword + `","username":"admin","newPassword":"short"}`
	if recorder := env.do(http.MethodPost, BasePath+"/account", body, token); recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", recorder.Code)
	}
}

func TestLoginAndTokenEnforcement(t *testing.T) {
	env := newEnv(t)
	if recorder := env.do(http.MethodGet, BasePath+"/config", "", ""); recorder.Code != http.StatusUnauthorized {
		t.Fatalf("missing token status = %d, want 401", recorder.Code)
	}
	if recorder := env.do(http.MethodGet, BasePath+"/config", "", "not-a-token"); recorder.Code != http.StatusUnauthorized {
		t.Fatalf("bad token status = %d, want 401", recorder.Code)
	}
	if recorder := env.do(http.MethodPost, BasePath+"/login",
		`{"username":"admin","password":"wrong-password"}`, ""); recorder.Code != http.StatusUnauthorized {
		t.Fatalf("wrong password status = %d, want 401", recorder.Code)
	}
	// 密码对但用户名不对同样要拒。
	if recorder := env.do(http.MethodPost, BasePath+"/login",
		`{"username":"nobody","password":"`+testPassword+`"}`, ""); recorder.Code != http.StatusUnauthorized {
		t.Fatalf("wrong username status = %d, want 401", recorder.Code)
	}
	token := env.login(t, testUsername, testPassword)
	if recorder := env.do(http.MethodGet, BasePath+"/config", "", token); recorder.Code != http.StatusOK {
		t.Fatalf("authorised status = %d, want 200", recorder.Code)
	}
}

func TestLogoutRevokesToken(t *testing.T) {
	env := newEnv(t)
	token := env.login(t, testUsername, testPassword)
	if recorder := env.do(http.MethodPost, BasePath+"/logout", "{}", token); recorder.Code != http.StatusOK {
		t.Fatalf("logout status = %d", recorder.Code)
	}
	if recorder := env.do(http.MethodGet, BasePath+"/config", "", token); recorder.Code != http.StatusUnauthorized {
		t.Fatalf("token still works after logout: %d", recorder.Code)
	}
}

func TestAccountUpdateRevokesAllSessions(t *testing.T) {
	env := newEnv(t)
	token := env.login(t, testUsername, testPassword)
	body := `{"currentPassword":"` + testPassword + `","username":"admin","newPassword":"brand-new-password"}`
	recorder := env.do(http.MethodPost, BasePath+"/account", body, token)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", recorder.Code, recorder.Body.String())
	}
	if env.sessions.Count() != 0 {
		t.Fatalf("sessions = %d, want 0", env.sessions.Count())
	}
	env.login(t, "admin", "brand-new-password")
}

func TestAccountUpdateRejectsWrongCurrent(t *testing.T) {
	env := newEnv(t)
	token := env.login(t, testUsername, testPassword)
	body := `{"currentPassword":"nope-nope","username":"admin","newPassword":"brand-new-password"}`
	recorder := env.do(http.MethodPost, BasePath+"/account", body, token)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", recorder.Code)
	}
}

// The break-glass token exists for a forgotten password and must keep working
// without a login round trip.
func TestBreakGlassTokenIsAccepted(t *testing.T) {
	env := newEnv(t)
	if err := env.rt.Apply(func(draft *config.Config) error {
		draft.Server.AdminToken = "break-glass-token"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if recorder := env.do(http.MethodGet, BasePath+"/config", "", "break-glass-token"); recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
}

func upstreamPayloadJSON(name, root string, port int) string {
	return `{"name":"` + name + `","type":"audiobookshelf","baseUrl":"http://127.0.0.1:1","apiKey":"jwt-key",` +
		`"listenPort":` + strconv.Itoa(port) + `,"strmRoots":["` + root + `"],"pathMappings":[{"from":"/audiobooks","to":"` + root + `"}]}`
}

func TestUpstreamCRUDPersistsAndHotReloads(t *testing.T) {
	env := newEnv(t)
	token := env.login(t, testUsername, testPassword)
	root := filepath.ToSlash(filepath.Dir(env.strmPath))

	recorder := env.do(http.MethodPost, BasePath+"/upstreams", upstreamPayloadJSON("abs", root, 13378), token)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body=%s", recorder.Code, recorder.Body.String())
	}
	// A new upstream must be live immediately, without restarting the process.
	if env.rt.ProviderByName("abs") == nil {
		t.Fatal("upstream was not mounted after create")
	}

	// Omitting apiKey on update must keep the stored key instead of clearing it.
	recorder = env.do(http.MethodPut, BasePath+"/upstreams/abs", `{"listenPort":18096}`, token)
	if recorder.Code != http.StatusOK {
		t.Fatalf("update status = %d, body=%s", recorder.Code, recorder.Body.String())
	}
	updated := env.rt.Config().UpstreamByName("abs")
	if updated == nil || updated.ListenPort != 18096 {
		t.Fatalf("listen port not updated: %+v", updated)
	}
	if updated.APIKey != "jwt-key" {
		t.Fatalf("api key was lost on update: %q", updated.APIKey)
	}

	// The change must survive a reload from disk.
	reloaded, err := config.Load(env.confPath)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.UpstreamByName("abs").ListenPort != 18096 {
		t.Fatal("update was not persisted")
	}

	recorder = env.do(http.MethodDelete, BasePath+"/upstreams/abs", "", token)
	if recorder.Code != http.StatusOK {
		t.Fatalf("delete status = %d", recorder.Code)
	}
	if env.rt.ProviderByName("abs") != nil {
		t.Fatal("upstream is still mounted after delete")
	}
}

// 「无视上游的不可直放判定」开关删掉之后，接口两侧都不能再出现它：列表响应不带
// ignoreDirectPlayVerdict（否则界面上那个复选框会以 undefined 的形式回来），落盘
// 的配置里也不带。快捷切跳转模式的局部更新语义照旧。
func TestUpstreamAPIStopsExposingIgnoreDirectPlayVerdict(t *testing.T) {
	env := newEnv(t)
	token := env.login(t, testUsername, testPassword)
	root := filepath.ToSlash(filepath.Dir(env.strmPath))

	recorder := env.do(http.MethodPost, BasePath+"/upstreams", upstreamPayloadJSON("abs", root, 13378), token)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body=%s", recorder.Code, recorder.Body.String())
	}
	created := env.rt.Config().UpstreamByName("abs")
	if created == nil {
		t.Fatal("新建的上游不见了")
	}
	if created.IgnoreDirectPlayVerdict != nil {
		t.Fatal("已废弃的开关不该再被 API 写进配置")
	}
	if strings.Contains(recorder.Body.String(), "ignoreDirectPlayVerdict") {
		t.Fatalf("列表响应仍在回已废弃的开关：%s", recorder.Body.String())
	}
	raw, err := os.ReadFile(env.confPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "ignore_direct_play_verdict") {
		t.Fatalf("已废弃的开关不该再落盘：\n%s", raw)
	}

	// 卡片上点标签快捷切模式只发 redirectMode，这条局部更新路径照旧要能用。
	if recorder := env.do(http.MethodPut, BasePath+"/upstreams/abs", `{"redirectMode":"never"}`, token); recorder.Code != http.StatusOK {
		t.Fatalf("切换跳转模式 status = %d, body=%s", recorder.Code, recorder.Body.String())
	}
	if mode := env.rt.Config().UpstreamByName("abs").RedirectMode; mode != config.RedirectNever {
		t.Fatalf("redirect mode = %q, want never", mode)
	}
}

// 卡片级「不中继的客户端」名单删掉之后，接口两侧都不能再出现它：列表响应不带
// relayExemptUserAgents（否则卡片编辑框会以一个空格子回来），落盘的配置里也不带。
func TestUpstreamAPIStopsExposingRelayExemptUserAgents(t *testing.T) {
	env := newEnv(t)
	token := env.login(t, testUsername, testPassword)
	root := filepath.ToSlash(filepath.Dir(env.strmPath))

	recorder := env.do(http.MethodPost, BasePath+"/upstreams", upstreamPayloadJSON("abs", root, 13378), token)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body=%s", recorder.Code, recorder.Body.String())
	}
	if strings.Contains(recorder.Body.String(), "relayExemptUserAgents") {
		t.Fatalf("列表响应仍在回已废弃的名单：%s", recorder.Body.String())
	}
	raw, err := os.ReadFile(env.confPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "relay_exempt_user_agents") {
		t.Fatalf("已废弃的名单不该再落盘：\n%s", raw)
	}
}

func TestCreateUpstreamRejectsDuplicateAndInvalid(t *testing.T) {
	env := newEnv(t)
	token := env.login(t, testUsername, testPassword)
	root := filepath.ToSlash(filepath.Dir(env.strmPath))

	if recorder := env.do(http.MethodPost, BasePath+"/upstreams", upstreamPayloadJSON("abs", root, 13378), token); recorder.Code != http.StatusCreated {
		t.Fatalf("first create status = %d", recorder.Code)
	}
	if recorder := env.do(http.MethodPost, BasePath+"/upstreams", upstreamPayloadJSON("abs", root, 18096), token); recorder.Code != http.StatusBadRequest {
		t.Fatalf("duplicate name status = %d, want 400", recorder.Code)
	}
	// 端口是每个上游的唯一入口，撞车必须当场拒绝。
	if recorder := env.do(http.MethodPost, BasePath+"/upstreams", upstreamPayloadJSON("emby", root, 13378), token); recorder.Code != http.StatusBadRequest {
		t.Fatalf("duplicate port status = %d, want 400", recorder.Code)
	}
	// 管理端口不能被抢走，否则界面自己就没了。
	if recorder := env.do(http.MethodPost, BasePath+"/upstreams", upstreamPayloadJSON("clash", root, 5151), token); recorder.Code != http.StatusBadRequest {
		t.Fatalf("admin port status = %d, want 400", recorder.Code)
	}
	bad := `{"name":"broken","type":"audiobookshelf","baseUrl":"10.0.0.31:13378","listenPort":18097}`
	if recorder := env.do(http.MethodPost, BasePath+"/upstreams", bad, token); recorder.Code != http.StatusBadRequest {
		t.Fatalf("invalid base url status = %d, want 400", recorder.Code)
	}
	// A rejected upstream must not have been mounted.
	if env.rt.ProviderByName("broken") != nil {
		t.Fatal("invalid upstream was mounted")
	}
}

// 界面上添加上游只要求填地址和密钥，端口留空时后端应当自动挑一个不冲突的。
func TestCreateUpstreamAssignsAFreePortWhenOmitted(t *testing.T) {
	env := newEnv(t)
	token := env.login(t, testUsername, testPassword)
	payload := `{"name":"abs","type":"audiobookshelf","baseUrl":"http://127.0.0.1:1","apiKey":"jwt-key"}`
	if recorder := env.do(http.MethodPost, BasePath+"/upstreams", payload, token); recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d, body=%s", recorder.Code, recorder.Body.String())
	}
	created := env.rt.Config().UpstreamByName("abs")
	if created == nil || created.ListenPort != 5152 {
		t.Fatalf("listen port = %+v, want the first free port after the admin one", created)
	}

	second := `{"name":"emby","type":"emby","baseUrl":"http://127.0.0.1:2","apiKey":"emby-key"}`
	if recorder := env.do(http.MethodPost, BasePath+"/upstreams", second, token); recorder.Code != http.StatusCreated {
		t.Fatalf("second status = %d, body=%s", recorder.Code, recorder.Body.String())
	}
	if got := env.rt.Config().UpstreamByName("emby").ListenPort; got != 5153 {
		t.Fatalf("second listen port = %d, want 5153", got)
	}
}

func TestConfigNeverLeaksApiKeys(t *testing.T) {
	env := newEnv(t)
	token := env.login(t, testUsername, testPassword)
	root := filepath.ToSlash(filepath.Dir(env.strmPath))
	env.do(http.MethodPost, BasePath+"/upstreams", upstreamPayloadJSON("abs", root, 13378), token)

	recorder := env.do(http.MethodGet, BasePath+"/config", "", token)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d", recorder.Code)
	}
	body := recorder.Body.String()
	if strings.Contains(body, "jwt-key") {
		t.Fatalf("config response leaked the api key: %s", body)
	}
	if !strings.Contains(body, `"hasApiKey":true`) {
		t.Fatalf("config response should report hasApiKey: %s", body)
	}
	if strings.Contains(body, "password_hash") || strings.Contains(body, testPassword) {
		t.Fatalf("config response leaked auth material: %s", body)
	}
}

// 飞牛的账号密码：账号可以回显（界面上要能看到现在登的是谁），密码只能报
// 「有没有」。密码本身必须落盘，否则重启后登录就失效了。
func TestUpstreamUsernameIsEchoedButPasswordIsNot(t *testing.T) {
	env := newEnv(t)
	token := env.login(t, testUsername, testPassword)
	payload := `{"name":"fnos","type":"fnos","baseUrl":"http://127.0.0.1:8005",` +
		`"username":"kiro","password":"fnos-secret","listenPort":5154}`
	recorder := env.do(http.MethodPost, BasePath+"/upstreams", payload, token)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body=%s", recorder.Code, recorder.Body.String())
	}
	body := recorder.Body.String()
	if strings.Contains(body, "fnos-secret") {
		t.Fatalf("上游列表泄漏了密码: %s", body)
	}
	if !strings.Contains(body, `"username":"kiro"`) || !strings.Contains(body, `"hasPassword":true`) {
		t.Fatalf("上游列表应当回显账号并标明已存密码: %s", body)
	}

	reloaded, err := config.Load(env.confPath)
	if err != nil {
		t.Fatal(err)
	}
	stored := reloaded.UpstreamByName("fnos")
	if stored == nil || stored.Username != "kiro" || stored.Password != "fnos-secret" {
		t.Fatalf("账号密码没有被保存: %+v", stored)
	}

	// 更新其他字段时省略密码，必须保留已存的那一份。
	if recorder := env.do(http.MethodPut, BasePath+"/upstreams/fnos", `{"listenPort":18099}`, token); recorder.Code != http.StatusOK {
		t.Fatalf("update status = %d, body=%s", recorder.Code, recorder.Body.String())
	}
	updated := env.rt.Config().UpstreamByName("fnos")
	if updated == nil || updated.Password != "fnos-secret" || updated.Username != "kiro" {
		t.Fatalf("省略密码的更新弄丢了凭据: %+v", updated)
	}
}

// 只填一半凭据必须被拒，否则会存下一个永远登不上去的上游。
func TestUpstreamRejectsHalfCredentials(t *testing.T) {
	env := newEnv(t)
	token := env.login(t, testUsername, testPassword)
	payload := `{"name":"fnos","type":"fnos","baseUrl":"http://127.0.0.1:8005","username":"kiro","listenPort":5154}`
	if recorder := env.do(http.MethodPost, BasePath+"/upstreams", payload, token); recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body=%s", recorder.Code, recorder.Body.String())
	}
	if env.rt.ProviderByName("fnos") != nil {
		t.Fatal("被拒绝的上游不该被挂载")
	}
}

// 已保存的凭据要能按需查看：界面上点「显示」走的就是这条接口。
// 列表接口仍然只给 hasApiKey / hasPassword，秘密不在那里出现。
func TestUpstreamCredentialsAreRevealableOnDemand(t *testing.T) {
	env := newEnv(t)
	token := env.login(t, testUsername, testPassword)
	payload := `{"name":"fnos","type":"fnos","baseUrl":"http://127.0.0.1:8005",` +
		`"username":"kiro","password":"fnos-secret","listenPort":5154}`
	if recorder := env.do(http.MethodPost, BasePath+"/upstreams", payload, token); recorder.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body=%s", recorder.Code, recorder.Body.String())
	}

	recorder := env.do(http.MethodGet, BasePath+"/upstreams/fnos/credentials", "", token)
	if recorder.Code != http.StatusOK {
		t.Fatalf("credentials status = %d, body=%s", recorder.Code, recorder.Body.String())
	}
	var revealed upstreamCredentials
	if err := json.Unmarshal(recorder.Body.Bytes(), &revealed); err != nil {
		t.Fatalf("decode credentials: %v (body=%s)", err, recorder.Body.String())
	}
	if revealed.Username != "kiro" || revealed.Password != "fnos-secret" {
		t.Fatalf("credentials = %+v，want 取回保存的账号密码", revealed)
	}

	// 没有 token 就看不到秘密 —— 这条接口不比别的接口宽松。
	if recorder := env.do(http.MethodGet, BasePath+"/upstreams/fnos/credentials", "", ""); recorder.Code != http.StatusUnauthorized {
		t.Fatalf("无令牌访问 status = %d, want 401", recorder.Code)
	}
	if recorder := env.do(http.MethodGet, BasePath+"/upstreams/missing/credentials", "", token); recorder.Code != http.StatusNotFound {
		t.Fatalf("不存在的上游 status = %d, want 404", recorder.Code)
	}
}

func TestPutSettingsAppliesAndPersists(t *testing.T) {
	env := newEnv(t)
	token := env.login(t, testUsername, testPassword)
	payload := `{"logLevel":"debug","redirect":{"mode":"private","followUpstreamRedirects":true,"maxFollowHops":3,` +
		`"forwardUserAgent":false,"fallbackUserAgent":"AetherLink/test","probeTimeout":"20s","streamTimeout":"0",` +
		`"allowPublicTargets":false},"cache":{"ttl":"90s","maxSize":128}}`
	recorder := env.do(http.MethodPut, BasePath+"/settings", payload, token)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", recorder.Code, recorder.Body.String())
	}

	cfg := env.rt.Config()
	if cfg.Redirect.Mode != config.RedirectPrivate {
		t.Fatalf("mode = %q", cfg.Redirect.Mode)
	}
	if cfg.Redirect.ShouldForwardUserAgent() || cfg.Redirect.PublicTargetsAllowed() {
		t.Fatal("explicit false booleans were not applied")
	}
	if cfg.Cache.TTL != 90*time.Second || cfg.Cache.MaxSize != 128 {
		t.Fatalf("cache = %+v", cfg.Cache)
	}

	reloaded, err := config.Load(env.confPath)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Redirect.Mode != config.RedirectPrivate || reloaded.Redirect.ShouldForwardUserAgent() {
		t.Fatalf("settings were not persisted: %+v", reloaded.Redirect)
	}
}

// 配置里的 intranet_cidrs 要整条链路都能往返：/config 得下发它，保存得落盘，
// 读回来得还原。界面上已经**没有**这个输入框（自动识别那一层覆盖了绝大多数部署），
// 但设置页保存时会把手上的 settings 整体 PUT 回去 —— 少了任何一环，手改 YAML 填的
// 网段都会在用户点一次保存之后被静默抹掉，而现象跟没填一模一样。
func TestSettingsRoundTripKeepsIntranetCIDRs(t *testing.T) {
	env := newEnv(t)
	token := env.login(t, testUsername, testPassword)
	payload := `{"logLevel":"info","redirect":{"mode":"public","probeTimeout":"15s",` +
		`"intranetCidrs":["240e:390:1a2b:3c4d::/64","192.168.0.0/16"]},` +
		`"cache":{"ttl":"5m","maxSize":10}}`
	if recorder := env.do(http.MethodPut, BasePath+"/settings", payload, token); recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", recorder.Code, recorder.Body.String())
	}

	cfg := env.rt.Config()
	if len(cfg.Redirect.IntranetCIDRs) != 2 || cfg.Redirect.IntranetCIDRs[0] != "240e:390:1a2b:3c4d::/64" {
		t.Fatalf("内网网段没有被应用：%v", cfg.Redirect.IntranetCIDRs)
	}
	reloaded, err := config.Load(env.confPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(reloaded.Redirect.IntranetCIDRs) != 2 {
		t.Fatalf("内网网段没有落盘：%v", reloaded.Redirect.IntranetCIDRs)
	}

	recorder := env.do(http.MethodGet, BasePath+"/config", "", token)
	if recorder.Code != http.StatusOK {
		t.Fatalf("读取配置 status = %d", recorder.Code)
	}
	var response struct {
		Settings struct {
			Redirect struct {
				IntranetCIDRs []string `json:"intranetCidrs"`
			} `json:"redirect"`
		} `json:"settings"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode config: %v (body=%s)", err, recorder.Body.String())
	}
	if len(response.Settings.Redirect.IntranetCIDRs) != 2 || response.Settings.Redirect.IntranetCIDRs[1] != "192.168.0.0/16" {
		t.Fatalf("/config 没有下发内网网段：%v", response.Settings.Redirect.IntranetCIDRs)
	}
}

// 本机网段是只读的：随设置接口下发，让用户看得见「哪些客户端会被自动按内网处理」，
// 但请求里带上它不该被当成配置存下来。
func TestSettingsExposeLocalNetworkPrefixesReadOnly(t *testing.T) {
	env := newEnv(t)
	token := env.login(t, testUsername, testPassword)

	// 请求里塞一个假的本机网段，看它会不会被采纳。
	payload := `{"logLevel":"info","redirect":{"mode":"public","probeTimeout":"15s",` +
		`"intranetCidrs":[],"localNetworkPrefixes":["203.0.113.0/24"]},` +
		`"cache":{"ttl":"5m","maxSize":10}}`
	if recorder := env.do(http.MethodPut, BasePath+"/settings", payload, token); recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", recorder.Code, recorder.Body.String())
	}
	if cfg := env.rt.Config(); len(cfg.Redirect.IntranetCIDRs) != 0 {
		t.Fatalf("只读字段不该被写进配置：%v", cfg.Redirect.IntranetCIDRs)
	}

	recorder := env.do(http.MethodGet, BasePath+"/config", "", token)
	if recorder.Code != http.StatusOK {
		t.Fatalf("读取配置 status = %d", recorder.Code)
	}
	// 空的时候也必须是 []，不是 null —— 界面按数组处理。
	if !strings.Contains(recorder.Body.String(), `"localNetworkPrefixes":[`) {
		t.Fatalf("/config 没有把本机网段当成数组下发：%s", recorder.Body.String())
	}
	var response struct {
		Settings struct {
			Redirect struct {
				LocalNetworkPrefixes []string `json:"localNetworkPrefixes"`
			} `json:"redirect"`
		} `json:"settings"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode config: %v (body=%s)", err, recorder.Body.String())
	}
	// 下发的是这台机器真实检测到的网段，不是请求里塞的那个。
	machine := make([]string, 0, 4)
	for _, prefix := range resolver.LocalNetworkPrefixes() {
		machine = append(machine, prefix.String())
	}
	got := response.Settings.Redirect.LocalNetworkPrefixes
	if len(got) != len(machine) {
		t.Fatalf("本机网段 = %v, want %v", got, machine)
	}
	for index := range machine {
		if got[index] != machine[index] {
			t.Fatalf("本机网段 = %v, want %v", got, machine)
		}
	}
}

func TestPutSettingsRejectsBadValues(t *testing.T) {
	env := newEnv(t)
	token := env.login(t, testUsername, testPassword)
	for name, payload := range map[string]string{
		"bad duration": `{"redirect":{"mode":"always","probeTimeout":"soon"},"cache":{"ttl":"5m","maxSize":10}}`,
		"bad mode":     `{"redirect":{"mode":"sometimes","probeTimeout":"15s"},"cache":{"ttl":"5m","maxSize":10}}`,
	} {
		if recorder := env.do(http.MethodPut, BasePath+"/settings", payload, token); recorder.Code != http.StatusBadRequest {
			t.Errorf("%s status = %d, want 400", name, recorder.Code)
		}
	}
	// The rejected payload must not have changed the running config.
	if env.rt.Config().Redirect.Mode != config.RedirectAlways {
		t.Fatal("a rejected settings update changed the running config")
	}
}

func TestParseStrmEndpointNormalizesPickCode(t *testing.T) {
	env := newEnv(t)
	token := env.login(t, testUsername, testPassword)
	payload := `{"content":"http://10.0.0.31:19527/d/bi6jeznun2rvu88v6.m4a?/001.总序.m4a"}`
	recorder := env.do(http.MethodPost, BasePath+"/strm/parse", payload, token)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d", recorder.Code)
	}
	var response struct {
		OK     bool `json:"ok"`
		Target struct {
			Kind     string `json:"kind"`
			URL      string `json:"url"`
			Filename string `json:"filename"`
		} `json:"target"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !response.OK {
		t.Fatalf("ok = false, body=%s", recorder.Body.String())
	}
	if response.Target.Kind != "pickcode115" {
		t.Fatalf("kind = %q", response.Target.Kind)
	}
	if !strings.Contains(response.Target.URL, "%E6%80%BB%E5%BA%8F") {
		t.Fatalf("url not normalized: %q", response.Target.URL)
	}
	if response.Target.Filename != "001.总序.m4a" {
		t.Fatalf("filename = %q", response.Target.Filename)
	}
}

func TestParseStrmRejectsEmptyContent(t *testing.T) {
	env := newEnv(t)
	token := env.login(t, testUsername, testPassword)
	if recorder := env.do(http.MethodPost, BasePath+"/strm/parse", `{"content":"  "}`, token); recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", recorder.Code)
	}
}

func TestUnknownUpstreamReturns404(t *testing.T) {
	env := newEnv(t)
	token := env.login(t, testUsername, testPassword)
	if recorder := env.do(http.MethodGet, BasePath+"/upstreams/missing/ping", "", token); recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", recorder.Code)
	}
}

func TestStatusReportsRuntimeState(t *testing.T) {
	env := newEnv(t)
	token := env.login(t, testUsername, testPassword)
	recorder := env.do(http.MethodGet, BasePath+"/status", "", token)

	var response statusResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if response.RedirectMode != string(config.RedirectAlways) {
		t.Fatalf("redirect mode = %q", response.RedirectMode)
	}
	if response.UpstreamCount != 0 {
		t.Fatalf("upstream count = %d, want 0 on a fresh instance", response.UpstreamCount)
	}
	if response.ConfigPath != env.confPath {
		t.Fatalf("config path = %q, want %q", response.ConfigPath, env.confPath)
	}
	if response.RestartRequired {
		t.Fatal("restartRequired should be false when listen was never changed")
	}
	if time.Since(response.StartedAt) > time.Minute {
		t.Fatalf("startedAt looks wrong: %v", response.StartedAt)
	}
}

func TestPurgeCacheRequiresToken(t *testing.T) {
	env := newEnv(t)
	if recorder := env.do(http.MethodPost, BasePath+"/cache/purge", "", ""); recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", recorder.Code)
	}
}

// newPersistentEnv 和 newBareEnv 是同一套东西，只是会话存储挂在磁盘上——等价于
// main.go 里那一行 auth.OpenStore。用来验证「保持登录」真的能扛过容器重启。
// 返回值里带上会话文件路径，测试可以拿它在「重启后」重新开一个 Store。
func newPersistentEnv(t *testing.T) (*testEnv, string) {
	t.Helper()
	root := t.TempDir()
	strmPath := filepath.Join(root, "001.strm")
	if err := os.WriteFile(strmPath, []byte("http://10.0.0.31:19527/d/bi6jeznun2rvu88v6.m4a?/001.总序.m4a"), 0o600); err != nil {
		t.Fatal(err)
	}
	confPath := filepath.Join(root, "config", "config.yaml")
	cfg, _, err := config.LoadOrCreate(confPath)
	if err != nil {
		t.Fatal(err)
	}
	rt, err := runtime.New(cfg, stats.New(20))
	if err != nil {
		t.Fatal(err)
	}
	defaults, err := auth.Default()
	if err != nil {
		t.Fatal(err)
	}
	if err := rt.Apply(func(draft *config.Config) error {
		draft.Auth = defaults
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	sessionPath := filepath.Join(root, "sessions.json")
	sessions := auth.OpenStore(time.Hour, sessionPath)
	return &testEnv{
		handler:  New(rt, sessions).Handler(),
		rt:       rt,
		sessions: sessions,
		strmPath: pathmap.Normalize(strmPath),
		confPath: confPath,
	}, sessionPath
}

// 登录页勾了「保持登录」，后端就得按「7 天 + 落盘」签发，并把这个选择如实回报，
// 前端据此确认自己勾的到底生效没有。重启之后（同一个会话文件上重开一套 API）
// 那个令牌必须还能用。
func TestLoginRememberSurvivesRestart(t *testing.T) {
	env, sessionPath := newPersistentEnv(t)
	body := `{"username":"` + auth.DefaultUsername + `","password":"` + auth.DefaultPassword + `","remember":true}`
	recorder := env.do(http.MethodPost, BasePath+"/login", body, "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("login status = %d, body=%s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		Token    string    `json:"token"`
		Expires  time.Time `json:"expiresAt"`
		Remember bool      `json:"remember"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode login: %v (body=%s)", err, recorder.Body.String())
	}
	if !response.Remember {
		t.Fatalf("remember = false in %s", recorder.Body.String())
	}
	if delta := time.Until(response.Expires) - auth.DefaultRememberTTL; delta > time.Minute || delta < -time.Minute {
		t.Fatalf("expiresAt = %v, want about %v from now", response.Expires, auth.DefaultRememberTTL)
	}

	// 容器重启。
	restarted := auth.OpenStore(time.Hour, sessionPath)
	if !restarted.Valid(response.Token) {
		t.Fatal("a remembered token should still be accepted after a restart")
	}
}

// 没勾的那一半：普通的 12 小时内存会话，容器一重启就必须重新登录。
func TestLoginWithoutRememberDoesNotSurviveRestart(t *testing.T) {
	env, sessionPath := newPersistentEnv(t)
	token := env.login(t, auth.DefaultUsername, auth.DefaultPassword)
	if auth.OpenStore(time.Hour, sessionPath).Valid(token) {
		t.Fatal("a session without remember must not survive a restart")
	}
}
