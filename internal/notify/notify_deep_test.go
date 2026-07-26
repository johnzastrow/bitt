package notify

import (
	"context"
	"errors"
	"net/smtp"
	"strings"
	"testing"

	"github.com/johnzastrow/bitt/internal/config"
)

// The template validators run at input time, so a Provider cannot save a title
// or body that would fail every send later with nothing on screen to explain it.
// A title becomes an email Subject and an ntfy header, so it allows no control
// character at all; a body is a body, so it allows newlines but nothing else.
func TestValidTemplates(t *testing.T) {
	titleOK := []string{"Payment due", "Rent — $5.00", strings.Repeat("x", MaxTitleTemplate)}
	for _, s := range titleOK {
		if !ValidTitleTemplate(s) {
			t.Errorf("ValidTitleTemplate(%q) = false, want true", s)
		}
	}
	titleBad := []string{
		"",                                      // empty
		"   ",                                   // blank
		strings.Repeat("x", MaxTitleTemplate+1), // too long
		"line\nbreak",                           // newline is a control char in a header
		"tab\there",                             // any control char
		"null\x00",                              // NUL
	}
	for _, s := range titleBad {
		if ValidTitleTemplate(s) {
			t.Errorf("ValidTitleTemplate(%q) = true, want false", s)
		}
	}

	bodyOK := []string{"one line", "two\nlines", "Due {when}.\n\n{url}"}
	for _, s := range bodyOK {
		if !ValidBodyTemplate(s) {
			t.Errorf("ValidBodyTemplate(%q) = false, want true", s)
		}
	}
	bodyBad := []string{
		"",                                     // empty
		strings.Repeat("x", MaxBodyTemplate+1), // over the ntfy ceiling
		"bell\aring",                           // a control char that is not newline
		"carriage\rreturn",                     // lone CR is not an allowed newline
	}
	for _, s := range bodyBad {
		if ValidBodyTemplate(s) {
			t.Errorf("ValidBodyTemplate(%q) = true, want false", s)
		}
	}
}

// Deliver dispatches on the channel; an unknown one is an error, not a silent
// no-op that would let a caller believe a notice went out.
func TestDeliverUnknownChannel(t *testing.T) {
	n := New(config.NotifyConfig{})
	err := n.Deliver(context.Background(), Channel("carrier-pigeon"), Recipient{}, Message{Title: "x"})
	if err == nil || !strings.Contains(err.Error(), "unknown channel") {
		t.Errorf("Deliver over an unknown channel = %v, want an unknown-channel error", err)
	}
}

// With resolves the effective config per send (a setting changed in the app
// must take effect without a restart) and is nil-safe (a server built without
// notifications stays safe to call through).
func TestWith(t *testing.T) {
	var nilN *Notifier
	if got := nilN.With(config.NotifyConfig{SMTPHost: "x"}); got != nil {
		t.Errorf("nil.With(...) = %v, want nil", got)
	}

	base := New(config.NotifyConfig{})
	if base.Enabled() {
		t.Fatal("a zero config reports Enabled")
	}
	got := base.With(config.NotifyConfig{NtfyBaseURL: "https://ntfy.example"})
	if !got.Enabled() {
		t.Error("With(ntfy config) did not enable delivery")
	}
	if base.Enabled() {
		t.Error("With mutated the receiver instead of returning a copy")
	}
	// The safe HTTP client (and its SSRF-checking dialer) is shared, not dropped.
	if got.client != base.client {
		t.Error("With did not share the base client")
	}
}

// WithMailer swaps the SMTP sender for delivery, carrying the config and client
// over, and does not mutate the receiver (a nil receiver is safe).
func TestWithMailer(t *testing.T) {
	if (*Notifier)(nil).WithMailer(func(string, smtp.Auth, string, []string, []byte) error { return nil }) != nil {
		t.Error("nil.WithMailer(...) should be nil")
	}

	base := New(config.NotifyConfig{SMTPHost: "mail.example", SMTPPort: 587, EmailFrom: "bitt@example.com"})
	var called bool
	got := base.WithMailer(func(string, smtp.Auth, string, []string, []byte) error {
		called = true
		return nil
	})
	if err := got.Deliver(context.Background(), ChannelEmail, Recipient{Email: "p@example.com"}, Message{Title: "Hi", Body: "there"}); err != nil {
		t.Fatalf("deliver through the injected mailer: %v", err)
	}
	if !called {
		t.Error("the injected mailer was not used")
	}
	if base.sendMail != nil {
		t.Error("WithMailer mutated the receiver's sender")
	}
}

// The load-bearing SSRF test: ntfy delivers to an admin-pinned host, but the
// safe dialer must still refuse the addresses an SSRF wants -- loopback (the
// app's own container) and link-local (the cloud metadata endpoint) -- even
// when the configured URL names them directly.
func TestDeliverNtfyRefusesUnsafeHosts(t *testing.T) {
	for _, host := range []string{
		"https://127.0.0.1:9",     // loopback
		"https://[::1]:9",         // IPv6 loopback
		"https://169.254.169.254", // link-local: the metadata endpoint
	} {
		n := New(config.NotifyConfig{NtfyBaseURL: host})
		err := n.Deliver(context.Background(), ChannelNtfy, Recipient{Topic: "bitt-test"}, Message{Title: "x", Body: "y"})
		if err == nil {
			t.Errorf("ntfy send to %s was not refused", host)
			continue
		}
		if !errors.Is(err, ErrUnsafeAddress) {
			t.Errorf("ntfy send to %s failed with %v, want ErrUnsafeAddress", host, err)
		}
	}
}

// deliverNtfy refuses before any network work when it cannot build a legitimate
// request: no server configured, or a topic that failed validation (the only
// user-controlled part of the destination).
func TestDeliverNtfyValidation(t *testing.T) {
	off := New(config.NotifyConfig{})
	if err := off.Deliver(context.Background(), ChannelNtfy, Recipient{Topic: "ok"}, Message{Title: "x"}); err == nil {
		t.Error("ntfy send with no server configured was accepted")
	}

	on := New(config.NotifyConfig{NtfyBaseURL: "https://ntfy.example"})
	if err := on.Deliver(context.Background(), ChannelNtfy, Recipient{Topic: "bad/topic"}, Message{Title: "x"}); err == nil {
		t.Error("ntfy send with an invalid topic was accepted")
	}
}
