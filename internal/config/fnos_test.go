package config

import "testing"

func TestLoadAcceptsFnosUpstream(t *testing.T) {
	path := writeConfig(t, `
server:
  listen: ":5151"
upstreams:
  - name: fnos
    type: fnos
    base_url: "http://10.0.0.33:8005/"
    api_key: secret
    listen_port: 5154
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if len(cfg.Upstreams) != 1 {
		t.Fatalf("loaded %d upstreams, want 1", len(cfg.Upstreams))
	}
	upstream := cfg.Upstreams[0]
	if upstream.Type != UpstreamFnos {
		t.Fatalf("type = %q, want %q", upstream.Type, UpstreamFnos)
	}
	if !upstream.Type.IsEmbyFamily() {
		t.Fatal("fnos 应当属于 Emby 家族")
	}
	if upstream.BaseURL != "http://10.0.0.33:8005" {
		t.Fatalf("base_url = %q，结尾斜杠没有被去掉", upstream.BaseURL)
	}
}

func TestUnknownUpstreamTypeStillRejected(t *testing.T) {
	path := writeConfig(t, `
upstreams:
  - name: nope
    type: plex
    base_url: "http://10.0.0.34:32400"
    listen_port: 5154
`)
	if _, err := Load(path); err == nil {
		t.Fatal("未知类型应当被拒绝")
	}
}

// 飞牛影视与 Emby 共用一份 UA 屏蔽名单，所以 Emby 那一组的开关、
// 关键词与上游勾选都要对 fnos 生效。
func TestFnosSharesEmbyUserAgentPolicy(t *testing.T) {
	redirect := Redirect{
		Mode:                           RedirectAlways,
		BlockClientUserAgentEmby:       Bool(true),
		BlockedUserAgentsEmby:          []string{"Filmly"},
		BlockedUserAgentsEmbyUpstreams: []string{"fnos", "客厅 Emby"},
	}
	if !redirect.IsBlockedClientUserAgentForUpstream(UpstreamFnos, "fnos", "Filmly/99.0.0-217") {
		t.Fatal("fnos 应当命中 Emby 组的 UA 屏蔽规则")
	}
	if !redirect.IsBlockedClientUserAgentForUpstream(UpstreamEmby, "客厅 Emby", "Filmly/99.0.0-217") {
		t.Fatal("Emby 上游应当命中同一条规则")
	}
	if redirect.IsBlockedClientUserAgentForUpstream(UpstreamFnos, "未勾选的飞牛", "Filmly/99.0.0-217") {
		t.Fatal("没有勾选的上游不该被屏蔽")
	}
	if redirect.IsBlockedClientUserAgentForUpstream(UpstreamFnos, "fnos", "Infuse/8.0") {
		t.Fatal("未命中关键词的 UA 不该被屏蔽")
	}
	// 关掉 Emby 那一组的开关，fnos 也应当跟着关。
	disabled := Redirect{BlockClientUserAgentEmby: Bool(false), BlockedUserAgentsEmby: []string{"Filmly"}}
	if disabled.IsBlockedClientUserAgentForUpstream(UpstreamFnos, "fnos", "Filmly/99.0.0-217") {
		t.Fatal("Emby 组开关关闭时 fnos 不该继续屏蔽")
	}
}
