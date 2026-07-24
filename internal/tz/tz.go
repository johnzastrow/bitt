// Package tz supplies the list of IANA timezone names the interface offers.
//
// The list is embedded rather than read from the filesystem, for the same
// reason the binary embeds time/tzdata: a static binary in a scratch container
// has no /usr/share/zoneinfo to enumerate, and the timezone picker must work
// there. Reading the directory at runtime would produce a full list in
// development and an empty one in production, which is the worst of both.
//
// Go's standard library offers no way to enumerate the zones inside
// time/tzdata -- the embedded zip is registered through an unexported hook --
// so the names are generated separately and committed, the same arrangement as
// the vendored htmx asset. To regenerate against a newer tzdata:
//
//	find /usr/share/zoneinfo -type f -not -path "*/posix/*" -not -path "*/right/*" \
//	  | sed 's|/usr/share/zoneinfo/||' \
//	  | grep -E '^[A-Za-z][A-Za-z_+-]*/[A-Za-z][A-Za-z_+/-]*$' \
//	  | sort -u > internal/tz/zones.txt
//	echo UTC >> internal/tz/zones.txt && sort -u -o internal/tz/zones.txt internal/tz/zones.txt
//
// A stale list is a mild problem rather than a correctness one: a name the
// running tzdata cannot load is dropped by Zones, and a valid name absent from
// the list can still be typed, because the field accepts free text and the
// server validates with time.LoadLocation regardless. The list is a
// convenience, never the authority.
package tz

import (
	_ "embed"
	"sort"
	"strings"
	"sync"
	"time"
)

//go:embed zones.txt
var zonesRaw string

var (
	once   sync.Once
	loaded []string
)

// commonFirst are the zones offered ahead of the alphabetical list.
//
// A datalist shows matches in document order, so these surface first while
// someone is still typing. They are the zones a self-hosted instance is most
// likely to want. America/New_York leads because it is what config seeds a new
// instance with; UTC follows it as the answer for anyone unsure, and is what
// the server falls back to if a stored zone ever fails to load.
var commonFirst = []string{
	"America/New_York",
	"UTC",
	"America/Chicago",
	"America/Denver",
	"America/Los_Angeles",
	"America/Anchorage",
	"Pacific/Honolulu",
	"America/Toronto",
	"America/Vancouver",
	"America/Mexico_City",
	"America/Sao_Paulo",
	"Europe/London",
	"Europe/Dublin",
	"Europe/Paris",
	"Europe/Berlin",
	"Europe/Madrid",
	"Europe/Rome",
	"Europe/Amsterdam",
	"Europe/Stockholm",
	"Europe/Warsaw",
	"Europe/Kyiv",
	"Africa/Johannesburg",
	"Africa/Lagos",
	"Africa/Cairo",
	"Asia/Jerusalem",
	"Asia/Dubai",
	"Asia/Kolkata",
	"Asia/Bangkok",
	"Asia/Singapore",
	"Asia/Hong_Kong",
	"Asia/Shanghai",
	"Asia/Tokyo",
	"Asia/Seoul",
	"Australia/Perth",
	"Australia/Sydney",
	"Pacific/Auckland",
}

// Zones returns every offerable timezone name: a short list of common ones
// first, then everything else alphabetically.
//
// Every name is checked with time.LoadLocation and dropped if the running
// tzdata cannot resolve it, so the picker can never offer a zone the app would
// then reject. The result is computed once and shared; callers must not modify
// it.
func Zones() []string {
	once.Do(func() {
		all := make(map[string]bool)
		for line := range strings.SplitSeq(zonesRaw, "\n") {
			name := strings.TrimSpace(line)
			if name == "" || strings.HasPrefix(name, "#") {
				continue
			}
			if _, err := time.LoadLocation(name); err != nil {
				continue
			}
			all[name] = true
		}

		out := make([]string, 0, len(all))
		for _, name := range commonFirst {
			if all[name] {
				out = append(out, name)
				delete(all, name)
			}
		}

		rest := make([]string, 0, len(all))
		for name := range all {
			rest = append(rest, name)
		}
		sort.Strings(rest)
		loaded = append(out, rest...)
	})
	return loaded
}

// Valid reports whether name is a timezone the running binary can load.
//
// This is the same check the setup handler makes, exposed so a caller can ask
// before offering. It deliberately does not consult the embedded list: a zone
// the runtime can resolve is usable whether or not zones.txt has caught up.
func Valid(name string) bool {
	if strings.TrimSpace(name) == "" {
		return false
	}
	_, err := time.LoadLocation(name)
	return err == nil
}
