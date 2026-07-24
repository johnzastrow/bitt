package notify

import (
	"context"
	"errors"
	"net"
	"net/smtp"
	"strings"
	"testing"

	"github.com/johnzastrow/bitt/internal/config"
)

func TestSanitizeHeaderRejectsControlChars(t *testing.T) {
	ok := []string{"Payment due", "Café — $5.00", "tab: Rent"}
	for _, s := range ok {
		if _, err := sanitizeHeader(s); err != nil {
			t.Errorf("rejected safe header %q: %v", s, err)
		}
	}
	bad := []string{"a\r\nBcc: x@y", "a\nSubject: spoof", "a\x00b", "line\rreturn"}
	for _, s := range bad {
		if _, err := sanitizeHeader(s); !errors.Is(err, ErrHeaderInjection) {
			t.Errorf("accepted unsafe header %q (err=%v)", s, err)
		}
	}
}

func TestValidTopic(t *testing.T) {
	for _, s := range []string{"bitt-alex-7fk2", "Topic_1", "ABC"} {
		if !ValidTopic(s) {
			t.Errorf("ValidTopic(%q) = false", s)
		}
	}
	for _, s := range []string{"", "has space", "a/b", "a\r\nb", strings.Repeat("x", 65), "emoji😀"} {
		if ValidTopic(s) {
			t.Errorf("ValidTopic(%q) = true", s)
		}
	}
}

// The SSRF policy: loopback and link-local (cloud metadata) are refused; LAN
// and public are allowed, because the ntfy host is admin config and a self-
// hosted sidecar or LAN box is a legitimate destination.
func TestDestinationPolicy(t *testing.T) {
	blocked := []string{"127.0.0.1", "::1", "169.254.169.254", "0.0.0.0", "224.0.0.1", "100.64.0.1"}
	for _, s := range blocked {
		if isAllowedDestination(net.ParseIP(s)) {
			t.Errorf("%s should be blocked", s)
		}
	}
	allowed := []string{"192.168.1.5", "10.0.0.3", "172.16.0.1", "8.8.8.8", "1.1.1.1"}
	for _, s := range allowed {
		if !isAllowedDestination(net.ParseIP(s)) {
			t.Errorf("%s should be allowed (LAN/public are legitimate ntfy hosts)", s)
		}
	}
}

func TestBuildEmailPutsUserTextInBodyNotHeaders(t *testing.T) {
	m := Message{Title: "Payment due on Rent", Body: "You owe $50.00 on the Rent tab.", Link: "https://bitt.example/tabs/3"}
	msg, err := buildEmail("bitt@example.com", "payee@example.com", m.Title, m)
	if err != nil {
		t.Fatalf("buildEmail: %v", err)
	}
	s := string(msg)
	headers, body, _ := strings.Cut(s, "\r\n\r\n")
	if !strings.Contains(headers, "Subject: Payment due on Rent") {
		t.Errorf("subject missing from headers:\n%s", headers)
	}
	if !strings.Contains(body, "$50.00") || !strings.Contains(body, "https://bitt.example/tabs/3") {
		t.Errorf("body missing content/link:\n%s", body)
	}
	// Every header line is a single line -- no injected headers.
	for _, line := range strings.Split(headers, "\r\n") {
		if strings.ContainsAny(line, "\n") {
			t.Errorf("a header line contains a bare newline: %q", line)
		}
	}
}

// A CRLF in the title is refused rather than producing extra headers.
func TestBuildEmailRefusesHeaderInjection(t *testing.T) {
	if _, err := buildEmail("a@b.com", "c@d.com", "x\r\nBcc: evil@e.com", Message{}); !errors.Is(err, ErrHeaderInjection) {
		t.Errorf("buildEmail accepted a CRLF subject: %v", err)
	}
}

// deliverEmail routes through a swappable sender so no real server is hit, and
// asserts the To/From reaching SMTP are the clean parsed addresses.
func TestDeliverEmailUsesParsedAddresses(t *testing.T) {
	var gotTo []string
	var gotFrom string
	var gotMsg []byte
	n := &Notifier{
		cfg: config.NotifyConfig{SMTPHost: "mail.example", SMTPPort: 587, EmailFrom: "BitTabby <bitt@example.com>"},
		sendMail: func(addr string, a smtp.Auth, from string, to []string, msg []byte) error {
			gotFrom, gotTo, gotMsg = from, to, msg
			return nil
		},
	}
	err := n.deliverEmail(context.Background(), "Payee Person <payee@example.com>",
		Message{Title: "Reminder", Body: "Due soon."})
	if err != nil {
		t.Fatalf("deliverEmail: %v", err)
	}
	if gotFrom != "bitt@example.com" {
		t.Errorf("envelope from = %q, want the bare address", gotFrom)
	}
	if len(gotTo) != 1 || gotTo[0] != "payee@example.com" {
		t.Errorf("envelope to = %v, want [payee@example.com]", gotTo)
	}
	if !strings.Contains(string(gotMsg), "Subject: Reminder") {
		t.Error("message missing subject")
	}
}

// A recipient email carrying a CRLF is refused at the address parse.
func TestDeliverEmailRefusesInjectedRecipient(t *testing.T) {
	n := &Notifier{
		cfg:      config.NotifyConfig{SMTPHost: "mail.example", SMTPPort: 587, EmailFrom: "bitt@example.com"},
		sendMail: func(string, smtp.Auth, string, []string, []byte) error { return nil },
	}
	if err := n.deliverEmail(context.Background(), "a@b.com\r\nBcc: evil@e.com", Message{Title: "x"}); err == nil {
		t.Error("a CRLF recipient was accepted")
	}
}

func TestAvailableAndEnabled(t *testing.T) {
	off := New(config.NotifyConfig{})
	if off.Enabled() {
		t.Error("a zero config should be disabled")
	}
	n := New(config.NotifyConfig{SMTPHost: "m", SMTPPort: 25, EmailFrom: "a@b.com", NtfyBaseURL: "https://ntfy.sh"})
	if !n.Available(ChannelEmail, Recipient{Email: "x@y.com"}) {
		t.Error("email should be available with an address")
	}
	if n.Available(ChannelEmail, Recipient{}) {
		t.Error("email needs an address")
	}
	if !n.Available(ChannelNtfy, Recipient{Topic: "good-topic"}) {
		t.Error("ntfy should be available with a valid topic")
	}
	if n.Available(ChannelNtfy, Recipient{Topic: "bad/topic"}) {
		t.Error("ntfy must reject an invalid topic")
	}
}
