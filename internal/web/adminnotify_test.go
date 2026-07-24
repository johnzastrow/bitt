package web

import (
	"net/url"
	"strings"
	"testing"

	"github.com/johnzastrow/bitt/internal/config"
	"github.com/johnzastrow/bitt/internal/store"
)

// The environment wins over stored delivery settings, field by field.
//
// This is the rule that keeps a container reproducible: bring it up with the
// same compose file and it behaves the same way, whatever is in the volume. If
// it ever inverts, a stale value typed into a form months ago silently takes
// over a deployment.
func TestDeliveryEnvBeatsStored(t *testing.T) {
	h := newHarnessCfg(t, config.NotifyConfig{
		SMTPHost:  "env.example",
		SMTPPort:  2525,
		EmailFrom: "BitTabby <env@example.com>",
	})
	h.completeSetup()
	ctx := t.Context()

	if err := h.db.SetDelivery(ctx, store.Delivery{
		SMTPHost:     "stored.example",
		SMTPPort:     587,
		SMTPUsername: "stored-user",
		EmailFrom:    "stored@example.com",
		NtfyBaseURL:  "https://ntfy.example",
	}); err != nil {
		t.Fatalf("set delivery: %v", err)
	}

	got := h.srv().notifyConfig(ctx)
	if got.SMTPHost != "env.example" {
		t.Errorf("SMTP host = %q, want the environment's", got.SMTPHost)
	}
	if got.SMTPPort != 2525 {
		t.Errorf("SMTP port = %d, want the environment's 2525 -- the port belongs to the host", got.SMTPPort)
	}
	if got.EmailFrom != "BitTabby <env@example.com>" {
		t.Errorf("from address = %q, want the environment's", got.EmailFrom)
	}
	// Fields the environment is silent on fall through to the stored value.
	if got.SMTPUsername != "stored-user" {
		t.Errorf("username = %q, want the stored one -- the environment did not set it", got.SMTPUsername)
	}
	if got.NtfyBaseURL != "https://ntfy.example" {
		t.Errorf("ntfy URL = %q, want the stored one", got.NtfyBaseURL)
	}
}

// With the environment silent, the stored settings are what deliver -- and they
// take effect without a restart, which is the whole point of the screen.
func TestDeliveryStoredAppliesWithoutRestart(t *testing.T) {
	h := newHarness(t) // no notify config in the environment
	h.completeSetup()
	ctx := t.Context()

	if h.srv().notifyReady(ctx) {
		t.Fatal("a fresh instance reports delivery as ready")
	}

	if err := h.db.SetDelivery(ctx, store.Delivery{NtfyBaseURL: "https://ntfy.example"}); err != nil {
		t.Fatalf("set delivery: %v", err)
	}
	// No restart, no rebuild of the server: the next read resolves it.
	if !h.srv().notifyReady(ctx) {
		t.Error("a stored ntfy URL did not enable delivery")
	}
	if got := h.srv().notifyConfig(ctx); !got.NtfyEnabled() {
		t.Errorf("effective config = %+v, want ntfy enabled", got)
	}
}

// Secrets are environment-only. There is no column for them and no way to post
// one, and this test is here so that adding either has to break something.
func TestDeliveryNeverStoresSecrets(t *testing.T) {
	h := newHarnessCfg(t, config.NotifyConfig{SMTPPassword: "env-password", NtfyToken: "env-token"})
	h.completeSetup()
	ctx := t.Context()

	_, body := h.post("/admin/notifications/delivery", url.Values{
		"csrf_token":    {h.csrfToken("/admin/notifications")},
		"smtp_host":     {"mail.example"},
		"smtp_port":     {"587"},
		"email_from":    {"BitTabby <bitt@example.com>"},
		"smtp_password": {"typed-into-the-form"},
		"ntfy_token":    {"typed-into-the-form"},
		"tick_secret":   {"typed-into-the-form"},
	})
	if !strings.Contains(body, "Delivery settings saved") {
		t.Fatalf("save failed: %s", truncate(body))
	}

	// The credentials in force are still the environment's.
	got := h.srv().notifyConfig(ctx)
	if got.SMTPPassword != "env-password" || got.NtfyToken != "env-token" {
		t.Errorf("a form field changed a credential: password=%q token=%q", got.SMTPPassword, got.NtfyToken)
	}

	// And nothing resembling a secret reached the database.
	inst, err := h.db.GetInstance(ctx)
	if err != nil {
		t.Fatalf("get instance: %v", err)
	}
	blob := inst.Delivery.SMTPHost + inst.Delivery.SMTPUsername + inst.Delivery.EmailFrom + inst.Delivery.NtfyBaseURL
	if strings.Contains(blob, "typed-into-the-form") {
		t.Errorf("a posted secret was stored: %+v", inst.Delivery)
	}
}

