// Package loan computes installment-loan payments and projections.
//
// It is the fourth of the pure packages, alongside money, schedule, and fee:
// no database, no clock, no I/O, so the arithmetic that is awkward to get right
// is pinned by tests before any of it is wired to a tab.
//
// Three conventions govern everything here, and all three are the ones a US
// consumer lender uses, because the number this package produces is going to be
// compared against a bank's:
//
//   - Simple interest on the declining balance. Interest is charged on what is
//     still owed, not precomputed at origination and split across the term.
//     Precomputed interest -- the Rule of 78s -- would make an early payoff
//     save the borrower nothing, and is exactly what this package must not do.
//
//   - The U.S. Rule for allocation. Each payment covers accrued interest first
//     and reduces principal with the remainder. Interest that a short payment
//     does not cover accumulates in its own bucket and earns no interest of its
//     own: unpaid interest is never capitalized into principal. This is the
//     rule Regulation Z permits for closed-end credit, and it is why a missed
//     payment here does not compound away from what a lender's statement says.
//
//   - Rounding to the cent once per period, on the interest, before the payment
//     is split. That is how an amortization schedule is computed by hand and by
//     every lender's system, and rounding anywhere else drifts by the end of a
//     long term.
//
// Because the suggested payment is derived by *simulating* those rules rather
// than by evaluating the closed-form annuity formula, it agrees with what the
// ledger will actually post, per-period rounding included. The closed form
// needs a float or an arbitrary-precision rational, and disagrees with integer
// per-period rounding by a few cents over a 30-year term. The simulation
// cannot: it is the same arithmetic.
package loan

import "github.com/johnzastrow/bitt/internal/money"

// MaxPeriods bounds a simulation. It matches schedule.MaxPeriods -- 600
// periods is 50 years of monthly payments -- and is duplicated rather than
// imported to keep this package's dependencies to money alone.
const MaxPeriods = 600

// Terms describe a fixed-rate installment loan.
type Terms struct {
	// Principal is what is still owed on the loan itself. For a suggestion at
	// origination this is the full loan amount; for a true-up it is the
	// outstanding principal today.
	Principal money.Cents
	// AccruedUnpaid is interest already charged and not yet paid, held in the
	// U.S. Rule's separate bucket. It is repaid before principal and never
	// earns interest. Zero at origination.
	AccruedUnpaid money.Cents
	// APRBp is the annual rate in basis points (100 bp = 1%).
	APRBp int64
	// RateNum and RateDen give the fraction of a year one period covers --
	// schedule.Schedule.RateBasis supplies them. Monthly is 1/12; a three-week
	// period is 21/365.
	RateNum, RateDen int
	// Term is how many periods the loan is scheduled to run for.
	Term int
}

// Valid reports whether the terms can be simulated at all.
func (t Terms) Valid() bool {
	return t.Principal > 0 && t.APRBp >= 0 && t.RateNum > 0 && t.RateDen > 0 &&
		t.AccruedUnpaid >= 0
}

// Outcome is what a given payment does to a loan.
type Outcome struct {
	// Retires reports whether the loan reaches zero at all. A payment that does
	// not cover a period's interest never will.
	Retires bool
	// Periods is how many payments it takes, the last one included. Zero when
	// the loan does not retire.
	Periods int
	// Final is the last payment, which is normally smaller than the others --
	// the standard practice of adjusting the final payment to zero the balance
	// rather than leaving a stub or overpaying by a few cents.
	Final money.Cents
	// Interest is the total interest charged over those periods.
	Interest money.Cents
	// Total is every payment added together: principal plus interest.
	Total money.Cents
}

