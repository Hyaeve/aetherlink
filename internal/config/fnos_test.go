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

// 飞牛没有 Emby 那种静态 API 密钥，它只认客户端账号密码换来的令牌。
// 账号密码是可选项，但必须成对：只填一半既登不上去，又会让界面误以为
// 「已经配好了」。
func TestFnosLoginCredentialsMustBePaired(t *testing.T) {
	onlyUsername := writeConfig(t, `
upstreams:
  - name: fnos
    type: fnos
    base_url: "http://10.0.0.33:8005"
    username: kiro
    listen_port: 5154
`)
	if _, err := Load(onlyUsername); err == nil {
		t.Fatal("只填账号时应当被拒绝")
	}

	onlyPassword := writeConfig(t, `
upstreams:
  - name: fnos
    type: fnos
    base_url: "http://10.0.0.33:8005"
    password: s3cret
    listen_port: 5154
`)
	if _, err := Load(onlyPassword); err == nil {
		t.Fatal("只填密码时应当被拒绝")
	}
}

func TestFnosLoginCredentialsLoadAndNormalize(t *testing.T) {
	path := writeConfig(t, `
upstreams:
  - name: fnos
    type: fnos
    base_url: "http://10.0.0.33:8005"
    username: "  kiro  "
    password: "p@ss word"
    listen_port: 5154
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	upstream := cfg.Upstreams[0]
	if upstream.Username != "kiro" {
		t.Fatalf("username = %q，首尾空格应当被裁掉", upstream.Username)
	}
	// 密码是口令，裁剪会悄悄改掉它，必须原样保留。
	if upstream.Password != "p@ss word" {
		t.Fatalf("password = %q，应当原样保留", upstream.Password)
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
