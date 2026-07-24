package tz

import (
	"strings"
	"testing"
	"time"
)

func TestZonesAreAllLoadable(t *testing.T) {
	zones := Zones()
	if len(zones) < 300 {
		t.Fatalf("only %d zones offered, expected the full IANA list", len(zones))
	}
	for _, name := range zones {
		if _, err := time.LoadLocation(name); err != nil {
			t.Errorf("offered zone %q cannot be loaded: %v", name, err)
		}
	}
}

// The picker must not offer a zone twice, or a browser's suggestion list shows
// duplicates for the common entries that are also in the alphabetical tail.
func TestZonesHaveNoDuplicates(t *testing.T) {
	seen := make(map[string]bool)
	for _, name := range Zones() {
		if seen[name] {
			t.Errorf("zone %q offered twice", name)
		}
		seen[name] = true
	}
}

// A datalist shows matches in document order, so the common zones have to come
// first for the ordering to do any work.
func TestCommonZonesComeFirst(t *testing.T) {
	zones := Zones()
	if zones[0] != "America/New_York" {
		t.Errorf("first zone is %q, want America/New_York -- it is what a new "+
			"instance is seeded with", zones[0])
	}
	if zones[1] != "UTC" {
		t.Errorf("second zone is %q, want UTC -- the answer for anyone unsure, "+
			"and the server's fallback", zones[1])
	}

	head := zones[:len(commonFirst)]
	for _, want := range []string{"America/New_York", "Europe/London", "Asia/Tokyo"} {
		var found bool
		for _, got := range head {
			if got == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("%q is not in the leading common block", want)
		}
	}

	// And the tail is sorted, so browsing it is predictable.
	tail := zones[len(commonFirst):]
	for i := 1; i < len(tail); i++ {
		if tail[i-1] > tail[i] {
			t.Fatalf("tail is not sorted: %q before %q", tail[i-1], tail[i])
		}
	}
}

// The awkward zones the schedule package is tested against must be offerable,
// or someone in one of them cannot configure the instance correctly.
func TestZonesIncludeTheAwkwardOnes(t *testing.T) {
	// America/Santiago moves its clock at midnight, which is the case
	// schedule.Date.Time walks forward past.
	for _, want := range []string{
		"America/Santiago", "Australia/Lord_Howe", "Asia/Kathmandu", "Pacific/Chatham",
	} {
		if !contains(Zones(), want) {
			t.Errorf("%q is not offered", want)
		}
	}
}

func TestValid(t *testing.T) {
	for _, good := range []string{"UTC", "America/New_York", "Europe/London"} {
		if !Valid(good) {
			t.Errorf("Valid(%q) = false", good)
		}
	}
	for _, bad := range []string{"", "   ", "Not/AZone", "America/Nowhere", "EST5EDT/bogus"} {
		if Valid(bad) {
			t.Errorf("Valid(%q) = true", bad)
		}
	}
}

// Zones is memoized behind a sync.Once; a second call must not rebuild or
// reorder it.
func TestZonesIsStable(t *testing.T) {
	a, b := Zones(), Zones()
	if len(a) != len(b) {
		t.Fatalf("two calls returned %d and %d zones", len(a), len(b))
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("zone %d differs between calls: %q vs %q", i, a[i], b[i])
		}
	}
}

func TestNoBlankOrCommentedEntries(t *testing.T) {
	for _, name := range Zones() {
		if strings.TrimSpace(name) != name || name == "" {
			t.Errorf("zone %q has stray whitespace", name)
		}
		if strings.HasPrefix(name, "#") {
			t.Errorf("comment line %q leaked into the list", name)
		}
	}
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
