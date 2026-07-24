package notify

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/mail"
	"net/smtp"
	"regexp"
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

// Enabled reports whether any channel is configured.
func (n *Notifier) Enabled() bool { return n.cfg.EmailEnabled() || n.cfg.NtfyEnabled() }

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

	msg, err := buildEmail(fromAddr.String(), toAddr.Address, subject, m)
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

// buildEmail assembles the RFC 5322 message. Every header value has already
// been through address parsing or oneLine, so no user text reaches a header
// unchecked; the user's free text lives only in the body.
func buildEmail(from, to, subject string, m Message) ([]byte, error) {
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
