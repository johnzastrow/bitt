// Package version is the single source of truth for the application version.
//
// Number is a plain constant, so a bare `go build` produces a correctly
// versioned binary with no build tooling. Commit and Date are optional and are
// injected at release time with -ldflags; they stay "unknown" for a local
// build, which is the honest thing to show for one.
//
// Versioning follows semver. Pre-1.0, the minor tracks the delivered phase:
// 0.1 walking skeleton, 0.2 the settle loop, 0.3 recurrence, 0.4 payoff tabs
// and late fees, 0.5 loan terms and scheduled payments, 0.6 account profiles,
// 0.7 notifications, 0.8 ship (Docker, deploy, backup/restore, MariaDB).
// The patch increments for fixes between phases. See CHANGELOG.md.
package version

import "fmt"

// Number is the semantic version. Bump it on every functional change: patch for
// a fix, minor for a feature, major for a break. This is the value to edit.
const Number = "1.1.1"

// Commit and Date are set at build time via -ldflags, e.g.
//
//	-ldflags "-X github.com/johnzastrow/bitt/internal/version.Commit=$(git rev-parse --short HEAD)"
var (
	Commit = "unknown"
	Date   = "unknown"
)

// Short is the version as a person cites it: "v0.7.1".
func Short() string { return "v" + Number }

// Full adds the build provenance for logs and diagnostics, degrading gracefully
// when the ldflags were not supplied.
func Full() string {
	if Commit == "unknown" {
		return Short() + " (local build)"
	}
	return fmt.Sprintf("%s (%s, %s)", Short(), Commit, Date)
}
