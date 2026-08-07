package web

import (
	"net/smtp"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/johnzastrow/bitt/internal/config"
	"github.com/johnzastrow/bitt/internal/money"
	"github.com/johnzastrow/bitt/internal/notify"
	"github.com/johnzastrow/bitt/internal/store"
)

// NOTIF-01: a notice when a payment is made.

// paymentHarness builds a tick-enabled harness with a tab that has a Provider
// (user 1, the creator) and one payee, both wanting email, and a $100 charge
// outstanding. Returns the harness, the tab id, and a reader for what was sent.
func paymentHarness(t *testing.T) (*harness, int64, func() []mailCapture) {
	t.Helper()
	h := newHarnessCfg(t, config.NotifyConfig{TickSecret: "the-cron-secret"})
	h.completeSetup() // user 1 is the provider
	ctx := t.Context()

	// The Provider wants email too -- they are a participant in their own right
	// and the point of this feature is that they hear about money arriving.
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
		t.Fatalf("set delivery: %v", err)
	}

	var mu sync.Mutex
	var got []mailCapture
	h.srv().notifier = h.srv().notifier.WithMailer(func(_ string, _ smtp.Auth, _ string, to []string, msg []byte) error {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, mailCapture{to: strings.Join(to, ","), body: string(msg)})
		return nil
	})

	tabID := h.createPlainTab("Rent")
	if err := h.db.AddParticipant(ctx, store.Participant{
		TabID: tabID, UserID: payee.ID, Role: store.RolePayee,
	}); err != nil {
		t.Fatalf("add payee: %v", err)
	}
	if resp, b := h.post(tabPath(tabID)+"/charges", url.Values{
		"csrf_token": {h.csrfToken(tabPath(tabID))},
		"amount":     {"100.00"},
		"memo":       {"Service"},
	}); resp.StatusCode != 200 {
		t.Fatalf("charge returned %d: %s", resp.StatusCode, truncate(b))
	}

	return h, tabID, func() []mailCapture {
		mu.Lock()
		defer mu.Unlock()
		return append([]mailCapture(nil), got...)
	}
}

type mailCapture struct{ to, body string }

func (h *harness) pay(t *testing.T, tabID int64, amount string) {
	t.Helper()
	if resp, b := h.post(tabPath(tabID)+"/payments", url.Values{
		"csrf_token": {h.csrfToken(tabPath(tabID))},
		"amount":     {amount},
		"method":     {string(store.MethodTransfer)},
	}); resp.StatusCode != 200 {
		t.Fatalf("payment returned %d: %s", resp.StatusCode, truncate(b))
	}
}

// payAsPayee records the payment as the payee themselves, which is the ordinary
// case. Who the notice names is the entry's actor, so it matters which account
// posts it -- see TestPaymentNoticeNamesWhoeverRecordedIt.
func (h *harness) payAsPayee(t *testing.T, tabID int64, amount string) {
	t.Helper()
	c := h.newClient()
	c.loginAs("payee@example.com", "a-long-enough-password")
	c.pay(t, tabID, amount)
}

// A payment reaches every party on the tab -- the Provider, who is owed the
// money and previously heard nothing at all, and the payer, whose copy is the
// receipt (D-A).
func TestPaymentNoticeReachesEveryParty(t *testing.T) {
	h, tabID, sent := paymentHarness(t)
	h.payAsPayee(t, tabID, "40.00")

	tickBody(t, h)

	got := sent()
	if len(got) != 2 {
		t.Fatalf("payment produced %d notices, want 2 (provider + payer)", len(got))
	}
	var toProvider, toPayer bool
	for _, m := range got {
		if strings.Contains(m.to, "jane@example.com") {
			toProvider = true
		}
		if strings.Contains(m.to, "payee@example.com") {
			toPayer = true
		}
	}
	if !toProvider {
		t.Errorf("the Provider was not told money arrived: %+v", got)
	}
	if !toPayer {
		t.Errorf("the payer did not get their receipt: %+v", got)
	}
}

