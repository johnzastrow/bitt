package web

import (
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/johnzastrow/bitt/internal/store"
)

// settleForm scrapes the hidden fields out of a settle confirmation fragment,
// so a test submits what the browser would.
func settleForm(t *testing.T, body string) url.Values {
	t.Helper()
	form := url.Values{}
	for _, name := range []string{"csrf_token", "idempotency_key"} {
		m := regexp.MustCompile(`name="` + name + `" value="([^"]+)"`).FindStringSubmatch(body)
		if len(m) < 2 {
			t.Fatalf("no %s in the confirmation fragment", name)
		}
		form.Set(name, m[1])
	}
	return form
}

// A payment need not be the whole balance. The dashboard offers a second
// control that opens the amount for editing (PAY-01).
func TestSettleAcceptsAPartialAmount(t *testing.T) {
	h := newHarness(t)
	h.completeSetup()

	tab := h.makeTab("Phone plan", "Line", "100.00")
	form := url.Values{
		"csrf_token": {h.csrfToken(tab)},
		"amount":     {"100.00"},
		"memo":       {"first month"},
	}
	if resp, body := h.post(tab+"/charges", form); resp.StatusCode != http.StatusOK {
		t.Fatalf("charge returned %d: %s", resp.StatusCode, truncate(body))
	}

	// The card offers both the full settle and the other-amount control.
	_, dash := h.get("/")
	if !strings.Contains(dash, "Settle $100.00") {
		t.Errorf("dashboard card is missing the one-tap settle: %s", truncate(dash))
	}
	if !strings.Contains(dash, "Other amount") {
		t.Errorf("dashboard card is missing the different-amount control: %s", truncate(dash))
	}

	// The default confirmation still needs no typing: the amount is fixed.
	_, confirm := h.get(tab + "/settle")
	if !strings.Contains(confirm, `name="amount" value="100.00"`) {
		t.Errorf("the default confirmation does not prefill the full balance: %s", truncate(confirm))
	}
	if !strings.Contains(confirm, "Different amount") {
		t.Errorf("the confirmation does not offer a different amount: %s", truncate(confirm))
	}

	// The custom confirmation opens the amount for editing, still prefilled.
	_, custom := h.get(tab + "/settle?custom=1")
	if !strings.Contains(custom, `name="amount"`) || !strings.Contains(custom, `type="text"`) {
		t.Errorf("the custom confirmation does not offer an editable amount: %s", truncate(custom))
	}
	if !strings.Contains(custom, `value="100.00"`) {
		t.Errorf("the custom confirmation does not prefill the balance: %s", truncate(custom))
	}

	// Pay part of it.
	pay := settleForm(t, custom)
	pay.Set("amount", "40.00")
	pay.Set("method", string(store.MethodCash))
	resp, card := h.post(tab+"/settle", pay)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("partial settle returned %d: %s", resp.StatusCode, truncate(card))
	}

	tabID := tabIDFrom(t, mustBody(t, h, tab))
	balance, err := h.db.SumEntries(t.Context(), tabID)
	if err != nil {
		t.Fatalf("balance: %v", err)
	}
	if balance != -6000 {
		t.Errorf("balance %s after a $40 payment against $100, want -60.00", balance)
	}
	if !strings.Contains(card, "$60.00") {
		t.Errorf("the returned card does not show the remaining $60.00: %s", truncate(card))
	}
}

// Paying beyond the balance carries the tab into credit rather than being
// clamped or refused (PAY-05).
func TestSettleAcceptsMoreThanTheBalance(t *testing.T) {
	h := newHarness(t)
	h.completeSetup()

	tab := h.makeTab("Phone plan", "Line", "50.00")
	h.post(tab+"/charges", url.Values{
		"csrf_token": {h.csrfToken(tab)},
		"amount":     {"50.00"},
	})

	_, custom := h.get(tab + "/settle?custom=1")
	pay := settleForm(t, custom)
	pay.Set("amount", "75.00")
	pay.Set("method", string(store.MethodTransfer))
	if resp, body := h.post(tab+"/settle", pay); resp.StatusCode != http.StatusOK {
		t.Fatalf("overpayment returned %d: %s", resp.StatusCode, truncate(body))
	}

	tabID := tabIDFrom(t, mustBody(t, h, tab))
	balance, err := h.db.SumEntries(t.Context(), tabID)
	if err != nil {
		t.Fatalf("balance: %v", err)
	}
	if balance != 2500 {
		t.Errorf("balance %s after paying $75 against $50, want 25.00 in credit", balance)
	}
}

