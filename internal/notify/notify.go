package notify

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/mail"
	"net/smtp"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/johnzastrow/bitt/internal/config"
)

// sendTimeout bounds a single delivery attempt. A stuck SMTP or ntfy server
// must not hold the tick handler open.
const sendTimeout = 15 * time.Second

// Channel names a delivery method.
type Channel string

const (
	ChannelEmail Channel = "email"
	ChannelNtfy  Channel = "ntfy"
)

// Message is a notification to deliver. Title is a short header-safe line; Body
// is the plain-text content; Link is an optional absolute URL appended to the
// body (never placed in a header).
type Message struct {
	Title string
	Body  string
	Link  string
}

// Recipient is where a message goes. Both fields are optional; delivery uses
// whichever the enabled channels and the recipient's preferences support.
type Recipient struct {
	Email string
	// Topic is the recipient's ntfy topic, appended to the admin-pinned server
	// URL. It is the only user-controlled part of an ntfy destination.
	Topic string
}

// topicPattern is the strict charset an ntfy topic must match. ntfy itself
// allows a broad set, but bounding it to this closes any chance of a topic
// altering the request path or a header: no slashes, no whitespace, no control
// characters, and a bounded length.
var topicPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// ValidTopic reports whether s is an acceptable ntfy topic. Exposed so the
// profile handler validates a topic at input, not only at send.
func ValidTopic(s string) bool { return topicPattern.MatchString(s) }

// Message template bounds, in BYTES -- which is what the limits they encode are
// measured in, and what len() on a Go string already counts.
//
// The body ceiling is not an aesthetic choice: ntfy.sh caps a message at 4,096
// bytes on its free tier, and a message over it is REFUSED, not truncated. So
// that is the real limit a Provider is writing against, and the app enforces the
// same one rather than inventing a smaller number that would look arbitrary.
//
// The title bound is our own. A title becomes an email Subject and a phone's
// lock-screen heading, and neither shows anything like 120 characters.
const (
	MaxTitleTemplate = 120
	MaxBodyTemplate  = NtfyMessageLimit
)

// NtfyMessageLimit is the message size ntfy.sh accepts on a free account. A
// send over it fails, so it is worth showing a Provider before they save.
const NtfyMessageLimit = 4096

// NtfyWarnAt is where the interface starts cautioning. Ten percent of headroom
// is roughly what variable substitution needs: a long tab name and a {url} can
// add a couple of hundred bytes to a template that fits comfortably on screen,
// and the size that matters is the one after they are filled in.
const NtfyWarnAt = NtfyMessageLimit * 9 / 10

// ValidTitleTemplate reports whether s is safe and sensible as a notification
// title template.
//
// Titles become an email Subject and an ntfy header, so this is the same rule
// sanitizeHeader enforces at send time, applied at input instead: no control
// characters at all, newlines included. Checking here is not redundant with the
// send-time check -- it is what stops a Provider from saving a template that
// would fail every one of their payees' reminders closed, days later, with
// nothing on screen to explain why.
//
// The substituted values are not covered by this; a hostile tab name reaching a
// title is still caught at send time, which is where it can be seen.
func ValidTitleTemplate(s string) bool {
	if strings.TrimSpace(s) == "" || len(s) > MaxTitleTemplate {
		return false
	}
	return !hasControl(s, false)
}

// ValidBodyTemplate reports whether s is safe as a notification body template.
//
// A body is not a header, so newlines are legitimate and expected -- the
// default message uses them. Every other control character is refused, since
// none of them belongs in a message a person reads and their presence means
// either a paste accident or an attempt at something.
func ValidBodyTemplate(s string) bool {
	if strings.TrimSpace(s) == "" || len(s) > MaxBodyTemplate {
		return false
	}
	return !hasControl(s, true)
}

