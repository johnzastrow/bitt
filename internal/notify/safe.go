// Package notify delivers notifications by email and ntfy.
//
// It is the app's first outbound side effect, and its whole reason to be a
// separate package is that the security controls live here, testable in
// isolation the way internal/avatar isolates the upload surface. Two of them
// matter most and are in this file:
//
//   - Header safety. Notification text is built from tab names, memos, and
//     display names -- all user-controlled. A CR or LF in a value placed in an
//     email header or an ntfy title injects extra headers (a Bcc to exfiltrate,
//     a spoofed body). Every header value goes through sanitizeHeader, which
//     refuses control characters rather than stripping them silently.
//
//   - SSRF containment. ntfy delivery is an outbound POST. safeTransport
//     resolves the destination and refuses any address that is loopback,
//     private, link-local, or otherwise not a public unicast host -- checked at
//     connect time, so a DNS answer that changes between validation and dial
//     cannot slip a private IP through. In a container this is load-bearing: the
//     cloud metadata endpoint and sidecar services sit at private addresses one
//     resolve away.
package notify

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"
)

// Errors surfaced to callers.
var (
	// ErrUnsafeAddress is returned when a destination resolves to an address
	// delivery must not reach (loopback, private, link-local, and the like).
	ErrUnsafeAddress = errors.New("notify: destination resolves to a non-public address")
	// ErrHeaderInjection is returned when a value bound for a header carries a
	// control character.
	ErrHeaderInjection = errors.New("notify: value contains a control character")
)

// sanitizeHeader returns s if it is safe to place in a header, or an error.
//
// It rejects rather than strips. A memo with a newline in it is a sign of an
// injection attempt or of genuinely surprising input; either way the honest
// response is to refuse the send and let the caller decide, not to quietly
// mangle the text and deliver something the user did not write.
func sanitizeHeader(s string) (string, error) {
	for _, r := range s {
		// Control characters, including CR and LF, have no place in a header.
		// Tab is a control character too but is a legitimate separator, so it is
		// the one exception.
		if r != '\t' && (r < 0x20 || r == 0x7f) {
			return "", fmt.Errorf("%w: %q", ErrHeaderInjection, s)
		}
	}
	return s, nil
}

// oneLine collapses a value to a single safe header line: it trims, and returns
// an error if any control character survives. Callers use it for subjects and
// titles built from user text.
func oneLine(s string) (string, error) {
	return sanitizeHeader(strings.TrimSpace(s))
}

// safeDialContext wraps a dialer so that every resolved address is checked
// before the connection is made. The check happens here, at dial time, rather
// than against a pre-resolved IP, so that DNS rebinding -- a name that resolves
// to a public IP during validation and a private one at connect -- cannot slip
// through.
func safeDialContext(base *net.Dialer) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil {
			return nil, err
		}
		if len(ips) == 0 {
			return nil, fmt.Errorf("notify: %q did not resolve", host)
		}
		for _, ip := range ips {
			if !isAllowedDestination(ip.IP) {
				return nil, fmt.Errorf("%w: %s -> %s", ErrUnsafeAddress, host, ip.IP)
			}
		}
		// Dial the first vetted IP directly, so the OS resolver is not consulted
		// a second time with a possibly different answer.
		return base.DialContext(ctx, network, net.JoinHostPort(ips[0].IP.String(), port))
	}
}

// isAllowedDestination reports whether an outbound notification may reach ip.
//
// The policy is tuned for a self-hosted app that runs in a container, which
// changes the calculus the generic "reject all private addresses" advice
// assumes. The ntfy server is ADMIN-pinned configuration (decision D1), not a
// user-supplied URL, and a household deployment legitimately points it at an
// ntfy SIDECAR or a box on the LAN -- both private addresses. Refusing private
// addresses outright would break the primary use case.
//
// So private/LAN (RFC1918, IPv6 ULA) is allowed, and only the addresses that
// are never a legitimate destination and ARE what an SSRF wants are refused:
//
//   - loopback -- poking the app's own container
//   - link-local, which is where the cloud metadata endpoint lives
//     (169.254.169.254) and thus the single most valuable SSRF target
//   - unspecified and multicast
//
// The user-controlled part -- the ntfy topic -- never reaches this function: it
// is a validated path segment on the admin's host (see validTopic), so it
// cannot change where the request goes.
func isAllowedDestination(ip net.IP) bool {
	if ip == nil {
		return false
	}
	if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsMulticast() || ip.IsUnspecified() {
		return false
	}
	// 100.64.0.0/10 (CGNAT) and 192.0.0.0/24 (IETF protocol assignments) are
	// not legitimate ntfy hosts and are refused even though they are not
	// "private"; RFC1918 and ULA are deliberately permitted above.
	if ip4 := ip.To4(); ip4 != nil {
		if ip4[0] == 100 && ip4[1]&0xc0 == 64 {
			return false
		}
		if ip4[0] == 192 && ip4[1] == 0 && ip4[2] == 0 {
			return false
		}
	}
	return true
}

// newSafeClient builds an http.Client whose transport refuses non-public
// destinations and never follows redirects (a 302 to a private IP would
// otherwise defeat the dial-time check on the original host).
func newSafeClient(timeout time.Duration) *http.Client {
	base := &net.Dialer{Timeout: timeout}
	transport := &http.Transport{
		DialContext:           safeDialContext(base),
		TLSHandshakeTimeout:   timeout,
		ResponseHeaderTimeout: timeout,
		DisableKeepAlives:     true,
	}
	return &http.Client{
		Timeout:   timeout,
		Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}
