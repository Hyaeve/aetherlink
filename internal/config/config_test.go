package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeConfig(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadAppliesDefaultsAndNormalizes(t *testing.T) {
	path := writeConfig(t, `
server:
  listen: ":9000"
upstreams:
  - name: abs
    type: audiobookshelf
    base_url: "http://10.0.0.31:13378/"
    api_key: secret
    listen_port: 15152
    strm_roots:
      - "/NetDisk/"
    path_mappings:
      - from: "/audiobooks/"
        to: "/NetDisk/115-Strm/"
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if cfg.Server.Listen != ":9000" {
		t.Fatalf("listen = %q", cfg.Server.Listen)
	}
	// Defaults must survive a partial config document.
	if cfg.Redirect.Mode != RedirectAlways {
		t.Fatalf("redirect mode = %q, want always", cfg.Redirect.Mode)
	}
	if !cfg.Redirect.ShouldForwardUserAgent() || !cfg.Redirect.PublicTargetsAllowed() {
		t.Fatal("optional booleans must keep their true defaults when the key is absent")
	}
	if cfg.Redirect.ShouldBlockClientUserAgent() {
		t.Fatal("client User-Agent blocking should be disabled by default")
	}
	if cfg.Cache.TTL != 2*time.Hour {
		t.Fatalf("cache ttl = %v, want 2h", cfg.Cache.TTL)
	}
	if cfg.Server.LogBuffer != 5000 {
		t.Fatalf("log buffer = %d, want 5000", cfg.Server.LogBuffer)
	}
	upstream := cfg.Upstreams[0]
	if upstream.BaseURL != "http://10.0.0.31:13378" {
		t.Fatalf("base_url = %q, trailing slash should be trimmed", upstream.BaseURL)
	}
	if upstream.ListenPort != 15152 {
		t.Fatalf("listen_port = %d, want 15152", upstream.ListenPort)
	}
	if upstream.StrmRoots[0] != "/NetDisk" {
		t.Fatalf("strm root = %q", upstream.StrmRoots[0])
	}
	if upstream.PathMappings[0].To != "/NetDisk/115-Strm" {
		t.Fatalf("mapping to = %q", upstream.PathMappings[0].To)
	}
	if !upstream.IsEnabled() {
		t.Fatal("upstream should default to enabled")
	}
}

func TestLoadKeepsExplicitFalseBooleans(t *testing.T) {
	path := writeConfig(t, "redirect:\n  forward_user_agent: false\n  allow_public_targets: false\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if cfg.Redirect.ShouldForwardUserAgent() {
		t.Fatal("forward_user_agent: false must survive the merge")
	}
	if cfg.Redirect.PublicTargetsAllowed() {
		t.Fatal("allow_public_targets: false must survive the merge")
	}
}

func TestBlockedClientUserAgentMatchesCaseInsensitiveFragments(t *testing.T) {
	cfg := Default()
	cfg.Redirect.BlockClientUserAgent = Bool(true)
	cfg.Redirect.BlockedUserAgents = []string{"Forward", "Infuse-Library"}

	if !cfg.Redirect.IsBlockedClientUserAgent("Player/Forward/1.0") {
		t.Fatal("Forward fragment should be blocked")
	}
	if !cfg.Redirect.IsBlockedClientUserAgent("infuse-library/8.0") {
		t.Fatal("matching should be case insensitive")
	}
	if cfg.Redirect.IsBlockedClientUserAgent("Emby/4.8") {
		t.Fatal("unlisted User-Agent should not be blocked")
	}
}

func TestLoadRejectsInvalidValues(t *testing.T) {
	cases := map[string]string{
		"bad redirect mode": "redirect:\n  mode: sometimes\nupstreams:\n  - name: a\n    type: emby\n    base_url: http://x:1\n    listen_port: 8096\n",
		"missing type":      "upstreams:\n  - name: a\n    base_url: http://x:1\n    listen_port: 8096\n",
		"bad base url":      "upstreams:\n  - name: a\n    type: emby\n    base_url: x:1\n    listen_port: 8096\n",
		"port out of range": "upstreams:\n  - name: a\n    type: emby\n    base_url: http://x:1\n    listen_port: 70000\n",
		"duplicate port":    "upstreams:\n  - name: a\n    type: emby\n    base_url: http://x:1\n    listen_port: 8096\n  - name: b\n    type: emby\n    base_url: http://y:1\n    listen_port: 8096\n",
		"admin port taken":  "upstreams:\n  - name: a\n    type: emby\n    base_url: http://x:1\n    listen_port: 5151\n",
		"duplicate name":    "upstreams:\n  - name: a\n    type: emby\n    base_url: http://x:1\n    listen_port: 8096\n  - name: a\n    type: emby\n    base_url: http://y:1\n    listen_port: 8097\n",
		"slash in name":     "upstreams:\n  - name: a/b\n    type: emby\n    base_url: http://x:1\n    listen_port: 8096\n",
	}
	for name, contents := range cases {
		if _, err := Load(writeConfig(t, contents)); err == nil {
			t.Errorf("%s: Load should have failed", name)
		}
	}
}

// 手写配置漏了 listen_port 时不该让容器起不来：加载阶段会补一个空闲端口。
// 但从管理界面提交的草稿必须显式带端口，否则 Validate 要拦下来。
func TestValidateRequiresListenPort(t *testing.T) {
	cfg := Default()
	cfg.Upstreams = []Upstream{{Name: "a", Type: UpstreamEmby, BaseURL: "http://x:1"}}
	if err := cfg.Validate(); err == nil {
		t.Fatal("Validate should reject an upstream without a listen port")
	}
}

func TestLoadFillsMissingListenPort(t *testing.T) {
	path := writeConfig(t, "upstreams:\n  - name: a\n    type: emby\n    base_url: http://x:1\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load should assign a port instead of failing: %v", err)
	}
	if cfg.Upstreams[0].ListenPort != 5152 {
		t.Fatalf("listen_port = %d, want 5152", cfg.Upstreams[0].ListenPort)
	}
}

// A freshly bootstrapped instance has no upstreams and no password yet; that is
// a valid state, not a config error.
func TestLoadAcceptsEmptyDocument(t *testing.T) {
	cfg, err := Load(writeConfig(t, ""))
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if len(cfg.Upstreams) != 0 {
		t.Fatalf("upstreams = %d, want 0", len(cfg.Upstreams))
	}
	if cfg.Auth.IsConfigured() {
		t.Fatal("auth should be unconfigured")
	}
	if cfg.Server.Listen != ":5151" {
		t.Fatalf("listen = %q, want :5151", cfg.Server.Listen)
	}
}

func TestLoadRejectsUnknownFields(t *testing.T) {
	// Typos in a config file should fail loudly instead of silently doing nothing.
	path := writeConfig(t, "server:\n  listten: \":9000\"\n")
	if _, err := Load(path); err == nil {
		t.Fatal("Load should reject unknown fields")
	}
}

func TestLoadMigratesLegacyPrefixToListenPort(t *testing.T) {
	// 旧版本用 prefix 区分上游，升级后这份配置必须仍能加载，
	// 否则容器会卡在「解析配置失败」的重启循环里。
	path := writeConfig(t, `
server:
  listen: ":5151"
upstreams:
  - name: abs
    type: audiobookshelf
    base_url: "http://10.0.0.31:13378"
    api_key: secret
    prefix: "/"
  - name: emby
    type: emby
    base_url: "http://10.0.0.31:8096"
    api_key: secret
    prefix: "/emby"
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load should accept a legacy config: %v", err)
	}
	if !cfg.Migrated() {
		t.Fatal("Migrated should report that the legacy fields were rewritten")
	}
	if cfg.Upstreams[0].ListenPort != 5152 || cfg.Upstreams[1].ListenPort != 5153 {
		t.Fatalf("ports = %d, %d; want 5152, 5153", cfg.Upstreams[0].ListenPort, cfg.Upstreams[1].ListenPort)
	}
	for _, upstream := range cfg.Upstreams {
		if upstream.Prefix != "" {
			t.Fatalf("prefix should be cleared, got %q", upstream.Prefix)
		}
	}

	// 迁移结果落盘后不得再包含已废弃的 prefix。
	if err := cfg.Save(path); err != nil {
		t.Fatalf("Save returned error: %v", err)
	}
	saved, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(saved), "prefix:") {
		t.Fatalf("saved config still carries prefix:\n%s", saved)
	}
	reloaded, err := Load(path)
	if err != nil {
		t.Fatalf("reload returned error: %v", err)
	}
	if reloaded.Migrated() {
		t.Fatal("a migrated config should not need migrating again")
	}
}