// hasControl reports whether s contains a control character, optionally
// allowing the newline that a multi-line body needs.
func hasControl(s string, allowNewline bool) bool {
	for _, r := range s {
		if r == '\n' && allowNewline {
			continue
		}
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}

// Notifier delivers messages over the configured channels. A zero Notifier
// sends nothing, which is the correct behaviour when notifications are off.
type Notifier struct {
	cfg    config.NotifyConfig
	client *http.Client
	// send is the SMTP send function, swappable in tests so no real mail server
	// is contacted. Nil uses the real smtp.SendMail.
	sendMail func(addr string, a smtp.Auth, from string, to []string, msg []byte) error
}

// New builds a Notifier from configuration.
func New(cfg config.NotifyConfig) *Notifier {
	return &Notifier{
		cfg:    cfg,
		client: newSafeClient(sendTimeout),
	}
}

// With returns a Notifier that delivers under cfg, sharing this one's HTTP
// client and test hook.
//
// Delivery settings can now come from the database as well as the environment,
// and a setting changed through the interface has to take effect without a
// restart -- so the effective configuration is resolved per use rather than
// once at startup. Sharing the client is what keeps that from discarding the
// connection pool (and the SSRF-checking dialer with it) on every call.
//
// A nil receiver returns nil, so a server built without notifications stays
// safe to call through.
func (n *Notifier) With(cfg config.NotifyConfig) *Notifier {
	if n == nil {
		return nil
	}
	out := *n
	out.cfg = cfg
	return &out
}

// Enabled reports whether any channel is configured.
func (n *Notifier) Enabled() bool { return n.cfg.EmailEnabled() || n.cfg.NtfyEnabled() }

// MailFunc is the SMTP send signature, identical to smtp.SendMail. It exists so
// a caller can route mail through an alternative transport -- a custom dialer,
// or a test double that captures the message instead of contacting a server.
type MailFunc = func(addr string, a smtp.Auth, from string, to []string, msg []byte) error

// WithMailer returns a copy of n whose email delivery goes through fn instead of
// smtp.SendMail. It mirrors With: the config and the SSRF-safe HTTP client are
// carried over, only the mail sender is swapped. A nil receiver returns nil.
func (n *Notifier) WithMailer(fn MailFunc) *Notifier {
	if n == nil {
		return nil
	}
	out := *n
	out.sendMail = fn
	return &out
}

// Deliver sends m to r over the given channel. It returns nil only on a
// confirmed success, which is what lets the caller claim a send-then-claim
// notification (decision D2) exactly when it actually went out.
func (n *Notifier) Deliver(ctx context.Context, ch Channel, r Recipient, m Message) error {
	switch ch {
	case ChannelEmail:
		return n.deliverEmail(ctx, r.Email, m)
	case ChannelNtfy:
		return n.deliverNtfy(ctx, r.Topic, m)
	default:
		return fmt.Errorf("notify: unknown channel %q", ch)
	}
}

// Available reports whether a channel can be used for a recipient right now:
// the channel is configured and the recipient has the coordinate it needs.
func (n *Notifier) Available(ch Channel, r Recipient) bool {
	switch ch {
	case ChannelEmail:
		return n.cfg.EmailEnabled() && r.Email != ""
	case ChannelNtfy:
		return n.cfg.NtfyEnabled() && ValidTopic(r.Topic)
	default:
		return false
	}
}

// ---------------------------------------------------------------------------
// ntfy
// ---------------------------------------------------------------------------

func (n *Notifier) deliverNtfy(ctx context.Context, topic string, m Message) error {
	if !n.cfg.NtfyEnabled() {
		return errors.New("notify: ntfy is not configured")
	}
	if !ValidTopic(topic) {
		return fmt.Errorf("notify: %q is not a valid ntfy topic", topic)
	}
	title, err := oneLine(m.Title)
	if err != nil {
		return err
	}

	// The topic is a validated path segment on the admin-pinned host; it cannot
	// change the host or inject a header.
	url := n.cfg.NtfyBaseURL + "/" + topic

	body := m.Body
	if m.Link != "" {
		body = strings.TrimRight(body, "\n") + "\n\n" + m.Link
	}

	ctx, cancel := context.WithTimeout(ctx, sendTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		return err
	}
	// ntfy takes the notification title in a header; it must be header-safe.
	if title != "" {
		req.Header.Set("Title", title)
	}
	if n.cfg.NtfyToken != "" {
		req.Header.Set("Authorization", "Bearer "+n.cfg.NtfyToken)
	}

	resp, err := n.client.Do(req)
	if err != nil {
		return fmt.Errorf("notify: ntfy request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))

	// A redirect is returned rather than followed (CheckRedirect), so a 3xx is a
	// failure here, not a silent hop to another host.
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("notify: ntfy returned %s", resp.Status)
	}
	return nil
}

// ---------------------------------------------------------------------------
// email
// ---------------------------------------------------------------------------

func (n *Notifier) deliverEmail(ctx context.Context, to string, m Message) error {
	if !n.cfg.EmailEnabled() {
		return errors.New("notify: email is not configured")
	}
	// Parse and re-format both addresses through net/mail, which rejects a
	// control character. A raw string in a To/From header is where CRLF
	// injection lives; this is the gate.
	fromAddr, err := mail.ParseAddress(n.cfg.EmailFrom)
	if err != nil {
		return fmt.Errorf("notify: from address: %w", err)
	}
	toAddr, err := mail.ParseAddress(to)
	if err != nil {
		return fmt.Errorf("notify: recipient address: %w", err)
	}
	subject, err := oneLine(m.Title)
	if err != nil {
		return err
	}

	msg, err := buildEmail(fromAddr.String(), fromAddr.Address, toAddr.Address, subject, m)
	if err != nil {
		return err
	}

	addr := fmt.Sprintf("%s:%d", n.cfg.SMTPHost, n.cfg.SMTPPort)
	var auth smtp.Auth
	if n.cfg.SMTPUsername != "" {
		auth = smtp.PlainAuth("", n.cfg.SMTPUsername, n.cfg.SMTPPassword, n.cfg.SMTPHost)
	}

	send := n.sendMail
	if send == nil {
		send = smtp.SendMail
	}
	// smtp.SendMail issues STARTTLS when the server offers it and refuses to
	// send the password over an unencrypted link, so credentials are not sent
	// in the clear against a TLS-capable server.
	if err := send(addr, auth, fromAddr.Address, []string{toAddr.Address}, msg); err != nil {
		return fmt.Errorf("notify: smtp send: %w", err)
	}
	_ = ctx
	return nil
}

// now is the clock, swappable so a test can pin the Date header.
var now = time.Now

// messageID returns a globally unique Message-ID local part plus the sending
// domain, e.g. "3f2b...@example.com".
//
// The randomness is from crypto/rand rather than math/rand. A Message-ID is not
// a secret, but a predictable one lets an outsider guess identifiers for
// messages they never saw, and the cost of using the good generator is nil. A
// failure to read randomness falls back to the clock rather than failing the
// send: a duplicate-prone Message-ID is far better than no notification.
func messageID(fromAddr string) string {
	var b [16]byte
	local := ""
	if _, err := rand.Read(b[:]); err == nil {
		local = hex.EncodeToString(b[:])
	} else {
		local = strconv.FormatInt(now().UnixNano(), 36)
	}
	domain := "localhost"
	if i := strings.LastIndex(fromAddr, "@"); i >= 0 && i+1 < len(fromAddr) {
		domain = fromAddr[i+1:]
	}
	return local + "@" + domain
}

// buildEmail assembles the message. fromAddr is the bare address (no display
// name), used for the Message-ID domain; from is the full header value.
func buildEmail(from, fromAddr, to, subject string, m Message) ([]byte, error) {
	// Defence in depth: re-check the assembled header values.
	for _, h := range []string{from, to, subject} {
		if _, err := sanitizeHeader(h); err != nil {
			return nil, err
		}
	}
	body := m.Body
	if m.Link != "" {
		body = strings.TrimRight(body, "\n") + "\n\n" + m.Link
	}

	var buf bytes.Buffer
	// Date is REQUIRED by RFC 5322 section 3.6, and Message-ID is expected by
	// every filter that has an opinion. Omitting them is not a cosmetic lapse:
	// SpamAssassin scores MISSING_DATE and MISSING_MID directly, so a message
	// without them starts with spam points before its content is read. That is
	// enough to land otherwise well-authenticated mail -- SPF, DKIM and DMARC
	// all passing -- in a spam folder.
	fmt.Fprintf(&buf, "Date: %s\r\n", now().Format(time.RFC1123Z))
	fmt.Fprintf(&buf, "Message-ID: <%s>\r\n", messageID(fromAddr))
	fmt.Fprintf(&buf, "From: %s\r\n", from)
	fmt.Fprintf(&buf, "To: %s\r\n", to)
	fmt.Fprintf(&buf, "Subject: %s\r\n", subject)
	buf.WriteString("MIME-Version: 1.0\r\n")
	buf.WriteString("Content-Type: text/plain; charset=utf-8\r\n")
	buf.WriteString("\r\n")
	// Normalise body line endings to CRLF and leave the text otherwise as-is;
	// it is a body, not a header, so its content is not a header-injection risk.
	buf.WriteString(strings.ReplaceAll(strings.ReplaceAll(body, "\r\n", "\n"), "\n", "\r\n"))
	return buf.Bytes(), nil
}