// A settled tab still offers a way to record a payment, since paying ahead is
// how a credit is built.
func TestSettledCardStillOffersAPayment(t *testing.T) {
	h := newHarness(t)
	h.completeSetup()

	tab := h.makeTab("Quiet tab", "Line", "10.00")

	_, dash := h.get("/")
	if strings.Contains(dash, "Settle $") {
		t.Errorf("a settled tab offered a one-tap settle: %s", truncate(dash))
	}
	if !strings.Contains(dash, "Record a payment") {
		t.Errorf("a settled tab offers no way to pay ahead: %s", truncate(dash))
	}

	// And the confirmation opens rather than bouncing back to the plain card.
	_, custom := h.get(tab + "/settle?custom=1")
	if !strings.Contains(custom, `name="amount"`) {
		t.Errorf("no amount field on a settled tab's payment form: %s", truncate(custom))
	}

	pay := settleForm(t, custom)
	pay.Set("amount", "15.00")
	pay.Set("method", string(store.MethodCash))
	if resp, body := h.post(tab+"/settle", pay); resp.StatusCode != http.StatusOK {
		t.Fatalf("paying ahead returned %d: %s", resp.StatusCode, truncate(body))
	}

	tabID := tabIDFrom(t, mustBody(t, h, tab))
	balance, err := h.db.SumEntries(t.Context(), tabID)
	if err != nil {
		t.Fatalf("balance: %v", err)
	}
	if balance != 1500 {
		t.Errorf("balance %s after paying ahead $15, want 15.00 in credit", balance)
	}
}

// A bad amount comes back on the card with the reason, rather than as a status
// code htmx would decline to swap. The message is the one written for the user,
// with no error-plumbing text in front of it.
func TestSettleReportsABadAmountOnTheCard(t *testing.T) {
	h := newHarness(t)
	h.completeSetup()

	tab := h.makeTab("Phone plan", "Line", "50.00")
	h.post(tab+"/charges", url.Values{
		"csrf_token": {h.csrfToken(tab)},
		"amount":     {"50.00"},
	})

	cases := map[string]string{
		"not-a-number": "not a valid dollar figure",
		"0":            "greater than zero",
		"-5.00":        "greater than zero",
	}
	for amount, want := range cases {
		t.Run(amount, func(t *testing.T) {
			_, custom := h.get(tab + "/settle?custom=1")
			pay := settleForm(t, custom)
			pay.Set("amount", amount)
			pay.Set("method", string(store.MethodCash))

			resp, body := h.post(tab+"/settle", pay)
			// 200 so htmx swaps it; the card carries the reason.
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("returned %d, want 200 so the fragment swaps", resp.StatusCode)
			}
			if !strings.Contains(body, want) {
				t.Errorf("card does not explain the problem (%q): %s", want, truncate(body))
			}
			if strings.Contains(body, "web: invalid input") {
				t.Errorf("internal error text reached the screen: %s", truncate(body))
			}
			// The amount stays editable so it can be corrected in place.
			if !strings.Contains(body, `name="amount"`) {
				t.Errorf("the amount field did not come back for correction")
			}
		})
	}

	// Nothing was recorded by any of the refused attempts.
	tabID := tabIDFrom(t, mustBody(t, h, tab))
	balance, err := h.db.SumEntries(t.Context(), tabID)
	if err != nil {
		t.Fatalf("balance: %v", err)
	}
	if balance != -5000 {
		t.Errorf("balance %s after refused payments, want the original -50.00", balance)
	}
}

// The full-page payment form reports the same clean message (PAY-01).
func TestPaymentFormReportsACleanMessage(t *testing.T) {
	h := newHarness(t)
	h.completeSetup()

	tab := h.makeTab("Phone plan", "Line", "50.00")
	_, body := h.post(tab+"/payments", url.Values{
		"csrf_token": {h.csrfToken(tab)},
		"amount":     {"not-a-number"},
		"method":     {string(store.MethodCash)},
	})
	if !strings.Contains(body, "not a valid dollar figure") {
		t.Errorf("payment form did not explain the problem: %s", truncate(body))
	}
	if strings.Contains(body, "web: invalid input") {
		t.Errorf("internal error text reached the screen: %s", truncate(body))
	}
}

func mustBody(t *testing.T, h *harness, path string) string {
	t.Helper()
	_, body := h.get(path)
	return body
}
