package ledger

import (
	"time"

	"github.com/johnzastrow/bitt/internal/loan"
	"github.com/johnzastrow/bitt/internal/money"
	"github.com/johnzastrow/bitt/internal/schedule"
	"github.com/johnzastrow/bitt/internal/store"
)

// PayoffStatus is how a Payoff tab is tracking against its expected schedule.
type PayoffStatus string

const (
	// StatusSettled means the loan and any fees are fully paid (PAYOFF-03).
	StatusSettled PayoffStatus = "settled"
	// StatusAhead means the Payee has paid at least a full installment beyond
	// what the schedule expected by now.
	StatusAhead PayoffStatus = "ahead"
	// StatusOnTrack means payments have kept up with the expected schedule.
	StatusOnTrack PayoffStatus = "on track"
	// StatusBehind means payments have fallen short of what was expected by now.
	StatusBehind PayoffStatus = "behind"
)

// Payoff is the derived state of a Payoff tab (PAYOFF-01, PAYOFF-02).
//
// Everything here is computed from ledger entries and the schedule on each
// render and stored nowhere, so it cannot drift from the balance the same
// entries produce -- the same commitment that governs every other figure in
// the app.
type Payoff struct {
	// Principal is the original loan total: the sum of non-interest charges
	// posted, excluding reversed ones (interest and fees are not principal).
	Principal money.Cents
	// Interest is the interest charged over the life of the loan so far, waived
	// entries excluded.
	Interest money.Cents
	// Paid is the sum of payments recorded.
	Paid money.Cents
	// Fees is the late fees still standing, waived ones excluded.
	Fees money.Cents
	// Remaining is what is still owed on the loan itself -- principal plus
	// interest, less payments -- floored at zero. Fees are shown separately.
	Remaining money.Cents
	// Balance is the tab balance: negative while anything is owed, including
	// fees. Zero or positive means settled.
	Balance money.Cents
	// ProgressPercent is payments as a whole-number percent of the principal,
	// capped at 100. Computed in integer cents, never through a float.
	ProgressPercent int
	// ExpectedByNow is the cumulative installment the schedule expected to have
	// been paid by today, capped at what the loan costs over its whole term --
	// principal plus the interest the schedule will charge, not principal
	// alone. Capping at the principal understated an interest-bearing loan and
	// could read an on-time borrower as behind.
	ExpectedByNow money.Cents
	// Behind is how far short of ExpectedByNow the payments are, floored at zero.
	Behind money.Cents
	// Installment is the expected payment each period, as the Provider entered
	// it -- the lender's figure, not a derived one.
	Installment money.Cents
	// TermPeriods is how many periods the loan is scheduled to run for. Zero
	// means open-ended.
	TermPeriods int
	// PeriodsElapsed is how many scheduled payments have come due, never more
	// than the term.
	PeriodsElapsed int
	// Maturity is the due date of the term's final period. Zero without a term.
	Maturity schedule.Date
	// PrincipalOutstanding is what is left of the principal, which is what
	// interest accrues on.
	PrincipalOutstanding money.Cents
	// UnpaidInterest is interest charged and not yet covered by a payment. It
	// is owed and repaid before principal, and under the U.S. Rule it never
	// accrues interest of its own.
	UnpaidInterest money.Cents
	// Credited is what the Provider has written off through adjustments. It
	// reduces the obligation rather than satisfying it, and it comes off
	// principal first so the reduction is permanent.
	Credited money.Cents
	// Status summarizes the comparison (PAYOFF-02).
	Status PayoffStatus
}

// RemainingPeriods is how many scheduled payments are still to be made, which
// is the basis a true-up is computed on. Zero for an open-ended loan, which has
// no answer.
//
// It counts payments not yet made rather than periods off the calendar, because
// that is what a lender recasts against and because the calendar answer is
// wrong in the ordinary case: a tab created today has one period already due
// and nothing paid, and dividing the whole principal over the remaining
// calendar periods would report every correctly-configured new loan as
// underfunded. Payments made is the honest denominator -- pay nothing and all
// the payments remain.
//
// A part payment does not count, since a period is only satisfied once its
// installment is covered; the shortfall shows up as a larger required payment,
// which is exactly the true-up's job.
func (p Payoff) RemainingPeriods() int {
	if p.TermPeriods <= 0 || p.Installment <= 0 {
		return 0
	}
	made := int(p.Paid / p.Installment)
	if made > p.TermPeriods {
		made = p.TermPeriods
	}
	if left := p.TermPeriods - made; left > 0 {
		return left
	}
	return 0
}

// Satisfied is how much of the schedule's expectation has been met: payments
// made plus what the Provider has credited off. Both discharge the obligation;
// only one of them is money.
func (p Payoff) Satisfied() money.Cents { return p.Paid + p.Credited }

// Settled reports whether the loan and its fees are fully paid (PAYOFF-03).
func (p Payoff) Settled() bool { return p.Status == StatusSettled }

