package config

import (
	"os"
	"path/filepath"
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
    prefix: audiobooks/
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
	if cfg.Cache.TTL != 5*time.Minute {
		t.Fatalf("cache ttl = %v, want 5m", cfg.Cache.TTL)
	}
	upstream := cfg.Upstreams[0]
	if upstream.BaseURL != "http://10.0.0.31:13378" {
		t.Fatalf("base_url = %q, trailing slash should be trimmed", upstream.BaseURL)
	}
	if upstream.Prefix != "/audiobooks" {
		t.Fatalf("prefix = %q, want /audiobooks", upstream.Prefix)
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

func TestLoadRejectsInvalidValues(t *testing.T) {
	cases := map[string]string{
		"bad redirect mode": "redirect:\n  mode: sometimes\nupstreams:\n  - name: a\n    type: emby\n    base_url: http://x:1\n",
		"missing type":      "upstreams:\n  - name: a\n    base_url: http://x:1\n",
		"bad base url":      "upstreams:\n  - name: a\n    type: emby\n    base_url: x:1\n",
		"duplicate prefix":  "upstreams:\n  - name: a\n    type: emby\n    base_url: http://x:1\n  - name: b\n    type: emby\n    base_url: http://y:1\n",
		"duplicate name":    "upstreams:\n  - name: a\n    type: emby\n    base_url: http://x:1\n    prefix: /x\n  - name: a\n    type: emby\n    base_url: http://y:1\n    prefix: /y\n",
		"slash in name":     "upstreams:\n  - name: a/b\n    type: emby\n    base_url: http://x:1\n",
	}
	for name, contents := range cases {
		if _, err := Load(writeConfig(t, contents)); err == nil {
			t.Errorf("%s: Load should have failed", name)
		}
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
	cfg.Upstreams = []Upstream{{Name: "abs", Type: UpstreamAudiobookshelf, BaseURL: "http://x:1", StrmRoots: []string{"/a"}}}
	clone := cfg.Clone()
	clone.Upstreams[0].StrmRoots[0] = "/changed"
	clone.Redirect.ForwardUserAgent = Bool(false)
	if cfg.Upstreams[0].StrmRoots[0] != "/a" {
		t.Fatal("clone shares the StrmRoots backing array")
	}
	if !cfg.Redirect.ShouldForwardUserAgent() {
		t.Fatal("clone shares the ForwardUserAgent pointer")
	}
}

func TestEnvOverridesServerSettings(t *testing.T) {
	t.Setenv("AETHERLINK_ADMIN_TOKEN", "token-from-env")
	t.Setenv("AETHERLINK_LOG_LEVEL", "debug")
	cfg, err := Load(writeConfig(t, "upstreams:\n  - name: my-abs\n    type: audiobookshelf\n    base_url: http://x:1\n"))
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
