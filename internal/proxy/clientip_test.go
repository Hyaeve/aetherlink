package proxy

import (
	"net/http/httptest"
	"testing"
)

func TestClientIPTrustedProxyChain(t *testing.T) {
	tests := []struct {
		name, peer, forwarded, real, want string
		trusted                           []string
	}{
		{"lan domain", "192.168.1.20:5000", "", "", "192.168.1.20", nil},
		{"public spoof", "8.8.8.8:5000", "192.168.1.20", "", "8.8.8.8", nil},
		{"untrusted lan", "192.168.1.10:5000", "8.8.8.8", "", "192.168.1.10", nil},
		{"trusted public", "192.168.1.10:5000", "8.8.8.8", "", "8.8.8.8", []string{"192.168.1.10/32"}},
		{"trusted lan", "192.168.1.10:5000", "192.168.1.20", "", "192.168.1.20", []string{"192.168.1.10/32"}},
		{"spoofed leftmost", "192.168.1.10:5000", "192.168.1.20, 8.8.8.8", "", "8.8.8.8", []string{"192.168.1.10/32"}},
		{"multiple proxies", "172.18.0.2:5000", "8.8.8.8, 192.168.1.10", "", "8.8.8.8", []string{"172.18.0.2/32", "192.168.1.10/32"}},
		{"real ip", "192.168.1.10:5000", "", "fd00::20", "fd00::20", []string{"192.168.1.10/32"}},
		{"mapped ipv6", "[::ffff:192.168.1.20]:5000", "", "", "192.168.1.20", nil},
		{"invalid header", "192.168.1.10:5000", "invalid", "8.8.8.8", "", []string{"192.168.1.10/32"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest("GET", "http://media.example/stream", nil)
			request.RemoteAddr = test.peer
			request.Header.Set("X-Forwarded-For", test.forwarded)
			request.Header.Set("X-Real-IP", test.real)
			if got := clientIP(request, test.trusted...); got != test.want {
				t.Fatalf("client = %q, want %q", got, test.want)
			}
		})
	}
}
