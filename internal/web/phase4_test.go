package web

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/johnzastrow/bitt/internal/schedule"
	"github.com/johnzastrow/bitt/internal/store"
)

// createPayoffTab makes a Payoff tab with a loan amount, an expected monthly
// installment, a schedule, and a late fee, all in one create.
func (h *harness) createPayoffTab(loan, installment string, anchor schedule.Date, feeForm url.Values) (int64, string) {
	h.t.Helper()
	form := url.Values{
		"csrf_token":      {h.csrfToken("/tabs/new")},
		"name":            {"Truck loan"},
		"kind":            {string(store.TabPayoff)},
		"loan_amount":     {loan},
		"item_name":       {"Repayment"},
		"item_amount":     {installment},
		"schedule_kind":   {string(schedule.MonthlyDay)},
		"schedule_anchor": {anchor.String()},
	}
	for k, v := range feeForm {
		form[k] = v
	}
	resp, body := h.post("/tabs", form)
	if resp.StatusCode != http.StatusOK {
		h.t.Fatalf("create payoff tab returned %d: %s", resp.StatusCode, truncate(body))
	}
	return tabIDFrom(h.t, body), body
}

// The Phase 4 exit criteria for Payoff tabs, walked end to end.
func TestPayoffTabEndToEnd(t *testing.T) {
	h := newHarness(t)
	h.completeSetup()

	today := instanceToday(t)
	// Anchor the loan two months back so two payments are already expected.
	tabID, body := h.createPayoffTab("5000.00", "250.00", today.AddDays(-62), nil)

	// It reads as a Payoff tab, shows the loan, and the opening balance posted.
	if !strings.Contains(body, "Payoff") {
		t.Errorf("not shown as a Payoff tab: %s", truncate(body))
	}
	if !strings.Contains(body, "Loan progress") {
		t.Errorf("no loan progress panel: %s", truncate(body))
	}
	balance, err := h.db.SumEntries(t.Context(), tabID)
	if err != nil {
		t.Fatalf("balance: %v", err)
	}
	if balance != -500000 {
		t.Fatalf("balance %s after creating a $5,000 loan, want -$5,000", balance)
	}

	// PAYOFF: a Payoff tab posts no scheduled charges, however overdue.
	periods, err := h.db.ListPostedPeriods(t.Context(), tabID)
	if err != nil {
		t.Fatalf("list periods: %v", err)
	}
	if len(periods) != 0 {
		t.Errorf("a Payoff tab claimed %d billing cycles, want 0", len(periods))
	}

	// Record two payments; progress and remaining move.
	for range 2 {
		form := url.Values{
			"csrf_token": {h.csrfToken(tabPath(tabID))},
			"amount":     {"250.00"},
			"method":     {string(store.MethodTransfer)},
		}
		if resp, b := h.post(tabPath(tabID)+"/payments", form); resp.StatusCode != http.StatusOK {
			t.Fatalf("payment returned %d: %s", resp.StatusCode, truncate(b))
		}
	}

	_, body = h.get(tabPath(tabID))
	if !strings.Contains(body, "$4,500.00") {
		t.Errorf("remaining does not show $4,500 after two payments: %s", truncate(body))
	}
	if !strings.Contains(body, "10% of the loan") {
		t.Errorf("progress percent not shown: %s", truncate(body))
	}
}

// PAYOFF-03: a fully paid Payoff loan reads as settled and leaves the active
// dashboard.
func TestPayoffSettlesAtZero(t *testing.T) {
	h := newHarness(t)
	h.completeSetup()

	today := instanceToday(t)
	tabID, _ := h.createPayoffTab("100.00", "100.00", today, nil)

	// Pay the whole loan.
	form := url.Values{
		"csrf_token": {h.csrfToken(tabPath(tabID))},
		"amount":     {"100.00"},
		"method":     {string(store.MethodCash)},
	}
	if resp, b := h.post(tabPath(tabID)+"/payments", form); resp.StatusCode != http.StatusOK {
		t.Fatalf("payment returned %d: %s", resp.StatusCode, truncate(b))
	}

	_, tab := h.get(tabPath(tabID))
	if !strings.Contains(tab, "Settled") {
		t.Errorf("a fully paid loan does not read as settled: %s", truncate(tab))
	}

	// On the dashboard it is marked settled and dimmed out of the active set.
	_, dash := h.get("/")
	if !strings.Contains(dash, "Paid off") {
		t.Errorf("the dashboard does not mark the loan paid off: %s", truncate(dash))
	}
}

