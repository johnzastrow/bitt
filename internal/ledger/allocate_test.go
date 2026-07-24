package ledger

import (
	"testing"
	"time"

	"github.com/johnzastrow/bitt/internal/money"
	"github.com/johnzastrow/bitt/internal/schedule"
	"github.com/johnzastrow/bitt/internal/store"
)

// entryAt builds a ledger entry on a given day, in the sign convention the
// store uses: a charge is negative because it increases what is owed, and a
// payment is positive.
func entryAt(seq int64, kind store.EntryKind, amount money.Cents, y int, m time.Month, d int) store.Entry {
	return store.Entry{
		Seq:         seq,
		Kind:        kind,
		Amount:      amount,
		EffectiveAt: time.Date(y, m, d, 12, 0, 0, 0, time.UTC),
	}
}

func interestAt(seq int64, amount money.Cents, y int, m time.Month, d int) store.Entry {
	e := entryAt(seq, store.KindCharge, amount, y, m, d)
	e.Category = store.CategoryInterest
	return e
}

func on(y int, m time.Month, d int) schedule.Date { return schedule.NewDate(y, m, d) }

// TestAllocateLoanSplitsInterestFirst is the U.S. Rule's core: a payment covers
// accrued interest before it touches principal.
func TestAllocateLoanSplitsInterestFirst(t *testing.T) {
	entries := []store.Entry{
		entryAt(1, store.KindCharge, -100_000, 2026, time.January, 1), // $1,000 principal
		interestAt(2, -1_000, 2026, time.February, 1),                 // $10 interest
		entryAt(3, store.KindPayment, 25_000, 2026, time.February, 5), // $250 payment
	}

	a := AllocateLoan(entries, on(2026, time.February, 28), time.UTC)

	if a.PrincipalCharged != 100_000 {
		t.Errorf("principal charged = %s, want $1,000.00", a.PrincipalCharged.Display())
	}
	if a.InterestCharged != 1_000 {
		t.Errorf("interest charged = %s, want $10.00", a.InterestCharged.Display())
	}
	if a.UnpaidInterest != 0 {
		t.Errorf("unpaid interest = %s, want zero -- the payment covered it", a.UnpaidInterest.Display())
	}
	// $250 - $10 of interest = $240 against principal.
	if want := money.Cents(100_000 - 24_000); a.Principal != want {
		t.Errorf("principal outstanding = %s, want %s", a.Principal.Display(), want.Display())
	}
	if a.PrincipalPaid() != 24_000 {
		t.Errorf("principal retired = %s, want $240.00", a.PrincipalPaid().Display())
	}
	if a.Owed() != 76_000 {
		t.Errorf("owed = %s, want $760.00", a.Owed().Display())
	}
}

// TestAllocateLoanShortPaymentBanksTheInterest covers the case the U.S. Rule
// exists for: a payment too small to clear the interest leaves the remainder in
// its own bucket, and principal does not move.
func TestAllocateLoanShortPaymentBanksTheInterest(t *testing.T) {
	entries := []store.Entry{
		entryAt(1, store.KindCharge, -100_000, 2026, time.January, 1),
		interestAt(2, -1_000, 2026, time.February, 1),
		entryAt(3, store.KindPayment, 400, 2026, time.February, 5), // $4 against $10 of interest
	}

	a := AllocateLoan(entries, on(2026, time.February, 28), time.UTC)

	if a.UnpaidInterest != 600 {
		t.Errorf("unpaid interest = %s, want $6.00 left in its own bucket", a.UnpaidInterest.Display())
	}
	if a.Principal != 100_000 {
		t.Errorf("principal = %s, want the full $1,000.00 -- a short payment must not "+
			"reach principal", a.Principal.Display())
	}
	if a.PrincipalPaid() != 0 {
		t.Errorf("principal retired = %s, want zero", a.PrincipalPaid().Display())
	}
}

