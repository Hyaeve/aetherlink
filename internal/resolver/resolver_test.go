package resolver

import (
	"fmt"
	"net"
	"net/netip"
	"path/filepath"
	"strings"
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

func TestPersistentCacheReportsRestoredThenNormalHit(t *testing.T) {
	cachePath := filepath.Join(t.TempDir(), "direct-links-cache.json")
	first := newPersistentLRUCache(time.Hour, 4, cachePath)
	first.put("provider:item:ua", &Resolution{Target: &strm.Target{Type: strm.TargetRemote, URL: "https://cdn.example/book.m4a"}}, time.Hour)

	reopened := newPersistentLRUCache(time.Hour, 4, cachePath)
	if _, _, restored, ok := reopened.getWithSource("provider:item:ua"); !ok || !restored {
		t.Fatalf("first lookup after reopening should be restored hit: ok=%v restored=%v", ok, restored)
	}
	if _, _, restored, ok := reopened.getWithSource("provider:item:ua"); !ok || restored {
		t.Fatalf("second lookup after reopening should be normal hit: ok=%v restored=%v", ok, restored)
	}
}

func TestPersistentCacheSkipsExpiredEntries(t *testing.T) {
	cachePath := filepath.Join(t.TempDir(), "direct-links-cache.json")
	cache := newPersistentLRUCache(time.Hour, 4, cachePath)
	cache.put("expired", &Resolution{Target: &strm.Target{Type: strm.TargetRemote, URL: "https://cdn.example/expired"}}, time.Millisecond)
	time.Sleep(10 * time.Millisecond)

	reopened := newPersistentLRUCache(time.Hour, 4, cachePath)
	if reopened.size() != 0 {
		t.Fatalf("expired persistent entry was restored: %d entries", reopened.size())
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

// 飞牛影视与 Emby 共用同一套直链缓存策略，这里把「共用」本身钉成断言。
//
// 摘掉这个共用关系不会有任何编译或运行报错：飞牛会静默退回配置里的 ttl，
// 直链上的 t 参数不再被读，签名地址会被整段保留到过期之后 —— 客户端拿到的
// 是已经失效的直链，现象是「偶尔一两条播不了」，而播放流水里还写着缓存命中。
func TestFnosSharesEmbyFamilyDirectLinkCachePolicy(t *testing.T) {
	fnos, err := upstream.New(config.Upstream{
		Name:    "飞牛影视",
		Type:    config.UpstreamFnos,
		BaseURL: "http://127.0.0.1:8005",
	})
	if err != nil {
		t.Fatal(err)
	}
	// 缓存文件里的 ttl 刻意配成 5 小时：它不该出现在飞牛的直链缓存上。
	resolver := New(config.Cache{TTL: 5 * time.Hour, MaxSize: 4}, config.Redirect{})

	// 直链上没有可用的 t 时与 Emby 一样固定 2 小时。
	if got := resolver.cacheTTL(fnos); got != 2*time.Hour {
		t.Fatalf("飞牛回退缓存 ttl = %v, want 2h（应与 Emby 一致）", got)
	}

	// 直链带了数字 t 时按该直链的实际到期时间缓存 —— 这条对飞牛的签名直链是常态。
	expiry := time.Now().Add(40 * time.Second).Unix()
	signed := &Resolution{
		Target: &strm.Target{
			Type: strm.TargetRemote,
			URL:  fmt.Sprintf("https://cdn.example/白色巨塔/S01E01.mkv?t=%d", expiry),
		},
	}
	ttl := resolver.cacheTTLFor(fnos, signed, resolver.cacheTTL(fnos))
	if ttl < 30*time.Second || ttl > 41*time.Second {
		t.Fatalf("飞牛签名直链的缓存 ttl = %v, want 约 40s（应按直链的 t 到期）", ttl)
	}
}

func TestRedirectModesApplyToClientIPRegardlessOfTarget(t *testing.T) {
	resolver := New(config.Cache{}, config.Redirect{})
	publicResolution := &Resolution{Target: &strm.Target{Type: strm.TargetRemote, URL: "https://cdn.example/video.mkv"}}
	privateResolution := &Resolution{Target: &strm.Target{Type: strm.TargetRemote, URL: "http://10.0.0.31:19527/d/video.mkv"}}

	cases := []struct {
		mode        config.RedirectMode
		wantPublic  bool
		wantPrivate bool
	}{
		{config.RedirectAlways, true, true},
		{config.RedirectPublic, true, false},
		{config.RedirectPrivate, false, true},
		{config.RedirectNever, false, false},
	}
	for _, test := range cases {
		redirect := config.Redirect{Mode: test.mode}
		for _, resolution := range []*Resolution{publicResolution, privateResolution} {
			for _, client := range []string{"8.8.8.8", "2001:4860:4860::8888"} {
				if got := resolver.ShouldRedirectForClient(resolution, redirect, client); got != test.wantPublic {
					t.Errorf("mode %s client %s = %v, want %v", test.mode, client, got, test.wantPublic)
				}
			}
			for _, client := range []string{"192.168.1.3", "::ffff:192.168.1.3", "fd00::1", "127.0.0.1", "::1", "fe80::1"} {
				if got := resolver.ShouldRedirectForClient(resolution, redirect, client); got != test.wantPrivate {
					t.Errorf("mode %s client %s = %v, want %v", test.mode, client, got, test.wantPrivate)
				}
			}
			for _, client := range []string{"", "example.org", "0.0.0.0", "::"} {
				if got := resolver.ShouldRedirectForClient(resolution, redirect, client); got != (test.mode == config.RedirectAlways) {
					t.Errorf("mode %s unknown client %q = %v", test.mode, client, got)
				}
			}
		}
	}
}

func TestScopeOfClient(t *testing.T) {
	cases := []struct {
		client string
		want   ClientScope
	}{
		{"8.8.8.8", ClientScopePublic},
		{"2001:4860:4860::8888", ClientScopePublic},
		{"192.168.1.3", ClientScopePrivate},
		{"10.0.0.31", ClientScopePrivate},
		{"::ffff:192.168.1.3", ClientScopePrivate},
		{"fd00::1", ClientScopePrivate},
		{"127.0.0.1", ClientScopePrivate},
		{"fe80::1", ClientScopePrivate},
		{"", ClientScopeUnknown},
		{"example.org", ClientScopeUnknown},
		{"0.0.0.0", ClientScopeUnknown},
		{"::", ClientScopeUnknown},
		// 带 zone 的链路本地地址：netip.ParseAddr 不接受 zone，而这是最明确的
		// 内网地址，不能掉进「无法识别」。
		{"fe80::1%eth0", ClientScopePrivate},
		{"[fe80::1%eth0]:5000", ClientScopePrivate},
	}
	for _, test := range cases {
		if got := ScopeOfClient(test.client); got != test.want {
			t.Errorf("ScopeOfClient(%q) = %q, want %q", test.client, got, test.want)
		}
	}
}

// 「公网跳转」对家里的 IPv6 客户端失效，成因是运营商下发给每台设备的全局地址
// （240e:: 这类 GUA）按地址类型算公网，可它就在局域网里。配置里补充的网段必须
// 能把它拉回内网，而且只拉到该网段内 —— 别人家的 IPv6 一律不受影响。
// 样本一律用 2001:db8::/32（文档保留段）而不是真的运营商前缀：判定现在也会参考
// 「本机网段」，拿真前缀当「应当算公网」的样本时，跑测试那台机器万一就有那个网段，
// 断言会莫名其妙地翻车；文档段不可能出现在任何网卡上。
func TestScopeOfClientAcceptsConfiguredIntranetCIDRs(t *testing.T) {
	const lan = "2001:db8:1a2b:3c4d::/64"
	cases := []struct {
		name   string
		client string
		want   ClientScope
	}{
		{"同网段的客户端", "2001:db8:1a2b:3c4d::12", ClientScopePrivate},
		{"同网段的末位地址", "2001:db8:1a2b:3c4d:ffff:ffff:ffff:ffff", ClientScopePrivate},
		// 带 zone 的地址必须先去掉 zone 再比网段：netip 的 Prefix.Contains 对
		// 带 zone 的地址恒为 false，留着 zone 这条就会漏判。
		{"带 zone 的同网段客户端", "2001:db8:1a2b:3c4d::12%eth0", ClientScopePrivate},
		{"相邻网段不受影响", "2001:db8:1a2b:3c4e::12", ClientScopePublic},
		{"别人家的 IPv6 仍是公网", "2001:4860:4860::8888", ClientScopePublic},
		{"内置规则照旧", "192.168.1.9", ClientScopePrivate},
		{"无法识别的照旧", "", ClientScopeUnknown},
	}
	for _, test := range cases {
		if got := ScopeOfClient(test.client, lan); got != test.want {
			t.Errorf("%s：ScopeOfClient(%q, %q) = %q, want %q", test.name, test.client, lan, got, test.want)
		}
	}
	if got := ScopeOfClient("2001:db8:1a2b:3c4d::12"); got != ClientScopePublic {
		t.Errorf("没配置内网网段、又不与本机同网段时 GUA 仍应按公网处理（保持原行为），得到 %q", got)
	}
}

// 跳转策略必须跟着补充网段一起动：公网跳转下，命中内网网段的 IPv6 客户端要被
// 中继；内网跳转下反过来。
func TestRedirectModesHonourConfiguredIntranetCIDRs(t *testing.T) {
	mediaResolver := New(config.Cache{}, config.Redirect{})
	redirect := config.Redirect{Mode: config.RedirectPublic, IntranetCIDRs: []string{"2001:db8:1a2b:3c4d::/64"}}
	resolution := &Resolution{Target: &strm.Target{Type: strm.TargetRemote, URL: "https://cdn.example/video.mkv"}}

	if mediaResolver.ShouldRedirectForClient(resolution, redirect, "2001:db8:1a2b:3c4d::12") {
		t.Error("公网跳转下，配置为内网网段的客户端不该拿到 302")
	}
	if !mediaResolver.ShouldRedirectForClient(resolution, redirect, "2001:db8:1a2b:3c4e::12") {
		t.Error("公网跳转下，网段之外的 IPv6 客户端应当拿到 302")
	}

	redirect.Mode = config.RedirectPrivate
	if !mediaResolver.ShouldRedirectForClient(resolution, redirect, "2001:db8:1a2b:3c4d::12") {
		t.Error("内网跳转下，配置为内网网段的客户端应当拿到 302")
	}
	if mediaResolver.ShouldRedirectForClient(resolution, redirect, "2001:db8:1a2b:3c4e::12") {
		t.Error("内网跳转下，网段之外的 IPv6 客户端应当被中继")
	}
}

// 自动识别那一层：与本机同网段的客户端不用配置也算内网。它由本机地址推算而来、
// 界面上看不见，所以判定必须同时给出一句能进日志的说明。
func TestScopeOfClientTreatsOwnNetworkAsIntranet(t *testing.T) {
	// 本机可能同时挂着好几条链路（有线 + 无线 + VPN），每一条都算。
	local := []netip.Prefix{
		netip.MustParsePrefix("2001:db8:1a2b:3c4d::/64"),
		netip.MustParsePrefix("2001:db8:9a9b::/64"),
		netip.MustParsePrefix("fd00:7a7b::/64"),
	}
	cases := []struct {
		name       string
		client     string
		local      []netip.Prefix
		want       ClientScope
		wantInNote string // 日志说明里该出现的关键词；空串表示不该有说明
	}{
		{"本机网段里的 GUA", "2001:db8:1a2b:3c4d:a5f4:ce6f:b8bd:e4e9", local, ClientScopePrivate, "2001:db8:1a2b:3c4d::/64"},
		{"本机第二条链路的网段", "2001:db8:9a9b::12", local, ClientScopePrivate, "2001:db8:9a9b::/64"},
		// ULA(fc00::/7) 本来就按内置规则算内网，轮不到自动识别，说明留空是对的。
		{"本机网段里的 ULA", "fd00:7a7b::12", local, ClientScopePrivate, ""},
		{"带 zone 也认（先剥 zone 再比网段）", "2001:db8:1a2b:3c4d::12%eth0", local, ClientScopePrivate, "2001:db8:1a2b:3c4d::/64"},
		{"相邻网段不算", "2001:db8:1a2b:3c4e::12", local, ClientScopePublic, ""},
		{"别人家的 IPv6 不算", "2001:4860:4860::8888", local, ClientScopePublic, ""},
		{"看不到本机网段时按原样判（bridge 网络即此情形）", "2001:db8:1a2b:3c4d::12", nil, ClientScopePublic, ""},
		{"内置规则命中不需要说明", "192.168.1.9", local, ClientScopePrivate, ""},
		{"无法识别的照旧", "", local, ClientScopeUnknown, ""},
	}
	for _, test := range cases {
		scope, note := scopeOfClient(ClientAddress(test.client), nil, test.local)
		if scope != test.want {
			t.Errorf("%s：scopeOfClient(%q) = %q, want %q", test.name, test.client, scope, test.want)
		}
		if test.wantInNote == "" {
			if note != "" {
				t.Errorf("%s：不该给日志说明，得到 %q", test.name, note)
			}
			continue
		}
		if !strings.Contains(note, test.wantInNote) {
			t.Errorf("%s：日志说明 %q 里应当带上 %q", test.name, note, test.wantInNote)
		}
	}

	// 用户自己声明的网段照旧生效，且不需要解释（那是他自己填的）。
	scope, note := scopeOfClient(netip.MustParseAddr("2001:db8:4c4d::7"), []string{"2001:db8:4c4d::/64"}, nil)
	if scope != ClientScopePrivate || note != "" {
		t.Errorf("声明网段命中时 = %q / 说明 %q，want 内网且无说明", scope, note)
	}
}

// 网卡地址 → 本机网段：只收 IPv6 的链路网段，/128 归到所在 /64。
func TestPrefixesOfAddrsKeepsOnLinkIPv6Prefixes(t *testing.T) {
	addrs := []net.Addr{
		// Windows 上 SLAAC 地址常以 /128 出现（那条链路的 /64 是另一条），
		// 按单机看待会漏掉整条链路，所以要归到 /64。
		&net.IPNet{IP: net.ParseIP("2001:db8:1a2b:3c4d:a5f4:ce6f:b8bd:e4e9"), Mask: net.CIDRMask(128, 128)},
		&net.IPNet{IP: net.ParseIP("fd00:7a7b::1"), Mask: net.CIDRMask(64, 128)},
		// 比 /64 宽的前缀原样保留（运营商下发的 PD 前缀可能是 /56、/48）。
		&net.IPNet{IP: net.ParseIP("2409:8a4c:5a46::1"), Mask: net.CIDRMask(48, 128)},
		// IPv4 一律不收：私网已由内置规则覆盖，把公网子网也算进来会在 VPS 上
		// 把邻居当成内网。
		&net.IPNet{IP: net.ParseIP("192.168.1.5"), Mask: net.CIDRMask(24, 32)},
		&net.IPNet{IP: net.ParseIP("100.115.1.40"), Mask: net.CIDRMask(32, 32)},
		// 链路本地 / 回环 / 未指定，以及压根不是 IPNet 的条目。
		&net.IPNet{IP: net.ParseIP("fe80::1"), Mask: net.CIDRMask(64, 128)},
		&net.IPNet{IP: net.ParseIP("::1"), Mask: net.CIDRMask(128, 128)},
		&net.IPNet{IP: net.ParseIP("::"), Mask: net.CIDRMask(0, 128)},
		&net.IPAddr{IP: net.ParseIP("2001:db8:1a2b:3c4d::1")},
	}
	want := []string{"2001:db8:1a2b:3c4d::/64", "fd00:7a7b::/64", "2409:8a4c:5a46::/48"}
	got := make([]string, 0, len(want))
	for _, prefix := range prefixesOfAddrs(addrs) {
		got = append(got, prefix.String())
	}
	if len(got) != len(want) {
		t.Fatalf("prefixesOfAddrs = %v, want %v", got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("prefixesOfAddrs = %v, want %v", got, want)
		}
	}
}

// 虚拟 / 隧道网卡上的地址不属于这台机器所在的局域网（对端是容器或 VPN 节点），
// 拿它们判「同网段即内网」会让「公网跳转」下的远程客户端被无谓地中继。
func TestVirtualInterfaceNamesAreNotLocalNetworks(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		{"docker0", true},
		{"br-1a2b3c4d5e6f", true},
		{"veth9f8a7b", true},
		{"virbr0", true},
		{"tun0", true},
		{"tap0", true},
		{"wg0", true},
		{"ppp0", true},
		{"tailscale0", true},
		{"Tailscale", true},
		{"ztabcdefgh", true},
		{"nebula1", true},
		{"", true},
		// 真实网卡名一个都不能被误伤：NAS 上的局域网网桥就叫 br0，
		// 而 Docker 的网桥是带连字符的 br-<12 位十六进制>。
		{"eth0", false},
		{"enp1s0", false},
		{"br0", false},
		{"ovs_eth0", false},
		{"bond0", false},
		{"wlan0", false},
		{"以太网 6", false},
	}
	for _, test := range cases {
		if got := isVirtualInterface(test.name); got != test.want {
			t.Errorf("isVirtualInterface(%q) = %v, want %v", test.name, got, test.want)
		}
	}
}

// 用真网卡跑一遍整条路：读网卡 → 得出网段 → 网段里的地址判成内网。
// 这台机器没有全局 IPv6 网段时（容器不是 host 网络就是这个样子）跳过而不是失败 ——
// 那正是设置页会提示「未检测到」的环境。
func TestLocalNetworkPrefixesMatchThisMachine(t *testing.T) {
	prefixes := LocalNetworkPrefixes()
	if len(prefixes) == 0 {
		t.Skip("这台机器没有全局 IPv6 网段，跳过真实网卡用例")
	}
	// 留一行输出：在部署机上跑这条用例（-v）就能看到实际检测到了哪些网段。
	t.Logf("本机检测到的网段：%v", prefixes)
	for _, prefix := range prefixes {
		if prefix.Addr().Is4() || prefix.Addr().IsLinkLocalUnicast() || prefix.Addr().IsLoopback() || prefix.Bits() > 64 {
			t.Errorf("本机网段 %s 不该出现：只收非链路本地的 IPv6 且不超过 /64", prefix)
		}
	}

	address := prefixes[0].Addr().Next()
	scope, note := ScopeOfClientWithReason(address.String())
	if scope != ClientScopePrivate {
		t.Fatalf("本机网段 %s 里的地址 %s 应当算内网，得到 %q", prefixes[0], address, scope)
	}
	if address.IsPrivate() {
		// 只有 ULA 的机器（fd00::/8）本来就落进内置规则，说明留空是对的，
		// 这一层没什么可断言的。
		return
	}
	if note == "" {
		t.Fatalf("地址 %s 的判定来自自动识别，日志说明不该为空", address)
	}
	for _, prefix := range prefixes {
		if prefix.Contains(address) && strings.Contains(note, prefix.String()) {
			return
		}
	}
	t.Errorf("日志说明 %q 里应当带上命中的那个网段（本机网段：%v）", note, prefixes)
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
