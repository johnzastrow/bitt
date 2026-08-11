package web

import (
	"net/smtp"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/johnzastrow/bitt/internal/config"
	"github.com/johnzastrow/bitt/internal/schedule"
	"github.com/johnzastrow/bitt/internal/store"
)

// REM-02: send a tab's reminder to yourself, now.

// previewSendHarness builds a provider (user 1) who wants email, a payee who
// also wants email, and a scheduled tab that owes money.
func previewSendHarness(t *testing.T) (*harness, int64, func() []mailCapture) {
	t.Helper()
	h := newHarnessCfg(t, config.NotifyConfig{TickSecret: "the-cron-secret"})
	h.completeSetup()
	ctx := t.Context()

	if err := h.db.SetNotifyPrefs(ctx, 1, "", true, false); err != nil {
		t.Fatalf("provider prefs: %v", err)
	}
	payee := h.addUser("payee@example.com", "Pat Payee", false)
	if err := h.db.SetNotifyPrefs(ctx, payee.ID, "", true, false); err != nil {
		t.Fatalf("payee prefs: %v", err)
	}
	if err := h.db.SetDelivery(ctx, store.Delivery{
		SMTPHost: "mail.example", SMTPPort: 587, EmailFrom: "BitTabby <bitt@example.com>",
	}); err != nil {
		t.Fatalf("delivery: %v", err)
	}

	var mu sync.Mutex
	var got []mailCapture
	h.srv().notifier = h.srv().notifier.WithMailer(func(_ string, _ smtp.Auth, _ string, to []string, msg []byte) error {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, mailCapture{to: strings.Join(to, ","), body: string(msg)})
		return nil
	})

	// Due in 7 days, so the built-in 7-day rule is in force -- but NOT today's
	// lead day for the 14 or 1 day rules, which is the point of several tests.
	tabID, _ := h.createScheduledTab(instanceToday(t).AddDays(7), schedule.Weekly, schedule.InAdvance)
	if err := h.db.AddParticipant(ctx, store.Participant{
		TabID: tabID, UserID: payee.ID, Role: store.RolePayee,
	}); err != nil {
		t.Fatalf("add payee: %v", err)
	}
	if resp, b := h.post(tabPath(tabID)+"/charges", url.Values{
		"csrf_token": {h.csrfToken(tabPath(tabID))},
		"amount":     {"100.00"}, "memo": {"Service"},
	}); resp.StatusCode != 200 {
		t.Fatalf("charge: %d %s", resp.StatusCode, truncate(b))
	}

	return h, tabID, func() []mailCapture {
		mu.Lock()
		defer mu.Unlock()
		return append([]mailCapture(nil), got...)
	}
}

func (h *harness) sendPreview(t *testing.T, tabID int64, days string) string {
	t.Helper()
	_, body := h.post(tabPath(tabID)+"/reminders/preview", url.Values{
		"csrf_token": {h.csrfToken(tabPath(tabID))},
		"days":       {days},
	})
	return body
}

// It goes to the requester and to nobody else. The payee must not be mailed:
// this is a rehearsal, and mailing a real person by accident is the failure
// that would make the button unusable.
func TestPreviewSendGoesToTheRequesterOnly(t *testing.T) {
	h, tabID, sent := previewSendHarness(t)

	h.sendPreview(t, tabID, "7")

	got := sent()
	if len(got) != 1 {
		t.Fatalf("sent %d messages, want exactly 1 (to the requester): %+v", len(got), got)
	}
	if !strings.Contains(got[0].to, "jane@example.com") {
		t.Errorf("not addressed to the requesting administrator: %q", got[0].to)
	}
	for _, m := range got {
		if strings.Contains(m.to, "payee@example.com") {
			t.Error("the payee was mailed -- a preview must never notify a real recipient")
		}
	}
}