// TestAllocateLoanNeverCapitalizesInterest is the property that separates this
// from the compounding model it replaced: unpaid interest is owed, but it is
// not principal, so the base a later interest accrual is computed on does not
// grow.
func TestAllocateLoanNeverCapitalizesInterest(t *testing.T) {
	entries := []store.Entry{
		entryAt(1, store.KindCharge, -100_000, 2026, time.January, 1),
		interestAt(2, -1_000, 2026, time.February, 1),
		interestAt(3, -1_000, 2026, time.March, 1),
		interestAt(4, -1_000, 2026, time.April, 1),
	}

	a := AllocateLoan(entries, on(2026, time.April, 30), time.UTC)

	// Three periods of interest, nothing paid.
	if a.InterestCharged != 3_000 {
		t.Errorf("interest charged = %s, want $30.00", a.InterestCharged.Display())
	}
	if a.UnpaidInterest != 3_000 {
		t.Errorf("unpaid interest = %s, want $30.00", a.UnpaidInterest.Display())
	}
	// The base for the next accrual is unchanged. If unpaid interest were
	// capitalized this would read $1,030.00 and the loan would compound away
	// from what a lender's statement says.
	if a.Principal != 100_000 {
		t.Errorf("principal = %s, want $1,000.00 -- unpaid interest must not be "+
			"capitalized into the base interest accrues on", a.Principal.Display())
	}
}

func TestAllocateLoanRespectsAsOf(t *testing.T) {
	entries := []store.Entry{
		entryAt(1, store.KindCharge, -100_000, 2026, time.January, 1),
		entryAt(2, store.KindPayment, 25_000, 2026, time.March, 5),
	}

	// Before the payment lands, the whole principal is outstanding.
	before := AllocateLoan(entries, on(2026, time.February, 1), time.UTC)
	if before.Principal != 100_000 || before.Paid != 0 {
		t.Errorf("as of February: principal %s, paid %s -- a March payment must not count",
			before.Principal.Display(), before.Paid.Display())
	}

	after := AllocateLoan(entries, on(2026, time.March, 31), time.UTC)
	if after.Principal != 75_000 || after.Paid != 25_000 {
		t.Errorf("as of March: principal %s, paid %s", after.Principal.Display(), after.Paid.Display())
	}
}

// A backdated entry must be replayed in its own place, because a payment can
// only pay interest that had already been charged when it landed.
func TestAllocateLoanReplaysInEffectiveDateOrder(t *testing.T) {
	// Recorded out of order: the payment has a lower seq than the interest it
	// precedes in time, and the interest was entered afterward.
	entries := []store.Entry{
		entryAt(1, store.KindCharge, -100_000, 2026, time.January, 1),
		entryAt(2, store.KindPayment, 500, 2026, time.January, 10), // before any interest
		interestAt(3, -1_000, 2026, time.February, 1),
	}

	a := AllocateLoan(entries, on(2026, time.February, 28), time.UTC)

	// The January payment had no interest to cover, so all $5 went to
	// principal, and February's $10 stands unpaid.
	if a.Principal != 99_500 {
		t.Errorf("principal = %s, want $995.00", a.Principal.Display())
	}
	if a.UnpaidInterest != 1_000 {
		t.Errorf("unpaid interest = %s, want $10.00", a.UnpaidInterest.Display())
	}
}

func TestAllocateLoanIgnoresReversedEntriesAndFees(t *testing.T) {
	seq2 := int64(2)
	entries := []store.Entry{
		entryAt(1, store.KindCharge, -100_000, 2026, time.January, 1),
		entryAt(2, store.KindPayment, 25_000, 2026, time.February, 5),
		// A reversal of the payment above.
		{
			Seq: 3, Kind: store.KindReversal, Amount: -25_000,
			EffectiveAt: time.Date(2026, time.February, 6, 12, 0, 0, 0, time.UTC),
			ReversesSeq: &seq2,
		},
		// A late fee, which is owed but is not part of the loan.
		entryAt(4, store.KindFee, -2_500, 2026, time.February, 10),
	}

	a := AllocateLoan(entries, on(2026, time.February, 28), time.UTC)

	if a.Paid != 0 {
		t.Errorf("paid = %s, want zero -- the payment was reversed", a.Paid.Display())
	}
	if a.Principal != 100_000 {
		t.Errorf("principal = %s, want the full $1,000.00", a.Principal.Display())
	}
	if a.Owed() != 100_000 {
		t.Errorf("owed = %s, want $1,000.00 -- a late fee is owed but is not the loan",
			a.Owed().Display())
	}
}

