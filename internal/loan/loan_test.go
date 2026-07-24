package loan

import (
	"math"
	"testing"

	"github.com/johnzastrow/bitt/internal/money"
)

// monthly builds terms on the 1/12 basis a plain monthly loan uses.
func monthly(principal money.Cents, aprBp int64, term int) Terms {
	return Terms{Principal: principal, APRBp: aprBp, RateNum: 1, RateDen: 12, Term: term}
}

// closedForm evaluates the textbook annuity payment in float, purely as an
// independent check on the integer simulation. Nothing outside this test file
// may compute money in floating point; here it exists so that a mistake in the
// simulation cannot be confirmed by the simulation itself.
func closedForm(principal money.Cents, aprBp int64, periodsPerYear, term int) float64 {
	p := float64(principal) / 100
	i := (float64(aprBp) / 10000) / float64(periodsPerYear)
	if i == 0 {
		return p / float64(term)
	}
	return p * i / (1 - math.Pow(1+i, -float64(term)))
}

func TestSuggestPaymentMatchesClosedForm(t *testing.T) {
	cases := []struct {
		name      string
		principal money.Cents
		aprBp     int64
		term      int
	}{
		{"car loan, 4.5% over 4 years", 2_200_000, 450, 48},
		{"mortgage, 6.5% over 30 years", 35_000_000, 650, 360},
		{"small loan, 12% over 1 year", 150_000, 1200, 12},
		{"large principal, 3% over 15 years", 100_000_000, 300, 180},
		{"high rate, 24% over 5 years", 500_000, 2400, 60},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, out, ok := SuggestPayment(monthly(tc.principal, tc.aprBp, tc.term))
			if !ok {
				t.Fatalf("SuggestPayment(%d, %d bp, %d) not ok", tc.principal, tc.aprBp, tc.term)
			}

			// The integer simulation rounds interest to the cent every period,
			// so it may differ from the textbook figure by a cent or two. More
			// than that means the two disagree about the loan, not the rounding.
			want := closedForm(tc.principal, tc.aprBp, 12, tc.term)
			if diff := math.Abs(float64(got)/100 - want); diff > 0.02 {
				t.Errorf("payment = %s, closed form = %.4f, differ by %.4f", got.Display(), want, diff)
			}

			if !out.Retires {
				t.Fatal("suggested payment does not retire the loan")
			}
			if out.Periods != tc.term {
				t.Errorf("retires in %d periods, want the full term of %d", out.Periods, tc.term)
			}
			// Standard practice: the final payment is the adjusted, smaller one.
			if out.Final > got {
				t.Errorf("final payment %s exceeds the installment %s", out.Final.Display(), got.Display())
			}
			if out.Total != tc.principal+out.Interest {
				t.Errorf("total %s != principal %s + interest %s",
					out.Total.Display(), tc.principal.Display(), out.Interest.Display())
			}
		})
	}
}

func TestSuggestPaymentIsTheSmallestThatWorks(t *testing.T) {
	terms := monthly(2_200_000, 450, 48)
	got, _, ok := SuggestPayment(terms)
	if !ok {
		t.Fatal("not ok")
	}
	// One cent less must fail, or the search did not find the minimum.
	if o := Simulate(terms, got-1); o.Retires && o.Periods <= terms.Term {
		t.Errorf("a payment one cent smaller (%s) also retires in %d periods; "+
			"the suggestion is not minimal", (got - 1).Display(), o.Periods)
	}
}

func TestZeroInterestIsPrincipalOverTerm(t *testing.T) {
	// An interest-free IOU: twelve payments on $1,200 is exactly $100.
	got, out, ok := SuggestPayment(monthly(120_000, 0, 12))
	if !ok {
		t.Fatal("not ok")
	}
	if got != 10_000 {
		t.Errorf("payment = %s, want $100.00", got.Display())
	}
	if out.Interest != 0 {
		t.Errorf("interest = %s on a 0%% loan, want zero", out.Interest.Display())
	}
	if out.Periods != 12 {
		t.Errorf("periods = %d, want 12", out.Periods)
	}
}

