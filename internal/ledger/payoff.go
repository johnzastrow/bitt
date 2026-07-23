package ledger

import (
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
	// been paid by today, capped at the principal.
	ExpectedByNow money.Cents
	// Behind is how far short of ExpectedByNow the payments are, floored at zero.
	Behind money.Cents
	// Installment is the expected payment each period.
	Installment money.Cents
	// Status summarizes the comparison (PAYOFF-02).
	Status PayoffStatus
}

// Settled reports whether the loan and its fees are fully paid (PAYOFF-03).
func (p Payoff) Settled() bool { return p.Status == StatusSettled }

// ComputePayoff derives a Payoff tab's state from its entries and schedule.
//
// today is the current date in the instance timezone. installment is the
// expected payment each period, taken from the tab's current items.
func ComputePayoff(tab store.Tab, entries []store.Entry, installment money.Cents, today schedule.Date) Payoff {
	reversed := ReversedSeqs(entries)

	var principal, interest, paid, fees money.Cents
	for _, e := range entries {
		if reversed[e.Seq] {
			continue
		}
		switch {
		case e.Kind == store.KindCharge && e.Category == store.CategoryInterest:
			interest += e.Amount.Neg()
		case e.Kind == store.KindCharge:
			principal += e.Amount.Neg()
		case e.Kind == store.KindPayment:
			paid += e.Amount
		case e.Kind == store.KindFee:
			fees += e.Amount.Neg()
		}
	}

	p := Payoff{
		Principal:   principal,
		Interest:    interest,
		Paid:        paid,
		Fees:        fees,
		Installment: installment,
		Balance:     paid - principal - interest - fees,
	}

	// What is still owed on the loan is principal plus interest, less payments.
	if remaining := principal + interest - paid; remaining > 0 {
		p.Remaining = remaining
	}

	// Progress is principal retired, as a percent of the original principal.
	// Payments cover interest first (standard amortization), so principal paid
	// is what remains of a payment after the interest charged so far.
	if principal > 0 {
		principalPaid := paid - interest
		if principalPaid < 0 {
			principalPaid = 0
		}
		if principalPaid > principal {
			principalPaid = principal
		}
		p.ProgressPercent = int(int64(principalPaid) * 100 / int64(principal))
	}

	// Expected cumulative through today, capped at the principal.
	if installment > 0 && principal > 0 && tab.Schedule.Set() {
		var expected money.Cents
		for n := 0; n < schedule.MaxPeriods; n++ {
			due := tab.Schedule.Period(n).Due
			if due.After(today) {
				break
			}
			expected += installment
			if expected >= principal {
				expected = principal
				break
			}
		}
		p.ExpectedByNow = expected
		if behind := expected - paid; behind > 0 {
			p.Behind = behind
		}
	}

	p.Status = payoffStatus(p)
	return p
}

// payoffStatus classifies a Payoff tab against its schedule.
func payoffStatus(p Payoff) PayoffStatus {
	// Settled wins over everything: the loan and its fees are covered.
	if p.Principal > 0 && p.Balance >= 0 {
		return StatusSettled
	}
	if p.Paid < p.ExpectedByNow {
		return StatusBehind
	}
	// Ahead means a full installment prepaid beyond what was expected by now.
	if p.Installment > 0 && p.Paid >= p.ExpectedByNow+p.Installment {
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