// ComputePayoff derives a Payoff tab's state from its entries and schedule.
//
// today is the current date in the instance timezone. The expected payment and
// the term both come from the tab, and every figure is derived on each render
// rather than stored, so nothing here can drift from the balance the same
// entries produce.
func ComputePayoff(tab store.Tab, entries []store.Entry, today schedule.Date, loc *time.Location) Payoff {
	// One allocation, under the U.S. Rule, shared with interest accrual and
	// the fee expectations so the three cannot disagree about what a payment
	// paid for.
	alloc := AllocateLoan(entries, today, loc)

	reversed := ReversedSeqs(entries)
	var fees money.Cents
	for _, e := range entries {
		if !reversed[e.Seq] && e.Kind == store.KindFee {
			fees += e.Amount.Neg()
		}
	}

	installment := tab.LoanPayment
	p := Payoff{
		Principal:            alloc.PrincipalCharged,
		Interest:             alloc.InterestCharged,
		Paid:                 alloc.Paid,
		Fees:                 fees,
		Installment:          installment,
		TermPeriods:          tab.LoanTermPeriods,
		PrincipalOutstanding: alloc.Principal,
		UnpaidInterest:       alloc.UnpaidInterest,
		Credited:             alloc.Credited,
		// The tab balance, which must equal the plain sum of entries or the
		// app's central promise is broken. Every kind appears exactly once:
		// payments and credits reduce what is owed, charges (principal and
		// interest) and fees increase it. A negative adjustment is already
		// inside PrincipalCharged.
		Balance: alloc.Paid + alloc.Credited - alloc.PrincipalCharged - alloc.InterestCharged - fees,
	}

	// What is still owed on the loan itself: outstanding principal plus the
	// unpaid interest bucket. Fees are shown separately.
	p.Remaining = alloc.Owed()

	// Progress is principal actually retired, which is what the allocation
	// already worked out by spending each payment on interest first.
	if alloc.PrincipalCharged > 0 {
		p.ProgressPercent = int(int64(alloc.PrincipalPaid()) * 100 / int64(alloc.PrincipalCharged))
	}

	// How many periods have come due, and where the term ends.
	if tab.Schedule.Set() {
		for n := 0; n < schedule.MaxPeriods; n++ {
			if tab.LoanTermPeriods > 0 && n >= tab.LoanTermPeriods {
				break
			}
			if tab.Schedule.Period(n).Due.After(today) {
				break
			}
			p.PeriodsElapsed++
		}
		if tab.LoanTermPeriods > 0 && tab.LoanTermPeriods <= schedule.MaxPeriods {
			p.Maturity = tab.Schedule.Period(tab.LoanTermPeriods - 1).Due
		}
	}

	// The cumulative payment the schedule expected by today.
	//
	// The cap is the whole reason the term matters. It used to be the
	// principal, which on an interest-bearing loan is less than the loan will
	// ever cost -- so the schedule stopped expecting payments while interest
	// was still accruing, and a borrower who was exactly on time could read as
	// behind. The cap is now what the payments actually add up to.
	if installment > 0 && alloc.PrincipalCharged > 0 && tab.Schedule.Set() {
		expected, ok := money.Mul(installment, int64(p.PeriodsElapsed))
		if !ok {
			expected = alloc.PrincipalCharged + alloc.InterestCharged
		}
		if cap := p.scheduledTotal(tab, alloc); cap > 0 && expected > cap {
			expected = cap
		}
		p.ExpectedByNow = expected
		// A credit reduces the obligation, so it counts toward meeting the
		// schedule just as a payment does. Judging the Payee on payments alone
		// would leave them reading as behind by exactly the amount the Provider
		// just forgave, which would make the credit look like it did nothing.
		if behind := expected - p.Satisfied(); behind > 0 {
			p.Behind = behind
		}
	}

	p.Status = payoffStatus(p)
	return p
}

// scheduledTotal is everything the loan is expected to cost over its life:
// principal plus all the interest the schedule will charge.
//
// With a term it is simulated on the same arithmetic the ledger posts, so the
// final, smaller payment is accounted for. Without one -- an open-ended IOU,
// which stays a legitimate way to run a Payoff tab -- the loan has no knowable
// end, so the honest ceiling is what has actually been charged to date.
func (p Payoff) scheduledTotal(tab store.Tab, alloc Allocation) money.Cents {
	charged := alloc.PrincipalCharged + alloc.InterestCharged
	if tab.LoanTermPeriods <= 0 || alloc.PrincipalCharged <= 0 {
		return charged
	}
	num, den := tab.Schedule.RateBasis()
	if num <= 0 || den <= 0 {
		return charged
	}
	out := loan.Simulate(loan.Terms{
		Principal: alloc.PrincipalCharged,
		APRBp:     tab.InterestAPRBp,
		RateNum:   num, RateDen: den,
		Term: tab.LoanTermPeriods,
	}, p.Installment)
	if !out.Retires {
		// The payment does not clear the loan inside its term, so the schedule
		// really does demand an installment every period of it.
		if total, ok := money.Mul(p.Installment, int64(tab.LoanTermPeriods)); ok {
			return total
		}
		return charged
	}
	return out.Total
}

// payoffStatus classifies a Payoff tab against its schedule.
func payoffStatus(p Payoff) PayoffStatus {
	// Settled wins over everything: the loan and its fees are covered.
	if p.Principal > 0 && p.Balance >= 0 {
		return StatusSettled
	}
	if p.Satisfied() < p.ExpectedByNow {
		return StatusBehind
	}
	// Ahead means a full installment prepaid beyond what was expected by now.
	if p.Installment > 0 && p.Satisfied() >= p.ExpectedByNow+p.Installment {
		return StatusAhead
	}
	return StatusOnTrack
}

// Label renders the status for display.
func (s PayoffStatus) Label() string {
	switch s {
	case StatusSettled:
		return "Settled"
	case StatusAhead:
		return "Ahead of schedule"
	case StatusOnTrack:
		return "On track"
	case StatusBehind:
		return "Behind"
	default:
		return ""
	}
}
