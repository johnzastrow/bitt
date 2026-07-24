package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A file: secret that cannot be read must fail the whole load, not fall back to
// a default. This is the fail-closed guarantee the Phase 5 secrets depend on:
// a mistyped SMTP or tick-secret path must refuse to start, not start blank.
func TestLoadFailsClosedOnUnreadableSecretFile(t *testing.T) {
	t.Setenv("BITT_ADDR", "file:/no/such/secret/file")
	if _, err := Load(); err == nil {
		t.Fatal("Load succeeded with an unreadable file: value; it must fail closed")
	} else if !strings.Contains(err.Error(), "cannot be read") {
		t.Errorf("error does not name the read failure: %v", err)
	}
}

// A readable file: value is used as the setting.
func TestLoadReadsASecretFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "addr")
	if err := os.WriteFile(path, []byte("  :9999\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	t.Setenv("BITT_ADDR", "file:"+path)

	c, err := Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if c.Addr != ":9999" {
		t.Errorf("Addr = %q, want :9999 (trimmed file contents)", c.Addr)
	}
}

// An unset variable keeps its default; the file: fail-closed rule applies only
// when the operator actually named a file.
func TestLoadDefaultsWhenUnset(t *testing.T) {
	for _, k := range []string{"BITT_ADDR", "BITT_DB_PATH", "BITT_TIMEZONE"} {
		t.Setenv(k, "")
		_ = os.Unsetenv(k)
	}
	c, err := Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if c.Addr != ":8080" {
		t.Errorf("Addr default = %q, want :8080", c.Addr)
	}
	if c.DefaultTimezone != "America/New_York" {
		t.Errorf("timezone default = %q, want America/New_York", c.DefaultTimezone)
	}
	if !c.SecureCookies {
		t.Error("SecureCookies should default to true")
	}
}

// A plain (non-file:) value is taken literally, unaffected by the file: path.
func TestLoadReadsPlainValues(t *testing.T) {
	t.Setenv("BITT_ADDR", ":7000")
	t.Setenv("BITT_SECURE_COOKIES", "false")
	c, err := Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if c.Addr != ":7000" || c.SecureCookies {
		t.Errorf("plain values not honoured: addr=%q secure=%v", c.Addr, c.SecureCookies)
	}
}

// An invalid timezone is rejected rather than silently accepted.
func TestLoadRejectsBadTimezone(t *testing.T) {
	t.Setenv("BITT_TIMEZONE", "Mars/Olympus")
	if _, err := Load(); err == nil {
		t.Error("an invalid timezone was accepted")
	}
}
