package ledger

import (
	"context"
	"testing"
	"time"

	"github.com/johnzastrow/bitt/internal/fee"
	"github.com/johnzastrow/bitt/internal/money"
	"github.com/johnzastrow/bitt/internal/schedule"
	"github.com/johnzastrow/bitt/internal/store"
)

// A Payoff tab's schedule describes expected payments, not charges. Reading it
// must post no period charges, however overdue it is (the Model A decision, and
// the Phase 3 behaviour this corrects).
func TestPayoffTabPostsNoPeriodCharges(t *testing.T) {
	led, db, tab, user := newFeeFixture(t, store.TabPayoff,
		schedule.Schedule{Kind: schedule.MonthlyDay, Anchor: schedule.NewDate(2026, time.January, 1), Billing: schedule.InAdvance},
		fee.Policy{},
		[]store.TabItem{{Name: "Repayment", Amount: 25000}}, // $250/mo expected
	)
	ctx := context.Background()

	// The Provider charges the principal once: a $5,000 loan.
	if _, _, err := at(led, 2026, time.January, 1).Charge(ctx, Post{
		TabID: tab.ID, Amount: 500000, ActorUserID: user.ID, IdempotencyKey: "principal",
	}); err != nil {
		t.Fatalf("principal charge: %v", err)
	}

	// Six months later, reading the tab has posted nothing new: no installment
	// charges, just the principal.
	accrue(t, at(led, 2026, time.July, 1), db, tab, time.UTC)

	entries, err := db.ListEntries(ctx, tab.ID)
	if err != nil {
		t.Fatalf("list entries: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("a Payoff tab has %d entries after six months, want 1 -- it posted period charges", len(entries))
	}
	periods, err := db.ListPostedPeriods(ctx, tab.ID)
	if err != nil {
		t.Fatalf("list periods: %v", err)
	}
	if len(periods) != 0 {
		t.Errorf("a Payoff tab claimed %d billing cycles, want 0", len(periods))
	}
	if balance, _ := led.Balance(ctx, tab.ID); balance != -500000 {
		t.Errorf("balance %s, want -$5,000 (the principal, undrawn)", balance)
	}
}

// A Payoff tab that misses expected payment dates accrues a fee per missed
// date, sized on the expected installment, once each.
func TestPayoffLateFeesOnMissedPayments(t *testing.T) {
	led, db, tab, user := newFeeFixture(t, store.TabPayoff,
		schedule.Schedule{Kind: schedule.MonthlyDay, Anchor: schedule.NewDate(2026, time.January, 1), Billing: schedule.InAdvance},
		fee.Policy{Kind: fee.Percent, PercentBP: 1000, GraceDays: 5}, // 10% of the installment
		[]store.TabItem{{Name: "Repayment", Amount: 25000}},
	)
	ctx := context.Background()

	if _, _, err := at(led, 2026, time.January, 1).Charge(ctx, Post{
		TabID: tab.ID, Amount: 500000, ActorUserID: user.ID, IdempotencyKey: "principal",
	}); err != nil {
		t.Fatalf("principal: %v", err)
	}

	// The Payee pays January and March on time, but misses February. Each
	// installment is judged on its own, so only February is fined -- paying
	// March on time clears March even though February was missed.
	pay(t, led, tab, user, 25000, 2026, time.January, 3)
	pay(t, led, tab, user, 25000, 2026, time.March, 3)

	accrue(t, at(led, 2026, time.March, 10), db, tab, time.UTC)

	fees, total := feeEntries(t, db, tab.ID)
	if len(fees) != 1 {
		t.Fatalf("got %d fees, want 1 -- only February was missed", len(fees))
	}
	// 10% of the $250 expected installment.
	if total != 2500 {
		t.Errorf("fee total %s, want $25.00 (10%% of the $250 installment)", total)
	}
	if fees[0].EffectiveAt.Month() != time.February {
		t.Errorf("the fee is dated %s, want it to answer to February", fees[0].EffectiveAt.Month())
	}
}

// Paying an installment on time clears that period even while an earlier miss
// stays fined: the per-period rule your "May paid, no fee" example describes.
// Miss January and February, pay March on time -- March is not fined.
func TestPayoffPerPeriodIndependence(t *testing.T) {
	led, db, tab, user := newFeeFixture(t, store.TabPayoff,
		schedule.Schedule{Kind: schedule.MonthlyDay, Anchor: schedule.NewDate(2026, time.January, 1), Billing: schedule.InAdvance},
		fee.Policy{Kind: fee.Fixed, Fixed: 2500, GraceDays: 5},
		[]store.TabItem{{Name: "Repayment", Amount: 25000}},
	)
	ctx := context.Background()

	if _, _, err := at(led, 2026, time.January, 1).Charge(ctx, Post{
		TabID: tab.ID, Amount: 500000, ActorUserID: user.ID, IdempotencyKey: "principal",
	}); err != nil {
		t.Fatalf("principal: %v", err)
	}

	// Nothing in January or February; pay March's installment on time.
	pay(t, led, tab, user, 25000, 2026, time.March, 3)

	accrue(t, at(led, 2026, time.March, 10), db, tab, time.UTC)

	// January and February are each fined; March, paid in its own window, is
	// clear. A paid period is never dragged down by an earlier miss.
	fees, _ := feeEntries(t, db, tab.ID)
	if len(fees) != 2 {
		t.Fatalf("got %d fees, want 2 -- January and February missed, March paid", len(fees))
	}
	for _, f := range fees {
		if f.EffectiveAt.Month() == time.March {
			t.Errorf("March was fined despite being paid on time")
		}
	}
}

// ---------------------------------------------------------------------------
// ComputePayoff: progress and status (PAYOFF-01, PAYOFF-02, PAYOFF-03)
// ---------------------------------------------------------------------------

func payoffTab() store.Tab {
	return store.Tab{
		Kind:     store.TabPayoff,
		Schedule: schedule.Schedule{Kind: schedule.MonthlyDay, Anchor: schedule.NewDate(2026, time.January, 1), Billing: schedule.InAdvance},
	}
}

// charge and payment (undated) live in statement_test.go; a Payoff test needs
// payments with an effective date, so it gets its own dated helper.
func paymentEntry(seq int64, amount money.Cents, y int, m time.Month, d int) store.Entry {
	return store.Entry{Seq: seq, Kind: store.KindPayment, Amount: amount, EffectiveAt: time.Date(y, m, d, 0, 0, 0, 0, time.UTC)}
}

func TestComputePayoffProgress(t *testing.T) {
	tab := payoffTab()
	entries := []store.Entry{
		charge(1, 500000), // $5,000 principal
		paymentEntry(2, 25000, 2026, time.January, 3),
		paymentEntry(3, 25000, 2026, time.February, 3),
	}
	// As of mid-February: two installments expected, two paid -> on track.
	tab.LoanPayment = 25000

	p := ComputePayoff(tab, entries, schedule.NewDate(2026, time.February, 15), time.UTC)

	if p.Principal != 500000 {
		t.Errorf("principal %s, want $5,000", p.Principal)
	}
	if p.Paid != 50000 {
		t.Errorf("paid %s, want $500", p.Paid)
	}
	if p.Remaining != 450000 {
		t.Errorf("remaining %s, want $4,500", p.Remaining)
	}
	if p.ProgressPercent != 10 {
		t.Errorf("progress %d%%, want 10%%", p.ProgressPercent)
	}
	if p.Status != StatusOnTrack {
		t.Errorf("status %q, want on track", p.Status)
	}
}

func TestComputePayoffStatuses(t *testing.T) {
	tab := payoffTab()
	principal := charge(1, 500000)
	today := schedule.NewDate(2026, time.March, 15) // three installments expected: $750

	cases := []struct {
		name string
		paid money.Cents
		want PayoffStatus
	}{
		{"behind", 50000, StatusBehind},    // paid $500 of $750 expected
		{"on track", 75000, StatusOnTrack}, // paid exactly $750
		{"ahead", 100000, StatusAhead},     // paid $1000 -- a full installment ahead
		{"settled", 500000, StatusSettled}, // paid the whole loan
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			entries := []store.Entry{principal, paymentEntry(2, tc.paid, 2026, time.January, 3)}
			tab.LoanPayment = 25000

			p := ComputePayoff(tab, entries, today, time.UTC)
			if p.Status != tc.want {
				t.Errorf("paid %s -> status %q, want %q (expected by now %s)", tc.paid, p.Status, tc.want, p.ExpectedByNow)
			}
		})
	}
}