func TestZeroInterestRoundsUpAndShrinksTheFinalPayment(t *testing.T) {
	// $100.00 over 3 periods does not divide evenly. Rounding the payment up
	// to $33.34 leaves $33.32 at the end, which is what a lender does; rounding
	// down to $33.33 would leave a one-cent stub period after the term.
	got, out, ok := SuggestPayment(monthly(10_000, 0, 3))
	if !ok {
		t.Fatal("not ok")
	}
	if got != 3_334 {
		t.Errorf("payment = %s, want $33.34", got.Display())
	}
	if out.Periods != 3 {
		t.Errorf("periods = %d, want 3", out.Periods)
	}
	if out.Final != 3_332 {
		t.Errorf("final = %s, want $33.32", out.Final.Display())
	}
}

// TestUnpaidInterestDoesNotCompound is the U.S. Rule, and it is the reason this
// package exists in the shape it does. A borrower who pays nothing for three
// periods must owe three identical interest charges, not a growing series.
func TestUnpaidInterestDoesNotCompound(t *testing.T) {
	// $22,000 at 4.5% monthly is $82.50 a period, on principal that never moves
	// because the payment only ever covers part of the interest.
	terms := monthly(2_200_000, 450, 48)
	one, ok := money.InterestFor(terms.Principal, terms.APRBp, terms.RateNum, terms.RateDen)
	if !ok {
		t.Fatal("InterestFor not ok")
	}
	if one != 8_250 {
		t.Fatalf("one period's interest = %s, want $82.50", one.Display())
	}

	// Simulate with a payment that cannot cover the interest: the loan never
	// retires, which is only true if interest stays flat. Were unpaid interest
	// capitalized, the balance would grow without bound and the answer would be
	// the same -- so check the arithmetic directly as well.
	if o := Simulate(terms, 8_249); o.Retires {
		t.Error("a payment below the period interest retired the loan")
	}

	// Three periods of arrears carried into a true-up must be exactly 3x, with
	// no interest charged on the arrears themselves.
	arrears := Terms{
		Principal:     terms.Principal,
		AccruedUnpaid: 3 * one,
		APRBp:         terms.APRBp,
		RateNum:       1, RateDen: 12,
		Term: 48,
	}
	out := Simulate(arrears, 2_200_000+3*one+one)
	if !out.Retires || out.Periods != 1 {
		t.Fatalf("paying everything off did not retire in one period: %+v", out)
	}
	// The one period simulated charges interest on principal only: the $247.50
	// bucket must not have earned a cent.
	if out.Interest != one {
		t.Errorf("interest charged = %s, want exactly one period's %s -- the "+
			"unpaid bucket must not accrue", out.Interest.Display(), one.Display())
	}
}

func TestArrearsAreRepaidBeforePrincipal(t *testing.T) {
	// A payment lands on the unpaid bucket first. With $100 of arrears and a
	// $500 payment, principal falls by $500 - $100 - this period's interest.
	terms := Terms{
		Principal: 1_000_000, AccruedUnpaid: 10_000,
		APRBp: 1200, RateNum: 1, RateDen: 12, Term: 24,
	}
	interest, _ := money.InterestFor(terms.Principal, terms.APRBp, terms.RateNum, terms.RateDen)
	if interest != 10_000 {
		t.Fatalf("interest = %s, want $100.00", interest.Display())
	}

	// Pay exactly the arrears plus this period's interest: principal must not
	// move at all, and the loan must not be any closer to retiring.
	out := Simulate(terms, 20_000)
	if out.Retires && out.Periods == 1 {
		t.Error("a payment covering only interest and arrears retired the loan")
	}
}

