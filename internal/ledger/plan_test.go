package ledger

import (
	"testing"
	"time"

	"github.com/johnzastrow/bitt/internal/money"
	"github.com/johnzastrow/bitt/internal/schedule"
	"github.com/johnzastrow/bitt/internal/store"
)

// lenderTab is the quoted amortization schedule the loan package is pinned
// against: $21,852.48 at 5.24% over 48 monthly payments, quoted at $505.65.
func lenderTab(payment money.Cents, term int) store.Tab {
	return store.Tab{
		Kind: store.TabPayoff,
		Schedule: schedule.Schedule{
			Kind:     schedule.MonthlyDay,
			Anchor:   schedule.NewDate(2026, time.January, 15),
			Billing:  schedule.InAdvance,
			Interval: 1,
		},
		InterestAPRBp:   524,
		LoanTermPeriods: term,
		LoanPayment:     payment,
	}
}

func lenderEntries() []store.Entry {
	return []store.Entry{
		entryAt(1, store.KindCharge, -2_185_248, 2026, time.January, 15),
	}
}

func TestComputePlanSuggestsTheLendersPayment(t *testing.T) {
	tab := lenderTab(50_565, 48)
	p := ComputePayoff(tab, lenderEntries(), on(2026, time.January, 20), time.UTC)
	plan := ComputePlan(tab, p)

	if !plan.HasSuggestion {
		t.Fatal("no suggestion for a termed loan")
	}
	// Within the lender's own rounding of $505.65.
	if diff := 50_565 - plan.Suggested; diff < 0 || diff > 5 {
		t.Errorf("suggested %s, lender quoted $505.65", plan.Suggested.Display())
	}
	if plan.Final <= 0 || plan.Final > plan.Suggested {
		t.Errorf("final payment %s should be positive and no larger than the installment",
			plan.Final.Display())
	}
	if plan.TotalInterest <= 0 {
		t.Errorf("total interest %s, want a positive figure", plan.TotalInterest.Display())
	}

	// The lender's own payment is not drift: two cents is their rounding.
	if plan.Drifting() {
		t.Errorf("the lender's payment was flagged as drifting by %s",
			plan.TrueUp.Difference.Display())
	}
}

func TestComputePlanFlagsAnUnderfundedPayment(t *testing.T) {
	tab := lenderTab(40_000, 48) // $400 against a loan that needs ~$505
	p := ComputePayoff(tab, lenderEntries(), on(2026, time.January, 20), time.UTC)
	plan := ComputePlan(tab, p)

	if !plan.HasTrueUp {
		t.Fatal("no true-up computed")
	}
	if !plan.Drifting() {
		t.Error("a payment $100 short of what the term needs is not flagged")
	}
	if !plan.TrueUp.Behind() {
		t.Error("an underfunded payment does not read as behind")
	}
	if plan.TrueUp.Difference >= 0 {
		t.Errorf("difference = %s, want negative", plan.TrueUp.Difference.Display())
	}
	if plan.TrueUp.Projected.Retires && plan.TrueUp.Projected.Periods <= 48 {
		t.Errorf("projected %d periods, expected more than the 48 allowed",
			plan.TrueUp.Projected.Periods)
	}
}

func TestComputePlanFlagsAnOverfundedPayment(t *testing.T) {
	tab := lenderTab(70_000, 48) // $700 clears it early
	p := ComputePayoff(tab, lenderEntries(), on(2026, time.January, 20), time.UTC)
	plan := ComputePlan(tab, p)

	if !plan.Drifting() {
		t.Error("a payment $200 above what the term needs is not flagged")
	}
	if plan.TrueUp.Behind() {
		t.Error("an overfunded payment reads as behind")
	}
	if plan.TrueUp.Projected.Periods >= 48 {
		t.Errorf("projected %d periods, expected fewer than 48", plan.TrueUp.Projected.Periods)
	}
}

// A loan with no term has nothing to solve for, and must not invent one.
func TestComputePlanIsSilentWithoutATerm(t *testing.T) {
	tab := lenderTab(50_565, 0)
	p := ComputePayoff(tab, lenderEntries(), on(2026, time.January, 20), time.UTC)
	plan := ComputePlan(tab, p)

	if plan.HasSuggestion {
		t.Error("an open-ended loan produced a suggested payment")
	}
	if plan.HasTrueUp || plan.Drifting() {
		t.Error("an open-ended loan produced a true-up")
	}
}