// A field the environment owns renders read-only -- and a read-only input still
// posts its value, so saving the form must not copy the environment's settings
// into the database, where they would silently take over the day the variable
// was unset.
func TestDeliverySaveDoesNotCaptureEnvValues(t *testing.T) {
	h := newHarnessCfg(t, config.NotifyConfig{SMTPHost: "env.example", EmailFrom: "env@example.com"})
	h.completeSetup()
	ctx := t.Context()

	_, body := h.post("/admin/notifications/delivery", url.Values{
		"csrf_token": {h.csrfToken("/admin/notifications")},
		"smtp_host":  {"env.example"},          // what the read-only field posts back
		"email_from": {"env@example.com"},      // likewise
		"ntfy_url":   {"https://ntfy.example"}, // the one field actually being edited
	})
	if !strings.Contains(body, "Delivery settings saved") {
		t.Fatalf("save failed: %s", truncate(body))
	}

	inst, err := h.db.GetInstance(ctx)
	if err != nil {
		t.Fatalf("get instance: %v", err)
	}
	if inst.Delivery.SMTPHost != "" || inst.Delivery.EmailFrom != "" {
		t.Errorf("the environment's settings were copied into the database: %+v", inst.Delivery)
	}
	if inst.Delivery.NtfyBaseURL != "https://ntfy.example" {
		t.Errorf("the edited field did not save: %+v", inst.Delivery)
	}
}

// Delivery settings are validated on the way in, to the same rules the
// environment is held to -- a value accepted here must not be one that would
// have refused to start.
func TestDeliveryValidation(t *testing.T) {
	h := newHarness(t)
	h.completeSetup()

	cases := []struct {
		name       string
		form       url.Values
		wantPhrase string
	}{
		{"a server with no from address", url.Values{"smtp_host": {"mail.example"}}, "needs a from address"},
		{"an invalid from address", url.Values{
			"smtp_host": {"mail.example"}, "email_from": {"not an address"},
		}, "not a valid email address"},
		{"a from address carrying a newline", url.Values{
			"smtp_host": {"mail.example"}, "email_from": {"a@b.com\nBcc: c@d.com"},
		}, "not a valid email address"},
		{"a URL where a hostname belongs", url.Values{
			"smtp_host": {"smtp://mail.example/"}, "email_from": {"a@b.com"},
		}, "no scheme, path, or spaces"},
		{"a plain-http ntfy server", url.Values{"ntfy_url": {"http://ntfy.example"}}, "must use https"},
		{"an ntfy value that is not a URL", url.Values{"ntfy_url": {":::"}}, "must be a URL"},
		{"an out-of-range port", url.Values{"smtp_port": {"70000"}}, "between 1 and 65535"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			form := url.Values{"csrf_token": {h.csrfToken("/admin/notifications")}}
			for k, v := range tc.form {
				form[k] = v
			}
			_, body := h.post("/admin/notifications/delivery", form)
			if !strings.Contains(body, tc.wantPhrase) {
				t.Errorf("no refusal mentioning %q: %s", tc.wantPhrase, truncate(body))
			}
		})
	}
}