// FEE-03/FEE-05 through the web: a percentage late fee posts on an unpaid
// Services period once its grace elapses.
func TestLateFeeAppearsOnUnpaidPeriod(t *testing.T) {
	h := newHarness(t)
	h.completeSetup()

	today := instanceToday(t)
	// A weekly tab anchored five weeks back with a 10% fee, no grace.
	form := url.Values{
		"csrf_token":      {h.csrfToken("/tabs/new")},
		"name":            {"Cleaning"},
		"item_name":       {"Service"},
		"item_amount":     {"100.00"},
		"schedule_kind":   {string(schedule.Weekly)},
		"schedule_anchor": {today.AddDays(-35).String()},
		"fee_kind":        {"percent"},
		"fee_percent":     {"10"},
		"fee_grace_days":  {"0"},
	}
	resp, body := h.post("/tabs", form)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("create returned %d: %s", resp.StatusCode, truncate(body))
	}
	tabID := tabIDFrom(t, body)

	// Reading it posted the overdue charges and their fees.
	fees, err := h.db.ListPostedFees(t.Context(), tabID)
	if err != nil {
		t.Fatalf("list fees: %v", err)
	}
	if len(fees) == 0 {
		t.Fatal("no late fees were assessed on an overdue tab")
	}
	// Each fee is 10% of the $100 charge.
	if fees[0].Base != 10000 {
		t.Errorf("fee base %s, want the $100 charge", fees[0].Base)
	}

	entries, err := h.db.ListEntries(t.Context(), tabID)
	if err != nil {
		t.Fatalf("list entries: %v", err)
	}
	var feeTotal, feeCount int
	for _, e := range entries {
		if e.Kind == store.KindFee {
			feeCount++
			feeTotal += int(e.Amount.Neg())
		}
	}
	if feeTotal != feeCount*1000 {
		t.Errorf("fees total %d cents across %d fees, want $10 each", feeTotal, feeCount)
	}

	// The page announces the fees it just assessed and shows a fee row.
	if !strings.Contains(body, "late fee") && !strings.Contains(body, "late fees") {
		t.Errorf("the page does not mention the late fees: %s", truncate(body))
	}
}

// FEE-07: a Provider can waive a late fee with a reason, and it does not return.
func TestWaiveLateFee(t *testing.T) {
	h := newHarness(t)
	h.completeSetup()

	today := instanceToday(t)
	form := url.Values{
		"csrf_token":      {h.csrfToken("/tabs/new")},
		"name":            {"Rent"},
		"item_name":       {"Rent"},
		"item_amount":     {"1000.00"},
		"schedule_kind":   {string(schedule.MonthlyDay)},
		"schedule_anchor": {today.AddDays(-40).String()},
		"fee_kind":        {"fixed"},
		"fee_fixed":       {"25.00"},
		"fee_grace_days":  {"0"},
	}
	_, body := h.post("/tabs", form)
	tabID := tabIDFrom(t, body)

	fees, err := h.db.ListPostedFees(t.Context(), tabID)
	if err != nil || len(fees) == 0 {
		t.Fatalf("expected a fee to assess: %v", err)
	}

	// Find the fee entry to waive.
	entries, _ := h.db.ListEntries(t.Context(), tabID)
	var feeSeq int64
	for _, e := range entries {
		if e.Kind == store.KindFee {
			feeSeq = e.Seq
		}
	}
	if feeSeq == 0 {
		t.Fatal("no fee entry found")
	}

	balanceBefore, _ := h.db.SumEntries(t.Context(), tabID)

	resp, body := h.post(tabPath(tabID)+"/entries/"+strconv.FormatInt(feeSeq, 10)+"/undo",
		url.Values{"csrf_token": {h.csrfToken(tabPath(tabID))}, "reason": {"first-time courtesy"}})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("waive returned %d: %s", resp.StatusCode, truncate(body))
	}
	if !strings.Contains(body, "waived") && !strings.Contains(body, "Waived") {
		t.Errorf("the page does not confirm the waiver: %s", truncate(body))
	}

	// The reason rode into the reversal's memo.
	entries, _ = h.db.ListEntries(t.Context(), tabID)
	var found bool
	for _, e := range entries {
		if e.Kind == store.KindReversal && strings.Contains(e.Memo, "first-time courtesy") {
			found = true
		}
	}
	if !found {
		t.Error("the waiver reason was not recorded in the reversal")
	}

	// The waiver cleared the fee from the balance.
	balanceAfter, _ := h.db.SumEntries(t.Context(), tabID)
	if balanceAfter != balanceBefore+2500 {
		t.Errorf("balance moved from %s to %s; the $25 waiver should have added it back", balanceBefore, balanceAfter)
	}

	// Reading again does not re-assess the waived date.
	h.get(tabPath(tabID))
	afterFees, _ := h.db.ListPostedFees(t.Context(), tabID)
	if len(afterFees) != len(fees) {
		t.Errorf("a waived date was re-assessed: %d fees, want %d", len(afterFees), len(fees))
	}
}

