package ledger

import (
	"github.com/johnzastrow/bitt/internal/loan"
	"github.com/johnzastrow/bitt/internal/money"
	"github.com/johnzastrow/bitt/internal/schedule"
	"github.com/johnzastrow/bitt/internal/store"
)

// Forecast is the payment schedule a Payoff tab still has ahead of it: every
// remaining payment, when it falls, and what is left owed once it lands.
//
// Like every other figure on a tab it is derived on each render and stored
// nowhere, so it cannot drift from the balance the same entries produce. It is
// a projection, not a promise: it assumes the payment being made now keeps
// being made, on the schedule the tab carries, with no further charges. Late
// fees are excluded, matching the loan progress card, which shows them apart
// from the loan itself.
type Forecast struct {
	// Rows are the remaining payments, soonest first.
	Rows []ForecastRow
	// Total is every projected payment added together -- what it costs to
	// finish from here.
	Total money.Cents
	// Payoff is when the last payment falls: the projected date the loan is
	// repaid.
	Payoff schedule.Date
}

// Show reports whether there is a projection to display.
func (f Forecast) Show() bool { return len(f.Rows) > 0 }

// Count is how many payments remain.
func (f Forecast) Count() int { return len(f.Rows) }

// ForecastRow is one projected future payment.
type ForecastRow struct {
	// Number counts from one at the next payment due.
	Number int
	// Due is when the payment falls, off the tab's own schedule.
	Due schedule.Date
	// Amount is what falls due -- the installment every period but the last,
	// which is whatever is left.
	Amount money.Cents
	// Balance is what is still owed on the loan after this payment: outstanding
	// principal plus unpaid interest. Zero on the final row.
	Balance money.Cents
}

// ComputeForecast projects a Payoff tab's remaining payments from where the
// loan stands today.
//
// It returns an empty Forecast whenever there is honestly nothing to project: a
// tab that is not a loan, one with no schedule or no expected payment, one that
// is already settled, and one whose payment is too small to ever retire the
// balance -- that last case is the true-up banner's business, and a schedule
// that never ends is not a schedule.
func ComputeForecast(tab store.Tab, p Payoff, today schedule.Date) Forecast {
	var f Forecast
	if tab.Kind != store.TabPayoff || !tab.Schedule.Set() {
		return f
	}
	if p.Installment <= 0 || p.Remaining <= 0 {
		return f
	}

	payments := projectPayments(tab, p)
	if len(payments) == 0 {
		return f
	}

	// The dates come off the tab's own schedule, starting at the next period
	// whose due date has not already passed, so the projection lines up with
	// what the tab will actually bill.
	start, ok := nextDueIndex(tab.Schedule, today)
	if !ok {
		return f
	}

	f.Rows = make([]ForecastRow, 0, len(payments))
	for i, pay := range payments {
		if start+i >= schedule.MaxPeriods {
			// The schedule runs out of periods before the loan retires. Showing
			// a partial run would read as a payoff date that is not one.
			return Forecast{}
		}
		total, ok := money.Add(f.Total, pay.Payment)
		if !ok {
			return Forecast{}
		}
		f.Total = total
		f.Rows = append(f.Rows, ForecastRow{
			Number:  pay.Number,
			Due:     tab.Schedule.Period(start + i).Due,
			Amount:  pay.Payment,
			Balance: pay.Balance,
		})
	}
	f.Payoff = f.Rows[len(f.Rows)-1].Due
	return f
}

// projectPayments runs the loan forward at the payment actually being made and
// returns what remains to pay.
func projectPayments(tab store.Tab, p Payoff) []loan.Installment {
	// Principal already retired, with an interest arrears left standing. Under
	// the U.S. Rule that bucket earns no interest of its own, so nothing more
	// accrues and the rest is simply the arrears paid down at the installment.
	if p.PrincipalOutstanding <= 0 {
		return flatPayments(p.UnpaidInterest, p.Installment)
	}

	num, den := tab.Schedule.RateBasis()
	if num <= 0 || den <= 0 {
		return nil
	}
	return loan.Project(loan.Terms{
		Principal:     p.PrincipalOutstanding,
		AccruedUnpaid: p.UnpaidInterest,
		APRBp:         tab.InterestAPRBp,
		RateNum:       num, RateDen: den,
	}, p.Installment)
}

// flatPayments pays owed down at payment a period, with no interest in play.
func flatPayments(owed, payment money.Cents) []loan.Installment {
	if owed <= 0 || payment <= 0 {
		return nil
	}
	var out []loan.Installment
	for n := 1; owed > 0 && n <= schedule.MaxPeriods; n++ {
		due := min(payment, owed)
		owed -= due
		out = append(out, loan.Installment{Number: n, Payment: due, Balance: owed})
	}
	return out
}

// nextDueIndex finds the first period whose due date has not already passed.
// It reports false when the schedule has no such period left to offer.
func nextDueIndex(s schedule.Schedule, today schedule.Date) (int, bool) {
	for n := 0; n < schedule.MaxPeriods; n++ {
		if !s.Period(n).Due.Before(today) {
			return n, true
		}
	}
	return 0, false
}
