package resolver

import (
	"net"
	"net/netip"
	"strings"
)

// 为什么还要有这一层：内置规则只按地址类型认内网（RFC1918 / 回环 / 链路本地 /
// IPv6 ULA），而家里的设备拿到的常常是运营商下发的 IPv6 全局地址（240e:: 这类
// GUA）—— 按类型它是公网，可它就在 AetherLink 所在的局域网里。配置文件里的
// intranet_cidrs 能补上，但运营商的前缀会变（重新拨号 / 光猫重启），填死迟早过期，
// 而且它要用户自己盯着 —— 所以那一项在界面上干脆没有入口，只留给 bridge 网络这种
// 看不到局域网网段的部署兜底。
//
// 于是主判据是这一层不需要维护的：**凡是与 AetherLink 本机处于同一网段的客户端，
// 一律按内网处理**。前缀随运营商变时，本机地址跟着一起变，这一层自动跟随。
// 前提是容器能看到局域网网段（host / macvlan 网络；bridge 网络里只有 172.x）。
//
// 这一层是推算出来的，所以它必须出声：命中的判定会带上一句「，与本机为同一网段
// 2409:…::/64」（原本接进日志，设置页也另有一行只读提示），
// 见 ScopeOfClientWithReason 与 SettingsView 那张「网络地址」卡片。

// virtualInterfacePrefixes 是「不参与同网段判定」的网卡名前缀：这些网卡上的地址
// 属于虚拟 / 隧道网络，对端（容器、VPN 节点）并不在这台机器所在的局域网里，把它们
// 算成内网会让「公网跳转」下的远程客户端被无谓地中继。判据取保守的一眼可辨的那些，
// 真实网卡名（eth0 / enp1s0 / br0 / ovs_eth0 / bond0 / wlan0）都不在里面。
// 注意 Docker 的网桥是 `br-<12 位十六进制>`，而 NAS 上的真实网桥叫 `br0`，
// 所以这里排除的是带连字符的 `br-`。
var virtualInterfacePrefixes = []string{
	"docker", "br-", "veth", "virbr", "tun", "tap", "wg", "tailscale", "zt", "nebula", "ppp",
}

// LocalNetworkPrefixes 返回本机自己的 IPv6 网段，供「同一网段即内网」判定与设置页
// 的提示使用。每次调用都重新读网卡：前缀被运营商换掉之后要立刻跟上，而读一次
// 网卡只是一次 netlink 查询，播放请求的量级完全不需要缓存。
func LocalNetworkPrefixes() []netip.Prefix {
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	prefixes := make([]netip.Prefix, 0, 4)
	seen := make(map[netip.Prefix]bool, 4)
	for _, iface := range interfaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		if isVirtualInterface(iface.Name) {
			continue
		}
		addrs, addrErr := iface.Addrs()
		if addrErr != nil {
			continue
		}
		for _, prefix := range prefixesOfAddrs(addrs) {
			if seen[prefix] {
				continue
			}
			seen[prefix] = true
			prefixes = append(prefixes, prefix)
		}
	}
	return prefixes
}

// isVirtualInterface 判断网卡名是否属于虚拟 / 隧道那一类。名字取自各系统官方的
// 默认命名（Linux 的 docker0 / br-xxxx / tailscale0、BSD 与 Windows 的 tun0 等），
// 大小写不敏感。
func isVirtualInterface(name string) bool {
	lower := strings.ToLower(strings.TrimSpace(name))
	if lower == "" {
		return true
	}
	for _, prefix := range virtualInterfacePrefixes {
		if strings.HasPrefix(lower, prefix) {
			return true
		}
	}
	return false
}

// prefixesOfAddrs 从一串网卡地址里挑出可用的网段。
//
// 只认 IPv6：IPv4 的私网地址已经由内置规则（IsPrivate）判成内网，把公网 IPv4
// 子网也算进来反而会在 VPS 上把邻居当成内网 —— 那台机器的 /29 里坐的都是别人。
//
// 前缀长度比 /64 还长的按所在 /64 计：Windows 上 SLAAC 地址常报成 /128（第二条
// 是那条链路的 /64 前缀），报 /128 时按单机看待会漏掉整条链路。链路本地、回环、
// 未指定地址与带包或不是 IPNet 的条目一律跳过。
func prefixesOfAddrs(addrs []net.Addr) []netip.Prefix {
	prefixes := make([]netip.Prefix, 0, len(addrs))
	for _, addr := range addrs {
		ipNet, ok := addr.(*net.IPNet)
		if !ok || ipNet.IP == nil {
			continue
		}
		address, ok := netip.AddrFromSlice(ipNet.IP)
		if !ok {
			continue
		}
		// Unmap 必须在 Is6 之前：IPv4 地址从 net.IPNet 出来常是 ::ffff: 形式，
		// 不还原会被当成 IPv6 收进列表（而 IPv4 网段是要跳过的）。
		address = address.Unmap().WithZone("")
		if !address.Is6() || address.IsLoopback() || address.IsLinkLocalUnicast() || address.IsUnspecified() {
			continue
		}
		ones, bits := ipNet.Mask.Size()
		if bits != 128 || ones < 0 {
			continue
		}
		if ones > 64 {
			ones = 64
		}
		prefix, err := address.Prefix(ones)
		if err != nil {
			continue
		}
		prefixes = append(prefixes, prefix.Masked())
	}
	return prefixes
}