// Simulate runs payment against terms period by period and reports what
// happens, stopping at MaxPeriods.
//
// Each period accrues interest on the outstanding principal only, adds it to
// the unpaid-interest bucket, then applies the payment to that bucket first and
// to principal with what is left. The final period pays exactly what remains.
func Simulate(t Terms, payment money.Cents) Outcome {
	var out Outcome
	if !t.Valid() || payment <= 0 {
		return out
	}

	principal, unpaid := t.Principal, t.AccruedUnpaid
	for n := 1; n <= MaxPeriods; n++ {
		interest, ok := money.InterestFor(principal, t.APRBp, t.RateNum, t.RateDen)
		if !ok {
			return Outcome{}
		}
		unpaid += interest
		out.Interest += interest

		// The last payment is whatever is actually left, not the installment.
		owed := principal + unpaid
		if owed <= payment {
			out.Retires = true
			out.Periods = n
			out.Final = owed
			out.Total += owed
			return out
		}
		out.Total += payment

		// U.S. Rule: interest first, and only then principal.
		if payment >= unpaid {
			principal -= payment - unpaid
			unpaid = 0
			continue
		}
		unpaid -= payment
		// Principal did not move this period. Whether it ever will depends on
		// the payment against *this period's* interest, not against the whole
		// bucket: a payment that beats the interest still whittles an inherited
		// arrears down and eventually reaches principal, but one that does not
		// is outrun every period and never gets there.
		if payment <= interest {
			return Outcome{}
		}
	}
	return Outcome{}
}

// SuggestPayment returns the smallest per-period payment that retires the loan
// within its term, together with what that schedule costs.
//
// It binary-searches the payment and simulates each candidate, so the answer is
// consistent with the per-period rounding the ledger will actually apply. The
// smallest qualifying payment is the right one to suggest: rounding a payment
// up is what lenders do, and it makes the final payment the small one rather
// than leaving a stub period after the term.
//
// ok is false if the terms are unusable or no payment retires the loan in time.
func SuggestPayment(t Terms) (payment money.Cents, out Outcome, ok bool) {
	if !t.Valid() || t.Term < 1 || t.Term > MaxPeriods {
		return 0, Outcome{}, false
	}

	// Paying the whole balance plus one period's interest always retires the
	// loan in a single period, so it is an upper bound for any term of 1 or
	// more. Anything above it is wasted search space.
	interest, iok := money.InterestFor(t.Principal, t.APRBp, t.RateNum, t.RateDen)
	if !iok {
		return 0, Outcome{}, false
	}
	hi := t.Principal + t.AccruedUnpaid + interest
	if !within(t, hi) {
		return 0, Outcome{}, false
	}

	// A larger payment never retires a loan more slowly, so the predicate is
	// monotonic and the search finds the smallest payment that qualifies.
	lo := money.Cents(1)
	for lo < hi {
		mid := lo + (hi-lo)/2
		if within(t, mid) {
			hi = mid
		} else {
			lo = mid + 1
		}
	}
	return lo, Simulate(t, lo), true
}

// within reports whether payment retires the loan inside the term.
func within(t Terms, payment money.Cents) bool {
	o := Simulate(t, payment)
	return o.Retires && o.Periods <= t.Term
}

// Drift compares the payment actually being made against what the term
// requires, which is the true-up: bank payments and this schedule disagree for
// reasons no period-based model can eliminate -- a lender accruing per diem
// splits a payment differently depending on the day it lands -- so the entered
// payment stays authoritative and this reports the consequence of the gap.
type Drift struct {
	// Suggested is the payment the term requires from here.
	Suggested money.Cents
	// Actual is the payment the Provider entered.
	Actual money.Cents
	// Difference is Actual less Suggested: negative means underpaying, which
	// runs past the term; positive means ahead of it.
	Difference money.Cents
	// Projected is what the actual payment does. Its Periods is how many more
	// payments are really left, against the term's remaining count.
	Projected Outcome
	// Remaining is the number of periods the term still allows.
	Remaining int
	// OnTerm reports whether the actual payment retires the loan within them.
	OnTerm bool
}

// Behind reports whether the payment will not retire the loan in time -- either
// it runs past the term or it never gets there at all.
func (d Drift) Behind() bool { return !d.OnTerm }

// Compare works out the drift between the payment being made and the payment
// the remaining term requires. remaining is how many periods of the term are
// left; terms carries the balance as it stands today.
func Compare(t Terms, actual money.Cents, remaining int) (Drift, bool) {
	if !t.Valid() || actual <= 0 || remaining < 1 {
		return Drift{}, false
	}
	t.Term = remaining
	suggested, _, ok := SuggestPayment(t)
	if !ok {
		return Drift{}, false
	}
	projected := Simulate(t, actual)
	return Drift{
		Suggested:  suggested,
		Actual:     actual,
		Difference: actual - suggested,
		Projected:  projected,
		Remaining:  remaining,
		OnTerm:     projected.Retires && projected.Periods <= remaining,
	}, true
}
