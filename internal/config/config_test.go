package config

import (
	"io"
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

func TestNotifyConfigValidation(t *testing.T) {
	// Email needs a valid From.
	t.Setenv("BITT_SMTP_HOST", "mail.example")
	t.Setenv("BITT_EMAIL_FROM", "")
	if _, err := Load(); err == nil {
		t.Error("SMTP host without a From address was accepted")
	}
	t.Setenv("BITT_EMAIL_FROM", "not an address")
	if _, err := Load(); err == nil {
		t.Error("an invalid From address was accepted")
	}
	t.Setenv("BITT_EMAIL_FROM", "BitTabby <bitt@example.com>")
	if _, err := Load(); err != nil {
		t.Errorf("a valid email config was rejected: %v", err)
	}
}

func TestNtfyMustBeHTTPS(t *testing.T) {
	t.Setenv("BITT_NTFY_URL", "http://ntfy.sh")
	if _, err := Load(); err == nil {
		t.Error("an http ntfy URL was accepted; https is required")
	}
	t.Setenv("BITT_NTFY_URL", "https://ntfy.sh")
	c, err := Load()
	if err != nil {
		t.Fatalf("https ntfy URL rejected: %v", err)
	}
	if !c.Notify.NtfyEnabled() || c.Notify.TickEnabled() {
		t.Error("channel-enabled flags are wrong")
	}
}

func TestLoadReminders(t *testing.T) {
	// Default when unset.
	if r, fromEnv, err := loadReminders(); err != nil || len(r) != 3 || r[0].Days != 14 || fromEnv {
		t.Fatalf("default reminders = %+v, %v", r, err)
	}

	t.Setenv("BITT_REMINDER_DAYS", "30, 7, 7, 1")
	t.Setenv("BITT_REMINDER_BODY", "Owe {amount} on {tab} by {due}")
	t.Setenv("BITT_REMINDER_BODY_1", "Last call: {amount} due tomorrow")
	r, fromEnv, err := loadReminders()
	if err != nil {
		t.Fatalf("loadReminders: %v", err)
	}
	// The environment spoke, so it wins over anything stored in the database.
	if !fromEnv {
		t.Error("loadReminders did not report the environment as the source")
	}
	if len(r) != 3 { // 7 deduped
		t.Fatalf("got %d reminders, want 3 (7 deduped): %+v", len(r), r)
	}
	if r[0].Days != 30 || r[2].Days != 1 {
		t.Errorf("days = %d,%d,%d", r[0].Days, r[1].Days, r[2].Days)
	}
	if r[0].Body != "Owe {amount} on {tab} by {due}" {
		t.Errorf("default body override not applied: %q", r[0].Body)
	}
	if r[2].Body != "Last call: {amount} due tomorrow" {
		t.Errorf("per-day body override not applied: %q", r[2].Body)
	}

	t.Setenv("BITT_REMINDER_DAYS", "0,-3")
	if _, _, err := loadReminders(); err == nil {
		t.Error("a non-positive day count was accepted")
	}
}

// The built-in message is the worked example an administrator edits from, so
// between the title and the body it must use every variable there is. A new
// variable added without a place in the default leaves it undemonstrated.
func TestDefaultReminderUsesEveryVariable(t *testing.T) {
	both := defaultReminderTitle + "\n" + defaultReminderBody
	for _, v := range []string{"{tab}", "{amount}", "{due}", "{days}", "{when}", "{url}"} {
		if !strings.Contains(both, v) {
			t.Errorf("the default reminder never uses %s:\ntitle: %s\nbody:  %s",
				v, defaultReminderTitle, defaultReminderBody)
		}
	}
}

// The loose-secret warning has to understand directories, or it fires on the
// deployment this project documents.
//
// Docker bind-mounts a secret file into the container with the host's
// ownership, and the container runs as a different non-root user -- so the file
// must be group/other-readable for the app to read it at all. Inside a 0700
// directory that is not an exposed secret, and warning about it would train an
// operator to ignore the warning that matters.
func TestSecretFileWarningConsidersTheDirectory(t *testing.T) {
	cases := []struct {
		name     string
		dirMode  os.FileMode
		fileMode os.FileMode
		wantWarn bool
	}{
		{"owner-only file, owner-only dir", 0o700, 0o600, false},
		{"owner-only file, open dir", 0o755, 0o600, false},
		{"readable file inside a closed dir", 0o700, 0o644, false},
		{"readable file inside an open dir", 0o755, 0o644, true},
		{"world-writable file inside an open dir", 0o755, 0o666, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "secrets")
			if err := os.Mkdir(dir, tc.dirMode); err != nil {
				t.Fatalf("mkdir: %v", err)
			}
			// Mkdir applies the umask, so set the mode explicitly.
			if err := os.Chmod(dir, tc.dirMode); err != nil {
				t.Fatalf("chmod dir: %v", err)
			}
			path := filepath.Join(dir, "tick_secret")
			if err := os.WriteFile(path, []byte("s3cret\n"), tc.fileMode); err != nil {
				t.Fatalf("write: %v", err)
			}
			if err := os.Chmod(path, tc.fileMode); err != nil {
				t.Fatalf("chmod file: %v", err)
			}

			warned := captureStderr(t, func() { warnIfSecretFileLoose("BITT_TICK_SECRET", path) })
			if got := strings.Contains(warned, "warning"); got != tc.wantWarn {
				t.Errorf("warned = %v, want %v (dir %#o, file %#o): %q",
					got, tc.wantWarn, tc.dirMode, tc.fileMode, warned)
			}
		})
	}
}

// captureStderr runs fn with os.Stderr redirected and returns what it wrote.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	saved := os.Stderr
	os.Stderr = w
	fn()
	os.Stderr = saved
	_ = w.Close()

	var buf strings.Builder
	if _, err := io.Copy(&buf, r); err != nil {
		t.Fatalf("read: %v", err)
	}
	_ = r.Close()
	return buf.String()
}