func TestComputePlanIgnoresNonPayoffTabs(t *testing.T) {
	tab := lenderTab(50_565, 48)
	tab.Kind = store.TabServices
	plan := ComputePlan(tab, Payoff{})
	if plan.HasSuggestion || plan.HasTrueUp {
		t.Error("a Services tab produced a loan plan")
	}
}

// TestComputePlanRecastsOnRemainingBalance is the true-up proper: once payments
// have been made, the suggestion comes from what is left, not the original loan.
//
// The history is built the way accrual actually posts it -- an interest charge
// each period on the outstanding principal, then the payment -- because a
// history of payments with no interest pays the loan down faster than it
// really would and the recast would correctly report the loan as ahead.
func TestComputePlanRecastsOnRemainingBalance(t *testing.T) {
	tab := lenderTab(50_565, 48)
	entries := lenderEntries()

	principal := money.Cents(2_185_248)
	seq := int64(10)
	for i := range 12 {
		month := time.February + time.Month(i)
		interest, ok := money.InterestFor(principal, 524, 1, 12)
		if !ok {
			t.Fatal("InterestFor not ok")
		}
		entries = append(entries, interestAt(seq, interest.Neg(), 2026, month, 15))
		seq++
		entries = append(entries, entryAt(seq, store.KindPayment, 50_565, 2026, month, 15))
		seq++
		// U.S. Rule: the payment covers the interest, the rest cuts principal.
		principal -= 50_565 - interest
	}

	p := ComputePayoff(tab, entries, on(2027, time.February, 1), time.UTC)
	plan := ComputePlan(tab, p)

	if !plan.HasTrueUp {
		t.Fatal("no true-up after a year of payments")
	}
	if plan.TrueUp.Remaining != 36 {
		t.Errorf("remaining payments = %d, want 36", plan.TrueUp.Remaining)
	}
	if p.PrincipalOutstanding != principal {
		t.Errorf("principal outstanding = %s, want %s",
			p.PrincipalOutstanding.Display(), principal.Display())
	}
	if p.UnpaidInterest != 0 {
		t.Errorf("unpaid interest = %s, want zero -- every payment covered its interest",
			p.UnpaidInterest.Display())
	}
	// A loan paid exactly on schedule needs the same payment for the rest of
	// its term. That is the property a recast has to have, and it is what makes
	// the notice meaningful when it does fire.
	if plan.Drifting() {
		t.Errorf("a loan paid exactly on schedule drifted by %s (suggested %s against %s)",
			plan.TrueUp.Difference.Display(), plan.TrueUp.Suggested.Display(),
			plan.TrueUp.Actual.Display())
	}
}

// TestPayoffExpectationsSpanTheWholeTerm covers the bug the term fixes: the
// expected cumulative used to be capped at the principal, which on an
// interest-bearing loan stops short of what the schedule really asks for.
func TestPayoffExpectationsSpanTheWholeTerm(t *testing.T) {
	tab := lenderTab(50_565, 48)
	entries := lenderEntries()

	// At the very end of the term, everything the schedule ever asked for is
	// expected -- principal plus all the interest, which is more than the
	// principal alone.
	p := ComputePayoff(tab, entries, on(2030, time.January, 15), time.UTC)

	if p.ExpectedByNow <= p.Principal {
		t.Errorf("expected by the end of the term = %s, which is no more than the "+
			"principal %s -- the interest is being left out",
			p.ExpectedByNow.Display(), p.Principal.Display())
	}
	if p.PeriodsElapsed != 48 {
		t.Errorf("periods elapsed = %d, want the term's 48 (never more)", p.PeriodsElapsed)
	}
	if p.Maturity.IsZero() {
		t.Error("a termed loan has no maturity date")
	}
	// 48 monthly periods from 2026-01-15 matures on 2029-12-15.
	if want := schedule.NewDate(2029, time.December, 15); p.Maturity != want {
		t.Errorf("maturity = %s, want %s", p.Maturity, want)
	}
}

