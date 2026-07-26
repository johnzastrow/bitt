package web

import (
	"net/http"
	"testing"
)

// clientIP is what every security log line records as the source. Behind the
// production Caddy the connection peer is loopback, so the real client must come
// from X-Forwarded-For -- but only when the peer is loopback, and only the entry
// the proxy itself appended, or the field becomes a place to forge an address.
func TestClientIP(t *testing.T) {
	cases := []struct {
		name       string
		remoteAddr string
		xff        string
		want       string
	}{
		{
			name:       "direct request, no proxy",
			remoteAddr: "203.0.113.7:54321",
			want:       "203.0.113.7",
		},
		{
			name:       "loopback proxy supplies the client",
			remoteAddr: "127.0.0.1:41000",
			xff:        "203.0.113.7",
			want:       "203.0.113.7",
		},
		{
			name:       "forged prepended entry cannot displace the proxy-appended one",
			remoteAddr: "127.0.0.1:41000",
			xff:        "1.2.3.4, 203.0.113.7", // client sent 1.2.3.4; Caddy appended the real 203.0.113.7
			want:       "203.0.113.7",
		},
		{
			name:       "XFF from a non-loopback peer is ignored (cannot spoof off the network)",
			remoteAddr: "203.0.113.9:5000",
			xff:        "10.0.0.1",
			want:       "203.0.113.9",
		},
		{
			name:       "IPv6 loopback peer with a client",
			remoteAddr: "[::1]:41000",
			xff:        "203.0.113.7",
			want:       "203.0.113.7",
		},
		{
			name:       "loopback peer, empty XFF falls back to the peer",
			remoteAddr: "127.0.0.1:41000",
			xff:        "  ",
			want:       "127.0.0.1",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &http.Request{RemoteAddr: tc.remoteAddr, Header: http.Header{}}
			if tc.xff != "" {
				r.Header.Set("X-Forwarded-For", tc.xff)
			}
			if got := clientIP(r); got != tc.want {
				t.Errorf("clientIP(%q, xff=%q) = %q, want %q", tc.remoteAddr, tc.xff, got, tc.want)
			}
		})
	}
}
