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
	// 写盘现在是后台做的：重开之前得先关掉这一个，把还没落盘的改动补写出去。
	// 不关就重开，读到的是旧文件，缓存会「凭空少一条」而不是报错。
	mediaResolver.Close()
	mediaResolver = NewWithPersistence(cacheConfig, config.Redirect{}, cachePath)
	defer mediaResolver.Close()
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

// 移动云盘（cmecloud.cn）的直链缓存**不绑定 UA，也不绑定播放器身份**（用户
// 2026-09-22 明确）：同一个文件在缓存期内被另一个播放器请求，直接把已解析出的
// 直链给它，不再回头问上游 —— 这类直链的有效期只有 15 分钟，回头问一次就多一分
// 起播延迟，而地址本身对谁都一样。
//
// 这条**只对这一档开放**，对照写在同一用例里：换成普通直链，同样的「换 UA 换令牌」
// 必须仍然是未命中。别把共享键扩大成「所有直链都不看 UA」—— 别的网盘的直链可能
// 与 UA 绑定，跨 UA 复用会把一条只有某个播放器才播得动的地址发给别人。
func TestCmecloudDirectLinkIsSharedAcrossUserAgents(t *testing.T) {
	newProvider := func(name, target string) *scopedTestProvider {
		base, err := upstream.New(config.Upstream{Name: name, Type: config.UpstreamFnos, BaseURL: "http://" + name + ".test"})
		if err != nil {
			t.Fatal(err)
		}
		return &scopedTestProvider{Provider: base, target: target}
	}
	// 换一个播放器：UA 与令牌一起换掉，两者都进完整键。
	asPlayer := func(token, userAgent string) (context.Context, string) {
		request := httptest.NewRequest(http.MethodGet, "/", nil)
		request.Header.Set("X-Emby-Token", token)
		return upstream.WithClientCredentials(context.Background(), request), userAgent
	}

	mediaResolver := New(config.Cache{TTL: time.Hour, MaxSize: 20}, config.Redirect{})
	ref := upstream.MediaRef{Kind: upstream.RefStream, ItemID: "movie"}
	firstCtx, firstAgent := asPlayer("player-a", "Infuse/8.0")
	secondCtx, secondAgent := asPlayer("player-b", "VidHub/2.0")

	cmecloud := newProvider("cmecloud", "https://dl.cmecloud.cn/abc/movie.mkv")
	if _, source, _, err := mediaResolver.ResolveWithSource(firstCtx, cmecloud, ref, firstAgent); err != nil || source != CacheSourceMiss {
		t.Fatalf("第一个播放器 source=%s err=%v", source, err)
	}
	resolution, source, _, err := mediaResolver.ResolveWithSource(secondCtx, cmecloud, ref, secondAgent)
	if err != nil || source == CacheSourceMiss {
		t.Fatalf("换了 UA 与令牌之后应当命中共享缓存：source=%s err=%v", source, err)
	}
	if resolution.PlayURL() != cmecloud.target {
		t.Fatalf("共享缓存给出的直链 = %q, want %q", resolution.PlayURL(), cmecloud.target)
	}
	if calls := cmecloud.calls.Load(); calls != 1 {
		t.Fatalf("移动云盘直链应当只解析一次（缓存期内跨 UA 复用），实际问了上游 %d 次", calls)
	}

	// 对照：普通直链不受这条规则影响，换 UA 仍然是各存各的。
	plain := newProvider("plain", "https://cdn.example/movie.mkv")
	if _, source, _, err := mediaResolver.ResolveWithSource(firstCtx, plain, ref, firstAgent); err != nil || source != CacheSourceMiss {
		t.Fatalf("普通直链第一个播放器 source=%s err=%v", source, err)
	}
	if _, source, _, err := mediaResolver.ResolveWithSource(secondCtx, plain, ref, secondAgent); err != nil || source != CacheSourceMiss {
		t.Fatalf("普通直链不该跨 UA 复用：source=%s err=%v", source, err)
	}

	// 换一台上游（同一张卡、同一个文件）也必须各存各的：共享键里带着「哪台上游」。
	otherServer := newProvider("other", "https://dl.cmecloud.cn/abc/movie.mkv")
	if _, source, _, err := mediaResolver.ResolveWithSource(secondCtx, otherServer, ref, secondAgent); err != nil || source != CacheSourceMiss {
		t.Fatalf("换了上游之后不该命中：source=%s err=%v", source, err)
	}
}

// 共享键回退必须回读到内容再判一次。手工写缓存文件、或者将来把 putResolution
// 改坏了，都可能让一条**非**移动云盘的直链落进共享键；那时绝不能把它发给别的播放器。
func TestSharedCacheKeyOnlyServesCmecloudResolutions(t *testing.T) {
	mediaResolver := New(config.Cache{TTL: time.Hour, MaxSize: 20}, config.Redirect{})
	sharedKey := "same-card|stream|movie|||" + "\x00server=ns"
	scopedKey := sharedKey + "\x00ua=Infuse/8.0\x00scope=abc"

	mediaResolver.cache.put(sharedKey, remoteResolution("https://cdn.example/movie.mkv"), time.Hour)
	if _, _, _, ok := mediaResolver.lookup(scopedKey, sharedKey); ok {
		t.Fatal("共享键里的非移动云盘直链不该被回退命中")
	}
	mediaResolver.cache.put(sharedKey, remoteResolution("https://dl.cmecloud.cn/abc/movie.mkv"), time.Hour)
	if _, source, _, ok := mediaResolver.lookup(scopedKey, sharedKey); !ok || source != CacheSourceHit {
		t.Fatalf("共享键里的移动云盘直链应当被回退命中：ok=%v source=%s", ok, source)
	}
}
