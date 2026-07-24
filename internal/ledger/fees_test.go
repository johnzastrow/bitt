package ledger

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/johnzastrow/bitt/internal/fee"
	"github.com/johnzastrow/bitt/internal/money"
	"github.com/johnzastrow/bitt/internal/schedule"
	"github.com/johnzastrow/bitt/internal/store"
	"github.com/johnzastrow/bitt/internal/store/sqlite"
)

// newFeeFixture builds a tab of a given kind with a schedule, items, and a fee
// policy, plus a ledger whose clock the test controls.
func newFeeFixture(t *testing.T, kind store.TabKind, sched schedule.Schedule, policy fee.Policy, items []store.TabItem) (*Service, *sqlite.DB, store.Tab, store.User) {
	t.Helper()
	db, err := sqlite.Open(sqlite.Options{
		Path:               filepath.Join(t.TempDir(), "fees.db"),
		AppendOnlyTriggers: true,
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	ctx := context.Background()
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	user, err := db.CreateUser(ctx, store.User{
		Email: "provider@example.com", DisplayName: "Provider", PasswordHash: "$argon2id$x",
	})
	if err != nil {
		t.Fatalf("user: %v", err)
	}
	// On a Payoff tab the expected payment is its own field. These fixtures
	// state it as items because that is what a Services tab bills from and the
	// two used to share the field; translating it here is the same move
	// migration 0006 makes for existing tabs.
	newTab := store.Tab{
		Name:      "Loan",
		Kind:      kind,
		CreatedBy: user.ID,
		Schedule:  sched.Normalize(),
		Fee:       policy,
		CreatedAt: time.Date(2025, time.December, 1, 0, 0, 0, 0, time.UTC),
	}
	if kind == store.TabPayoff {
		var payment money.Cents
		for _, it := range items {
			payment += it.Amount
		}
		newTab.LoanPayment = payment
	}

	tab, err := db.CreateTab(ctx, newTab, items)
	if err != nil {
		t.Fatalf("tab: %v", err)
	}
	return New(db), db, tab, user
}

// pay records a payment dated on a given day.
func pay(t *testing.T, led *Service, tab store.Tab, user store.User, amount money.Cents, y int, m time.Month, d int) {
	t.Helper()
	clocked := led.WithClock(func() time.Time { return time.Date(y, m, d, 9, 0, 0, 0, time.UTC) })
	_, _, err := clocked.Payment(context.Background(), Post{
		TabID:          tab.ID,
		Amount:         amount,
		ActorUserID:    user.ID,
		Method:         store.MethodTransfer,
		EffectiveAt:    time.Date(y, m, d, 9, 0, 0, 0, time.UTC),
		IdempotencyKey: "pay-" + schedule.NewDate(y, m, d).String() + "-" + amount.String(),
	})
	if err != nil {
		t.Fatalf("payment: %v", err)
	}
}

// feeEntries returns a tab's fee entries, and their total magnitude.
func feeEntries(t *testing.T, db *sqlite.DB, tabID int64) ([]store.Entry, money.Cents) {
	t.Helper()
	entries, err := db.ListEntries(context.Background(), tabID)
	if err != nil {
		t.Fatalf("list entries: %v", err)
	}
	var out []store.Entry
	var total money.Cents
	for _, e := range entries {
		if e.Kind == store.KindFee {
			out = append(out, e)
			total += e.Amount.Neg()
		}
	}
	return out, total
}

// ---------------------------------------------------------------------------
// FEE-03: a fee posts once grace elapses on an unpaid period
// ---------------------------------------------------------------------------

// A Services tab that bills monthly and goes unpaid past grace gets one fee
// per unpaid period.
func TestServicesLateFeePostsAfterGrace(t *testing.T) {
	led, db, tab, _ := newFeeFixture(t, store.TabServices,
		schedule.Schedule{Kind: schedule.MonthlyDay, Anchor: schedule.NewDate(2026, time.January, 1), Billing: schedule.InAdvance},
		fee.Policy{Kind: fee.Fixed, Fixed: 2500, GraceDays: 5},
		[]store.TabItem{{Name: "Rent", Amount: 100000}},
	)

	// Jan 1 charge posts; by Jan 5 (== due + grace... grace is 5 days, so
	// deadline is Jan 6) nothing is late yet.
	accrue(t, at(led, 2026, time.January, 6), db, tab, time.UTC)
	if _, total := feeEntries(t, db, tab.ID); total != 0 {
		t.Fatalf("a fee posted on the grace deadline itself: %s", total)
	}

	// Jan 7: grace has elapsed on the unpaid January charge -> one fee.
	accrue(t, at(led, 2026, time.January, 7), db, tab, time.UTC)
	fees, total := feeEntries(t, db, tab.ID)
	if len(fees) != 1 || total != 2500 {
		t.Fatalf("got %d fees totaling %s, want one $25.00 fee", len(fees), total)
	}

	// Reading again the same day posts no second fee (FEE-04).
	accrue(t, at(led, 2026, time.January, 8), db, tab, time.UTC)
	if fees, _ := feeEntries(t, db, tab.ID); len(fees) != 1 {
		t.Fatalf("a date was assessed twice: %d fees", len(fees))
	}
}

// Paying within grace prevents the fee entirely.
func TestPaymentWithinGraceAvoidsFee(t *testing.T) {
	led, db, tab, user := newFeeFixture(t, store.TabServices,
		schedule.Schedule{Kind: schedule.MonthlyDay, Anchor: schedule.NewDate(2026, time.January, 1), Billing: schedule.InAdvance},
		fee.Policy{Kind: fee.Fixed, Fixed: 2500, GraceDays: 5},
		[]store.TabItem{{Name: "Rent", Amount: 100000}},
	)

	// Post January's charge, then pay it on Jan 4, inside the grace window.
	accrue(t, at(led, 2026, time.January, 2), db, tab, time.UTC)
	pay(t, led, tab, user, 100000, 2026, time.January, 4)

	// Well past the deadline, still no fee: the period was paid in time.
	accrue(t, at(led, 2026, time.January, 20), db, tab, time.UTC)
	if fees, total := feeEntries(t, db, tab.ID); len(fees) != 0 {
		t.Fatalf("a paid-on-time period was fined %s across %d fees", total, len(fees))
	}
}

// A late catch-up payment does not erase a fee that was validly due at the
// grace deadline. The assessment is deterministic regardless of read time.
func TestLateCatchUpDoesNotErasePastFee(t *testing.T) {
	led, db, tab, user := newFeeFixture(t, store.TabServices,
		schedule.Schedule{Kind: schedule.MonthlyDay, Anchor: schedule.NewDate(2026, time.January, 1), Billing: schedule.InAdvance},
		fee.Policy{Kind: fee.Fixed, Fixed: 2500, GraceDays: 5},
		[]store.TabItem{{Name: "Rent", Amount: 100000}},
	)

	// The Payee pays late, on Jan 20 -- after the Jan 6 grace deadline.
	accrue(t, at(led, 2026, time.January, 2), db, tab, time.UTC)
	pay(t, led, tab, user, 100000, 2026, time.January, 20)

	// The tab is not read until Jan 25, after both the deadline and the late
	// payment. The fee for January still stands: it was owed as of Jan 6, and a
	// payment dated afterward cannot reach back past the deadline.
	accrue(t, at(led, 2026, time.January, 25), db, tab, time.UTC)
	if _, total := feeEntries(t, db, tab.ID); total != 2500 {
		t.Errorf("late fee total %s, want $25.00 -- a late payment must not erase a due fee", total)
	}
}

// ---------------------------------------------------------------------------
// FEE-05: percentage on the period charge, deterministic rounding
// ---------------------------------------------------------------------------

func TestPercentageFeeOnPeriodCharge(t *testing.T) {
	led, db, tab, _ := newFeeFixture(t, store.TabServices,
		schedule.Schedule{Kind: schedule.MonthlyDay, Anchor: schedule.NewDate(2026, time.January, 1), Billing: schedule.InAdvance},
		fee.Policy{Kind: fee.Percent, PercentBP: 1000, GraceDays: 0}, // 10%, no grace
		[]store.TabItem{{Name: "Rent", Amount: 25000}},
	)

	// By Feb 15, the Jan 1 and Feb 1 charges are both overdue (grace 0, deadline
	// strictly past). Each is a $250 charge -> a $25 fee each, on the charge and
	// never on a balance that already includes the first fee (FEE-05). March is
	// not due yet.
	accrue(t, at(led, 2026, time.February, 15), db, tab, time.UTC)

	fees, total := feeEntries(t, db, tab.ID)
	if len(fees) != 2 {
		t.Fatalf("got %d fees, want 2 (January and February unpaid)", len(fees))
	}
	if total != 5000 {
		t.Errorf("fees total %s, want $50.00 -- 10%% of $250 twice, not compounding", total)
	}
	for _, f := range fees {
		if f.Amount.Neg() != 2500 {
			t.Errorf("a fee is %s, want $25.00 -- each computed on the $250 charge alone", f.Amount.Neg())
		}
	}
}

// ---------------------------------------------------------------------------
// FEE-06: the cap bounds total accrued fees
// ---------------------------------------------------------------------------

func TestFeesStopAtTheCap(t *testing.T) {
	led, db, tab, _ := newFeeFixture(t, store.TabServices,
		schedule.Schedule{Kind: schedule.MonthlyDay, Anchor: schedule.NewDate(2026, time.January, 1), Billing: schedule.InAdvance},
		fee.Policy{Kind: fee.Fixed, Fixed: 3000, GraceDays: 0, Cap: 10000},
		[]store.TabItem{{Name: "Rent", Amount: 100000}},
	)

	// Six months unpaid would be six $30 fees = $180, but the cap is $100.
	accrue(t, at(led, 2026, time.July, 1), db, tab, time.UTC)

	_, total := feeEntries(t, db, tab.ID)
	if total != 10000 {
		t.Errorf("fees total %s, want the $100.00 cap", total)
	}

	// Reading further never exceeds the cap.
	accrue(t, at(led, 2027, time.January, 1), db, tab, time.UTC)
	if _, total := feeEntries(t, db, tab.ID); total != 10000 {
		t.Errorf("fees grew past the cap to %s", total)
	}
}

// ---------------------------------------------------------------------------
// FEE-07: a waiver frees cap room and does not re-trigger
// ---------------------------------------------------------------------------

func TestWaivedFeeDoesNotComeBack(t *testing.T) {
	led, db, tab, user := newFeeFixture(t, store.TabServices,
		schedule.Schedule{Kind: schedule.MonthlyDay, Anchor: schedule.NewDate(2026, time.January, 1), Billing: schedule.InAdvance},
		fee.Policy{Kind: fee.Fixed, Fixed: 2500, GraceDays: 0},
		[]store.TabItem{{Name: "Rent", Amount: 100000}},
	)
	ctx := context.Background()

	// January goes unpaid and gets a fee.
	acc := accrue(t, at(led, 2026, time.January, 15), db, tab, time.UTC)
	if len(acc.Fees) != 1 {
		t.Fatalf("expected one fee, got %d", len(acc.Fees))
	}
	feeSeq := acc.Fees[0].Seq

	// Waive it: a reversal carrying a reason (FEE-07).
	if _, _, err := led.Reverse(ctx, feeSeq, user.ID, "Waived: first-time courtesy", ""); err != nil {
		t.Fatalf("waive: %v", err)
	}

	// Reading again must not re-assess the same date. The claim survives the
	// waiver, so the fee stays gone.
	accrue(t, at(led, 2026, time.January, 20), db, tab, time.UTC)
	entries, err := db.ListEntries(ctx, tab.ID)
	if err != nil {
		t.Fatalf("list entries: %v", err)
	}
	var feeCount, reversalCount int
	for _, e := range entries {
		switch e.Kind {
		case store.KindFee:
			feeCount++
		case store.KindReversal:
			reversalCount++
		}
	}
	if feeCount != 1 {
		t.Errorf("%d fee entries, want 1 -- a waived fee was re-assessed", feeCount)
	}
	if reversalCount != 1 {
		t.Errorf("%d reversals, want 1", reversalCount)
	}

	// The waiver cleared the fee's effect on the balance.
	balance, _ := led.Balance(ctx, tab.ID)
	if balance != -100000 {
		t.Errorf("balance %s after waiving the fee, want just the -$1000 charge", balance)
	}
}

// A cleared cap can be re-used once a waiver frees room.
func TestWaiverFreesCapRoom(t *testing.T) {
	led, db, tab, user := newFeeFixture(t, store.TabServices,
		schedule.Schedule{Kind: schedule.MonthlyDay, Anchor: schedule.NewDate(2026, time.January, 1), Billing: schedule.InAdvance},
		fee.Policy{Kind: fee.Fixed, Fixed: 5000, GraceDays: 0, Cap: 10000},
		[]store.TabItem{{Name: "Rent", Amount: 100000}},
	)
	ctx := context.Background()

	// Two months unpaid hit the $100 cap exactly (two $50 fees).
	acc := accrue(t, at(led, 2026, time.February, 15), db, tab, time.UTC)
	if len(acc.Fees) != 2 {
		t.Fatalf("expected two fees, got %d", len(acc.Fees))
	}
	_, total := feeEntries(t, db, tab.ID)
	if total != 10000 {
		t.Fatalf("fees total %s, want the $100 cap", total)
	}

	// Waive January's fee: $50 of cap room opens up.
	if _, _, err := led.Reverse(ctx, acc.Fees[0].Seq, user.ID, "Waived", ""); err != nil {
		t.Fatalf("waive: %v", err)
	}

	// March is also unpaid; now a fee for it can post into the freed room.
	acc = accrue(t, at(led, 2026, time.March, 15), db, tab, time.UTC)
	if len(acc.Fees) != 1 {
		t.Errorf("expected one new fee after the waiver freed cap room, got %d", len(acc.Fees))
	}
}

// ---------------------------------------------------------------------------
// FEE-04: concurrent reads assess each date exactly once
// ---------------------------------------------------------------------------

func TestConcurrentFeeAssessment(t *testing.T) {
	led, db, tab, _ := newFeeFixture(t, store.TabServices,
		schedule.Schedule{Kind: schedule.Weekly, Anchor: schedule.NewDate(2026, time.January, 5), Billing: schedule.InAdvance},
		fee.Policy{Kind: fee.Fixed, Fixed: 1000, GraceDays: 0},
		[]store.TabItem{{Name: "Service", Amount: 6000}},
	)
	ctx := context.Background()

	frozen := at(led, 2026, time.March, 1)
	history, err := db.ListItemHistory(ctx, tab.ID)
	if err != nil {
		t.Fatalf("history: %v", err)
	}

	const readers = 10
	done := make(chan int, readers)
	for range readers {
		go func() {
			acc, err := frozen.Accrue(ctx, tab, history, time.UTC)
			if err != nil {
				done <- -1
				return
			}
			done <- len(acc.Fees)
		}()
	}
	total := 0
	for range readers {
		n := <-done
		if n < 0 {
			t.Fatal("a concurrent accrual failed")
		}
		total += n
	}

	// Jan 5 through Mar 1 weekly is 8 charges; all unpaid, each fined once.
	const wantFees = 8
	if total != wantFees {
		t.Errorf("readers reported %d fees in total, want %d -- a date was fined more than once", total, wantFees)
	}
	fees, _ := feeEntries(t, db, tab.ID)
	if len(fees) != wantFees {
		t.Errorf("ledger holds %d fees, want %d", len(fees), wantFees)
	}
	assessed, err := db.ListPostedFees(ctx, tab.ID)
	if err != nil {
		t.Fatalf("list posted fees: %v", err)
	}
	seen := map[string]bool{}
	for _, f := range assessed {
		if seen[f.Key] {
			t.Errorf("date %s assessed twice", f.Key)
		}
		seen[f.Key] = true
	}
}
