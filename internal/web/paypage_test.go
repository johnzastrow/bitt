package web

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/johnzastrow/bitt/internal/config"
	"github.com/johnzastrow/bitt/internal/schedule"
	"github.com/johnzastrow/bitt/internal/store"
)

// The dedicated payment screen, and the link that leads to it.

// A reminder's link must point at the payment screen, not the whole tab. That
// variable was always documented as "a link to the tab's payment page".
func TestReminderLinkPointsAtThePaymentScreen(t *testing.T) {
	h := newHarnessCfg(t, config.NotifyConfig{})
	h.completeSetup()
	ctx := t.Context()
	srv := h.srv()
	// {url} is empty unless the instance knows its own origin.
	srv.cfg.BaseURL = "https://bitt.example.test"

	tabID := h.createPlainTab("Rent")
	tab, err := h.db.GetTab(ctx, tabID)
	if err != nil {
		t.Fatalf("get tab: %v", err)
	}

	msg := srv.reminderMessage(ctx,
		config.Reminder{Days: 7, Title: "T", Body: "{url}"},
		tab, instanceToday(t), 7, -5000)

	if !strings.Contains(msg.Body, "/pay") {
		t.Errorf("the reminder link does not reach the payment screen: %q", msg.Body)
	}
}

// The screen prefills one period's payment, which is the amount the reminder
// just told them about -- not the whole loan.
func TestPayScreenPrefillsThePeriodPayment(t *testing.T) {
	h := newHarness(t)
	h.completeSetup()

	tabID, _ := h.createPayoffTab("10000.00", "250.00", instanceToday(t).AddDays(7), nil)

	_, body := h.get(tabPath(tabID) + "/pay")
	if !strings.Contains(body, "$250.00") {
		t.Errorf("the screen does not lead with the installment: %s", truncate(body))
	}
	if !strings.Contains(body, "one period") {
		t.Errorf("it should say the figure is one period, not the total: %s", truncate(body))
	}
	if !strings.Contains(body, "$10,000.00") {
		t.Errorf("the total owed should still be stated: %s", truncate(body))
	}
}

// It posts to the existing endpoint, so a payment recorded here is an ordinary
// payment: same rules, same ledger, same idempotency.
func TestPayScreenRecordsARealPayment(t *testing.T) {
	h := newHarness(t)
	h.completeSetup()
	ctx := t.Context()

	tabID := h.createPlainTab("Rent")
	if resp, b := h.post(tabPath(tabID)+"/charges", url.Values{
		"csrf_token": {h.csrfToken(tabPath(tabID))},
		"amount":     {"100.00"}, "memo": {"Rent"},
	}); resp.StatusCode != 200 {
		t.Fatalf("charge: %d %s", resp.StatusCode, truncate(b))
	}

	before, _ := h.srv().ledger.Balance(ctx, tabID)

	_, body := h.get(tabPath(tabID) + "/pay")
	if !strings.Contains(body, "Record payment") {
		t.Fatalf("no form on the pay screen: %s", truncate(body))
	}
	if resp, b := h.post(tabPath(tabID)+"/payments", url.Values{
		"csrf_token":      {h.csrfToken(tabPath(tabID) + "/pay")},
		"idempotency_key": {"pay-screen-test-key"},
		"amount":          {"40.00"},
		"method":          {string(store.MethodTransfer)},
	}); resp.StatusCode != 200 {
		t.Fatalf("payment: %d %s", resp.StatusCode, truncate(b))
	}

	after, _ := h.srv().ledger.Balance(ctx, tabID)
	if after == before {
		t.Error("the balance did not move -- the form is not posting a real payment")
	}
}

