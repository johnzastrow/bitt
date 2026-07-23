package ledger

import (
	"context"
	"testing"
	"time"

	"github.com/johnzastrow/bitt/internal/fee"
	"github.com/johnzastrow/bitt/internal/money"
	"github.com/johnzastrow/bitt/internal/schedule"
	"github.com/johnzastrow/bitt/internal/store"
	"github.com/johnzastrow/bitt/internal/store/sqlite"
)

// interestEntries returns a tab's interest charges and their total magnitude.
func interestEntries(t *testing.T, db *sqlite.DB, tabID int64) ([]store.Entry, money.Cents) {
	t.Helper()
	entries, err := db.ListEntries(context.Background(), tabID)
	if err != nil {
		t.Fatalf("list entries: %v", err)
	}
	var out []store.Entry
	var total money.Cents
	for _, e := range entries {
		if e.Kind == store.KindCharge && e.Category == store.CategoryInterest {
			out = append(out, e)
			total += e.Amount.Neg()
		}
	}
	return out, total
}

// newLoan builds a Payoff tab with an interest rate and its principal posted.
func newLoan(t *testing.T, aprBP int64, installment, principal money.Cents) (*Service, *sqlite.DB, store.Tab, store.User) {
	t.Helper()
	led, db, tab, user := newFeeFixture(t, store.TabPayoff,
		schedule.Schedule{Kind: schedule.MonthlyDay, Anchor: schedule.NewDate(2026, time.January, 1), Billing: schedule.InAdvance},
		fee.Policy{},
		[]store.TabItem{{Name: "Repayment", Amount: installment}},
	)
	if err := db.SetInterestRate(context.Background(), tab.ID, aprBP); err != nil {
		t.Fatalf("set interest: %v", err)
	}
	tab.InterestAPRBp = aprBP

	if _, _, err := at(led, 2026, time.January, 1).Charge(context.Background(), Post{
		TabID: tab.ID, Amount: principal, ActorUserID: user.ID, IdempotencyKey: "principal",
	}); err != nil {
		t.Fatalf("principal: %v", err)
	}
	return led, db, tab, user
}

// Interest accrues on the declining balance: each month's interest is computed
// on what is still owed, which falls as payments are made and rises with any
// interest left unpaid.
func TestInterestDecliningBalance(t *testing.T) {
	led, db, tab, user := newLoan(t, 600, 25000, 500000) // 6% APR, $250/mo, $5,000

	// Month 1 (Jan): interest on $5,000 = $25. Pay $250 -> balance falls.
	accrue(t, at(led, 2026, time.January, 2), db, tab, time.UTC)
	pay(t, led, tab, user, 25000, 2026, time.January, 15)

	// Month 2 (Feb): balance is 5000 + 25 (Jan interest) - 250 = $4,775.
	// Interest = 0.5% of $4,775 = $23.88 (2387.5 rounds up).
	accrue(t, at(led, 2026, time.February, 2), db, tab, time.UTC)

	interest, _ := interestEntries(t, db, tab.ID)
	if len(interest) != 2 {
		t.Fatalf("got %d interest charges, want 2 (Jan and Feb)", len(interest))
	}
	// Oldest first in the ledger; find by month.
	byMonth := map[time.Month]money.Cents{}
	for _, e := range interest {
		byMonth[e.EffectiveAt.Month()] = e.Amount.Neg()
	}
	if byMonth[time.January] != 2500 {
		t.Errorf("January interest %s, want $25.00 on the full $5,000", byMonth[time.January])
	}
	if byMonth[time.February] != 2388 {
		t.Errorf("February interest %s, want $23.88 on the declined $4,775", byMonth[time.February])
	}
}

