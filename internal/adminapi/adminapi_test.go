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

const testPassword = "aetherlink-test-pw"

type testEnv struct {
	handler  http.Handler
	rt       *runtime.Runtime
	sessions *auth.Store
	strmPath string
	confPath string
	token    string
}

// newEnv builds an API over a real runtime backed by a temp config file, with an
// admin password already set.
func newEnv(t *testing.T) *testEnv {
	t.Helper()
	env := newBareEnv(t)
	env.setup(t, testPassword)
	return env
}

// newBareEnv builds an uninitialised instance: no password, no upstreams. This
// is what a freshly started container looks like.
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

func (e *testEnv) setup(t *testing.T, password string) {
	t.Helper()
	recorder := e.do(http.MethodPost, BasePath+"/setup", `{"password":"`+password+`"}`, "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("setup status = %d, body=%s", recorder.Code, recorder.Body.String())
	}
	e.token = decodeToken(t, recorder)
}

func (e *testEnv) login(t *testing.T, password string) string {
	t.Helper()
	recorder := e.do(http.MethodPost, BasePath+"/login", `{"password":"`+password+`"}`, "")
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

func TestHealthAndSetupStateNeedNoToken(t *testing.T) {
	env := newBareEnv(t)
	for _, target := range []string{BasePath + "/health", BasePath + "/setup/state"} {
		if recorder := env.do(http.MethodGet, target, "", ""); recorder.Code != http.StatusOK {
			t.Errorf("%s status = %d, want 200", target, recorder.Code)
		}
	}
	recorder := env.do(http.MethodGet, BasePath+"/setup/state", "", "")
	if !strings.Contains(recorder.Body.String(), `"configured":false`) {
		t.Fatalf("a bare instance must report configured=false: %s", recorder.Body.String())
	}
}

// Before the password is set every protected route must say so explicitly, so
// the UI can show the wizard instead of a login prompt.
func TestProtectedRoutesAskForSetupFirst(t *testing.T) {
	env := newBareEnv(t)
	recorder := env.do(http.MethodGet, BasePath+"/config", "", "")
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", recorder.Code)
	}
	if !strings.Contains(recorder.Body.String(), "setup_required") {
		t.Fatalf("body = %s", recorder.Body.String())
	}
}

func TestSetupPersistsPasswordAndCannotRun_Twice(t *testing.T) {
	env := newBareEnv(t)
	env.setup(t, testPassword)

	// The verifier must be on disk, not only in memory: a container restart has
	// to keep the password.
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

	recorder := env.do(http.MethodPost, BasePath+"/setup", `{"password":"another-password"}`, "")
	if recorder.Code != http.StatusConflict {
		t.Fatalf("second setup status = %d, want 409", recorder.Code)
	}
}

func TestSetupRejectsShortPassword(t *testing.T) {
	env := newBareEnv(t)
	recorder := env.do(http.MethodPost, BasePath+"/setup", `{"password":"short"}`, "")
	if recorder.Code != http.StatusBadRequest {
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
	if recorder := env.do(http.MethodPost, BasePath+"/login", `{"password":"wrong-password"}`, ""); recorder.Code != http.StatusUnauthorized {
		t.Fatalf("wrong password status = %d, want 401", recorder.Code)
	}
	token := env.login(t, testPassword)
	if recorder := env.do(http.MethodGet, BasePath+"/config", "", token); recorder.Code != http.StatusOK {
		t.Fatalf("authorised status = %d, want 200", recorder.Code)
	}
}

func TestLogoutRevokesToken(t *testing.T) {
	env := newEnv(t)
	token := env.login(t, testPassword)
	if recorder := env.do(http.MethodPost, BasePath+"/logout", "{}", token); recorder.Code != http.StatusOK {
		t.Fatalf("logout status = %d", recorder.Code)
	}
	if recorder := env.do(http.MethodGet, BasePath+"/config", "", token); recorder.Code != http.StatusUnauthorized {
		t.Fatalf("token still works after logout: %d", recorder.Code)
	}
}

func TestChangePasswordRevokesAllSessions(t *testing.T) {
	env := newEnv(t)
	token := env.login(t, testPassword)
	recorder := env.do(http.MethodPost, BasePath+"/password", `{"currentPassword":"`+testPassword+`","newPassword":"brand-new-password"}`, token)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", recorder.Code, recorder.Body.String())
	}
	if env.sessions.Count() != 0 {
		t.Fatalf("sessions = %d, want 0", env.sessions.Count())
	}
	env.login(t, "brand-new-password")
}

func TestChangePasswordRejectsWrongCurrent(t *testing.T) {
	env := newEnv(t)
	token := env.login(t, testPassword)
	recorder := env.do(http.MethodPost, BasePath+"/password", `{"currentPassword":"nope-nope","newPassword":"brand-new-password"}`, token)
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
	token := env.login(t, testPassword)
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
	token := env.login(t, testPassword)
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
	token := env.login(t, testPassword)
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
	token := env.login(t, testPassword)
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
	token := env.login(t, testPassword)
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
	token := env.login(t, testPassword)
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
	token := env.login(t, testPassword)
	if recorder := env.do(http.MethodPost, BasePath+"/strm/parse", `{"content":"  "}`, token); recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", recorder.Code)
	}
}

func TestUnknownUpstreamReturns404(t *testing.T) {
	env := newEnv(t)
	token := env.login(t, testPassword)
	if recorder := env.do(http.MethodGet, BasePath+"/upstreams/missing/ping", "", token); recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", recorder.Code)
	}
}

func TestStatusReportsRuntimeState(t *testing.T) {
	env := newEnv(t)
	token := env.login(t, testPassword)
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
