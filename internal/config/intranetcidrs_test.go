package config

import (
	"path/filepath"
	"strings"
	"testing"
)

// 内网网段是用户按自家网络填的（典型就是运营商下发给家里设备的 IPv6 网段）：
// 必须原样落盘、原样读回，克隆时不共享底层数组。
func TestIntranetCIDRsRoundTrip(t *testing.T) {
	cfg := Default()
	cfg.Redirect.IntranetCIDRs = []string{"240e:390:1a2b:3c4d::/64", "192.168.0.0/16"}
	filename := filepath.Join(t.TempDir(), "config.yaml")
	if err := cfg.Save(filename); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(filename)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Redirect.IntranetCIDRs) != 2 || loaded.Redirect.IntranetCIDRs[0] != "240e:390:1a2b:3c4d::/64" {
		t.Fatalf("内网网段没有被保留：%v", loaded.Redirect.IntranetCIDRs)
	}
	clone := loaded.Clone()
	clone.Redirect.IntranetCIDRs[0] = "10.0.0.0/8"
	if loaded.Redirect.IntranetCIDRs[0] != "240e:390:1a2b:3c4d::/64" {
		t.Fatal("clone shares intranet CIDR list")
	}
}

// 合法网段照收（顺带去掉两侧空白）；不是网段的、以及覆盖全部地址的要当场拒绝
// —— 后者会让内外网判断整体失效，属于配置错误，该用跳转模式表达的别写在这里。
func TestIntranetCIDRsValidation(t *testing.T) {
	cases := []struct {
		name    string
		values  []string
		want    []string
		wantErr string
	}{
		{
			name:   "IPv6 与 IPv4 混合并去掉空白",
			values: []string{" 240e:390:1a2b:3c4d::/64 ", "192.168.0.0/16", ""},
			want:   []string{"240e:390:1a2b:3c4d::/64", "192.168.0.0/16"},
		},
		{name: "单个地址写成 /32", values: []string{"192.168.1.10/32"}, want: []string{"192.168.1.10/32"}},
		{name: "不是网段", values: []string{"240e:390:1a2b:3c4d::"}, wantErr: "不是合法的 IP/CIDR 网段"},
		{name: "覆盖全部 IPv4", values: []string{"0.0.0.0/0"}, wantErr: "覆盖全部地址"},
		{name: "覆盖全部 IPv6", values: []string{"::/0"}, wantErr: "覆盖全部地址"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			cfg := Default()
			cfg.Redirect.IntranetCIDRs = test.values
			err := cfg.Validate()
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("Validate() 错误 = %v，want 含 %q", err, test.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Validate() 不该报错：%v", err)
			}
			if len(cfg.Redirect.IntranetCIDRs) != len(test.want) {
				t.Fatalf("规范化后 = %v，want %v", cfg.Redirect.IntranetCIDRs, test.want)
			}
			for index, value := range test.want {
				if cfg.Redirect.IntranetCIDRs[index] != value {
					t.Fatalf("规范化后 = %v，want %v", cfg.Redirect.IntranetCIDRs, test.want)
				}
			}
		})
	}
}
