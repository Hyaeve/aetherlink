package runtime

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

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
	return config.Upstream{
		Name:       name,
		Type:       config.UpstreamAudiobookshelf,
		BaseURL:    baseURL,
		APIKey:     "key",
		ListenPort: 13378,
	}
}

// newFakeUpstream 起一个最小的「媒体服务器」，只用来确认请求被原样反代过去。
func newFakeUpstream(t *testing.T) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Write([]byte("upstream:" + request.URL.Path))
	}))
	t.Cleanup(server.Close)
	return server.URL
}

// get 通过 AetherLink 的反代端口发一次请求，读回响应体。
func get(t *testing.T, port int, path string) string {
	t.Helper()
	client := &http.Client{Timeout: 5 * time.Second}
	response, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d%s", port, path))
	if err != nil {
		t.Fatalf("请求反代端口 %d 失败: %v", port, err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

// freePort 借一个空闲端口号后立刻放手，用来让测试绑定真实端口而不撞车。
func freePort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	return port
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

// 上游各占一个端口，因此新增上游必须当场开始监听，删除必须把端口放掉——
// 这是「原地址端口反代到另一个端口」的核心行为。
func TestApplyOpensAndClosesUpstreamPorts(t *testing.T) {
	backend := newFakeUpstream(t)
	rt, _ := newRuntime(t)
	if err := rt.Start(); err != nil {
		t.Fatal(err)
	}
	defer rt.Shutdown(context.Background())

	port := freePort(t)
	if err := rt.Apply(func(draft *config.Config) error {
		up := absUpstream("abs", backend)
		up.ListenPort = port
		draft.Upstreams = []config.Upstream{up}
		return nil
	}); err != nil {
		t.Fatalf("Apply returned error: %v", err)
	}
	if !rt.PortActive(port) {
		t.Fatalf("port %d should be listening right after the upstream was added", port)
	}
	if body := get(t, port, "/library"); body != "upstream:/library" {
		t.Fatalf("body = %q, want the request proxied verbatim", body)
	}

	if err := rt.Apply(func(draft *config.Config) error {
		draft.Upstreams = nil
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if rt.PortActive(port) {
		t.Fatalf("port %d should have been released when the upstream was removed", port)
	}
}

// 停用上游等于关掉它的端口，但配置里要留着，方便随后再启用。
func TestDisablingUpstreamClosesItsPort(t *testing.T) {
	backend := newFakeUpstream(t)
	rt, _ := newRuntime(t)
	if err := rt.Start(); err != nil {
		t.Fatal(err)
	}
	defer rt.Shutdown(context.Background())

	port := freePort(t)
	if err := rt.Apply(func(draft *config.Config) error {
		up := absUpstream("abs", backend)
		up.ListenPort = port
		draft.Upstreams = []config.Upstream{up}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := rt.Apply(func(draft *config.Config) error {
		draft.Upstreams[0].Enabled = config.Bool(false)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if rt.PortActive(port) {
		t.Fatal("a disabled upstream must not keep its port bound")
	}
	if rt.Config().UpstreamByName("abs") == nil {
		t.Fatal("a disabled upstream must still be stored in the config")
	}
}

// 端口被别的程序占着时，Apply 必须整体失败：既不落盘，也不动正在服务的上游。
func TestApplyRejectsPortAlreadyInUse(t *testing.T) {
	backend := newFakeUpstream(t)
	rt, path := newRuntime(t)
	if err := rt.Start(); err != nil {
		t.Fatal(err)
	}
	defer rt.Shutdown(context.Background())

	// 占用整个网卡（":0"）而不是只占 127.0.0.1：Windows 允许 0.0.0.0 与
	// 127.0.0.1 同端口共存，只有同样绑定全部地址才能稳定复现冲突。
	squatter, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatal(err)
	}
	defer squatter.Close()
	taken := squatter.Addr().(*net.TCPAddr).Port

	if err := rt.Apply(func(draft *config.Config) error {
		up := absUpstream("abs", backend)
		up.ListenPort = taken
		draft.Upstreams = []config.Upstream{up}
		return nil
	}); err == nil {
		t.Fatal("Apply should fail when the port is already in use")
	}
	if rt.Config().UpstreamByName("abs") != nil {
		t.Fatal("a failed Apply added the upstream anyway")
	}
	reloaded, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(reloaded.Upstreams) != 0 {
		t.Fatal("a failed Apply wrote the upstream to disk")
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