// The instance default reminders resolve environment first, then stored, then
// the built-in set.
func TestInstanceRemindersPrecedence(t *testing.T) {
	h := newHarness(t)
	h.completeSetup()
	ctx := t.Context()

	// Nothing set anywhere: the built-in default, which the config already holds.
	if got := h.srv().instanceReminders(ctx); len(got) != 3 || got[0].Days != 14 {
		t.Fatalf("built-in default = %+v, want 14/7/1", got)
	}

	if err := h.db.SetInstanceReminders(ctx, []store.TabReminder{
		{Days: 5, Title: "stored", Body: "stored"},
	}); err != nil {
		t.Fatalf("set: %v", err)
	}
	got := h.srv().instanceReminders(ctx)
	if len(got) != 1 || got[0].Days != 5 || got[0].Title != "stored" {
		t.Fatalf("stored defaults not used: %+v", got)
	}

	// The environment having spoken outranks the stored set.
	h.srv().cfg.RemindersFromEnv = true
	h.srv().cfg.Reminders = []config.Reminder{{Days: 9, Title: "env", Body: "env"}}
	if got := h.srv().instanceReminders(ctx); len(got) != 1 || got[0].Days != 9 {
		t.Errorf("the environment did not win: %+v", got)
	}
}

// The reminder form saves, and refuses to save when the environment owns them.
func TestAdminRemindersForm(t *testing.T) {
	h := newHarness(t)
	h.completeSetup()

	_, body := h.get("/admin/notifications")
	if !strings.Contains(body, "Default reminders") {
		t.Fatalf("no notifications screen: %s", truncate(body))
	}
	// Credentials are reported, never rendered.
	if !strings.Contains(body, "BITT_SMTP_PASSWORD") {
		t.Errorf("credential status not shown: %s", truncate(body))
	}

	_, body = h.post("/admin/notifications/reminders", url.Values{
		"csrf_token":     {h.csrfToken("/admin/notifications")},
		"reminder_days":  {"20, 5"},
		"reminder_title": {"{tab} due {when}"},
		"reminder_body":  {"{amount} is owed on {due}.\r\n{days} days. {url}"},
	})
	if !strings.Contains(body, "Default reminders saved") {
		t.Fatalf("save failed: %s", truncate(body))
	}
	got, err := h.db.ListInstanceReminders(t.Context())
	if err != nil || len(got) != 2 || got[0].Days != 20 || got[1].Days != 5 {
		t.Fatalf("stored %+v (%v), want 20 and 5", got, err)
	}
	if strings.Contains(got[0].Body, "\r") {
		t.Errorf("a browser's CRLF was stored: %q", got[0].Body)
	}

	// With the environment in charge, the post is refused rather than written to
	// a table nothing reads.
	h.srv().cfg.RemindersFromEnv = true
	_, body = h.post("/admin/notifications/reminders", url.Values{
		"csrf_token":     {h.csrfToken("/admin/notifications")},
		"reminder_days":  {"1"},
		"reminder_title": {"T"},
		"reminder_body":  {"B"},
	})
	if !strings.Contains(body, "set in the environment") {
		t.Errorf("an environment-owned save was not refused: %s", truncate(body))
	}
	if got, _ := h.db.ListInstanceReminders(t.Context()); len(got) != 2 {
		t.Errorf("the refused save still wrote: %+v", got)
	}
}

// The whole screen is administrator-only.
func TestAdminNotifyRequiresAdmin(t *testing.T) {
	h := newHarness(t)
	h.completeSetup()
	h.addUser("plain@example.com", "Plain", false)
	h.loginAs("plain@example.com", "a-long-enough-password")

	resp, body := h.get("/admin/notifications")
	if resp.StatusCode == 200 && strings.Contains(body, "Default reminders") {
		t.Errorf("a non-admin reached the notifications screen: %s", truncate(body))
	}

	_, body = h.post("/admin/notifications/delivery", url.Values{
		"csrf_token": {h.csrfToken("/")},
		"ntfy_url":   {"https://evil.example"},
	})
	_ = body
	inst, err := h.db.GetInstance(t.Context())
	if err != nil {
		t.Fatalf("get instance: %v", err)
	}
	if inst.Delivery.NtfyBaseURL != "" {
		t.Errorf("a non-admin changed the ntfy server: %+v", inst.Delivery)
	}
}