// The message is generic third person and carries the new variables. {payee}
// and {paid} must both render -- an unsubstituted placeholder in a live notice
// is worse than no notice.
func TestPaymentNoticeRendersItsVariables(t *testing.T) {
	h, tabID, sent := paymentHarness(t)
	h.payAsPayee(t, tabID, "40.00")
	tickBody(t, h)

	got := sent()
	if len(got) == 0 {
		t.Fatal("no notice sent")
	}
	body := got[0].body
	for _, want := range []string{"Pat Payee", "$40.00"} {
		if !strings.Contains(body, want) {
			t.Errorf("notice does not contain %q:\n%s", want, body)
		}
	}
	// $60 of the $100 charge is still owed.
	if !strings.Contains(body, "$60.00") {
		t.Errorf("notice does not report the remaining balance:\n%s", body)
	}
	for _, leftover := range []string{"{payee}", "{paid}", "{balance}", "{tab}", "{url}"} {
		if strings.Contains(body, leftover) {
			t.Errorf("placeholder %s was not substituted:\n%s", leftover, body)
		}
	}
}

// Announced once, never twice, across ticks. This is the claim table doing its
// job with a ledger-derived event key.
func TestPaymentNoticeIsAnnouncedOnce(t *testing.T) {
	h, tabID, sent := paymentHarness(t)
	h.pay(t, tabID, "40.00")

	tickBody(t, h)
	first := len(sent())
	if first != 2 {
		t.Fatalf("first tick sent %d, want 2", first)
	}
	tickBody(t, h)
	if after := len(sent()); after != first {
		t.Errorf("second tick sent %d more notices; a payment is announced once", after-first)
	}
}

// A payment posted and undone before the tick runs never happened as far as the
// tab is concerned, and must not be announced. This is not the stale-state
// suppression that governs reminders -- it is about a retracted entry.
func TestReversedPaymentIsNotAnnounced(t *testing.T) {
	h, tabID, sent := paymentHarness(t)
	h.pay(t, tabID, "40.00")

	// The charge is seq 1, the payment seq 2.
	if resp, b := h.post(tabPath(tabID)+"/entries/2/undo", url.Values{
		"csrf_token": {h.csrfToken(tabPath(tabID))},
	}); resp.StatusCode != 200 {
		t.Fatalf("undo returned %d: %s", resp.StatusCode, truncate(b))
	}

	tickBody(t, h)
	if got := sent(); len(got) != 0 {
		t.Errorf("an undone payment was announced anyway: %+v", got)
	}
}

// A payment is a fact that occurred. Unlike a reminder, it is announced whether
// or not a balance is still owed -- the handoff calls event notices "exempt
// from the stale-state suppression", and a tab settled by the payment itself is
// exactly the case that would otherwise go quiet.
func TestPaymentNoticeSurvivesASettledTab(t *testing.T) {
	h, tabID, sent := paymentHarness(t)
	h.pay(t, tabID, "100.00") // settles the tab exactly

	tickBody(t, h)
	if got := sent(); len(got) != 2 {
		t.Errorf("a payment that settled the tab produced %d notices, want 2 -- "+
			"a payment is announced on its own terms, not on whether money is still owed", len(got))
	}
}

// The lookback floor drops history rather than announcing it after an outage.
func TestPaymentOlderThanLookbackIsDropped(t *testing.T) {
	entries := []store.Entry{
		{Seq: 1, Kind: store.KindCharge, CreatedAt: time.Now()},
		{Seq: 2, Kind: store.KindPayment, CreatedAt: time.Now().Add(-30 * 24 * time.Hour)},
		{Seq: 3, Kind: store.KindPayment, CreatedAt: time.Now()},
	}
	cutoff := time.Now().Add(-7 * 24 * time.Hour)

	got := announceablePayments(entries, cutoff)
	if len(got) != 1 || got[0].Seq != 3 {
		t.Fatalf("announceable = %+v, want only the recent payment (seq 3)", got)
	}

	// A zero cutoff means no floor, so the old one comes back.
	if all := announceablePayments(entries, time.Time{}); len(all) != 2 {
		t.Errorf("with no floor, announceable = %d, want both payments", len(all))
	}
}

// The reversal filter works on the ledger's own shape: a reversal entry points
// at what it undid via ReversesSeq.
func TestAnnounceablePaymentsSkipsReversed(t *testing.T) {
	two := int64(2)
	entries := []store.Entry{
		{Seq: 1, Kind: store.KindPayment, CreatedAt: time.Now()},
		{Seq: 2, Kind: store.KindPayment, CreatedAt: time.Now()},
		{Seq: 3, Kind: store.KindReversal, CreatedAt: time.Now(), ReversesSeq: &two},
	}
	got := announceablePayments(entries, time.Time{})
	if len(got) != 1 || got[0].Seq != 1 {
		t.Errorf("announceable = %+v, want only the un-reversed payment (seq 1)", got)
	}
}

