// Package config loads deployment settings from the environment.
//
// DEPLOY-06: every setting arrives from the environment or from a file the
// environment points at. Nothing is compiled in, and nothing sensitive is
// written to a log.
package config

import (
	"fmt"
	"net/mail"
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
	// BaseURL is the external origin used to build links in notifications, e.g.
	// "https://bitt.example.com". It is NOT derived from a request Host header:
	// the only caller that needs it is the notification sender, which runs off
	// a cron request whose Host is whatever the cron sent, and a link built from
	// an attacker-controlled Host is a phishing vector. Empty means links are
	// omitted from notifications rather than guessed.
	BaseURL string
	// Notify holds the notification delivery settings (Phase 5). A zero value
	// means notifications are configured off, which is a valid way to run.
	Notify NotifyConfig
	// Reminders are the payment-reminder lead times and their message
	// templates, in send order. Defaults to 14/7/1 days before the due date
	// with a built-in message; each is overridable.
	Reminders []Reminder
}

// Reminder is one payment-reminder rule: how many days before a due date it
// fires, and the title and body templates for its message.
//
// Templates are ADMIN-configured (from the environment), so they are trusted
// text. They interpolate a small set of {variables} filled per send:
//
//	{tab}     the tab name
//	{amount}  the amount owed, e.g. "$505.65"
//	{due}     the due date, e.g. "Aug 1, 2026"
//	{days}    the lead time in days, e.g. "7"
//	{when}    a phrase for the lead time, e.g. "in one week" / "tomorrow"
//	{url}     a link to the tab's payment page (empty if BITT_BASE_URL is unset)
//
// A tab name substituted into a title is still run through the sender's header
// check, so a control character in it fails the send closed rather than
// injecting a header.
type Reminder struct {
	Days  int
	Title string
	Body  string
}

// The built-in reminder message. Between them the two templates use every
// variable there is, so the shipped default doubles as the worked example an
// administrator edits from: nobody has to guess what {days} looks like beside
// {when}, or where {url} lands, before writing their own.
//
// Each variable earns its place rather than being there to be demonstrated. The
// title is what a phone shows on a locked screen, so it carries the tab, the
// amount, and how soon -- enough to act on without opening anything. The body
// names the lead time it is ({days}), says the same "when" in words and as a
// date, restates the amount now that there is room for it, and ends on the link.
const (
	defaultReminderTitle = "{tab}: {amount} due {when}"
	defaultReminderBody  = "Your {days}-day reminder: a payment on the tab \"{tab}\" is due {when}, on {due}.\n" +
		"{amount} is owed.\n{url}"
)

// NotifyConfig is the delivery configuration for notifications.
//
// Each channel is independent and each is off until configured: email sends
// only when an SMTP host is set, ntfy only when a base URL is set. The cron
// endpoint is authenticated only when TickSecret is set, and refuses all
// requests otherwise (fail closed) -- it is never open.
type NotifyConfig struct {
	// TickSecret authenticates the external cron to /internal/tick, presented
	// as "Authorization: Bearer <secret>". Empty means the endpoint fails closed
	// and no cron can drive delivery. Supply via BITT_TICK_SECRET, ideally
	// file:/run/secrets/... in a container.
	TickSecret string

	// Email (SMTP).
	SMTPHost     string
	SMTPPort     int
	SMTPUsername string
	SMTPPassword string
	// EmailFrom is the envelope/header From address. Required when SMTPHost is
	// set; validated as a real address so it cannot carry a header injection.
	EmailFrom string

	// NtfyBaseURL is the admin-pinned ntfy server, e.g. "https://ntfy.sh".
	// Users choose only a topic, never a URL (the SSRF decision, D1). Empty
	// means ntfy delivery is off.
	NtfyBaseURL string
	// NtfyToken is an optional bearer token for a private ntfy server.
	NtfyToken string
}

// EmailEnabled reports whether email delivery is configured.
func (n NotifyConfig) EmailEnabled() bool { return n.SMTPHost != "" }

// NtfyEnabled reports whether ntfy delivery is configured.
func (n NotifyConfig) NtfyEnabled() bool { return n.NtfyBaseURL != "" }

