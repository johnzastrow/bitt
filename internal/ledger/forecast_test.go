package ledger

import (
	"testing"
	"time"

	"github.com/johnzastrow/bitt/internal/money"
	"github.com/johnzastrow/bitt/internal/schedule"
	"github.com/johnzastrow/bitt/internal/store"
)

// The projection runs from where the loan stands to the day it is repaid, on
// the tab's own schedule, and lands on exactly zero.
func TestComputeForecastRunsToPayoff(t *testing.T) {
	tab := lenderTab(50_565, 48)
	p := ComputePayoff(tab, lenderEntries(), on(2026, time.January, 20), time.UTC)

	f := ComputeForecast(tab, p, on(2026, time.January, 20))
	if !f.Show() {
		t.Fatal("no forecast for a running loan")
	}
	if f.Count() != 48 {
		t.Errorf("%d payments projected, want the 48 of the term", f.Count())
	}

	last := f.Rows[f.Count()-1]
	if last.Balance != 0 {
		t.Errorf("balance after the last payment is %s, want zero", last.Balance.Display())
	}
	if last.Amount > p.Installment {
		t.Errorf("final payment %s exceeds the installment %s",
			last.Amount.Display(), p.Installment.Display())
	}
	if f.Payoff != last.Due {
		t.Errorf("payoff date %s, last payment falls %s", f.Payoff.Display(), last.Due.Display())
	}

	// The total is what it costs to finish, and covers the balance plus the
	// interest still to be charged.
	var sum money.Cents
	for _, r := range f.Rows {
		sum += r.Amount
	}
	if sum != f.Total {
		t.Errorf("rows total %s, Total says %s", sum.Display(), f.Total.Display())
	}
	if f.Total <= p.Remaining {
		t.Errorf("total %s does not exceed what is owed today, %s -- interest is missing",
			f.Total.Display(), p.Remaining.Display())
	}
}

// The dates come off the schedule, starting at the next payment still to come:
// a payment already made must not be projected again.
func TestComputeForecastStartsAtTheNextDueDate(t *testing.T) {
	tab := lenderTab(50_565, 48)
	entries := append(lenderEntries(),
		entryAt(2, store.KindPayment, 50_565, 2026, time.January, 15),
		entryAt(3, store.KindPayment, 50_565, 2026, time.February, 15),
	)
	today := on(2026, time.February, 20)

	p := ComputePayoff(tab, entries, today, time.UTC)
	f := ComputeForecast(tab, p, today)
	if !f.Show() {
		t.Fatal("no forecast after two payments")
	}

	// The 15th of March is the next payment date the schedule offers.
	if want := schedule.NewDate(2026, time.March, 15); f.Rows[0].Due != want {
		t.Errorf("first projected payment falls %s, want %s", f.Rows[0].Due.Display(), want.Display())
	}
	// Two of the 48 are behind us.
	if f.Count() != 46 {
		t.Errorf("%d payments projected after two were made, want 46", f.Count())
	}
	// Every date is monthly, in order, and each balance is smaller than the last.
	prev := f.Rows[0]
	for _, r := range f.Rows[1:] {
		if !r.Due.After(prev.Due) {
			t.Fatalf("payment %d falls %s, not after %s", r.Number, r.Due.Display(), prev.Due.Display())
		}
		if r.Balance >= prev.Balance {
			t.Fatalf("payment %d leaves %s owed, no less than the %s before it",
				r.Number, r.Balance.Display(), prev.Balance.Display())
		}
		prev = r
	}
}

// A payment too small to ever retire the loan has no payoff date, so there is
// no schedule to show -- that gap is the true-up banner's business.
func TestComputeForecastEmptyWhenNothingToProject(t *testing.T) {
	settled := append(lenderEntries(), entryAt(2, store.KindPayment, 2_185_248, 2026, time.January, 15))
	today := on(2026, time.January, 20)

	cases := []struct {
		name    string
		tab     store.Tab
		entries []store.Entry
	}{
		{"a services tab", func() store.Tab {
			s := lenderTab(50_565, 48)
			s.Kind = store.TabServices
			return s
		}(), lenderEntries()},
		{"no expected payment", lenderTab(0, 48), lenderEntries()},
		{"a payment below the interest", lenderTab(100, 48), lenderEntries()},
		{"no schedule", func() store.Tab {
			s := lenderTab(50_565, 48)
			s.Schedule = schedule.Schedule{}
			return s
		}(), lenderEntries()},
		{"already paid off", lenderTab(50_565, 48), settled},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := ComputePayoff(tc.tab, tc.entries, today, time.UTC)
			if f := ComputeForecast(tc.tab, p, today); f.Show() {
				t.Errorf("projected %d payments, want none", f.Count())
			}
		})
	}
}

// An interest-free IOU is a legitimate Payoff tab, and projects as a flat run
// of installments with a smaller last one.
func TestComputeForecastInterestFreeLoan(t *testing.T) {
	tab := lenderTab(10_000, 0)
	tab.InterestAPRBp = 0
	entries := []store.Entry{entryAt(1, store.KindCharge, -25_000, 2026, time.January, 15)}
	today := on(2026, time.January, 20)

	p := ComputePayoff(tab, entries, today, time.UTC)
	f := ComputeForecast(tab, p, today)
	if f.Count() != 3 {
		t.Fatalf("%d payments projected for $250 at $100 a month, want 3", f.Count())
	}
	if f.Total != 25_000 {
		t.Errorf("total %s, want the $250 owed with no interest", f.Total.Display())
	}
	if got := f.Rows[2].Amount; got != 5_000 {
		t.Errorf("final payment %s, want the $50 remainder", got.Display())
	}
	if f.Rows[2].Balance != 0 {
		t.Errorf("balance after the last payment is %s, want zero", f.Rows[2].Balance.Display())
	}
}
