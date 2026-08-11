package web

import (
	"net/url"
	"strings"
	"testing"

	"github.com/johnzastrow/bitt/internal/schedule"
	"github.com/johnzastrow/bitt/internal/store"
)

// REM-01: the preview must show the RENDERED message, from live figures.
//
// This is the test that encodes why the feature exists. A Payoff tab whose
// template says "{amount}" renders the whole loan; the setup screen showed the
// template and so showed nothing wrong. The preview renders it, and a $10,000
// figure where a $250 installment belongs is then obvious on sight.
func TestPreviewRendersLiveFiguresNotTheTemplate(t *testing.T) {
	h := newHarness(t)
	h.completeSetup()
	ctx := t.Context()

	tabID, _ := h.createPayoffTab("10000.00", "250.00", instanceToday(t).AddDays(7), nil)
	tab, err := h.db.GetTab(ctx, tabID)
	if err != nil {
		t.Fatalf("get tab: %v", err)
	}
	// The old template, exactly as production had it.
	if err := h.db.SetTabReminders(ctx, tabID, []store.TabReminder{
		{Days: 7, Title: "{tab}: {amount} due {when}", Body: "{amount} is owed.\n{url}"},
	}); err != nil {
		t.Fatalf("set reminders: %v", err)
	}

	pv := h.srv().reminderPreviews(ctx, tab)
	if len(pv) != 1 {
		t.Fatalf("got %d previews, want 1", len(pv))
	}
	p := pv[0]
	if strings.Contains(p.Title, "{amount}") || strings.Contains(p.Title, "{tab}") {
		t.Errorf("the preview shows the template, not the rendered message: %q", p.Title)
	}
	if !strings.Contains(p.Title, "$10,000.00") {
		t.Errorf("the preview should expose the whole-loan figure this template produces: %q", p.Title)
	}
	if p.Due == "" {
		t.Error("the preview should name the due date it counted from")
	}
}

// With the corrected template the same preview shows the installment, which is
// how an administrator confirms the fix without waiting for a real reminder.
func TestPreviewShowsTheInstallmentWithPaymentTemplate(t *testing.T) {
	h := newHarness(t)
	h.completeSetup()
	ctx := t.Context()

	tabID, _ := h.createPayoffTab("10000.00", "250.00", instanceToday(t).AddDays(7), nil)
	tab, _ := h.db.GetTab(ctx, tabID)
	if err := h.db.SetTabReminders(ctx, tabID, []store.TabReminder{
		{Days: 7, Title: "{tab}: {payment} due {when}", Body: "Balance owed: {amount}"},
	}); err != nil {
		t.Fatalf("set reminders: %v", err)
	}

	p := h.srv().reminderPreviews(ctx, tab)[0]
	if !strings.Contains(p.Title, "$250.00") {
		t.Errorf("title should lead with the installment: %q", p.Title)
	}
	if strings.Contains(p.Title, "$10,000.00") {
		t.Errorf("title should not quote the whole loan: %q", p.Title)
	}
	if !strings.Contains(p.Body, "$10,000.00") {
		t.Errorf("{amount} should still render the balance in the body: %q", p.Body)
	}
}

// The other half of the feature: who receives it, and why anyone does not.
// An unreachable payee listed WITHOUT a reason is what made the production
// fault need a database query, so the reason is asserted rather than presence.
func TestPreviewNamesUnreachableRecipientsAndWhy(t *testing.T) {
	h := newHarness(t)
	h.completeSetup()
	ctx := t.Context()

	reachable := h.addUser("reach@example.com", "Reachable Rita", false)
	silent := h.addUser("silent@example.com", "Silent Sam", false)
	if err := h.db.SetNotifyPrefs(ctx, reachable.ID, "", true, false); err != nil {
		t.Fatalf("prefs: %v", err)
	}
	// Wants ntfy, but has no topic -- the exact production shape.
	if err := h.db.SetNotifyPrefs(ctx, silent.ID, "", false, true); err != nil {
		t.Fatalf("prefs: %v", err)
	}
	if err := h.db.SetDelivery(ctx, store.Delivery{
		SMTPHost: "mail.example", SMTPPort: 587, EmailFrom: "BitTabby <bitt@example.com>",
	}); err != nil {
		t.Fatalf("delivery: %v", err)
	}

	tabID, _ := h.createScheduledTab(instanceToday(t).AddDays(7), schedule.Weekly, schedule.InAdvance)
	for _, u := range []int64{reachable.ID, silent.ID} {
		if err := h.db.AddParticipant(ctx, store.Participant{
			TabID: tabID, UserID: u, Role: store.RolePayee,
		}); err != nil {
			t.Fatalf("add payee: %v", err)
		}
	}
	if resp, b := h.post(tabPath(tabID)+"/charges", url.Values{
		"csrf_token": {h.csrfToken(tabPath(tabID))},
		"amount":     {"100.00"}, "memo": {"Service"},
	}); resp.StatusCode != 200 {
		t.Fatalf("charge: %d %s", resp.StatusCode, truncate(b))
	}

	tab, _ := h.db.GetTab(ctx, tabID)
	pv := h.srv().reminderPreviews(ctx, tab)
	if len(pv) == 0 {
		t.Fatal("no previews")
	}

	var sawReach, sawSilent bool
	for _, p := range pv {
		for _, rc := range p.Recipients {
			switch rc.Name {
			case "Reachable Rita":
				sawReach = true
				if !rc.Reachable() {
					t.Errorf("Rita has email on and configured, but shows unreachable: %+v", rc)
				}
			case "Silent Sam":
				sawSilent = true
				if rc.Reachable() {
					t.Errorf("Sam has no ntfy topic and email off, but shows reachable: %+v", rc)
				}
				if rc.Reason == "" {
					t.Error("an unreachable recipient must carry a reason, not just be absent")
				}
				if !strings.Contains(rc.Reason, "topic") {
					t.Errorf("the reason should name the missing topic, got %q", rc.Reason)
				}
			}
		}
	}
	if !sawReach || !sawSilent {
		t.Errorf("both payees should be listed: rita=%v sam=%v", sawReach, sawSilent)
	}
}

