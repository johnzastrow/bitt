package web

import (
	"net/url"
	"regexp"
	"strings"
	"testing"
)

// On a loan, every payment field starts at the monthly payment, not the whole
// loan (owner's request, 2026-10-01): the tab page, the payment screen, the
// one-tap settle confirmation, and the card's "other amount" field.

var amountValueRe = regexp.MustCompile(`name="amount"[^>]*value="([^"]*)"`)

// amountValues reads the payment amount fields. On a tab page that is the
// payment form in the top card: the charge and adjustment fields start empty
// on purpose.
func amountValues(body string) []string {
	if i := strings.Index(body, `class="heropay"`); i >= 0 {
		body = body[i:]
		body = body[:strings.Index(body, "</form>")]
	}
	var out []string
	for _, m := range amountValueRe.FindAllStringSubmatch(body, -1) {
		out = append(out, m[1])
	}
	return out
}

func TestLoanPaymentFieldsStartAtTheMonthlyPayment(t *testing.T) {
	h := newHarness(t)
	h.completeSetup()
	tabID, _ := h.createPayoffTab("10000.00", "500.00", instanceToday(t), nil)
	path := tabPath(tabID)

	pages := map[string]string{
		"tab page":       path,
		"payment screen": path + "/pay",
		"settle confirm": path + "/settle",
		"other amount":   path + "/settle?custom=1",
	}
	for name, p := range pages {
		_, body := h.get(p)
		vals := amountValues(body)
		if len(vals) == 0 {
			t.Errorf("%s: no amount field", name)
			continue
		}
		for _, v := range vals {
			if v != "500.00" {
				t.Errorf("%s: amount starts at %q, want the monthly 500.00", name, v)
			}
		}
	}
	// The tab page's note matches the field.
	_, body := h.get(path)
	if !strings.Contains(body, "Prefilled with one period&#39;s payment of $500.00") {
		t.Error("the note does not describe the prefill")
	}

	// Near the end, the payment is capped at what is left: pay all but $300.
	h.post(path+"/payments", url.Values{
		"csrf_token": {h.csrfToken(path)}, "amount": {"9700.00"}, "method": {"transfer"},
		"idempotency_key": {"k-almost"},
	})
	for name, p := range pages {
		_, body := h.get(p)
		for _, v := range amountValues(body) {
			if v != "300.00" {
				t.Errorf("%s near the end: amount %q, want the 300.00 left", name, v)
			}
		}
	}
}

// A loan with no installment has no monthly figure: the whole balance.
func TestLoanWithoutInstallmentPrefillsTheBalance(t *testing.T) {
	h := newHarness(t)
	h.completeSetup()
	tabID, _ := h.createPayoffTab("1200.00", "", instanceToday(t), nil)
	_, body := h.get(tabPath(tabID))
	if vals := amountValues(body); len(vals) == 0 || vals[0] != "1200.00" {
		t.Errorf("amount = %v, want the balance 1200.00", vals)
	}
}

// A Services tab's card "other amount" keeps the whole balance; its tab page
// field is one period's charge, as its note always said.
func TestServicesPrefills(t *testing.T) {
	h := newHarness(t)
	h.completeSetup()
	tabID := h.createPlainTab("Phone")
	h.post(tabPath(tabID)+"/items", url.Values{"csrf_token": {h.csrfToken(tabPath(tabID))}, "name": {"Line"}, "amount": {"40.00"}})
	for i := 0; i < 3; i++ {
		h.post(tabPath(tabID)+"/charges", url.Values{"csrf_token": {h.csrfToken(tabPath(tabID))}, "amount": {"40.00"}, "memo": {"m"}})
	}
	_, body := h.get(tabPath(tabID) + "/settle?custom=1")
	if vals := amountValues(body); len(vals) == 0 || vals[0] != "120.00" {
		t.Errorf("services other amount = %v, want the whole 120.00", vals)
	}
	_, body = h.get(tabPath(tabID))
	if vals := amountValues(body); len(vals) == 0 || vals[0] != "40.00" {
		t.Errorf("services tab page amount = %v, want one period's 40.00", vals)
	}
}