func TestAllocateLoanHoldsOverpaymentAsCredit(t *testing.T) {
	entries := []store.Entry{
		entryAt(1, store.KindCharge, -100_000, 2026, time.January, 1),
		entryAt(2, store.KindPayment, 150_000, 2026, time.February, 5),
	}

	a := AllocateLoan(entries, on(2026, time.February, 28), time.UTC)

	if a.Principal != 0 {
		t.Errorf("principal = %s, want zero", a.Principal.Display())
	}
	if a.PaidAhead != 50_000 {
		t.Errorf("paid ahead = %s, want $500.00 -- an overpayment must not drive "+
			"principal negative", a.PaidAhead.Display())
	}
	if a.Owed() != 0 {
		t.Errorf("owed = %s, want zero", a.Owed().Display())
	}
}

func TestAllocateLoanTreatsAdjustmentsBySign(t *testing.T) {
	entries := []store.Entry{
		entryAt(1, store.KindCharge, -100_000, 2026, time.January, 1),
		// Negative: more is owed, a principal correction.
		entryAt(2, store.KindAdjustment, -10_000, 2026, time.January, 15),
		// Positive: less is owed, behaving as a payment does.
		entryAt(3, store.KindAdjustment, 5_000, 2026, time.January, 20),
	}

	a := AllocateLoan(entries, on(2026, time.January, 31), time.UTC)

	if a.PrincipalCharged != 110_000 {
		t.Errorf("principal charged = %s, want $1,100.00", a.PrincipalCharged.Display())
	}
	if a.Principal != 105_000 {
		t.Errorf("principal outstanding = %s, want $1,050.00", a.Principal.Display())
	}
}

// ---------------------------------------------------------------------------
// Adjustments: a Provider's credit comes off principal first
// ---------------------------------------------------------------------------

// TestCreditComesOffPrincipalFirst is the decision that separates a credit from
// a payment. A payment settles the oldest thing owed, which is interest. A
// credit is the Provider deciding part of the debt should not exist, so it
// reduces the principal that keeps generating interest.
func TestCreditComesOffPrincipalFirst(t *testing.T) {
	entries := []store.Entry{
		entryAt(1, store.KindCharge, -100_000, 2026, time.January, 1),
		interestAt(2, -1_000, 2026, time.February, 1),
		// A $40 reconciliation credit while $10 of interest stands unpaid.
		entryAt(3, store.KindAdjustment, 4_000, 2026, time.February, 10),
	}

	a := AllocateLoan(entries, on(2026, time.February, 28), time.UTC)

	if a.Principal != 96_000 {
		t.Errorf("principal = %s, want $960.00 -- the whole credit should come "+
			"off principal", a.Principal.Display())
	}
	if a.UnpaidInterest != 1_000 {
		t.Errorf("unpaid interest = %s, want $10.00 untouched -- a credit is not "+
			"a payment and must not settle interest first", a.UnpaidInterest.Display())
	}
	if a.Credited != 4_000 {
		t.Errorf("credited = %s, want $40.00", a.Credited.Display())
	}
	// A credit is not money, so it must not inflate payments.
	if a.Paid != 0 {
		t.Errorf("paid = %s, want zero -- an adjustment is not a payment", a.Paid.Display())
	}
}