func TestLoadKeepsExplicitListenPortDuringMigration(t *testing.T) {
	path := writeConfig(t, `
server:
  listen: ":5151"
upstreams:
  - name: abs
    type: audiobookshelf
    base_url: "http://10.0.0.31:13378"
    api_key: secret
    prefix: "/"
    listen_port: 5160
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if cfg.Upstreams[0].ListenPort != 5160 {
		t.Fatalf("listen_port = %d, want the explicit 5160", cfg.Upstreams[0].ListenPort)
	}
}

func TestLoadOrCreateWritesDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "config.yaml")
	cfg, created, err := LoadOrCreate(path)
	if err != nil {
		t.Fatalf("LoadOrCreate returned error: %v", err)
	}
	if !created {
		t.Fatal("created should be true for a missing file")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("config file was not written: %v", err)
	}
	if cfg.Path() != path {
		t.Fatalf("path = %q", cfg.Path())
	}

	// The written file must reload cleanly, which is what a container restart does.
	reloaded, created, err := LoadOrCreate(path)
	if err != nil {
		t.Fatalf("reload returned error: %v", err)
	}
	if created {
		t.Fatal("created should be false on reload")
	}
	if reloaded.Server.Listen != cfg.Server.Listen {
		t.Fatalf("listen changed across reload: %q vs %q", reloaded.Server.Listen, cfg.Server.Listen)
	}
}

func TestSaveRoundTripsUpstreamsAndAuth(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	cfg := Default()
	cfg.SetPath(path)
	cfg.Auth = Auth{Algorithm: "pbkdf2-sha256", Iterations: 4096, Salt: "c2FsdA", PasswordHash: "aGFzaA"}
	cfg.Upstreams = []Upstream{{
		Name:         "abs",
		Type:         UpstreamAudiobookshelf,
		BaseURL:      "http://10.0.0.31:13378",
		APIKey:       "jwt-key",
		ListenPort:   13378,
		StrmRoots:    []string{"/NetDisk"},
		PathMappings: []PathMapping{{From: "/audiobooks", To: "/NetDisk/115-Strm/Set/Read"}},
	}}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate returned error: %v", err)
	}
	if err := cfg.Save(""); err != nil {
		t.Fatalf("Save returned error: %v", err)
	}

	reloaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if !reloaded.Auth.IsConfigured() || reloaded.Auth.PasswordHash != "aGFzaA" {
		t.Fatalf("auth did not round trip: %+v", reloaded.Auth)
	}
	if len(reloaded.Upstreams) != 1 || reloaded.Upstreams[0].APIKey != "jwt-key" {
		t.Fatalf("upstream did not round trip: %+v", reloaded.Upstreams)
	}
}

func TestCloneIsDeep(t *testing.T) {
	cfg := Default()
	cfg.Upstreams = []Upstream{{Name: "abs", Type: UpstreamAudiobookshelf, BaseURL: "http://x:1", ListenPort: 8096, StrmRoots: []string{"/a"}}}
	clone := cfg.Clone()
	clone.Upstreams[0].StrmRoots[0] = "/changed"
	clone.Redirect.ForwardUserAgent = Bool(false)
	clone.Redirect.BlockedUserAgents = []string{"Infuse"}
	if cfg.Upstreams[0].StrmRoots[0] != "/a" {
		t.Fatal("clone shares the StrmRoots backing array")
	}
	if !cfg.Redirect.ShouldForwardUserAgent() {
		t.Fatal("clone shares the ForwardUserAgent pointer")
	}
	if len(cfg.Redirect.BlockedUserAgents) != 0 {
		t.Fatal("clone shares the BlockedUserAgents backing array")
	}
}

func TestEnvOverridesServerSettings(t *testing.T) {
	t.Setenv("AETHERLINK_ADMIN_TOKEN", "token-from-env")
	t.Setenv("AETHERLINK_LOG_LEVEL", "debug")
	cfg, err := Load(writeConfig(t, "upstreams:\n  - name: my-abs\n    type: audiobookshelf\n    base_url: http://x:1\n    listen_port: 13378\n"))
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if cfg.Server.AdminToken != "token-from-env" {
		t.Fatalf("admin token = %q", cfg.Server.AdminToken)
	}
	if cfg.Server.LogLevel != "debug" {
		t.Fatalf("log level = %q", cfg.Server.LogLevel)
	}
}

// 反代端口是每个上游的唯一入口，界面新增上游时要自动挑一个不冲突的号。
func TestSuggestPortSkipsAdminAndTakenPorts(t *testing.T) {
	cfg := Default()
	first := cfg.SuggestPort()
	if first != 5152 {
		t.Fatalf("first suggestion = %d, want 5152 (管理端口 5151 之后的第一个)", first)
	}

	cfg.Upstreams = []Upstream{
		{Name: "abs", Type: UpstreamAudiobookshelf, BaseURL: "http://x:1", ListenPort: 5152},
		{Name: "emby", Type: UpstreamEmby, BaseURL: "http://y:1", ListenPort: 5153},
	}
	if got := cfg.SuggestPort(); got != 5154 {
		t.Fatalf("suggestion = %d, want the first free port after the taken ones", got)
	}
	if cfg.UpstreamByPort(5153).Name != "emby" {
		t.Fatal("UpstreamByPort did not find the upstream owning 5153")
	}
	if cfg.UpstreamByPort(9999) != nil {
		t.Fatal("UpstreamByPort must return nil for a free port")
	}
}

func TestPortOfParsesListenAddress(t *testing.T) {
	cases := map[string]int{":5151": 5151, "0.0.0.0:5151": 5151, "127.0.0.1:80": 80, "": 0, "nope": 0}
	for listen, want := range cases {
		if got := PortOf(listen); got != want {
			t.Errorf("PortOf(%q) = %d, want %d", listen, got, want)
		}
	}
}
