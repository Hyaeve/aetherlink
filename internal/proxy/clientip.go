package proxy

import (
	"net/http"
	"net/netip"
	"strings"

	"github.com/aetherlink/aetherlink/internal/resolver"
)

// clientAddress 与 resolver.ClientAddress 用同一套解析：裸 IP、`ip:port`、
// 带 zone 的链路本地地址。内外网判断与日志里的「客户端 IP」共用它，不要各写
// 一份 —— 两边不一致时，日志会显示一个判不出归属的地址。
func clientAddress(value string) netip.Addr {
	return resolver.ClientAddress(value)
}

func clientIP(request *http.Request, trustedCIDRs ...string) string {
	return ClientIP(request, trustedCIDRs...)
}

func ClientIP(request *http.Request, trustedCIDRs ...string) string {
	peer := clientAddress(request.RemoteAddr)
	if !peer.IsValid() {
		return ""
	}
	trusted := func(address netip.Addr) bool {
		for _, value := range trustedCIDRs {
			prefix, err := netip.ParsePrefix(value)
			if err == nil && prefix.Bits() > 0 && prefix.Contains(address) {
				return true
			}
		}
		return false
	}
	if !trusted(peer) {
		return peer.String()
	}
	if forwarded := strings.Join(request.Header.Values("X-Forwarded-For"), ","); forwarded != "" {
		hops := strings.Split(forwarded, ",")
		current := peer
		for index := len(hops) - 1; index >= 0 && trusted(current); index-- {
			address := clientAddress(hops[index])
			if !address.IsValid() {
				return ""
			}
			current = address
		}
		return current.String()
	}
	if real := request.Header.Get("X-Real-IP"); real != "" {
		if address := clientAddress(real); address.IsValid() {
			return address.String()
		}
		return ""
	}
	return peer.String()
}
