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
	c := Config{
		Addr:               envString("BITT_ADDR", ":8080"),
		DBPath:             envString("BITT_DB_PATH", filepath.Join("data", "bitt.db")),
		DefaultTimezone:    envString("BITT_TIMEZONE", defaultTimezone),
		SecureCookies:      envBool("BITT_SECURE_COOKIES", true),
		AppendOnlyTriggers: envBool("BITT_LEDGER_TRIGGERS", true),
		ReadTimeout:        envDuration("BITT_READ_TIMEOUT", 15*time.Second),
		WriteTimeout:       envDuration("BITT_WRITE_TIMEOUT", 30*time.Second),
		ShutdownTimeout:    envDuration("BITT_SHUTDOWN_TIMEOUT", 15*time.Second),
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

// envString reads a string, falling back to a default. A value of the form
// "file:/path" reads the contents of that file instead, so secrets can be
// supplied by mounted file rather than by environment variable (DEPLOY-06).
func envString(key, def string) string {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def
	}
	if path, found := strings.CutPrefix(v, "file:"); found {
		b, err := os.ReadFile(path)
		if err != nil {
			return def
		}
		return strings.TrimSpace(string(b))
	}
	return v
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