// TickEnabled reports whether the cron endpoint has a secret and will accept an
// authenticated request. When false the endpoint fails closed.
func (n NotifyConfig) TickEnabled() bool { return n.TickSecret != "" }

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
		BaseURL:            strings.TrimRight(ld.str("BITT_BASE_URL", ""), "/"),
		Notify: NotifyConfig{
			TickSecret:   ld.str("BITT_TICK_SECRET", ""),
			SMTPHost:     ld.str("BITT_SMTP_HOST", ""),
			SMTPPort:     envInt("BITT_SMTP_PORT", 587),
			SMTPUsername: ld.str("BITT_SMTP_USERNAME", ""),
			SMTPPassword: ld.str("BITT_SMTP_PASSWORD", ""),
			EmailFrom:    ld.str("BITT_EMAIL_FROM", ""),
			NtfyBaseURL:  strings.TrimRight(ld.str("BITT_NTFY_URL", ""), "/"),
			NtfyToken:    ld.str("BITT_NTFY_TOKEN", ""),
		},
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
	if err := c.Notify.validate(); err != nil {
		return Config{}, err
	}
	rem, err := loadReminders()
	if err != nil {
		return Config{}, err
	}
	c.Reminders = rem
	if c.BaseURL != "" && !strings.HasPrefix(c.BaseURL, "http://") && !strings.HasPrefix(c.BaseURL, "https://") {
		return Config{}, fmt.Errorf("config: BITT_BASE_URL %q must start with http:// or https://", c.BaseURL)
	}
	return c, nil
}

// validate rejects a partial or unsafe notification configuration at startup,
// so a misconfiguration fails to boot rather than silently mis-sending.
func (n NotifyConfig) validate() error {
	if n.EmailEnabled() {
		if n.EmailFrom == "" {
			return fmt.Errorf("config: BITT_SMTP_HOST is set but BITT_EMAIL_FROM is empty")
		}
		// The From address becomes a mail header; a control character in it is
		// header injection. net/mail parses and rejects that.
		if _, err := mail.ParseAddress(n.EmailFrom); err != nil {
			return fmt.Errorf("config: BITT_EMAIL_FROM %q is not a valid address: %w", n.EmailFrom, err)
		}
		if n.SMTPPort <= 0 || n.SMTPPort > 65535 {
			return fmt.Errorf("config: BITT_SMTP_PORT %d is out of range", n.SMTPPort)
		}
	}
	if n.NtfyEnabled() {
		// The pinned ntfy URL must be https: an http endpoint sends the topic
		// (and any token) in the clear, and https is the first line of the SSRF
		// guard. Localhost over http is the one exception a dev build might want,
		// but the safe default refuses it.
		if !strings.HasPrefix(n.NtfyBaseURL, "https://") {
			return fmt.Errorf("config: BITT_NTFY_URL %q must be https://", n.NtfyBaseURL)
		}
	}
	return nil
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

// loadReminders builds the reminder rules from the environment. BITT_REMINDER_DAYS
// is a comma-separated list of positive day counts (default "14,7,1"); each day
// may carry its own BITT_REMINDER_TITLE_<d> / BITT_REMINDER_BODY_<d>, falling
// back to BITT_REMINDER_TITLE / BITT_REMINDER_BODY, then to the built-ins.
func loadReminders() ([]Reminder, error) {
	days := []int{14, 7, 1}
	if raw := strings.TrimSpace(os.Getenv("BITT_REMINDER_DAYS")); raw != "" {
		days = nil
		seen := map[int]bool{}
		for _, part := range strings.Split(raw, ",") {
			p := strings.TrimSpace(part)
			if p == "" {
				continue
			}
			n, err := strconv.Atoi(p)
			if err != nil || n <= 0 || n > 3650 {
				return nil, fmt.Errorf("config: BITT_REMINDER_DAYS has an invalid day %q (want a positive number of days)", p)
			}
			if !seen[n] {
				seen[n] = true
				days = append(days, n)
			}
		}
		if len(days) == 0 {
			return nil, fmt.Errorf("config: BITT_REMINDER_DAYS is set but lists no days")
		}
	}
	defTitle := envOr("BITT_REMINDER_TITLE", defaultReminderTitle)
	defBody := envOr("BITT_REMINDER_BODY", defaultReminderBody)
	out := make([]Reminder, 0, len(days))
	for _, d := range days {
		out = append(out, Reminder{
			Days:  d,
			Title: envOr(fmt.Sprintf("BITT_REMINDER_TITLE_%d", d), defTitle),
			Body:  envOr(fmt.Sprintf("BITT_REMINDER_BODY_%d", d), defBody),
		})
	}
	return out, nil
}

// envOr returns the environment value or a default. It is the plain-value
// sibling of loader.str, for non-secret settings that never use file:.
func envOr(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil {
		return def
	}
	return n
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
