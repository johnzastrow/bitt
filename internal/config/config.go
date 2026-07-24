// Package config loads deployment settings from the environment.
//
// DEPLOY-06: every setting arrives from the environment or from a file the
// environment points at. Nothing is compiled in, and nothing sensitive is
// written to a log.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Config is the resolved deployment configuration.
// defaultTimezone seeds a new instance when BITT_TIMEZONE is not set.
//
// It only ever pre-fills the first-run form, which the operator can change
// before submitting, and it is not a fallback: a stored zone that fails to load
// falls back to UTC, because a broken value should degrade to something neutral
// rather than to a guess that silently shifts every boundary five hours.
const defaultTimezone = "America/New_York"

type Config struct {
	// Addr is the listen address, e.g. ":8080".
	Addr string
	// DBPath is the SQLite database file.
	DBPath string
	// DefaultTimezone seeds the instance timezone at first-run setup. After
	// setup the value stored in the database is authoritative.
	DefaultTimezone string
	// SecureCookies sets the Secure flag on session and CSRF cookies. Defaults
	// to true; set BITT_SECURE_COOKIES=false only for local plain-HTTP work.
	SecureCookies bool
	// AppendOnlyTriggers enables the LEDGER-01 database triggers. Defaults to
	// true; disable only for development or manual repair.
	AppendOnlyTriggers bool
	// ReadTimeout and WriteTimeout bound request handling so a stalled client
	// cannot hold a connection indefinitely.
	ReadTimeout  time.Duration
	WriteTimeout time.Duration
	// ShutdownTimeout bounds graceful shutdown.
	ShutdownTimeout time.Duration
}

// Load reads configuration from the environment, applying defaults.
func Load() (Config, error) {
	var ld loader
	c := Config{
		Addr:               ld.str("BITT_ADDR", ":8080"),
		DBPath:             ld.str("BITT_DB_PATH", filepath.Join("data", "bitt.db")),
		DefaultTimezone:    ld.str("BITT_TIMEZONE", defaultTimezone),
		SecureCookies:      envBool("BITT_SECURE_COOKIES", true),
		AppendOnlyTriggers: envBool("BITT_LEDGER_TRIGGERS", true),
		ReadTimeout:        envDuration("BITT_READ_TIMEOUT", 15*time.Second),
		WriteTimeout:       envDuration("BITT_WRITE_TIMEOUT", 30*time.Second),
		ShutdownTimeout:    envDuration("BITT_SHUTDOWN_TIMEOUT", 15*time.Second),
	}
	// A file: read that fails is a misconfiguration, not a reason to fall back
	// to a default. Surfacing it here is what makes a secret fail CLOSED: a
	// mistyped or unreadable secret path refuses to start rather than starting
	// with a blank credential that looks like "not configured". This matters
	// most for the Phase 5 secrets (SMTP, ntfy, the /internal/tick secret),
	// where a silent empty value would defeat the tick endpoint's own
	// fail-closed guard.
	if ld.err != nil {
		return Config{}, ld.err
	}

	if _, err := time.LoadLocation(c.DefaultTimezone); err != nil {
		return Config{}, fmt.Errorf("config: BITT_TIMEZONE %q is not a known timezone: %w", c.DefaultTimezone, err)
	}
	if c.Addr == "" {
		return Config{}, fmt.Errorf("config: BITT_ADDR must not be empty")
	}
	if c.DBPath == "" {
		return Config{}, fmt.Errorf("config: BITT_DB_PATH must not be empty")
	}
	return c, nil
}

// EnsureDataDir creates the database's parent directory with owner-only
// permissions, since it holds financial records.
func (c Config) EnsureDataDir() error {
	dir := filepath.Dir(c.DBPath)
	if dir == "" || dir == "." {
		return nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("config: create data directory %q: %w", dir, err)
	}
	return nil
}

// loader reads string settings, remembering the first error so Load can fail
// as a whole. It exists so an unreadable file: secret is a hard error rather
// than a silent fall-back to a default.
type loader struct {
	err error
}

// str reads a string, falling back to a default when the variable is unset. A
// value of the form "file:/path" reads the contents of that file instead, so
// secrets can be supplied by a mounted file rather than an environment variable
// (DEPLOY-06). A file: path that cannot be read records an error: an operator
// who names a file means that file, and a failure there must fail closed, not
// degrade to the default.
func (l *loader) str(key, def string) string {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def
	}
	path, isFile := strings.CutPrefix(v, "file:")
	if !isFile {
		return v
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if l.err == nil {
			l.err = fmt.Errorf("config: %s names file %q, which cannot be read: %w", key, path, err)
		}
		return def
	}
	warnIfSecretFileLoose(key, path)
	return strings.TrimSpace(string(b))
}

// warnIfSecretFileLoose notes a file: secret that is readable beyond its owner.
//
// It warns rather than refuses: the documented file:/run/secrets/name
// convention (Docker, Kubernetes) mounts secret files world-readable inside an
// isolated namespace, so a hard refusal would break the very deployment the
// feature exists for. The app reads the file rather than owning it, so a
// warning plus the 0600 note in .env.example is the right ceiling.
func warnIfSecretFileLoose(key, path string) {
	info, err := os.Stat(path)
	if err != nil {
		return
	}
	if info.Mode().Perm()&0o077 != 0 {
		fmt.Fprintf(os.Stderr,
			"config: warning: %s secret file %q is readable by group or others (%#o); prefer 0600\n",
			key, path, info.Mode().Perm())
	}
}

func envBool(key string, def bool) bool {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def
	}
	b, err := strconv.ParseBool(strings.TrimSpace(v))
	if err != nil {
		return def
	}
	return b
}

func envDuration(key string, def time.Duration) time.Duration {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def
	}
	d, err := time.ParseDuration(strings.TrimSpace(v))
	if err != nil {
		return def
	}
	return d
}
