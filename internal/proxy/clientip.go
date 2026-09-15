package proxy

import (
	"net/http"
	"net/netip"
	"strings"
)

func clientAddress(value string) netip.Addr {
	value = strings.TrimSpace(value)
	address, err := netip.ParseAddr(value)
	if err != nil {
		endpoint, endpointErr := netip.ParseAddrPort(value)
		if endpointErr != nil {
			return netip.Addr{}
		}
		address = endpoint.Addr()
	}
	address = address.Unmap()
	if address.IsUnspecified() || address.IsMulticast() {
		return netip.Addr{}
	}
	return address
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