// FEE-01/FEE-02: the fee policy can be set and cleared on an existing tab.
func TestFeePolicyCanBeSetAndCleared(t *testing.T) {
	h := newHarness(t)
	h.completeSetup()

	tab := h.makeTab("Phone plan", "Line", "50.00")
	tabID := tabIDFrom(t, mustBody(t, h, tab))

	resp, body := h.post(tab+"/fee", url.Values{
		"csrf_token":     {h.csrfToken(tab)},
		"fee_kind":       {"fixed"},
		"fee_fixed":      {"15.00"},
		"fee_grace_days": {"7"},
		"fee_cap":        {"60.00"},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("set fee returned %d: %s", resp.StatusCode, truncate(body))
	}
	got, err := h.db.GetTab(t.Context(), tabID)
	if err != nil {
		t.Fatalf("get tab: %v", err)
	}
	if !got.Fee.Set() || got.Fee.Fixed != 1500 || got.Fee.GraceDays != 7 || got.Fee.Cap != 6000 {
		t.Errorf("fee policy stored as %+v, want a $15 fixed fee, 7-day grace, $60 cap", got.Fee)
	}

	// Clear it.
	if resp, _ := h.post(tab+"/fee", url.Values{"csrf_token": {h.csrfToken(tab)}, "fee_kind": {""}}); resp.StatusCode != http.StatusOK {
		t.Fatalf("clear fee returned %d", resp.StatusCode)
	}
	if got, _ := h.db.GetTab(t.Context(), tabID); got.Fee.Set() {
		t.Errorf("fee policy still set after clearing: %+v", got.Fee)
	}
}

func TestFeePolicyRejectsBadInput(t *testing.T) {
	h := newHarness(t)
	h.completeSetup()
	tab := h.makeTab("Phone plan", "Line", "50.00")

	cases := []struct {
		name string
		form url.Values
		want string
	}{
		{"fixed of zero", url.Values{"fee_kind": {"fixed"}, "fee_fixed": {"0"}}, "greater than zero"},
		{"percent too high", url.Values{"fee_kind": {"percent"}, "fee_percent": {"150"}}, "between 0 and 100"},
		{"bad grace", url.Values{"fee_kind": {"fixed"}, "fee_fixed": {"10"}, "fee_grace_days": {"999"}}, "between 0 and 365"},
		{"unknown kind", url.Values{"fee_kind": {"compound"}}, "not a late-fee kind"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			form := url.Values{"csrf_token": {h.csrfToken(tab)}}
			for k, v := range tc.form {
				form[k] = v
			}
			_, body := h.post(tab+"/fee", form)
			if !strings.Contains(body, tc.want) {
				t.Errorf("message did not mention %q: %s", tc.want, truncate(body))
			}
		})
	}
}

// The in-app upcoming-payment notice appears within the two-week window.
func TestUpcomingPaymentNotice(t *testing.T) {
	h := newHarness(t)
	h.completeSetup()

	today := instanceToday(t)
	// A Payoff loan whose next expected payment is a week out: anchor last month
	// on a day about seven days ahead of today.
	next := today.AddDays(7)
	anchor := schedule.NewDate(today.Year, today.Month, next.Day)
	// Step the anchor back a month so the first upcoming due lands ~a week out.
	tabID, _ := h.createPayoffTab("1200.00", "100.00", anchor.AddDays(-31), nil)

	_, body := h.get(tabPath(tabID))
	if !strings.Contains(body, "Payment coming up") {
		t.Errorf("no upcoming-payment notice within the window: %s", truncate(body))
	}

	// A Services tab far from its next charge shows no notice.
	far := h.makeTab("Quiet", "Line", "10.00")
	// Give it a schedule anchored so the next charge is months away.
	h.post(far+"/schedule", url.Values{
		"csrf_token":      {h.csrfToken(far)},
		"schedule_kind":   {string(schedule.MonthlyDay)},
		"schedule_anchor": {today.AddDays(-2).String()}, // next charge ~a month out
	})
	_, body = h.get(far)
	if strings.Contains(body, "coming up") && strings.Contains(body, "Charge coming up") {
		// Next charge is ~28 days out, outside the 14-day window.
		if daysBetweenTest(today, today.AddDays(28)) > noticeWindowDays {
			t.Errorf("a charge a month out should not show a notice: %s", truncate(body))
		}
	}
}

