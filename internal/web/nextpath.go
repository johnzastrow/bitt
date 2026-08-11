package web

import (
	"net/url"
	"strings"
)

// safeNext validates a post-login destination.
//
// Carrying "where you were going" through a login is what makes a link in a
// notification work: tapped on a phone, where the session has usually expired,
// it would otherwise land on the dashboard and the person would have to find
// the tab themselves.
//
// It is also a textbook open redirect if taken on trust, so this is an
// ALLOWLIST of shapes rather than a blocklist of bad ones:
//
//   - must be a path on this site, beginning with a single "/"
//   - "//host" and "/\host" are refused: browsers treat both as
//     protocol-relative and would leave the site
//   - anything carrying a scheme or a host is refused outright
//   - anything that does not parse is refused
//
// The empty string means "no destination", and every caller falls back to its
// own default rather than to whatever was supplied.
func safeNext(raw string) string {
	if raw == "" || len(raw) > 512 {
		return ""
	}
	// A backslash is normalised to a forward slash by some browsers, so
	// "/\evil.example" would leave the site. Refuse it before parsing, since
	// url.Parse will not object.
	if strings.ContainsAny(raw, "\\\r\n") {
		return ""
	}
	if !strings.HasPrefix(raw, "/") || strings.HasPrefix(raw, "//") {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "" || u.Host != "" {
		return ""
	}
	// Re-render from the parsed form rather than returning the caller's string,
	// so anything the parser normalised away cannot survive.
	out := u.EscapedPath()
	if out == "" || !strings.HasPrefix(out, "/") {
		return ""
	}
	if u.RawQuery != "" {
		out += "?" + u.RawQuery
	}
	return out
}
