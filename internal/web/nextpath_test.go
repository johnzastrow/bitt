package web

import "testing"

// safeNext is an open-redirect gate, so its refusals matter more than its
// acceptances. Each hostile case below is a real technique.
func TestSafeNextRefusesEverythingOffSite(t *testing.T) {
	bad := []string{
		"",                           // nothing supplied
		"https://evil.example/",      // absolute
		"http://evil.example/",       //
		"//evil.example/",            // protocol-relative: a browser leaves the site
		"//evil.example",             //
		`/\evil.example`,             // backslash, normalised to // by some browsers
		`/\/evil.example`,            //
		"javascript:alert(1)",        // scheme, no slash
		"tabs/1/pay",                 // relative, could resolve anywhere
		"/tabs/1\r\nSet-Cookie: x=1", // header injection through a redirect
		"/tabs/1\npath",              //
	}
	for _, in := range bad {
		if got := safeNext(in); got != "" {
			t.Errorf("safeNext(%q) = %q, want it refused", in, got)
		}
	}
	// A very long value is refused rather than reflected back into a header.
	long := "/" + string(make([]byte, 600))
	if got := safeNext(long); got != "" {
		t.Errorf("an over-long path was accepted: %q", got)
	}
}

// The shapes it must accept, or a notification link does not survive a login.
func TestSafeNextAcceptsLocalPaths(t *testing.T) {
	for in, want := range map[string]string{
		"/tabs/2/pay":        "/tabs/2/pay",
		"/":                  "/",
		"/tabs/2/pay?from=n": "/tabs/2/pay?from=n",
	} {
		if got := safeNext(in); got != want {
			t.Errorf("safeNext(%q) = %q, want %q", in, got, want)
		}
	}
}
