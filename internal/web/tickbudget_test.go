package web

import (
	"io"
	"net/smtp"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/johnzastrow/bitt/internal/config"
	"github.com/johnzastrow/bitt/internal/schedule"
	"github.com/johnzastrow/bitt/internal/store"
)

// NOTIF-03: the per-tick send ceiling.
//
// Event notices are derived from the ledger rather than queued, so a long cron
// outage could otherwise turn one tick into a burst across every tab at once.
// The ceiling bounds a run; nothing is lost, because a claim is written only
// after a confirmed delivery.

// budgetHarness builds a tick-enabled harness with a send ceiling, two payees
// who both want email, and a scheduled tab that owes money with its next due
// date on the built-in 7-day reminder. Returns the harness and a func that
// reports what has been delivered so far.
func budgetHarness(t *testing.T, maxPerTick int) (*harness, func() []string) {
	t.Helper()
	h := newHarnessCfg(t, config.NotifyConfig{
		TickSecret: "the-cron-secret",
		MaxPerTick: maxPerTick,
	})
	h.completeSetup()
	ctx := t.Context()

	for _, u := range []string{"alice@example.com", "bob@example.com"} {
		user := h.addUser(u, u, false)
		if err := h.db.SetNotifyPrefs(ctx, user.ID, "", true, false); err != nil {
			t.Fatalf("set notify prefs: %v", err)
		}
	}
	if err := h.db.SetDelivery(ctx, store.Delivery{
		SMTPHost: "mail.example", SMTPPort: 587, EmailFrom: "BitTabby <bitt@example.com>",
	}); err != nil {
		t.Fatalf("set delivery: %v", err)
	}

	var mu sync.Mutex
	var to []string
	h.srv().notifier = h.srv().notifier.WithMailer(func(_ string, _ smtp.Auth, _ string, rcpt []string, _ []byte) error {
		mu.Lock()
		defer mu.Unlock()
		to = append(to, rcpt...)
		return nil
	})

	today := instanceToday(t)
	tabID, _ := h.createScheduledTab(today.AddDays(7), schedule.Weekly, schedule.InAdvance)
	for _, email := range []string{"alice@example.com", "bob@example.com"} {
		u, err := h.db.GetUserByEmail(ctx, email)
		if err != nil {
			t.Fatalf("get user: %v", err)
		}
		if err := h.db.AddParticipant(ctx, store.Participant{
			TabID: tabID, UserID: u.ID, Role: store.RolePayee,
		}); err != nil {
			t.Fatalf("add payee: %v", err)
		}
	}
	if resp, b := h.post(tabPath(tabID)+"/charges", url.Values{
		"csrf_token": {h.csrfToken(tabPath(tabID))},
		"amount":     {"100.00"},
		"memo":       {"Service"},
	}); resp.StatusCode != 200 {
		t.Fatalf("charge returned %d: %s", resp.StatusCode, truncate(b))
	}

	return h, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), to...)
	}
}

// tick runs one scan and returns the response body, which reports the counts.
func tickBody(t *testing.T, h *harness) string {
	t.Helper()
	resp := h.postRaw(t, "/internal/tick", "Bearer the-cron-secret")
	if resp.StatusCode != 200 {
		t.Fatalf("tick returned %d", resp.StatusCode)
	}
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read tick body: %v", err)
	}
	return strings.TrimSpace(string(b))
}

// A ceiling of one delivers one of the two payees and defers the other, and
// says so rather than reporting a quiet success.
func TestTickCeilingDefersRatherThanDrops(t *testing.T) {
	h, delivered := budgetHarness(t, 1)

	body := tickBody(t, h)
	if got := len(delivered()); got != 1 {
		t.Fatalf("first tick delivered %d, want 1 (the ceiling)", got)
	}
	if !strings.Contains(body, "sent=1") {
		t.Errorf("tick body %q should report sent=1", body)
	}
	if !strings.Contains(body, "deferred=1") {
		t.Errorf("tick body %q must report what it deferred -- a silent cap is "+
			"indistinguishable from having nothing to send", body)
	}
}

// The deferred payee is reached by the next tick. This is the property that
// makes a ceiling safe: no claim is written for an undelivered event, so it is
// still outstanding rather than lost.
//
// It also proves the ceiling is spent only on real work: on the second tick the
// already-claimed payee must NOT consume the single unit of budget, or the
// deferred one would be starved on every subsequent tick forever.
func TestTickCeilingDrainsOnLaterTicks(t *testing.T) {
	h, delivered := budgetHarness(t, 1)

	tickBody(t, h)
	first := delivered()
	if len(first) != 1 {
		t.Fatalf("first tick delivered %d, want 1", len(first))
	}

	body := tickBody(t, h)
	second := delivered()
	if len(second) != 2 {
		t.Fatalf("second tick brought the total to %d, want 2 -- the deferred payee "+
			"must be picked up, and an already-claimed one must not spend the budget "+
			"(body was %q)", len(second), body)
	}
	if second[0] == second[1] {
		t.Errorf("both deliveries went to %q; each payee should be reached once", second[0])
	}
	if strings.Contains(body, "deferred=") {
		t.Errorf("second tick body %q should have nothing left to defer", body)
	}

	// Everything is claimed now, so a third tick is silent.
	third := tickBody(t, h)
	if got := len(delivered()); got != 2 {
		t.Errorf("third tick delivered %d more, want 0", got-2)
	}
	if !strings.Contains(third, "sent=0") {
		t.Errorf("third tick body %q should report sent=0", third)
	}
}

// Zero means unbounded, which is what an instance that has never heard of the
// setting gets. Both payees go out in one tick and nothing is deferred.
func TestTickCeilingZeroIsUnbounded(t *testing.T) {
	h, delivered := budgetHarness(t, 0)

	body := tickBody(t, h)
	if got := len(delivered()); got != 2 {
		t.Fatalf("unbounded tick delivered %d, want 2", got)
	}
	if strings.Contains(body, "deferred") {
		t.Errorf("tick body %q should not mention deferred when uncapped", body)
	}
}

// A ceiling larger than the work available behaves exactly like no ceiling.
func TestTickCeilingAboveDemandIsInert(t *testing.T) {
	h, delivered := budgetHarness(t, 50)

	body := tickBody(t, h)
	if got := len(delivered()); got != 2 {
		t.Fatalf("delivered %d, want 2", got)
	}
	if strings.Contains(body, "deferred") {
		t.Errorf("tick body %q should not mention deferred when under the ceiling", body)
	}
}

// The budget itself, in isolation: the accounting has to be exact, because the
// deferred count is what an operator reads to decide whether to worry.
func TestSendBudgetAccounting(t *testing.T) {
	b := newSendBudget(2)
	if !b.take() || !b.take() {
		t.Fatal("the first two takes should be granted")
	}
	if b.take() || b.take() {
		t.Error("takes past the ceiling should be refused")
	}
	if b.deferred != 2 {
		t.Errorf("deferred = %d, want 2 -- every refusal is counted", b.deferred)
	}

	unbounded := newSendBudget(0)
	for i := 0; i < 100; i++ {
		if !unbounded.take() {
			t.Fatalf("unbounded budget refused at %d", i)
		}
	}
	if unbounded.deferred != 0 {
		t.Errorf("unbounded deferred = %d, want 0", unbounded.deferred)
	}

	if negative := newSendBudget(-5); !negative.take() {
		t.Error("a negative ceiling should be treated as unbounded, not as zero")
	}
}