// THE load-bearing test. A claim is what suppresses a later send of the same
// occasion, so a preview that wrote one would silently cancel the real reminder
// to the real payee -- turning a diagnostic tool into a way to lose
// notifications. Nothing may be recorded.
func TestPreviewSendWritesNoClaim(t *testing.T) {
	h, tabID, _ := previewSendHarness(t)
	ctx := t.Context()

	h.sendPreview(t, tabID, "7")

	// The occasion the scheduled scan would use for this rule.
	due := instanceToday(t).AddDays(7)
	event := eventFor("reminder:"+due.String()+":d7", 1)
	for _, ch := range []string{"email", "ntfy"} {
		already, err := h.db.WasSent(ctx, tabID, event, ch)
		if err != nil {
			t.Fatalf("WasSent: %v", err)
		}
		if already {
			t.Errorf("the preview claimed %s on %s -- the scheduled reminder to the "+
				"real payee would now be suppressed", event, ch)
		}
	}
}

// And the consequence of that, end to end: the scheduled scan still delivers to
// the payee afterwards. This is what the claim test protects, stated as
// behaviour rather than as a row in a table.
func TestPreviewSendDoesNotSuppressTheRealReminder(t *testing.T) {
	h, tabID, sent := previewSendHarness(t)
	_ = tabID

	h.sendPreview(t, tabID, "7")
	beforeTick := len(sent())

	if resp := h.postRaw(t, "/internal/tick", "Bearer the-cron-secret"); resp.StatusCode != 200 {
		t.Fatalf("tick: %d", resp.StatusCode)
	}

	var toPayee bool
	for _, m := range sent()[beforeTick:] {
		if strings.Contains(m.to, "payee@example.com") {
			toPayee = true
		}
	}
	if !toPayee {
		t.Error("the scheduled reminder did not reach the payee after a preview -- " +
			"the preview suppressed the real send")
	}
}

// The lead-day gate is bypassed: a rule can be previewed on any day, not only
// the one it would fire on. Today is 7 days before due, so the 14-day rule is
// nowhere near firing.
func TestPreviewSendIgnoresTheLeadDayMatch(t *testing.T) {
	h, tabID, sent := previewSendHarness(t)

	body := h.sendPreview(t, tabID, "14")

	if len(sent()) == 0 {
		t.Fatalf("a rule that is not due today should still preview: %s", truncate(body))
	}
}

// The claim gate is bypassed too: pressing it twice sends twice. A button that
// silently does nothing the second time is the failure this feature exists to
// remove.
func TestPreviewSendCanBeRepeated(t *testing.T) {
	h, tabID, sent := previewSendHarness(t)

	h.sendPreview(t, tabID, "7")
	first := len(sent())
	h.sendPreview(t, tabID, "7")

	if len(sent()) <= first {
		t.Error("a second preview sent nothing -- the claim gate is not bypassed")
	}
}

// A rule that is not configured on this tab cannot be sent, so a crafted form
// cannot make the app render and mail an arbitrary lead time.
func TestPreviewSendRefusesUnconfiguredRule(t *testing.T) {
	h, tabID, sent := previewSendHarness(t)

	body := h.sendPreview(t, tabID, "999")

	if len(sent()) != 0 {
		t.Error("an unconfigured lead time was sent")
	}
	if !strings.Contains(body, "no longer configured") {
		t.Errorf("no refusal shown: %s", truncate(body))
	}
}

// Only someone who may edit the tab may send its reminders, since the button
// sends what those settings produce.
func TestPreviewSendRequiresTabAuthority(t *testing.T) {
	h, tabID, sent := previewSendHarness(t)

	payee := h.newClient()
	payee.loginAs("payee@example.com", "a-long-enough-password")
	payee.post(tabPath(tabID)+"/reminders/preview", url.Values{
		"csrf_token": {payee.csrfToken(tabPath(tabID))},
		"days":       {"7"},
	})

	if len(sent()) != 0 {
		t.Error("a payee was able to send a reminder preview for the tab")
	}
}

// The button is bounded. Even self-addressed, an unbounded send is a way to
// mail yourself into a spam folder.
func TestPreviewSendIsRateLimited(t *testing.T) {
	h, tabID, sent := previewSendHarness(t)

	for range previewLimit + 3 {
		h.sendPreview(t, tabID, "7")
	}
	if n := len(sent()); n > previewLimit {
		t.Errorf("sent %d previews, want at most the limit of %d", n, previewLimit)
	}
}