// The contrast, stated directly: the same amount as a payment lands differently.
func TestCreditAndPaymentAllocateDifferently(t *testing.T) {
	base := []store.Entry{
		entryAt(1, store.KindCharge, -100_000, 2026, time.January, 1),
		interestAt(2, -1_000, 2026, time.February, 1),
	}

	credit := AllocateLoan(append(append([]store.Entry{}, base...),
		entryAt(3, store.KindAdjustment, 4_000, 2026, time.February, 10)),
		on(2026, time.February, 28), time.UTC)
	payment := AllocateLoan(append(append([]store.Entry{}, base...),
		entryAt(3, store.KindPayment, 4_000, 2026, time.February, 10)),
		on(2026, time.February, 28), time.UTC)

	// The payment covers $10 of interest and $30 of principal; the credit takes
	// the whole $40 off principal. The credit therefore leaves a smaller
	// principal, which is what makes every later interest charge smaller.
	if payment.Principal != 97_000 {
		t.Errorf("payment left principal %s, want $970.00", payment.Principal.Display())
	}
	if credit.Principal != 96_000 {
		t.Errorf("credit left principal %s, want $960.00", credit.Principal.Display())
	}
	if credit.Principal >= payment.Principal {
		t.Error("a credit should retire more principal than the same amount paid")
	}
	if payment.UnpaidInterest != 0 || credit.UnpaidInterest != 1_000 {
		t.Errorf("interest: payment left %s, credit left %s -- want zero and $10.00",
			payment.UnpaidInterest.Display(), credit.UnpaidInterest.Display())
	}
}

// A credit larger than the principal clears the interest still standing rather
// than sitting as a surplus while the loan is owed.
func TestOversizedCreditThenClearsInterest(t *testing.T) {
	entries := []store.Entry{
		entryAt(1, store.KindCharge, -10_000, 2026, time.January, 1),
		interestAt(2, -1_000, 2026, time.February, 1),
		entryAt(3, store.KindAdjustment, 15_000, 2026, time.February, 10),
	}

	a := AllocateLoan(entries, on(2026, time.February, 28), time.UTC)

	if a.Principal != 0 || a.UnpaidInterest != 0 {
		t.Errorf("principal %s, interest %s -- both should be cleared",
			a.Principal.Display(), a.UnpaidInterest.Display())
	}
	if a.Owed() != 0 {
		t.Errorf("owed = %s, want zero", a.Owed().Display())
	}
	if a.PaidAhead != 4_000 {
		t.Errorf("surplus = %s, want $40.00", a.PaidAhead.Display())
	}
}

// A negative adjustment increases what is owed, as principal.
func TestDebitAdjustmentAddsPrincipal(t *testing.T) {
	entries := []store.Entry{
		entryAt(1, store.KindCharge, -100_000, 2026, time.January, 1),
		entryAt(2, store.KindAdjustment, -2_500, 2026, time.February, 10),
	}

	a := AllocateLoan(entries, on(2026, time.February, 28), time.UTC)

	if a.Principal != 102_500 {
		t.Errorf("principal = %s, want $1,025.00", a.Principal.Display())
	}
	if a.Credited != 0 {
		t.Errorf("credited = %s, want zero for a debit", a.Credited.Display())
	}
}

// A reversed adjustment stops having happened, like any other entry.
func TestReversedCreditIsIgnored(t *testing.T) {
	seq2 := int64(2)
	entries := []store.Entry{
		entryAt(1, store.KindCharge, -100_000, 2026, time.January, 1),
		entryAt(2, store.KindAdjustment, 4_000, 2026, time.February, 10),
		{
			Seq: 3, Kind: store.KindReversal, Amount: -4_000,
			EffectiveAt: time.Date(2026, time.February, 11, 12, 0, 0, 0, time.UTC),
			ReversesSeq: &seq2,
		},
	}

	a := AllocateLoan(entries, on(2026, time.February, 28), time.UTC)

	if a.Credited != 0 || a.Principal != 100_000 {
		t.Errorf("credited %s, principal %s -- a reversed credit must not stand",
			a.Credited.Display(), a.Principal.Display())
	}
}
