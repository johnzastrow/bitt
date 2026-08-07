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
	// DBDriver selects the storage backend: "sqlite" (default) or "mariadb"
	// (DEPLOY-03). Set via BITT_DB_DRIVER.
	DBDriver string
	// DBPath is the SQLite database file. Used when DBDriver is sqlite.
	DBPath string
	// DBDSN is the MariaDB connection string, e.g.
	// "bitt:pass@tcp(db:3306)/bitt". Used when DBDriver is mariadb. It carries a
	// password, so it accepts the file: form and is never logged.
	DBDSN string
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
	// Reminders are the INSTANCE-WIDE payment-reminder lead times and their
	// message templates, in send order. Defaults to 14/7/1 days before the due date
	// and 1/7 days after it (overdue notices, held as negative lead times);
	// each is overridable by environment.
	//
	// These are the fallback layer. A tab whose Provider has set its own
	// reminders (store.TabReminder, migration 0009) uses those instead, and
	// ignores this list entirely -- see Server.reminderForTab.
	Reminders []Reminder
	// RemindersFromEnv reports whether the environment actually specified the
	// reminders, as against Reminders holding the built-in default.
	//
	// The distinction is load-bearing, and it is the reason this field exists
	// rather than a len() check: an administrator can also set the defaults
	// through the interface (migration 0010), and the environment wins. Without
	// this flag the built-in fallback is indistinguishable from a deliberate
	// environment setting, and the stored defaults would never be reachable.
	RemindersFromEnv bool
}

// Reminder is one payment-reminder rule: how many days before a due date it
// fires, and the title and body templates for its message.
//
// An instance-wide Reminder is ADMIN-configured (from the environment), so its
// templates are trusted text. The per-tab override is not: store.TabReminder
// carries the same fields from a Provider, who is an ordinary user, and the
// handler that accepts them validates both against notify.ValidTitleTemplate
// and notify.ValidBodyTemplate before storing.
//
// Either way the templates interpolate the same small set of {variables},
// filled per send:
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

	// The overdue wording is past tense throughout, because a notice that says
	// a payment "is due yesterday" reads as a bug.
	defaultOverdueTitle = "{tab}: {amount} was due {when}"
	defaultOverdueBody  = "A payment on the tab \"{tab}\" was due {when}, on {due}.\n" +
		"{amount} is still owed.\n{url}"
)

// DefaultReminders is the built-in reminder set: 14, 7, and 1 days before a due
// date, all carrying the built-in message.
//
// It is what applies when neither the environment nor the database says
// otherwise, and it is exported so that the resolution order has one definition
// of "the built-in default" rather than a constant here and a literal wherever
// the fallback is needed.
func DefaultReminders() []Reminder {
	days := []int{14, 7, 1}
	out := make([]Reminder, 0, len(days)+len(defaultOverdueDays))
	for _, d := range days {
		out = append(out, Reminder{Days: d, Title: defaultReminderTitle, Body: defaultReminderBody})
	}
	// Overdue notices, as negative lead times (decision D-F). They ship on
	// rather than waiting to be discovered: a feature nobody switches on is a
	// feature that does not exist. Two notices then silence -- the cadence IS
	// the cap, so there is no separate stop-nagging rule and no unbounded
	// dunning loop. An administrator can edit them, add a third, or clear them
	// to turn overdue off entirely.
	for _, d := range defaultOverdueDays {
		out = append(out, Reminder{Days: d, Title: defaultOverdueTitle, Body: defaultOverdueBody})
	}
	return out
}

// defaultOverdueDays are the built-in overdue lead times: the morning after,
// and a week later.
var defaultOverdueDays = []int{-1, -7}

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

	// LookbackDays bounds how far back the scan will look for an event it has
	// not yet announced. Event notices are derived from the ledger rather than
	// queued, so without a floor a long cron outage would eventually announce
	// history. A week-old payment notice is noise, not news: past this age an
	// event is dropped, permanently and deliberately.
	LookbackDays int

	// MaxPerTick bounds the deliveries one tick may perform, so an outage does
	// not become a burst across every tab at once. Nothing is lost when it
	// bites: no claim is written for an undelivered event, so the next tick
	// picks it up. Zero or negative means unbounded.
	MaxPerTick int
}

// Lookback is LookbackDays as a duration, for comparing against entry
// timestamps.
func (n NotifyConfig) Lookback() time.Duration {
	return time.Duration(n.LookbackDays) * 24 * time.Hour
}

