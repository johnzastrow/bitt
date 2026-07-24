package ledger

import (
	"github.com/johnzastrow/bitt/internal/loan"
	"github.com/johnzastrow/bitt/internal/money"
	"github.com/johnzastrow/bitt/internal/store"
)

// Plan is what the loan's own arithmetic says about a Payoff tab: the payment
// its terms imply, and how the payment actually being made compares.
//
// It is advisory in both directions and posts nothing. The Provider's entered
// payment stays authoritative because it comes off a lender's paperwork, and no
// period-based model can match a lender accruing per diem to the cent -- the
// split there depends on the days between deposits. So the app's job is to show
// the disagreement, not to resolve it.
type Plan struct {
	// Suggested is the payment the original loan amount, rate, and term imply.
	// Zero when the loan has no term, which leaves nothing to solve for.
	Suggested money.Cents
	// Final is the last payment under that schedule, normally smaller -- the
	// standard adjustment that lands the balance on exactly zero.
	Final money.Cents
	// TotalInterest is what the loan costs over its full term at Suggested.
	TotalInterest money.Cents
	// HasSuggestion reports whether Suggested could be computed at all.
	HasSuggestion bool

	// TrueUp compares the payment being made against what the *remaining*
	// balance and *remaining* periods require. This is the figure that matters
	// once a loan is running: it absorbs whatever drift has already happened
	// and says what it would take to still finish on time.
	TrueUp loan.Drift
	// HasTrueUp reports whether the comparison could be made.
	HasTrueUp bool
}

// Drifting reports whether the entered payment and the suggested one disagree
// by enough to be worth showing.
//
// Three filters, and each earns its place. A cent or two is a lender's rounding
// rather than a disagreement. Falling short of the term always matters, because
// the loan will not be repaid when it was meant to be. But paying *more* than
// the term strictly requires only matters if it actually shortens the loan --
// otherwise a small credit, or a lender's payment rounded up, produces a notice
// that reports the same number of payments before and after, which reads as
// nonsense and teaches the Provider to ignore the banner.
func (p Plan) Drifting() bool {
	if !p.HasTrueUp {
		return false
	}
	d := p.TrueUp.Difference
	if d < 0 {
		d = -d
	}
	if d <= driftTolerance {
		return false
	}
	if p.TrueUp.Behind() {
		return true
	}
	return p.TrueUp.Projected.Periods < p.TrueUp.Remaining
}

// driftTolerance is how far the entered payment may sit from the computed one
// before the tab says so. Five cents covers a lender rounding its annuity
// result up -- on the schedule this is pinned against, the quoted payment came
// in two cents above the computed figure -- while a genuine disagreement about
// rate, term, or balance moves the payment by dollars.
const driftTolerance = money.Cents(5)

// ComputePlan derives the suggested payment and the true-up for a Payoff tab.
//
// Both come from simulating the same arithmetic the ledger posts -- the same
// per-period rounding, the same U.S. Rule allocation -- so a suggestion the
// Provider accepts will actually retire the loan on the date the tab shows.
func ComputePlan(tab store.Tab, p Payoff) Plan {
	var plan Plan
	if tab.Kind != store.TabPayoff || !tab.Schedule.Set() {
		return plan
	}
	num, den := tab.Schedule.RateBasis()
	if num <= 0 || den <= 0 {
		return plan
	}

	// The suggestion at origination: the whole principal over the whole term.
	// This is what the Provider compares a bank's quote against when setting
	// the tab up.
	if tab.LoanTermPeriods > 0 && p.Principal > 0 {
		suggested, out, ok := loan.SuggestPayment(loan.Terms{
			Principal: p.Principal,
			APRBp:     tab.InterestAPRBp,
			RateNum:   num, RateDen: den,
			Term: tab.LoanTermPeriods,
		})
		if ok {
			plan.Suggested = suggested
			plan.Final = out.Final
			plan.TotalInterest = out.Interest
			plan.HasSuggestion = true
		}
	}

	// The true-up: what the balance still outstanding needs over the periods
	// still left. Unpaid interest rides along in its own bucket, repaid first
	// and never accruing, exactly as the ledger treats it.
	if remaining := p.RemainingPeriods(); remaining > 0 && p.Installment > 0 && p.PrincipalOutstanding > 0 {
		drift, ok := loan.Compare(loan.Terms{
			Principal:     p.PrincipalOutstanding,
			AccruedUnpaid: p.UnpaidInterest,
			APRBp:         tab.InterestAPRBp,
			RateNum:       num, RateDen: den,
		}, p.Installment, remaining)
		if ok {
			plan.TrueUp = drift
			plan.HasTrueUp = true
		}
	}

	return plan
}