// Header safety, and the structural reason the new variable does not weaken it.
//
// Only the Title becomes a header, and only {tab} reaches the Title. The
// display name is deliberately body-only, so a control character in it is
// ordinary body text and cannot inject a header -- the header/body separator
// has already been written by then.
//
// Both halves are asserted, because the safety here rests on WHERE the variable
// is placed. Moving {payee} into paymentTitle would turn a user-controlled name
// into header input, and this test is what should break if anyone does.
func TestPaymentNoticeHeaderSafety(t *testing.T) {
	deliver := func(msg notify.Message) (error, bool) {
		n := notify.New(config.NotifyConfig{
			SMTPHost: "mail.example", SMTPPort: 587, EmailFrom: "BitTabby <bitt@example.com>",
		})
		var delivered bool
		n = n.WithMailer(func(_ string, _ smtp.Auth, _ string, _ []string, _ []byte) error {
			delivered = true
			return nil
		})
		err := n.Deliver(t.Context(), notify.ChannelEmail, notify.Recipient{Email: "to@example.com"}, msg)
		return err, delivered
	}

	// A hostile tab name reaches the Subject, and must fail the send closed.
	hostileTab := store.Tab{ID: 1, Name: "Rent\r\nBcc: victim@example.com"}
	err, delivered := deliver(paymentMessage(hostileTab, "Pat", 4000, -6000, "https://example.test"))
	if err == nil {
		t.Error("a control character in the tab name must fail the send closed -- it reaches the Subject")
	}
	if delivered {
		t.Error("the message reached the transport despite a header-injection attempt")
	}

	// A hostile display name is body-only, so it is inert rather than fatal.
	tab := store.Tab{ID: 1, Name: "Rent"}
	msg := paymentMessage(tab, "Bad\r\nBcc: victim@example.com", 4000, -6000, "https://example.test")
	if strings.Contains(msg.Title, "Bcc:") {
		t.Fatal("the display name must never reach the Title -- that would make it header input")
	}
	if err, _ := deliver(msg); err != nil {
		t.Errorf("a display name is body text and should not fail a send: %v", err)
	}
}

// The reminder replacer keeps its own meaning. {amount} means "balance owed" in
// templates a Provider may already have saved; the payment variables must not
// have leaked into it or changed it.
func TestReminderVariablesAreUnchangedByPaymentVariables(t *testing.T) {
	h := newHarness(t)
	h.completeSetup()
	srv := h.srv()

	spec := config.Reminder{
		Title: "{tab}",
		Body:  "You owe {amount}, due {due}. Paid: {paid} by {payee}.",
	}
	tab := store.Tab{ID: 1, Name: "Rent"}
	msg := srv.reminderMessage(spec, tab, instanceToday(t), 7, money.Cents(-2500))

	if !strings.Contains(msg.Body, "$25.00") {
		t.Errorf("{amount} no longer renders the balance owed: %q", msg.Body)
	}
	// The payment variables are deliberately NOT part of the reminder set, so
	// they stay literal here rather than acquiring a second meaning.
	if !strings.Contains(msg.Body, "{paid}") || !strings.Contains(msg.Body, "{payee}") {
		t.Errorf("payment variables leaked into the reminder replacer: %q", msg.Body)
	}
}

// The notice names whoever RECORDED the payment, because the entry's actor is
// the only signal the ledger carries -- a payment entry does not record "on
// behalf of whom".
//
// Ordinarily the payee records their own payment and this reads naturally. When
// a Provider records one on a payee's behalf, the notice names the Provider.
// That is a known and accepted consequence of the data available, pinned here
// so it is a decision rather than a surprise.
func TestPaymentNoticeNamesWhoeverRecordedIt(t *testing.T) {
	h, tabID, sent := paymentHarness(t)
	h.pay(t, tabID, "40.00") // the Provider records it, not the payee
	tickBody(t, h)

	got := sent()
	if len(got) == 0 {
		t.Fatal("no notice sent")
	}
	if !strings.Contains(got[0].body, "Jane Provider made a payment") {
		t.Errorf("notice should name the actor who recorded the entry:\n%s", got[0].body)
	}
}