// A rule that cannot render says so, rather than showing a zero date or amount.
// "$0.00 due Jan 1, year 1" is worse than an honest sentence.
func TestPreviewExplainsWhenARuleCannotRender(t *testing.T) {
	h := newHarness(t)
	h.completeSetup()
	ctx := t.Context()

	// A plain tab with no schedule at all.
	tabID := h.createPlainTab("No schedule here")
	tab, _ := h.db.GetTab(ctx, tabID)

	for _, p := range h.srv().reminderPreviews(ctx, tab) {
		if p.Problem == "" {
			t.Errorf("a tab with no schedule should explain itself, got title %q", p.Title)
		}
		if p.Title != "" || p.Body != "" {
			t.Errorf("a rule that cannot render must not show a message: %+v", p)
		}
		if !strings.Contains(p.Problem, "schedule") {
			t.Errorf("the explanation should name the missing schedule: %q", p.Problem)
		}
	}
}

// The preview mirrors the scan's replace-not-merge rule: a tab with its own
// rules ignores the instance set entirely. A preview that merged them would
// confidently show reminders that will never fire.
func TestPreviewUsesTabRulesInsteadOfInstanceDefaults(t *testing.T) {
	h := newHarness(t)
	h.completeSetup()
	ctx := t.Context()

	tabID, _ := h.createScheduledTab(instanceToday(t).AddDays(3), schedule.Weekly, schedule.InAdvance)
	tab, _ := h.db.GetTab(ctx, tabID)
	if err := h.db.SetTabReminders(ctx, tabID, []store.TabReminder{
		{Days: 3, Title: "only mine", Body: "B"},
	}); err != nil {
		t.Fatalf("set: %v", err)
	}

	pv := h.srv().reminderPreviews(ctx, tab)
	if len(pv) != 1 {
		t.Fatalf("got %d previews, want exactly the tab's own 1 -- the instance set must not be merged in", len(pv))
	}
	if pv[0].Days != 3 {
		t.Errorf("preview shows day %d, want the tab's own 3", pv[0].Days)
	}
}

// The wiring: the preview must actually reach the rendered page. The tests
// above exercise the computation, which would pass just as happily if nobody
// ever put the result in the template.
func TestPreviewAppearsOnTheTabPage(t *testing.T) {
	h := newHarness(t)
	h.completeSetup()
	ctx := t.Context()

	tabID, _ := h.createPayoffTab("10000.00", "250.00", instanceToday(t).AddDays(7), nil)
	if err := h.db.SetTabReminders(ctx, tabID, []store.TabReminder{
		{Days: 7, Title: "{tab}: {payment} due {when}", Body: "Balance owed: {amount}"},
	}); err != nil {
		t.Fatalf("set reminders: %v", err)
	}

	_, body := h.get(tabPath(tabID))
	if !strings.Contains(body, "What these actually send") {
		t.Fatalf("the preview section is missing from the tab page: %s", truncate(body))
	}
	if !strings.Contains(body, "$250.00") {
		t.Errorf("the rendered installment does not appear on the page: %s", truncate(body))
	}
	if !strings.Contains(body, "7 days before") {
		t.Errorf("the rule's lead time is not labelled on the page: %s", truncate(body))
	}
}