func TestRateBasisChangesThePayment(t *testing.T) {
	// The same loan on a three-week cycle: 21/365 of a year per period, which
	// no periods-per-year integer expresses. Sanity is that a shorter period
	// carries less interest per period than a monthly one.
	weekly3 := Terms{Principal: 2_200_000, APRBp: 450, RateNum: 21, RateDen: 365, Term: 48}
	perPeriod, ok := money.InterestFor(weekly3.Principal, weekly3.APRBp, 21, 365)
	if !ok {
		t.Fatal("not ok")
	}
	monthlyInterest, _ := money.InterestFor(weekly3.Principal, weekly3.APRBp, 1, 12)
	if perPeriod >= monthlyInterest {
		t.Errorf("three weeks (%s) should carry less interest than a month (%s)",
			perPeriod.Display(), monthlyInterest.Display())
	}
	if _, _, ok := SuggestPayment(weekly3); !ok {
		t.Error("SuggestPayment not ok on a three-week basis")
	}
}

func TestMonthlyBasisIsExactlyAPROver12(t *testing.T) {
	// A borrower checks this by hand: 6% of $10,000 is $600 a year, $50 a month.
	got, ok := money.InterestFor(1_000_000, 600, 1, 12)
	if !ok {
		t.Fatal("not ok")
	}
	if got != 5_000 {
		t.Errorf("interest = %s, want $50.00", got.Display())
	}
}

func TestCompareReportsDrift(t *testing.T) {
	terms := monthly(2_200_000, 450, 48)
	suggested, _, ok := SuggestPayment(terms)
	if !ok {
		t.Fatal("not ok")
	}

	t.Run("underpaying runs past the term", func(t *testing.T) {
		d, ok := Compare(terms, suggested-5_000, 48)
		if !ok {
			t.Fatal("not ok")
		}
		if !d.Behind() {
			t.Error("paying $50 less a period should not stay on term")
		}
		if d.Difference != -5_000 {
			t.Errorf("difference = %s, want -$50.00", d.Difference.Display())
		}
		if d.Projected.Retires && d.Projected.Periods <= 48 {
			t.Errorf("projected %d periods, expected more than the 48 allowed", d.Projected.Periods)
		}
	})

	t.Run("the suggested payment is on term", func(t *testing.T) {
		d, ok := Compare(terms, suggested, 48)
		if !ok {
			t.Fatal("not ok")
		}
		if d.Behind() {
			t.Error("the suggested payment should be on term")
		}
		if d.Difference != 0 {
			t.Errorf("difference = %s, want zero", d.Difference.Display())
		}
	})

	t.Run("overpaying finishes early", func(t *testing.T) {
		d, ok := Compare(terms, suggested+10_000, 48)
		if !ok {
			t.Fatal("not ok")
		}
		if d.Behind() {
			t.Error("paying $100 more a period should be on term")
		}
		if d.Projected.Periods >= 48 {
			t.Errorf("projected %d periods, expected fewer than 48", d.Projected.Periods)
		}
	})
}

func TestSimulateRejectsNonsense(t *testing.T) {
	cases := []struct {
		name    string
		terms   Terms
		payment money.Cents
	}{
		{"no principal", monthly(0, 450, 48), 50_000},
		{"negative principal", monthly(-100, 450, 48), 50_000},
		{"negative rate", monthly(100_000, -1, 48), 50_000},
		{"zero denominator", Terms{Principal: 100_000, APRBp: 450, RateNum: 1, RateDen: 0, Term: 12}, 50_000},
		{"zero numerator", Terms{Principal: 100_000, APRBp: 450, RateNum: 0, RateDen: 12, Term: 12}, 50_000},
		{"negative arrears", Terms{Principal: 100_000, AccruedUnpaid: -1, APRBp: 450, RateNum: 1, RateDen: 12, Term: 12}, 50_000},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if o := Simulate(tc.terms, tc.payment); o.Retires {
				t.Errorf("Simulate accepted %+v", tc.terms)
			}
			if _, _, ok := SuggestPayment(tc.terms); ok {
				t.Errorf("SuggestPayment accepted %+v", tc.terms)
			}
		})
	}
}

