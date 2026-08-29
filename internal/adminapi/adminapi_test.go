package adminapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aetherlink/aetherlink/internal/auth"
	"github.com/aetherlink/aetherlink/internal/config"
	"github.com/aetherlink/aetherlink/internal/pathmap"
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

// bootstrap 免鉴权，所以既不能回显内置凭据，也不能透露还在用默认账号——
// 否则等于告诉扫端口的人「这里 admin/password 就能进」。
func TestBootstrapNeverMentionsCredentials(t *testing.T) {
	for name, env := range map[string]*testEnv{"fresh": newFreshEnv(t), "changed": newEnv(t)} {
		body := env.do(http.MethodGet, BasePath+"/bootstrap", "", "").Body.String()
		for _, leak := range []string{"defaultUsername", "defaultPassword", "defaultCredentials", "username", auth.DefaultPassword} {
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
	payload := `{"name":"abs","type":"audiobookshelf","baseUrl":"http://10.0.0.9:13378","apiKey":"super-secret-key","prefix":"/"}`
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

func upstreamPayloadJSON(name, root string) string {
	return `{"name":"` + name + `","type":"audiobookshelf","baseUrl":"http://127.0.0.1:1","apiKey":"jwt-key",` +
		`"prefix":"/","strmRoots":["` + root + `"],"pathMappings":[{"from":"/audiobooks","to":"` + root + `"}]}`
}

func TestUpstreamCRUDPersistsAndHotReloads(t *testing.T) {
	env := newEnv(t)
	token := env.login(t, testUsername, testPassword)
	root := filepath.ToSlash(filepath.Dir(env.strmPath))

	recorder := env.do(http.MethodPost, BasePath+"/upstreams", upstreamPayloadJSON("abs", root), token)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body=%s", recorder.Code, recorder.Body.String())
	}
	// A new upstream must be live immediately, without restarting the process.
	if env.rt.ProviderByName("abs") == nil {
		t.Fatal("upstream was not mounted after create")
	}

	// Omitting apiKey on update must keep the stored key instead of clearing it.
	recorder = env.do(http.MethodPut, BasePath+"/upstreams/abs", `{"prefix":"/audiobooks"}`, token)
	if recorder.Code != http.StatusOK {
		t.Fatalf("update status = %d, body=%s", recorder.Code, recorder.Body.String())
	}
	updated := env.rt.Config().UpstreamByName("abs")
	if updated == nil || updated.Prefix != "/audiobooks" {
		t.Fatalf("prefix not updated: %+v", updated)
	}
	if updated.APIKey != "jwt-key" {
		t.Fatalf("api key was lost on update: %q", updated.APIKey)
	}

	// The change must survive a reload from disk.
	reloaded, err := config.Load(env.confPath)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.UpstreamByName("abs").Prefix != "/audiobooks" {
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

func TestCreateUpstreamRejectsDuplicateAndInvalid(t *testing.T) {
	env := newEnv(t)
	token := env.login(t, testUsername, testPassword)
	root := filepath.ToSlash(filepath.Dir(env.strmPath))

	if recorder := env.do(http.MethodPost, BasePath+"/upstreams", upstreamPayloadJSON("abs", root), token); recorder.Code != http.StatusCreated {
		t.Fatalf("first create status = %d", recorder.Code)
	}
	if recorder := env.do(http.MethodPost, BasePath+"/upstreams", upstreamPayloadJSON("abs", root), token); recorder.Code != http.StatusBadRequest {
		t.Fatalf("duplicate create status = %d, want 400", recorder.Code)
	}
	bad := `{"name":"broken","type":"audiobookshelf","baseUrl":"10.0.0.31:13378"}`
	if recorder := env.do(http.MethodPost, BasePath+"/upstreams", bad, token); recorder.Code != http.StatusBadRequest {
		t.Fatalf("invalid base url status = %d, want 400", recorder.Code)
	}
	// A rejected upstream must not have been mounted.
	if env.rt.ProviderByName("broken") != nil {
		t.Fatal("invalid upstream was mounted")
	}
}

func TestConfigNeverLeaksApiKeys(t *testing.T) {
	env := newEnv(t)
	token := env.login(t, testUsername, testPassword)
	root := filepath.ToSlash(filepath.Dir(env.strmPath))
	env.do(http.MethodPost, BasePath+"/upstreams", upstreamPayloadJSON("abs", root), token)

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
