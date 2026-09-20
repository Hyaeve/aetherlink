package resolver

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aetherlink/aetherlink/internal/config"
	"github.com/aetherlink/aetherlink/internal/upstream"
)

type scopedTestProvider struct {
	upstream.Provider
	target string
	calls  atomic.Int32
}

func (provider *scopedTestProvider) MediaTarget(ctx context.Context, ref upstream.MediaRef) (upstream.MediaTarget, error) {
	provider.calls.Add(1)
	return upstream.MediaTarget{URL: provider.target}, nil
}

func TestResolvedCacheIsolatesServerAndPlayer(t *testing.T) {
	newProvider := func(address, target string) *scopedTestProvider {
		base, err := upstream.New(config.Upstream{Name: "same-card", Type: config.UpstreamFnos, BaseURL: address})
		if err != nil {
			t.Fatal(err)
		}
		return &scopedTestProvider{Provider: base, target: target}
	}
	cachePath := filepath.Join(t.TempDir(), "cache.json")
	cacheConfig := config.Cache{TTL: time.Hour, MaxSize: 20}
	mediaResolver := NewWithPersistence(cacheConfig, config.Redirect{}, cachePath)
	ref := upstream.MediaRef{Kind: upstream.RefStream, ItemID: "same-item"}
	first := newProvider("http://first.test", "https://cdn.example/first.mkv")
	second := newProvider("http://second.test", "https://cdn.example/second.mkv")
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.Header.Set("X-Emby-Token", "player-a")
	ctx := upstream.WithClientCredentials(context.Background(), request)
	if _, source, _, err := mediaResolver.ResolveWithSource(ctx, first, ref, "Player/A"); err != nil || source != CacheSourceMiss {
		t.Fatalf("first resolve source=%s err=%v", source, err)
	}
	mediaResolver = NewWithPersistence(cacheConfig, config.Redirect{}, cachePath)
	if _, source, _, err := mediaResolver.ResolveWithSource(ctx, first, ref, "Player/A"); err != nil || source != CacheSourceRestored {
		t.Fatalf("restored source=%s err=%v", source, err)
	}
	if resolution, source, _, err := mediaResolver.ResolveWithSource(ctx, second, ref, "Player/A"); err != nil || source != CacheSourceMiss || resolution.PlayURL() != second.target {
		t.Fatalf("changed server reused old cache: resolution=%+v source=%s err=%v", resolution, source, err)
	}
	request.Header.Set("X-Emby-Token", "player-b")
	otherCtx := upstream.WithClientCredentials(context.Background(), request)
	if _, source, _, err := mediaResolver.ResolveWithSource(otherCtx, first, ref, "Player/A"); err != nil || source != CacheSourceMiss {
		t.Fatalf("changed player source=%s err=%v", source, err)
	}
	if _, source, _, err := mediaResolver.ResolveWithSource(ctx, first, ref, "Player/B"); err != nil || source != CacheSourceMiss {
		t.Fatalf("changed UA source=%s err=%v", source, err)
	}
}

func TestEmptyUserAgentUsesSameNonemptyProbeAgent(t *testing.T) {
	var probeAgent string
	backend := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		probeAgent = request.UserAgent()
		writer.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()
	base, err := upstream.New(config.Upstream{Name: "empty-ua", Type: config.UpstreamFnos, BaseURL: "http://upstream.test"})
	if err != nil {
		t.Fatal(err)
	}
	provider := &scopedTestProvider{Provider: base, target: backend.URL + "/audio.m4a"}
	mediaResolver := New(config.Cache{TTL: time.Hour, MaxSize: 10}, config.Redirect{FollowUpstreamRedirects: true, MaxFollowHops: 3, FallbackUserAgent: "FallbackPlayer"})
	if _, _, _, err := mediaResolver.ResolveWithSource(context.Background(), provider, upstream.MediaRef{ItemID: "item"}, " "); err != nil {
		t.Fatal(err)
	}
	if probeAgent != "FallbackPlayer" {
		t.Fatalf("probe agent=%q", probeAgent)
	}
}
