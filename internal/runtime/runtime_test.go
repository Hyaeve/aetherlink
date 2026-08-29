package runtime

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/aetherlink/aetherlink/internal/config"
	"github.com/aetherlink/aetherlink/internal/stats"
)

func newRuntime(t *testing.T) (*Runtime, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	cfg, _, err := config.LoadOrCreate(path)
	if err != nil {
		t.Fatal(err)
	}
	rt, err := New(cfg, stats.New(10))
	if err != nil {
		t.Fatal(err)
	}
	return rt, path
}

func absUpstream(name, baseURL string) config.Upstream {
	return config.Upstream{Name: name, Type: config.UpstreamAudiobookshelf, BaseURL: baseURL, APIKey: "key"}
}

func TestApplyMountsUpstreamWithoutRestart(t *testing.T) {
	rt, path := newRuntime(t)
	if rt.ProviderByName("abs") != nil {
		t.Fatal("a fresh runtime should have no upstreams")
	}
	if err := rt.Apply(func(draft *config.Config) error {
		draft.Upstreams = append(draft.Upstreams, absUpstream("abs", "http://127.0.0.1:13378"))
		return nil
	}); err != nil {
		t.Fatalf("Apply returned error: %v", err)
	}
	if rt.ProviderByName("abs") == nil {
		t.Fatal("upstream was not mounted")
	}

	// The change must be on disk, otherwise it would vanish on restart.
	reloaded, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.UpstreamByName("abs") == nil {
		t.Fatal("upstream was not persisted")
	}
}

// A rejected draft must leave both the running stack and the file untouched.
func TestApplyIsAtomicOnValidationFailure(t *testing.T) {
	rt, path := newRuntime(t)
	if err := rt.Apply(func(draft *config.Config) error {
		draft.Upstreams = append(draft.Upstreams, absUpstream("abs", "http://127.0.0.1:13378"))
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	err := rt.Apply(func(draft *config.Config) error {
		draft.Redirect.Mode = config.RedirectMode("sometimes")
		draft.Upstreams = nil
		return nil
	})
	if err == nil {
		t.Fatal("Apply should reject an invalid redirect mode")
	}
	if rt.ProviderByName("abs") == nil {
		t.Fatal("a failed Apply removed a live upstream")
	}
	if rt.Config().Redirect.Mode != config.RedirectAlways {
		t.Fatalf("redirect mode = %q, want the original value", rt.Config().Redirect.Mode)
	}
	reloaded, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.UpstreamByName("abs") == nil {
		t.Fatal("a failed Apply rewrote the config file")
	}
}

func TestApplyPropagatesMutatorError(t *testing.T) {
	rt, _ := newRuntime(t)
	sentinel := errStub("nope")
	if err := rt.Apply(func(*config.Config) error { return sentinel }); err != sentinel {
		t.Fatalf("err = %v, want the mutator error", err)
	}
}

type errStub string

func (e errStub) Error() string { return string(e) }

func TestConfigReturnsADeepCopy(t *testing.T) {
	rt, _ := newRuntime(t)
	if err := rt.Apply(func(draft *config.Config) error {
		up := absUpstream("abs", "http://127.0.0.1:13378")
		up.StrmRoots = []string{"/NetDisk"}
		draft.Upstreams = append(draft.Upstreams, up)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	snapshot := rt.Config()
	snapshot.Upstreams[0].StrmRoots[0] = "/tampered"
	snapshot.Redirect.Mode = config.RedirectNever

	if rt.Config().Upstreams[0].StrmRoots[0] != "/NetDisk" {
		t.Fatal("Config() shares state with the live configuration")
	}
	if rt.Config().Redirect.Mode != config.RedirectAlways {
		t.Fatal("mutating the snapshot changed the live redirect mode")
	}
}

// Changing listen cannot rebind the socket, so the UI has to be told a restart
// is needed.
func TestRestartRequiredTracksListenChange(t *testing.T) {
	rt, _ := newRuntime(t)
	if rt.RestartRequired() {
		t.Fatal("restart should not be required initially")
	}
	if err := rt.Apply(func(draft *config.Config) error {
		draft.Server.Listen = ":6161"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !rt.RestartRequired() {
		t.Fatal("restart should be required after changing listen")
	}
	if rt.BootListen() != ":5151" {
		t.Fatalf("bootListen = %q", rt.BootListen())
	}
}

func TestServeHTTPWithoutUpstreamsIs404(t *testing.T) {
	rt, _ := newRuntime(t)
	recorder := httptest.NewRecorder()
	rt.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/items/x/file/1", nil))
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", recorder.Code)
	}
}

// RootUpstreamMounted 决定裸访问 / 是跳管理界面还是交给上游，因此要覆盖三态：
// 没有上游、上游挂在子路径、上游挂在根路径。
func TestRootUpstreamMounted(t *testing.T) {
	rt, _ := newRuntime(t)
	if rt.RootUpstreamMounted() {
		t.Fatal("a fresh runtime has no upstream on /")
	}

	if err := rt.Apply(func(draft *config.Config) error {
		up := absUpstream("abs", "http://127.0.0.1:13378")
		up.Prefix = "/read"
		draft.Upstreams = []config.Upstream{up}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if rt.RootUpstreamMounted() {
		t.Fatal("an upstream mounted on /read must not claim /")
	}

	if err := rt.Apply(func(draft *config.Config) error {
		draft.Upstreams = []config.Upstream{absUpstream("abs", "http://127.0.0.1:13378")}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !rt.RootUpstreamMounted() {
		t.Fatal("an upstream with the default prefix owns /")
	}

	// 停用的上游不再服务，根路径应当归还给管理界面。
	if err := rt.Apply(func(draft *config.Config) error {
		draft.Upstreams[0].Enabled = config.Bool(false)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if rt.RootUpstreamMounted() {
		t.Fatal("a disabled upstream must not keep claiming /")
	}
}

func TestDisabledUpstreamIsNotMounted(t *testing.T) {
	rt, _ := newRuntime(t)
	if err := rt.Apply(func(draft *config.Config) error {
		up := absUpstream("abs", "http://127.0.0.1:13378")
		up.Enabled = config.Bool(false)
		draft.Upstreams = append(draft.Upstreams, up)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if rt.ProviderByName("abs") != nil {
		t.Fatal("a disabled upstream must not be mounted")
	}
	if rt.Config().UpstreamByName("abs") == nil {
		t.Fatal("a disabled upstream must still be stored in the config")
	}
}