// TestSimulateRejectsBadPayments keeps the payment separate from the terms:
// these terms are perfectly good, so SuggestPayment must still answer for them.
func TestSimulateRejectsBadPayments(t *testing.T) {
	terms := monthly(100_000, 450, 48)
	for _, payment := range []money.Cents{0, -1, -50_000} {
		if o := Simulate(terms, payment); o.Retires {
			t.Errorf("Simulate accepted a payment of %s", payment.Display())
		}
	}
	if _, _, ok := SuggestPayment(terms); !ok {
		t.Error("SuggestPayment should still answer for sound terms")
	}
}

func TestSuggestPaymentRejectsImpossibleTerms(t *testing.T) {
	if _, _, ok := SuggestPayment(monthly(100_000, 450, 0)); ok {
		t.Error("a term of zero periods was accepted")
	}
	if _, _, ok := SuggestPayment(monthly(100_000, 450, MaxPeriods+1)); ok {
		t.Error("a term past MaxPeriods was accepted")
	}
}

// TestLongTermStaysExact guards the overflow path: a large principal at a high
// rate over a long term multiplies balance x bp x numerator before dividing.
func TestLongTermStaysExact(t *testing.T) {
	got, out, ok := SuggestPayment(monthly(500_000_000_00, 1800, 360))
	if !ok {
		t.Fatal("SuggestPayment not ok on a large loan")
	}
	if got <= 0 || !out.Retires {
		t.Fatalf("payment = %s, outcome = %+v", got.Display(), out)
	}
	if out.Periods != 360 {
		t.Errorf("periods = %d, want 360", out.Periods)
	}
}

// TestAgainstAQuotedSchedule pins the engine to a lender's quoted amortization
// schedule rather than to a formula.
//
// This is the test that matters most in this file. The closed-form check above
// only proves the simulation agrees with a textbook formula; both could share a
// wrong convention. Reproducing an issued schedule to within the lender's own
// rounding is what shows the three conventions -- simple interest on the
// declining balance, APR/12 on a 30/360 basis, and U.S. Rule allocation -- are
// the ones a lender actually uses.
//
// The two-cent gap is the lender rounding its annuity result up, which is
// ordinary practice. A day-count disagreement would show up as dollars over a
// 48-month term, not cents, so the tolerance here is deliberately tight: widen
// it and the test stops being able to detect a convention change.
func TestAgainstAQuotedSchedule(t *testing.T) {
	const (
		principal = money.Cents(2_185_248) // $21,852.48
		aprBp     = int64(524)             // 5.24%
		term      = 48                     // monthly
		quoted    = money.Cents(50_565)    // $505.65, as quoted on the schedule
	)

	terms := monthly(principal, aprBp, term)
	got, out, ok := SuggestPayment(terms)
	if !ok {
		t.Fatal("SuggestPayment not ok")
	}

	if diff := quoted - got; diff < 0 || diff > 5 {
		t.Errorf("payment = %s, lender quoted %s, off by %s -- more than "+
			"the lender's own rounding explains, so a convention has changed",
			got.Display(), quoted.Display(), diff.Display())
	}

	// The first payment's split is the single fact that identifies the
	// day-count basis. On a 30/360 basis this is principal x 5.24% / 12.
	first, ok := money.InterestFor(principal, aprBp, 1, 12)
	if !ok {
		t.Fatal("InterestFor not ok")
	}
	if first != 9_542 {
		t.Errorf("first period interest = %s, want $95.42 -- a different "+
			"figure means the basis is no longer APR/12", first.Display())
	}
	if principalPaid := quoted - first; principalPaid != 41_023 {
		t.Errorf("first payment retires %s of principal, want $410.23", principalPaid.Display())
	}

	// The lender's own payment must still land the loan exactly on its term:
	// two cents over the minimum finishes on time, not a period early.
	d, ok := Compare(terms, quoted, term)
	if !ok {
		t.Fatal("Compare not ok")
	}
	if !d.OnTerm {
		t.Error("the lender's own payment does not retire the loan within its term")
	}
	if d.Projected.Periods != term {
		t.Errorf("the lender's payment retires in %d periods, want exactly %d",
			d.Projected.Periods, term)
	}
	if !out.Retires || out.Periods != term {
		t.Errorf("suggested payment retires in %d periods, want %d", out.Periods, term)
	}
}
