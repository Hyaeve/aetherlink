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

// ignore_direct_play_verdict 是已废弃的开关（那个开关的作用只剩「关掉之后让一部分
// 客户端播不了」，于是连同界面一起删了）。字段声明必须留着：配置是严格解析
// （decoder.KnownFields(true)），删掉它会让所有已经写过这一项的配置直接起不来。
// 所以 Load 要能把它读进来，migrate 要把它清掉，保存后磁盘上不再出现这一项。
func TestDeprecatedIgnoreDirectPlayVerdictStillLoadsThenDisappears(t *testing.T) {
	path := writeConfig(t, `
upstreams:
  - name: fnos
    type: fnos
    base_url: "http://127.0.0.1:8005"
    listen_port: 15154
    ignore_direct_play_verdict: false
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("旧配置含已废弃的 ignore_direct_play_verdict 时必须仍能加载: %v", err)
	}
	if !cfg.Migrated() {
		t.Fatal("读到已废弃字段后应标记为已迁移，否则它清不掉")
	}
	if cfg.Upstreams[0].IgnoreDirectPlayVerdict != nil {
		t.Fatal("已废弃的取值必须被清空，不能留在内存里")
	}
	if err := cfg.Save(path); err != nil {
		t.Fatalf("Save returned error: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读回配置失败: %v", err)
	}
	if strings.Contains(string(raw), "ignore_direct_play_verdict") {
		t.Fatalf("已废弃字段不该再被写回磁盘:\n%s", raw)
	}
	if _, err := Load(path); err != nil {
		t.Fatalf("清过之后的配置仍应能加载: %v", err)
	}
}

// relay_exempt_user_agents（卡片级「不中继的客户端」名单）与判定一起删掉了，
// 但字段声明必须留着：配置是严格解析（decoder.KnownFields(true)），删掉它会让
// 所有已经写过这一项的配置直接起不来。所以 Load 要能把它读进来，migrate 要把它
// 清掉，保存后磁盘上不再出现这一项。
func TestDeprecatedRelayExemptListStillLoadsThenDisappears(t *testing.T) {
	path := writeConfig(t, `
upstreams:
  - name: fnos
    type: fnos
    base_url: "http://127.0.0.1:8005"
    listen_port: 15154
    relay_exempt_user_agents:
      - AfuseKt
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("旧配置含已废弃的 relay_exempt_user_agents 时必须仍能加载: %v", err)
	}
	if !cfg.Migrated() {
		t.Fatal("读到已废弃字段后应标记为已迁移，否则它清不掉")
	}
	if len(cfg.Upstreams[0].RelayExemptUserAgents) != 0 {
		t.Fatalf("已废弃的名单必须被清空，不能留在内存里：%v", cfg.Upstreams[0].RelayExemptUserAgents)
	}
	if err := cfg.Save(path); err != nil {
		t.Fatalf("Save returned error: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读回配置失败: %v", err)
	}
	if strings.Contains(string(raw), "relay_exempt_user_agents") {
		t.Fatalf("已废弃字段不该再被写回磁盘:\n%s", raw)
	}
	if _, err := Load(path); err != nil {
		t.Fatalf("清过之后的配置仍应能加载: %v", err)
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

func TestProviderBlockedClientUserAgent(t *testing.T) {
	redirect := Redirect{
		BlockClientUserAgent:            Bool(true),
		BlockedUserAgentsEmby:           []string{"Infuse"},
		BlockedUserAgentsAudiobookshelf: []string{"Komic-iOS"},
	}
	if !redirect.IsBlockedClientUserAgentFor(UpstreamEmby, "Infuse/8.0") {
		t.Fatal("Emby keyword should match when it appears in the User-Agent")
	}
	if !redirect.IsBlockedClientUserAgentFor(UpstreamEmby, "X/Infuse/8.0") {
		t.Fatal("Emby keyword should match within a User-Agent")
	}
	literalRedirect := redirect
	literalRedirect.BlockedUserAgentsEmby = []string{"/Infuse/"}
	if literalRedirect.IsBlockedClientUserAgentFor(UpstreamEmby, "Infuse/8.0") {
		t.Fatal("slash-wrapped values should not be stripped automatically")
	}
	if redirect.IsBlockedClientUserAgentFor(UpstreamAudiobookshelf, "Infuse/8.0") {
		t.Fatal("Emby fragment should not match Audiobookshelf")
	}
	if !redirect.IsBlockedClientUserAgentFor(UpstreamAudiobookshelf, "Komic-iOS/1.0") {
		t.Fatal("Audiobookshelf fragment should match")
	}
}

func TestProviderBlockedClientUserAgentScopesToSelectedUpstream(t *testing.T) {
	redirect := Redirect{
		BlockClientUserAgentEmby:       Bool(true),
		BlockedUserAgentsEmby:          []string{"Infuse"},
		BlockedUserAgentsEmbyUpstreams: []string{"客厅 Emby"},
	}
	if !redirect.IsBlockedClientUserAgentForUpstream(UpstreamEmby, "客厅 Emby", "Infuse/8.0") {
		t.Fatal("selected Emby upstream should block matching UA")
	}
	if redirect.IsBlockedClientUserAgentForUpstream(UpstreamEmby, "卧室 Emby", "Infuse/8.0") {
		t.Fatal("unselected Emby upstream should not block matching UA")
	}
	if redirect.IsBlockedClientUserAgentForUpstream(UpstreamAudiobookshelf, "客厅 Emby", "Infuse/8.0") {
		t.Fatal("provider switch should keep Emby rules out of ABS")
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

// AETHERLINK_PORT 是给容器用的简写：不填就沿用配置文件里的监听端口，
// 填了就改成容器内监听这个端口，方便 compose 里同一个变量既改监听又改映射。
func TestEnvOverrideListenPort(t *testing.T) {
	t.Setenv("AETHERLINK_PORT", "8080")
	cfg, err := Load(writeConfig(t, "upstreams: []\n"))
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if cfg.Server.Listen != ":8080" {
		t.Fatalf("listen = %q, want :8080", cfg.Server.Listen)
	}
	if port := PortOf(cfg.Server.Listen); port != 8080 {
		t.Fatalf("admin port = %d, want 8080", port)
	}
}

func TestEnvOverrideListenPortKeepsFileValueWhenUnset(t *testing.T) {
	t.Setenv("AETHERLINK_PORT", "")
	t.Setenv("AETHERLINK_LISTEN", "")
	cfg, err := Load(writeConfig(t, "server:\n  listen: \":9000\"\nupstreams: []\n"))
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if cfg.Server.Listen != ":9000" {
		t.Fatalf("listen = %q, want the file value :9000", cfg.Server.Listen)
	}
}

// AETHERLINK_LISTEN 能写完整地址，比只写端口号的 AETHERLINK_PORT 更具体，
// 两者同时存在时以它为准。
func TestEnvOverrideListenAddressWinsOverPort(t *testing.T) {
	t.Setenv("AETHERLINK_PORT", "8080")
	t.Setenv("AETHERLINK_LISTEN", "127.0.0.1:9090")
	cfg, err := Load(writeConfig(t, "upstreams: []\n"))
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if cfg.Server.Listen != "127.0.0.1:9090" {
		t.Fatalf("listen = %q, want 127.0.0.1:9090", cfg.Server.Listen)
	}
}

func TestEnvOverrideRejectsBadListenPort(t *testing.T) {
	for _, value := range []string{"abc", "0", "65536", "-1"} {
		t.Setenv("AETHERLINK_PORT", value)
		if _, err := Load(writeConfig(t, "upstreams: []\n")); err == nil {
			t.Fatalf("AETHERLINK_PORT=%q 应当报错而不是被静默忽略", value)
		}
	}
}

// 环境变量是「启动期覆盖」：界面保存配置时不能把监听端口顺带固化到磁盘，
// 否则删掉 AETHERLINK_PORT 之后端口会被永久改掉，compose 映射的 5151 进不去。
func TestEnvOverrideListenPortIsNotPersisted(t *testing.T) {
	t.Setenv("AETHERLINK_PORT", "8080")
	path := writeConfig(t, "server:\n  listen: \":9000\"\nupstreams: []\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if cfg.Server.Listen != ":8080" {
		t.Fatalf("生效的 listen = %q, want :8080", cfg.Server.Listen)
	}
	if err := cfg.Clone().Save(path); err != nil {
		t.Fatalf("Save returned error: %v", err)
	}
	t.Setenv("AETHERLINK_PORT", "")
	reloaded, err := Load(path)
	if err != nil {
		t.Fatalf("重新加载失败: %v", err)
	}
	if reloaded.Server.Listen != ":9000" {
		t.Fatalf("写回的 listen = %q, want 文件里原本的 :9000", reloaded.Server.Listen)
	}
}

// 应急令牌同理：它只是临时注入的绕过口令，不能因为保存一次配置就永久留盘。
func TestEnvOverrideAdminTokenIsNotPersisted(t *testing.T) {
	t.Setenv("AETHERLINK_ADMIN_TOKEN", "token-from-env")
	path := writeConfig(t, "upstreams: []\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if cfg.Server.AdminToken != "token-from-env" {
		t.Fatalf("生效的 admin token = %q", cfg.Server.AdminToken)
	}
	if err := cfg.Save(path); err != nil {
		t.Fatalf("Save returned error: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "token-from-env") {
		t.Fatalf("应急令牌被写进了配置文件：\n%s", raw)
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
