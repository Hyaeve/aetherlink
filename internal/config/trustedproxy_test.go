package config

import (
	"path/filepath"
	"testing"
)

func TestTrustedProxyConfigRoundTrip(t *testing.T) {
	cfg := Default()
	cfg.Redirect.TrustedProxyCIDRs = []string{"192.168.1.10/32", "fd00::10/128"}
	filename := filepath.Join(t.TempDir(), "config.yaml")
	if err := cfg.Save(filename); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(filename)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Redirect.TrustedProxyCIDRs) != 2 || loaded.Redirect.TrustedProxyCIDRs[0] != "192.168.1.10/32" {
		t.Fatalf("trusted proxies not preserved: %v", loaded.Redirect.TrustedProxyCIDRs)
	}
	clone := loaded.Clone()
	clone.Redirect.TrustedProxyCIDRs[0] = "10.0.0.1/32"
	if loaded.Redirect.TrustedProxyCIDRs[0] != "192.168.1.10/32" {
		t.Fatal("clone shares trusted proxy list")
	}
}
