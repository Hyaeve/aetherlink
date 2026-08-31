package resolver

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/aetherlink/aetherlink/internal/config"
	"github.com/aetherlink/aetherlink/internal/strm"
	"github.com/aetherlink/aetherlink/internal/upstream"
)

func TestDirectURLTTLUsesEarliestTExpiry(t *testing.T) {
	finalExpiry := time.Now().Add(90 * time.Second).Unix()
	originalExpiry := time.Now().Add(30 * time.Second).Unix()
	resolution := &Resolution{
		Target:   &strm.Target{Type: strm.TargetRemote, URL: fmt.Sprintf("https://origin.example/book.m4a?t=%d", originalExpiry)},
		FinalURL: fmt.Sprintf("https://cdn.example/book.m4a?t=%d", finalExpiry),
	}

	ttl, ok := directURLTTL(resolution)
	if !ok {
		t.Fatal("direct URL t expiry was not detected")
	}
	if ttl < 20*time.Second || ttl > 31*time.Second {
		t.Fatalf("ttl = %v, want about 30s", ttl)
	}
}

func TestDirectURLTTLAcceptsUnixMilliseconds(t *testing.T) {
	expiry := time.Now().Add(45 * time.Second)
	resolution := &Resolution{
		Target: &strm.Target{
			Type: strm.TargetRemote,
			URL:  fmt.Sprintf("https://cdn.example/book.m4a?t=%d", expiry.UnixMilli()),
		},
	}

	ttl, ok := directURLTTL(resolution)
	if !ok {
		t.Fatal("millisecond t expiry was not detected")
	}
	if ttl < 35*time.Second || ttl > 46*time.Second {
		t.Fatalf("ttl = %v, want about 45s", ttl)
	}
}

func TestDirectURLTTLDoesNotFallbackWhenTExpired(t *testing.T) {
	resolution := &Resolution{
		Target: &strm.Target{
			Type: strm.TargetRemote,
			URL:  fmt.Sprintf("https://cdn.example/book.m4a?t=%d", time.Now().Add(-time.Minute).Unix()),
		},
	}

	ttl, ok := directURLTTL(resolution)
	if !ok || ttl != 0 {
		t.Fatalf("ttl = %v, ok = %v, want expired cache entry", ttl, ok)
	}
}

func TestDirectURLTTLFallsBackForNonExpiryT(t *testing.T) {
	resolution := &Resolution{
		Target: &strm.Target{
			Type: strm.TargetRemote,
			URL:  "https://cdn.example/book.m4a?t=not-an-expiry",
		},
	}

	if ttl, ok := directURLTTL(resolution); ok || ttl != 0 {
		t.Fatalf("ttl = %v, ok = %v, want fallback for invalid t", ttl, ok)
	}
}

func TestCacheEntryUsesPerEntryTTL(t *testing.T) {
	cache := newLRUCache(time.Hour, 4)
	cache.put("short", &Resolution{}, 20*time.Millisecond)
	time.Sleep(30 * time.Millisecond)
	if _, _, ok := cache.get("short"); ok {
		t.Fatal("short-lived cache entry did not expire")
	}
}

func TestPersistentCacheSurvivesReopen(t *testing.T) {
	cachePath := filepath.Join(t.TempDir(), "direct-links-cache.json")
	value := &Resolution{
		Target: &strm.Target{Type: strm.TargetRemote, URL: "https://cdn.example/book.m4a"},
	}
	first := newPersistentLRUCache(time.Hour, 4, cachePath)
	first.put("provider:item:ua", value, time.Hour)

	second := newPersistentLRUCache(time.Hour, 4, cachePath)
	got, _, ok := second.get("provider:item:ua")
	if !ok || got.PlayURL() != value.PlayURL() {
		t.Fatalf("persistent cache entry was not restored: got=%+v ok=%v", got, ok)
	}
}

func TestEmbyCacheFallbackIsFixedAtTwoHours(t *testing.T) {
	provider, err := upstream.New(config.Upstream{
		Name:    "emby",
		Type:    config.UpstreamEmby,
		BaseURL: "http://127.0.0.1:8096",
		APIKey:  "test-key",
	})
	if err != nil {
		t.Fatal(err)
	}
	resolver := New(config.Cache{TTL: 5 * time.Hour, MaxSize: 4}, config.Redirect{})
	if got := resolver.cacheTTL(provider); got != 2*time.Hour {
		t.Fatalf("Emby fallback cache ttl = %v, want 2h", got)
	}
}

func TestBlockedClientUserAgentUsesFallback(t *testing.T) {
	resolver := New(config.Cache{TTL: time.Hour, MaxSize: 4}, config.Redirect{
		ForwardUserAgent:     config.Bool(true),
		FallbackUserAgent:    "AetherLink",
		BlockClientUserAgent: config.Bool(true),
		BlockedUserAgents:    []string{"Infuse-Direct"},
	})
	if got := resolver.EffectiveUserAgent("Infuse-Direct/8.0"); got != "AetherLink" {
		t.Fatalf("blocked User-Agent = %q, want AetherLink", got)
	}
	if got := resolver.EffectiveUserAgent("Emby/4.8"); got != "Emby/4.8" {
		t.Fatalf("unblocked User-Agent = %q, want client value", got)
	}
}
