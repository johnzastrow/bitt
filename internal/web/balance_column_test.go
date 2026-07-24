package web

import (
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/johnzastrow/bitt/internal/store"
)

// runningCell matches the balance column beside each history entry.
var runningCell = regexp.MustCompile(`class="right amount running[^"]*">([^<]*)<`)

// forecastRow matches a projected payment: its date, amount, and the balance
// left after it.
var forecastRow = regexp.MustCompile(
	`<td class="nowrap">([^<]*)</td><td class="right amount">([^<]*)</td>` +
		`<td class="right amount[^"]*">([^<]*)</td>`)

// The history carries a running balance, newest first, and its top row is the
// balance the page shows above it.
func TestHistoryShowsBalanceAfterEachEntry(t *testing.T) {
	h := newHarness(t)
	h.completeSetup()

	tabID := h.createPlainTab("Rent")

	// $400 charged, then $150 paid, then another $50 charged.
	if resp, b := h.post(tabPath(tabID)+"/charges", url.Values{
		"csrf_token": {h.csrfToken(tabPath(tabID))},
		"amount":     {"400.00"},
		"memo":       {"October"},
	}); resp.StatusCode != http.StatusOK {
		t.Fatalf("opening charge returned %d: %s", resp.StatusCode, truncate(b))
	}
	pay := url.Values{
		"csrf_token": {h.csrfToken(tabPath(tabID))},
		"amount":     {"150.00"},
		"method":     {string(store.MethodTransfer)},
	}
	if resp, b := h.post(tabPath(tabID)+"/payments", pay); resp.StatusCode != http.StatusOK {
		t.Fatalf("payment returned %d: %s", resp.StatusCode, truncate(b))
	}
	charge := url.Values{
		"csrf_token": {h.csrfToken(tabPath(tabID))},
		"amount":     {"50.00"},
		"memo":       {"Extra"},
	}
	if resp, b := h.post(tabPath(tabID)+"/charges", charge); resp.StatusCode != http.StatusOK {
		t.Fatalf("charge returned %d: %s", resp.StatusCode, truncate(b))
	}

	_, body := h.get(tabPath(tabID))
	if !strings.Contains(body, "<th class=\"right\">Balance</th>") {
		t.Fatalf("no balance column in the history: %s", truncate(body))
	}

	got := cells(runningCell, body)
	// Newest first: -$300 after the extra charge, -$250 after the payment,
	// -$400 after the opening charge.
	want := []string{"-$300.00", "-$250.00", "-$400.00"}
	if len(got) != len(want) {
		t.Fatalf("%d balance cells %v, want %d: %s", len(got), got, len(want), truncate(body))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("balance row %d = %s, want %s (all: %v)", i, got[i], want[i], got)
		}
	}

	// The top row must agree with the balance in the tab's own header.
	if !strings.Contains(body, "$300.00 owed") {
		t.Errorf("header balance is not $300 owed, so the column disagrees with it: %s", truncate(body))
	}
}

// A Payoff tab projects every payment still to come, with what is left owed
// after each, down to zero.
func TestPayoffShowsFuturePaymentsTable(t *testing.T) {
	h := newHarness(t)
	h.completeSetup()

	// $300 at $100 a month, interest free, starting today: three payments.
	today := instanceToday(t)
	tabID, _ := h.createPayoffTab("300.00", "100.00", today, nil)

	_, body := h.get(tabPath(tabID))
	if !strings.Contains(body, "Balance after payment") {
		t.Fatalf("no payments table on a Payoff tab: %s", truncate(body))
	}
	if !strings.Contains(body, "3 payments left, $300.00 in total, paid off ") {
		t.Errorf("the folded summary does not report the projection: %s", truncate(body))
	}

	rows := forecastRow.FindAllStringSubmatch(body, -1)
	if len(rows) != 3 {
		t.Fatalf("%d projected payments, want 3: %s", len(rows), truncate(body))
	}
	want := []struct{ amount, balance string }{
		{"$100.00", "$200.00"},
		{"$100.00", "$100.00"},
		{"$100.00", "$0.00"},
	}
	for i, w := range want {
		if rows[i][2] != w.amount || rows[i][3] != w.balance {
			t.Errorf("payment %d: %s leaving %s, want %s leaving %s",
				i+1, rows[i][2], rows[i][3], w.amount, w.balance)
		}
	}
	// The first payment falls on the schedule's own next date, not on today by
	// accident: the anchor is today, so it is today.
	if rows[0][1] != today.Display() {
		t.Errorf("first projected payment falls %s, want %s", rows[0][1], today.Display())
	}

	// A Services tab has no loan to project, so no table.
	other := h.createPlainTab("Rent")
	if _, b := h.get(tabPath(other)); strings.Contains(b, "Balance after payment") {
		t.Errorf("a Services tab showed a payments projection: %s", truncate(b))
	}
}

// createPlainTab makes an unscheduled Services tab, which posts nothing of its
// own -- every entry in its history is one this test put there.
func (h *harness) createPlainTab(name string) int64 {
	h.t.Helper()
	resp, body := h.post("/tabs", url.Values{
		"csrf_token": {h.csrfToken("/tabs/new")},
		"name":       {name},
		"kind":       {string(store.TabServices)},
	})
	if resp.StatusCode != http.StatusOK {
		h.t.Fatalf("create tab returned %d: %s", resp.StatusCode, truncate(body))
	}
	return tabIDFrom(h.t, body)
}

// cells pulls the first capture group of every match, in document order.
func cells(re *regexp.Regexp, body string) []string {
	matches := re.FindAllStringSubmatch(body, -1)
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		out = append(out, m[1])
	}
	return out
}