// Reading twice does not double-charge interest, and it accrues at most once
// per period even under concurrent reads.
func TestInterestIsChargedOncePerPeriod(t *testing.T) {
	led, db, tab, _ := newLoan(t, 1200, 25000, 500000)
	ctx := context.Background()

	accrue(t, at(led, 2026, time.March, 2), db, tab, time.UTC) // Jan, Feb, Mar
	_, firstTotal := interestEntries(t, db, tab.ID)
	claims, err := db.ListPostedInterest(ctx, tab.ID)
	if err != nil {
		t.Fatalf("list posted interest: %v", err)
	}
	if len(claims) != 3 {
		t.Fatalf("claimed %d periods, want 3", len(claims))
	}

	// A second read the same month adds nothing.
	accrue(t, at(led, 2026, time.March, 20), db, tab, time.UTC)
	if _, total := interestEntries(t, db, tab.ID); total != firstTotal {
		t.Errorf("a repeat read changed interest from %s to %s", firstTotal, total)
	}

	// Concurrent reads of an overdue loan accrue each period once.
	led2, db2, tab2, _ := newLoan(t, 1200, 25000, 500000)
	history, err := db2.ListItemHistory(ctx, tab2.ID)
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	frozen := at(led2, 2026, time.June, 1)
	done := make(chan int, 8)
	for range 8 {
		go func() {
			acc, err := frozen.Accrue(ctx, tab2, history, time.UTC)
			if err != nil {
				done <- -1
				return
			}
			done <- len(acc.Interest)
		}()
	}
	total := 0
	for range 8 {
		n := <-done
		if n < 0 {
			t.Fatal("a concurrent accrual failed")
		}
		total += n
	}
	// Jan through Jun inclusive = 6 periods.
	if total != 6 {
		t.Errorf("readers accrued %d interest periods in total, want 6", total)
	}
	if claims, _ := db2.ListPostedInterest(ctx, tab2.ID); len(claims) != 6 {
		t.Errorf("claimed %d periods, want 6", len(claims))
	}
}

// A loan with no rate accrues no interest, however overdue.
func TestNoInterestWhenRateIsZero(t *testing.T) {
	led, db, tab, _ := newLoan(t, 0, 25000, 500000)
	accrue(t, at(led, 2026, time.July, 1), db, tab, time.UTC)
	if interest, _ := interestEntries(t, db, tab.ID); len(interest) != 0 {
		t.Errorf("a 0%% loan accrued %d interest charges", len(interest))
	}
}

// Once the loan is paid off, no further interest accrues -- the base is zero.
func TestInterestStopsWhenPaidOff(t *testing.T) {
	led, db, tab, user := newLoan(t, 1200, 500000, 500000) // pay the whole loan at once

	// January interest on $5,000 at 1%/mo = $50. Then pay principal + that interest.
	accrue(t, at(led, 2026, time.January, 2), db, tab, time.UTC)
	pay(t, led, tab, user, 505000, 2026, time.January, 3) // $5,050 clears loan + Jan interest

	// Months later: the balance is zero, so no more interest.
	accrue(t, at(led, 2026, time.June, 1), db, tab, time.UTC)

	interest, _ := interestEntries(t, db, tab.ID)
	if len(interest) != 1 {
		t.Errorf("got %d interest charges, want just the first month before payoff", len(interest))
	}
	if balance, _ := led.Balance(context.Background(), tab.ID); balance != 0 {
		t.Errorf("balance %s after paying loan plus interest, want 0", balance)
	}
}

// ComputePayoff splits payments into interest then principal, so progress
// reflects principal actually retired.
func TestComputePayoffWithInterest(t *testing.T) {
	tab := payoffTab()
	tab.InterestAPRBp = 600
	// $1,000 loan, $10 interest charged, $110 paid: $10 covers interest, $100
	// retires principal -> 10% progress.
	interestSeq := int64(2)
	entries := []store.Entry{
		charge(1, 100000),
		{Seq: interestSeq, Kind: store.KindCharge, Category: store.CategoryInterest, Amount: -1000},
		paymentEntry(3, 11000, 2026, time.January, 15),
	}
	p := ComputePayoff(tab, entries, 10000, schedule.NewDate(2026, time.January, 20))

	if p.Principal != 100000 {
		t.Errorf("principal %s, want $1,000 (interest is not principal)", p.Principal)
	}
	if p.Interest != 1000 {
		t.Errorf("interest %s, want $10", p.Interest)
	}
	if p.ProgressPercent != 10 {
		t.Errorf("progress %d%%, want 10%% -- $100 of principal retired after interest", p.ProgressPercent)
	}
	// Remaining is principal + interest - paid = 1000 + 10 - 110 = $900.
	if p.Remaining != 90000 {
		t.Errorf("remaining %s, want $900", p.Remaining)
	}

	// A waived interest charge stops counting.
	waived := int64(2)
	entries = append(entries, store.Entry{Seq: 4, Kind: store.KindReversal, Amount: 1000, ReversesSeq: &waived})
	p = ComputePayoff(tab, entries, 10000, schedule.NewDate(2026, time.January, 20))
	if p.Interest != 0 {
		t.Errorf("waived interest still counts: %s", p.Interest)
	}
}
