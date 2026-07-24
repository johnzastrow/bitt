package ledger

import (
	"sort"
	"time"

	"github.com/johnzastrow/bitt/internal/money"
	"github.com/johnzastrow/bitt/internal/schedule"
	"github.com/johnzastrow/bitt/internal/store"
)

// Allocation is a loan's state under the U.S. Rule as of some date: how much
// principal is still outstanding, and how much charged interest is still
// unpaid.
//
// The distinction is the whole point. Under the U.S. Rule -- the allocation
// Regulation Z permits for closed-end credit, and the one a consumer lender
// runs -- a payment covers accrued interest first and reduces principal with
// what is left, and interest that a short payment fails to cover accumulates in
// its own bucket where it earns no interest of its own. Unpaid interest is
// never capitalized into principal.
//
// That is why Principal and UnpaidInterest are separate fields rather than one
// balance: interest accrues on the former and never on the latter. Collapsing
// them into a single "amount owed" is precisely the compounding this must not
// do, and is what would make a missed payment drift permanently away from what
// the lender's statement says.
//
// Late fees are deliberately absent. A fee is a penalty, not part of the loan;
// it is owed, and it appears in the tab balance, but interest is never charged
// on it and a payment is not modeled as retiring it before principal.
type Allocation struct {
	// Principal is what is still owed of the loan itself, floored at zero.
	// This is the base every interest accrual is computed on.
	Principal money.Cents
	// UnpaidInterest is interest charged and not yet covered by a payment. It
	// is owed, and it is repaid before principal, and it never accrues.
	UnpaidInterest money.Cents
	// PrincipalCharged and InterestCharged are the cumulative totals, before
	// any payment is applied.
	PrincipalCharged money.Cents
	InterestCharged  money.Cents
	// Paid is every payment recorded through the date.
	Paid money.Cents
	// PaidAhead is payment beyond everything the loan owed. It sits here rather
	// than driving principal negative.
	PaidAhead money.Cents
	// Credited is the net amount the Provider has written off the loan through
	// adjustments -- a reconciliation credit, a goodwill reduction, a correction
	// to an over-charge that is not a whole entry. It is money the Payee never
	// has to pay, which is what separates it from a payment: it reduces the
	// obligation rather than satisfying it.
	Credited money.Cents
}

// Owed is what the loan itself still costs to clear: outstanding principal plus
// the unpaid interest bucket. Fees are not included.
func (a Allocation) Owed() money.Cents { return a.Principal + a.UnpaidInterest }

// PrincipalPaid is how much of the original principal has actually been
// retired, which is what loan progress should be measured by. Paying interest
// is not progress against the loan.
func (a Allocation) PrincipalPaid() money.Cents {
	return a.PrincipalCharged - a.Principal
}

// AllocateLoan replays a tab's entries in effective-date order and reports the
// loan state as of asOf, applying the U.S. Rule to every payment.
//
// Entries effective after asOf are ignored, so the same function answers "what
// was the principal at period seven" and "what is it now". Reversed entries are
// skipped entirely, which is how an undone payment stops having allocated.
//
// Not to be confused with Allocate in statement.go, which spreads credits
// across billing cycles for a statement. This one splits payments between
// interest and principal within a loan.
func AllocateLoan(entries []store.Entry, asOf schedule.Date, loc *time.Location) Allocation {
	if loc == nil {
		loc = time.UTC
	}
	reversed := ReversedSeqs(entries)

	// Order matters here in a way it does not for a plain sum: a payment can
	// only pay interest that has already been charged, so a backdated entry has
	// to be replayed in its own place. The sequence breaks ties, since two
	// entries can share an effective date.
	ordered := make([]store.Entry, 0, len(entries))
	for _, e := range entries {
		if reversed[e.Seq] || e.Kind == store.KindReversal {
			continue
		}
		if schedule.DateOf(e.EffectiveAt.In(loc)).After(asOf) {
			continue
		}
		ordered = append(ordered, e)
	}
	sort.SliceStable(ordered, func(i, j int) bool {
		if ordered[i].EffectiveAt.Equal(ordered[j].EffectiveAt) {
			return ordered[i].Seq < ordered[j].Seq
		}
		return ordered[i].EffectiveAt.Before(ordered[j].EffectiveAt)
	})

	var a Allocation
	for _, e := range ordered {
		switch {
		case e.Kind == store.KindCharge && e.Category == store.CategoryInterest:
			amount := e.Amount.Neg()
			a.InterestCharged += amount
			a.UnpaidInterest += amount
		case e.Kind == store.KindCharge:
			amount := e.Amount.Neg()
			a.PrincipalCharged += amount
			a.Principal += amount
		case e.Kind == store.KindPayment:
			a.Paid += e.Amount
			a.apply(e.Amount)
		case e.Kind == store.KindAdjustment:
			// An adjustment moves the balance outside the normal charge path.
			// Negative means more is owed, which is a principal correction.
			if e.Amount < 0 {
				amount := e.Amount.Neg()
				a.PrincipalCharged += amount
				a.Principal += amount
				break
			}
			// Positive means less is owed, and it comes off principal FIRST --
			// deliberately unlike a payment, which covers interest first.
			//
			// A payment is the Payee meeting an obligation, and interest is the
			// oldest part of what they owe, so it is what a payment settles. A
			// credit is the Provider deciding part of the debt should not exist.
			// Applying it to interest would leave the principal that generated
			// that interest untouched, so the same interest accrues again next
			// period and the credit quietly funds it. Off principal, the
			// reduction is permanent and every future accrual is smaller --
			// which is what "you owe $40 less" is meant to mean.
			a.Credited += e.Amount
			a.applyPrincipalFirst(e.Amount)
		}
		// Fees are skipped: they are owed, but they are not the loan.
	}
	return a
}

// applyPrincipalFirst spends an amount against principal, then any unpaid
// interest, holding the excess. It is the allocation a Provider's credit takes.
func (a *Allocation) applyPrincipalFirst(amount money.Cents) {
	if amount <= 0 {
		return
	}
	if a.Principal > 0 {
		if amount < a.Principal {
			a.Principal -= amount
			return
		}
		amount -= a.Principal
		a.Principal = 0
	}
	// Principal is gone; anything left clears interest still standing, because
	// a credit larger than the principal is plainly meant to reduce the debt
	// rather than sit as a surplus while interest remains owed.
	if a.UnpaidInterest > 0 {
		if amount < a.UnpaidInterest {
			a.UnpaidInterest -= amount
			return
		}
		amount -= a.UnpaidInterest
		a.UnpaidInterest = 0
	}
	a.PaidAhead += amount
}

// apply spends an amount against interest first, then principal, and holds any
// excess. This is the U.S. Rule order, and it is what a payment takes.
func (a *Allocation) apply(amount money.Cents) {
	if amount <= 0 {
		return
	}
	if a.UnpaidInterest > 0 {
		if amount >= a.UnpaidInterest {
			amount -= a.UnpaidInterest
			a.UnpaidInterest = 0
		} else {
			a.UnpaidInterest -= amount
			return
		}
	}
	if a.Principal > 0 {
		if amount >= a.Principal {
			amount -= a.Principal
			a.Principal = 0
		} else {
			a.Principal -= amount
			return
		}
	}
	a.PaidAhead += amount
}