// Membership only, exactly as the payment endpoint enforces. An administrator
// who merely oversees a tab sees the figures but is not offered a form that
// would be refused on submit.
func TestPayScreenOffersNoFormToANonMemberAdmin(t *testing.T) {
	h := newHarness(t)
	h.completeSetup() // user 1 is an instance admin

	// A tab created by, and belonging to, someone else entirely. makeTabAs posts
	// as the client it is called on, so it runs on that person's session.
	other := h.addUser("other@example.com", "Other", false)
	theirs := h.newClient()
	theirs.loginAs("other@example.com", "a-long-enough-password")
	tabPathStr := theirs.makeTabAs("Theirs", other.ID)
	// Give it a balance, so the screen reaches the membership branch rather than
	// short-circuiting on "nothing is owed".
	if resp, b := theirs.post(tabPathStr+"/charges", url.Values{
		"csrf_token": {theirs.csrfToken(tabPathStr)},
		"amount":     {"50.00"}, "memo": {"Theirs"},
	}); resp.StatusCode != 200 {
		t.Fatalf("charge: %d %s", resp.StatusCode, truncate(b))
	}

	// Read it back as the instance administrator, who is not a member.
	_, body := h.get(tabPathStr + "/pay")
	if strings.Contains(body, "Record payment") {
		t.Error("an administrator who is not a member was offered a payment form")
	}
	if !strings.Contains(body, "not a member") {
		t.Errorf("no explanation shown: %s", truncate(body))
	}
}

// A settled tab says so rather than offering a form that would post zero.
func TestPayScreenSaysWhenNothingIsOwed(t *testing.T) {
	h := newHarness(t)
	h.completeSetup()

	tabID := h.createPlainTab("Settled")

	_, body := h.get(tabPath(tabID) + "/pay")
	if !strings.Contains(body, "Nothing is owed") {
		t.Errorf("a settled tab should say so: %s", truncate(body))
	}
}

// The link is useless if it does not survive the login it triggers on a phone.
// requireAuth must remember the destination, and the login form must carry it.
func TestPayLinkSurvivesLogin(t *testing.T) {
	h := newHarness(t)
	h.completeSetup()

	tabID, _ := h.createScheduledTab(instanceToday(t).AddDays(7), schedule.Weekly, schedule.InAdvance)
	want := tabPath(tabID) + "/pay"

	// A signed-out visitor following the link.
	anon := h.newClient()
	resp := anon.getNoRedirect(want)
	loc := resp.Header.Get("Location")
	if !strings.Contains(loc, "next=") {
		t.Fatalf("the login redirect did not remember where they were going: %q", loc)
	}
	if !strings.Contains(loc, url.QueryEscape(want)) {
		t.Errorf("the remembered destination is wrong: %q", loc)
	}

	// And the login page carries it into the form.
	_, body := anon.get(loc)
	if !strings.Contains(body, `name="next"`) {
		t.Errorf("the login form does not carry the destination: %s", truncate(body))
	}
}

// The redirect target is never trusted. An open redirect here would turn every
// notification into a phishing vector.
func TestLoginRefusesAnOffsiteNext(t *testing.T) {
	h := newHarness(t)
	h.completeSetup()

	anon := h.newClient()
	resp := anon.postNoRedirect("/login", url.Values{
		"csrf_token": {anon.csrfToken("/login")},
		"email":      {"jane@example.com"},
		"password":   {"correct-horse-battery"},
		"next":       {"https://evil.example/"},
	})
	if loc := resp.Header.Get("Location"); strings.Contains(loc, "evil.example") {
		t.Errorf("login redirected off-site: %q", loc)
	} else if loc != "/" {
		t.Errorf("expected the dashboard fallback, got %q", loc)
	}
	if resp.StatusCode != http.StatusSeeOther {
		t.Errorf("status = %d, want 303", resp.StatusCode)
	}
}

// And it honours a legitimate local destination, or the feature does nothing.
func TestLoginHonoursALocalNext(t *testing.T) {
	h := newHarness(t)
	h.completeSetup()

	anon := h.newClient()
	resp := anon.postNoRedirect("/login", url.Values{
		"csrf_token": {anon.csrfToken("/login")},
		"email":      {"jane@example.com"},
		"password":   {"correct-horse-battery"},
		"next":       {"/tabs/1/pay"},
	})
	if loc := resp.Header.Get("Location"); loc != "/tabs/1/pay" {
		t.Errorf("login did not land on the requested screen: %q", loc)
	}
}

// getNoRedirect performs a GET without following the redirect, so the Location
// header can be inspected.
func (h *harness) getNoRedirect(path string) *http.Response {
	h.t.Helper()
	c := *h.client
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := c.Get(h.server.URL + path)
	if err != nil {
		h.t.Fatalf("get %s: %v", path, err)
	}
	h.t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}