// Capped reports whether a per-tick ceiling is in force.
func (n NotifyConfig) Capped() bool { return n.MaxPerTick > 0 }

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
		DBDriver:           ld.str("BITT_DB_DRIVER", "sqlite"),
		DBPath:             ld.str("BITT_DB_PATH", filepath.Join("data", "bitt.db")),
		DBDSN:              ld.str("BITT_DB_DSN", ""),
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
			LookbackDays: envInt("BITT_NOTIFY_LOOKBACK_DAYS", 7),
			MaxPerTick:   envInt("BITT_NOTIFY_MAX_PER_TICK", 50),
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
	switch c.DBDriver {
	case "", "sqlite":
		c.DBDriver = "sqlite"
		if c.DBPath == "" {
			return Config{}, fmt.Errorf("config: BITT_DB_PATH must not be empty")
		}
	case "mariadb", "mysql":
		c.DBDriver = "mariadb"
		if c.DBDSN == "" {
			return Config{}, fmt.Errorf("config: BITT_DB_DRIVER=mariadb requires BITT_DB_DSN")
		}
	default:
		return Config{}, fmt.Errorf("config: BITT_DB_DRIVER %q is not supported (want sqlite or mariadb)", c.DBDriver)
	}
	if err := c.Notify.validate(); err != nil {
		return Config{}, err
	}
	rem, remFromEnv, err := loadReminders()
	if err != nil {
		return Config{}, err
	}
	c.Reminders, c.RemindersFromEnv = rem, remFromEnv
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
	// MariaDB keeps its data inside a server; there is no local directory to
	// create, and DBPath is unset.
	if c.DBDriver == "mariadb" {
		return nil
	}
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

// warnIfSecretFileLoose notes a file: secret that is genuinely readable beyond
// its owner -- meaning both the file's own bits allow it AND the directory
// holding the file can be traversed to reach it.
//
// The directory half is not pedantry, it is what makes the warning worth
// reading. Docker bind-mounts a secret file into the container with the host's
// ownership, and the container runs as a different, non-root user -- so the
// documented deployment needs a file the owner is not the reader of, which in
// practice means group/other-readable bits inside a 0700 directory. That is not
// an exposed secret: no other account on the host can traverse the directory to
// reach it. Warning about it anyway would fire on the setup this project's own
// compose file tells people to use, and a warning that fires on correct
// configuration is one operators learn to scroll past.
//
// It warns rather than refuses, because the file:/run/secrets/name convention
// (Docker, Kubernetes) also mounts secrets world-readable inside an isolated
// namespace, and a hard refusal would break the deployment the feature exists
// for.
//
// Only the immediate parent is checked. A full walk to the root would catch a
// loose grandparent, but the common mistake by a wide margin is a
// world-readable file in a directory nobody thought about, and that is what
// this catches.
func warnIfSecretFileLoose(key, path string) {
	info, err := os.Stat(path)
	if err != nil {
		return
	}
	if info.Mode().Perm()&0o077 == 0 {
		return
	}
	// A directory another account cannot enter is one they cannot read a file
	// out of, whatever that file's own bits say.
	if dir, err := os.Stat(filepath.Dir(path)); err == nil && dir.Mode().Perm()&0o055 == 0 {
		return
	}
	fmt.Fprintf(os.Stderr,
		"config: warning: %s secret file %q is readable by group or others (%#o), in a directory they can enter; prefer 0600, or 0700 on the directory\n",
		key, path, info.Mode().Perm())
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
// It also reports whether the environment said anything at all, which is what
// decides the precedence against reminders stored in the database.
func loadReminders() ([]Reminder, bool, error) {
	// Any BITT_REMINDER_* with a value counts, per-day overrides included, so
	// the whole environment is scanned rather than three known names.
	fromEnv := false
	for _, kv := range os.Environ() {
		name, value, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(name, "BITT_REMINDER_") && strings.TrimSpace(value) != "" {
			fromEnv = true
			break
		}
	}

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
				return nil, false, fmt.Errorf("config: BITT_REMINDER_DAYS has an invalid day %q (want a positive number of days)", p)
			}
			if !seen[n] {
				seen[n] = true
				days = append(days, n)
			}
		}
		if len(days) == 0 {
			return nil, false, fmt.Errorf("config: BITT_REMINDER_DAYS is set but lists no days")
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
	return out, fromEnv, nil
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