// TestPayoffBalanceEqualsTheEntrySum is the project's central promise, checked
// against every entry kind at once including both adjustment directions.
//
// Payoff.Balance is assembled from an allocation rather than summed directly,
// so it can drift from the ledger if a kind is ever counted twice or missed.
// It was missed once: when credits gained their own field, positive
// adjustments briefly reduced the principal without appearing in the balance.
func TestPayoffBalanceEqualsTheEntrySum(t *testing.T) {
	tab := lenderTab(50_565, 48)
	entries := []store.Entry{
		entryAt(1, store.KindCharge, -2_185_248, 2026, time.January, 15),  // principal
		interestAt(2, -9_542, 2026, time.February, 15),                    // interest
		entryAt(3, store.KindPayment, 50_565, 2026, time.February, 16),    // payment
		entryAt(4, store.KindFee, -2_500, 2026, time.February, 20),        // late fee
		entryAt(5, store.KindAdjustment, 4_000, 2026, time.February, 21),  // credit
		entryAt(6, store.KindAdjustment, -1_500, 2026, time.February, 22), // debit
	}

	var want money.Cents
	for _, e := range entries {
		want += e.Amount
	}

	p := ComputePayoff(tab, entries, on(2026, time.March, 1), time.UTC)
	if p.Balance != want {
		t.Errorf("payoff balance = %s, but the entries sum to %s -- the balance "+
			"must be the sum of entries and nothing else",
			p.Balance.Display(), want.Display())
	}

	if p.Credited != 4_000 {
		t.Errorf("credited = %s, want $40.00", p.Credited.Display())
	}
	// Satisfied covers both ways an obligation is discharged.
	if p.Satisfied() != p.Paid+p.Credited {
		t.Errorf("satisfied = %s, want paid %s plus credited %s",
			p.Satisfied().Display(), p.Paid.Display(), p.Credited.Display())
	}
}

// A credit moves a Payoff tab off "Behind" by the amount forgiven, because it
// reduced the obligation rather than leaving it unmet.
func TestCreditCountsTowardTheSchedule(t *testing.T) {
	tab := lenderTab(50_565, 48)
	principal := entryAt(1, store.KindCharge, -2_185_248, 2026, time.January, 15)

	// Three periods due, nothing paid: squarely behind.
	behind := ComputePayoff(tab, []store.Entry{principal}, on(2026, time.March, 20), time.UTC)
	if behind.Status != StatusBehind {
		t.Fatalf("status = %q, want behind", behind.Status)
	}

	// The Provider credits exactly what the schedule expected by now.
	credited := ComputePayoff(tab, []store.Entry{
		principal,
		entryAt(2, store.KindAdjustment, behind.ExpectedByNow, 2026, time.March, 19),
	}, on(2026, time.March, 20), time.UTC)

	if credited.Behind != 0 {
		t.Errorf("still %s behind after crediting the whole expectation",
			credited.Behind.Display())
	}
	if credited.Status == StatusBehind {
		t.Error("a loan credited up to its expectation still reads as behind")
	}
	// And the credit really did retire principal, not just paper over status.
	if credited.PrincipalOutstanding >= behind.PrincipalOutstanding {
		t.Errorf("principal did not fall: %s vs %s",
			credited.PrincipalOutstanding.Display(), behind.PrincipalOutstanding.Display())
	}
}

// Paying slightly more than the term needs is not news unless it actually
// shortens the loan. A credit that trims the balance leaves the payment a
// little larger than required, and reporting "48 payments rather than 48"
// is a sentence with no content.
func TestPlanDoesNotFlagAHarmlessOverpayment(t *testing.T) {
	tab := lenderTab(50_565, 48)
	entries := []store.Entry{
		entryAt(1, store.KindCharge, -2_185_248, 2026, time.January, 15),
		// A $40 credit: the scheduled payment now slightly overshoots.
		entryAt(2, store.KindAdjustment, 4_000, 2026, time.January, 16),
	}

	p := ComputePayoff(tab, entries, on(2026, time.January, 20), time.UTC)
	plan := ComputePlan(tab, p)

	if !plan.HasTrueUp {
		t.Fatal("no true-up")
	}
	if plan.TrueUp.Projected.Periods != plan.TrueUp.Remaining {
		t.Skip("the credit changed the payment count; this case no longer applies")
	}
	if plan.Drifting() {
		t.Errorf("flagged drift that does not change the number of payments: "+
			"%d projected against %d remaining",
			plan.TrueUp.Projected.Periods, plan.TrueUp.Remaining)
	}
}

// An overpayment that genuinely shortens the loan is still reported.
func TestPlanFlagsAnOverpaymentThatShortensTheLoan(t *testing.T) {
	tab := lenderTab(70_000, 48)
	p := ComputePayoff(tab, lenderEntries(), on(2026, time.January, 20), time.UTC)
	plan := ComputePlan(tab, p)

	if !plan.Drifting() {
		t.Error("a payment that clears the loan early was not reported")
	}
	if plan.TrueUp.Projected.Periods >= plan.TrueUp.Remaining {
		t.Errorf("projected %d periods against %d remaining -- expected fewer",
			plan.TrueUp.Projected.Periods, plan.TrueUp.Remaining)
	}
}