func daysBetweenTest(a, b schedule.Date) int {
	return int(b.Time(time.UTC).Sub(a.Time(time.UTC)).Hours()) / 24
}

// A Payoff tab that misses an expected payment shows a fee and a behind status.
func TestPayoffBehindShowsFeeAndStatus(t *testing.T) {
	h := newHarness(t)
	h.completeSetup()

	today := instanceToday(t)
	tabID, _ := h.createPayoffTab("5000.00", "250.00", today.AddDays(-40), url.Values{
		"fee_kind":       {"fixed"},
		"fee_fixed":      {"25.00"},
		"fee_grace_days": {"5"},
	})

	// No payments made; by now the first expected payment is well overdue.
	_, body := h.get(tabPath(tabID))
	if !strings.Contains(body, "Behind") {
		t.Errorf("an unpaid loan does not read as behind: %s", truncate(body))
	}
	fees, err := h.db.ListPostedFees(t.Context(), tabID)
	if err != nil {
		t.Fatalf("list fees: %v", err)
	}
	if len(fees) == 0 {
		t.Error("no late fee on a missed loan payment")
	}
}

// Interest on a Payoff loan: set at creation, accrues on the declining balance,
// shown in the panel and history, and configurable afterward.
func TestPayoffInterestEndToEnd(t *testing.T) {
	h := newHarness(t)
	h.completeSetup()

	today := instanceToday(t)
	// $5,000 loan, 6% APR, monthly, anchored today so the first period is due
	// now and accrues interest on the just-posted principal. (Multi-period
	// declining-balance accrual is covered by the frozen-clock ledger tests;
	// a web test runs on the real clock and can only reach the first period.)
	form := url.Values{
		"csrf_token":      {h.csrfToken("/tabs/new")},
		"name":            {"Truck loan"},
		"kind":            {string(store.TabPayoff)},
		"loan_amount":     {"5000.00"},
		"interest_apr":    {"6"},
		"item_name":       {"Repayment"},
		"item_amount":     {"250.00"},
		"schedule_kind":   {string(schedule.MonthlyDay)},
		"schedule_anchor": {today.String()},
	}
	resp, body := h.post("/tabs", form)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("create returned %d: %s", resp.StatusCode, truncate(body))
	}
	tabID := tabIDFrom(t, body)

	// The rate stored, and interest accrued on read.
	tab, err := h.db.GetTab(t.Context(), tabID)
	if err != nil {
		t.Fatalf("get tab: %v", err)
	}
	if tab.InterestAPRBp != 600 {
		t.Errorf("stored APR %d bp, want 600", tab.InterestAPRBp)
	}
	interest, err := h.db.ListPostedInterest(t.Context(), tabID)
	if err != nil {
		t.Fatalf("list interest: %v", err)
	}
	if len(interest) == 0 {
		t.Fatal("no interest accrued on an overdue loan")
	}
	// First month's interest is 0.5% of $5,000 = $25, on the balance.
	if interest[len(interest)-1].Base != 500000 {
		t.Errorf("first interest base %s, want the full $5,000", interest[len(interest)-1].Base)
	}

	// The panel shows interest, and the history labels it.
	if !strings.Contains(body, "interest so far") {
		t.Errorf("panel does not show interest: %s", truncate(body))
	}
	if !strings.Contains(body, "annual interest") {
		t.Errorf("panel does not state the rate")
	}

	// The rate can be changed afterward, and cleared.
	if resp, _ := h.post(tabPath(tabID)+"/interest", url.Values{
		"csrf_token": {h.csrfToken(tabPath(tabID))}, "interest_apr": {"0"},
	}); resp.StatusCode != http.StatusOK {
		t.Fatalf("clear interest returned %d", resp.StatusCode)
	}
	if got, _ := h.db.GetTab(t.Context(), tabID); got.Interest() {
		t.Errorf("interest still set after clearing: %d bp", got.InterestAPRBp)
	}
}