// PAYOFF-03: a fully paid loan reads as settled, and its balance is zero.
func TestComputePayoffSettledIncludesFees(t *testing.T) {
	tab := payoffTab()
	// A loan with a fee: settled only when principal AND the fee are covered.
	entries := []store.Entry{
		charge(1, 100000),
		{Seq: 2, Kind: store.KindFee, Amount: -2500},
		paymentEntry(3, 100000, 2026, time.January, 3),
	}
	tab.LoanPayment = 10000

	p := ComputePayoff(tab, entries, schedule.NewDate(2026, time.June, 1), time.UTC)
	if p.Settled() {
		t.Errorf("a loan with an unpaid fee reads as settled; balance %s", p.Balance)
	}

	entries = append(entries, paymentEntry(4, 2500, 2026, time.January, 4))
	tab.LoanPayment = 10000

	p = ComputePayoff(tab, entries, schedule.NewDate(2026, time.June, 1), time.UTC)
	if !p.Settled() {
		t.Errorf("a fully paid loan and fee does not read as settled; balance %s", p.Balance)
	}
	if p.Balance != 0 {
		t.Errorf("settled balance %s, want 0", p.Balance)
	}
}

// A reversed (waived) fee stops counting toward what must be paid to settle.
func TestComputePayoffIgnoresWaivedFees(t *testing.T) {
	tab := payoffTab()
	waived := int64(2)
	entries := []store.Entry{
		charge(1, 100000),
		{Seq: 2, Kind: store.KindFee, Amount: -2500},
		{Seq: 3, Kind: store.KindReversal, Amount: 2500, ReversesSeq: &waived},
		paymentEntry(4, 100000, 2026, time.January, 3),
	}
	tab.LoanPayment = 10000

	p := ComputePayoff(tab, entries, schedule.NewDate(2026, time.June, 1), time.UTC)
	if p.Fees != 0 {
		t.Errorf("waived fee still counts: fees %s", p.Fees)
	}
	if !p.Settled() {
		t.Errorf("a loan with its only fee waived and principal paid is not settled; balance %s", p.Balance)
	}
}

func TestPayoffStatusLabels(t *testing.T) {
	for _, s := range []PayoffStatus{StatusSettled, StatusAhead, StatusOnTrack, StatusBehind} {
		if s.Label() == "" {
			t.Errorf("status %q has no label", s)
		}
	}
}
