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

// NOTIF-02: a notice when a payment is missed.

// overdueHarness builds a tick-enabled harness with a Provider and a payee who
// both want email, and a weekly tab anchored `dueDaysAgo` days in the past with
// money outstanding.
func overdueHarness(t *testing.T, dueDaysAgo int) (*harness, int64, func() []mailCapture) {
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

	today := instanceToday(t)
	tabID, _ := h.createScheduledTab(today.AddDays(-dueDaysAgo), schedule.Weekly, schedule.InAdvance)
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

// The day after a missed due date, both the payee and the Provider hear about
// it (D-C). Before this the period simply went silent forever.
func TestOverdueNoticeReachesPayeeAndProvider(t *testing.T) {
	h, _, sent := overdueHarness(t, 1)

	tickBody(t, h)

	got := sent()
	if len(got) != 2 {
		t.Fatalf("overdue produced %d notices, want 2 (payee + provider): %+v", len(got), got)
	}
	var toProvider, toPayee bool
	for _, m := range got {
		if strings.Contains(m.to, "jane@example.com") {
			toProvider = true
		}
		if strings.Contains(m.to, "payee@example.com") {
			toPayee = true
		}
	}
	if !toPayee {
		t.Error("the payee was not chased")
	}
	if !toProvider {
		t.Error("the Provider was not told the money had not arrived")
	}
}

// The notice reads in the past tense, and {days} renders as a magnitude. A
// notice saying a payment "is due in -1 days" is the failure this guards.
func TestOverdueNoticeReadsInThePastTense(t *testing.T) {
	h, _, sent := overdueHarness(t, 1)
	tickBody(t, h)

	got := sent()
	if len(got) == 0 {
		t.Fatal("no notice sent")
	}
	body := got[0].body
	if !strings.Contains(body, "was due yesterday") {
		t.Errorf("overdue notice is not in the past tense:\n%s", body)
	}
	if strings.Contains(body, "-1") || strings.Contains(body, "in -") {
		t.Errorf("a negative lead leaked into the wording:\n%s", body)
	}
	if !strings.Contains(body, "is still owed") {
		t.Errorf("overdue notice does not use the overdue template:\n%s", body)
	}
}

// Only the configured leads fire. Two days after a due date is not one of the
// built-in overdue rules (-1 and -7), so nothing is sent -- the cadence is the
// cap, and silence between notices is the feature working.
func TestOverdueIsSilentBetweenItsLeads(t *testing.T) {
	h, _, sent := overdueHarness(t, 2)

	tickBody(t, h)
	if got := sent(); len(got) != 0 {
		t.Errorf("a day with no configured overdue rule sent %d notices: %+v", len(got), got)
	}
}

// Overdue re-derives live state before sending, unlike a payment notice. A tab
// settled after its due date must not still be chased at the later lead.
func TestOverdueStopsOnceThePeriodIsPaid(t *testing.T) {
	h, tabID, sent := overdueHarness(t, 1)

	// Settle it before the tick runs. The amount clears the manual charge and
	// anything the schedule has accrued, so the balance is certainly not owed.
	if resp, b := h.post(tabPath(tabID)+"/payments", url.Values{
		"csrf_token": {h.csrfToken(tabPath(tabID))},
		"amount":     {"5000.00"},
		"method":     {string(store.MethodTransfer)},
	}); resp.StatusCode != 200 {
		t.Fatalf("payment returned %d: %s", resp.StatusCode, truncate(b))
	}

	tickBody(t, h)

	// The payment notice still goes out -- that is NOTIF-01, and a payment is a
	// fact. What must not appear is a dunning notice for a paid period.
	for _, m := range sent() {
		if strings.Contains(m.body, "was due") || strings.Contains(m.body, "is still owed") {
			t.Errorf("a settled tab was still chased:\n%s", m.body)
		}
	}
}

// An archived tab is silent, as it is for reminders.
func TestOverdueSkipsArchivedTabs(t *testing.T) {
	h, tabID, sent := overdueHarness(t, 1)
	if err := h.db.SetTabArchived(t.Context(), tabID, true); err != nil {
		t.Fatalf("archive: %v", err)
	}

	tickBody(t, h)
	if got := sent(); len(got) != 0 {
		t.Errorf("an archived tab sent %d notices: %+v", len(got), got)
	}
}

// Announced once per lead, not on every tick for the days that follow.
func TestOverdueNoticeIsAnnouncedOncePerLead(t *testing.T) {
	h, _, sent := overdueHarness(t, 1)

	tickBody(t, h)
	first := len(sent())
	if first != 2 {
		t.Fatalf("first tick sent %d, want 2", first)
	}
	tickBody(t, h)
	if after := len(sent()); after != first {
		t.Errorf("second tick sent %d more; an overdue lead fires once", after-first)
	}
}

// The wording of every lead time, including the ones that only overdue reaches.
func TestLeadPhrase(t *testing.T) {
	cases := map[int]string{
		0: "today", 1: "tomorrow", 7: "in one week", 14: "in two weeks", 3: "in 3 days",
		-1: "yesterday", -7: "a week ago", -14: "two weeks ago", -3: "3 days ago",
	}
	for days, want := range cases {
		if got := leadPhrase(days); got != want {
			t.Errorf("leadPhrase(%d) = %q, want %q", days, got, want)
		}
	}
	// The property that matters most: no negative number ever reaches a reader.
	for d := -60; d < 0; d++ {
		if strings.Contains(leadPhrase(d), "-") {
			t.Errorf("leadPhrase(%d) = %q leaks a negative sign", d, leadPhrase(d))
		}
	}
}

func TestAbs(t *testing.T) {
	for in, want := range map[int]int{-7: 7, 0: 0, 7: 7} {
		if got := abs(in); got != want {
			t.Errorf("abs(%d) = %d, want %d", in, got, want)
		}
	}
}
