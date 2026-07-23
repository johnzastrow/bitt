package version

import (
	"regexp"
	"testing"
)

// The number is a valid semver, so the displayed version is never malformed.
func TestNumberIsSemver(t *testing.T) {
	if !regexp.MustCompile(`^\d+\.\d+\.\d+$`).MatchString(Number) {
		t.Errorf("Number %q is not a bare semver", Number)
	}
}

func TestShort(t *testing.T) {
	if Short() != "v"+Number {
		t.Errorf("Short() = %q, want v%s", Short(), Number)
	}
}

// Full degrades gracefully when the build did not inject provenance, so a local
// build shows something honest rather than "unknown, unknown".
func TestFullDegradesForLocalBuild(t *testing.T) {
	Commit, Date = "unknown", "unknown"
	if got := Full(); got != Short()+" (local build)" {
		t.Errorf("Full() = %q, want a local-build string", got)
	}

	Commit, Date = "abc1234", "2026-07-23"
	if got := Full(); got != "v"+Number+" (abc1234, 2026-07-23)" {
		t.Errorf("Full() = %q, want the injected provenance", got)
	}
	Commit, Date = "unknown", "unknown"
}
