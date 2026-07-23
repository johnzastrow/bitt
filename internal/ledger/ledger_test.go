package ledger

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/johnzastrow/bitt/internal/money"
	"github.com/johnzastrow/bitt/internal/store"
	"github.com/johnzastrow/bitt/internal/store/sqlite"
)

func newFixture(t *testing.T) (*Service, *sqlite.DB, store.User, store.Tab) {
	t.Helper()
	db, err := sqlite.Open(sqlite.Options{
		Path:               filepath.Join(t.TempDir(), "ledger.db"),
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
	tab, err := db.CreateTab(ctx, store.Tab{
		Name: "Phone plan", Kind: store.TabServices, CreatedBy: user.ID,
	}, nil)
	if err != nil {
		t.Fatalf("tab: %v", err)
	}
	return New(db), db, user, tab
}

// TAB-06: a charge drives the balance negative (owed); a payment drives it back
// toward zero and then into credit. Callers pass positive amounts throughout.
func TestSignConvention(t *testing.T) {
	led, _, user, tab := newFixture(t)
	ctx := context.Background()

	if _, _, err := led.Charge(ctx, Post{
		TabID: tab.ID, Amount: 4500, ActorUserID: user.ID, IdempotencyKey: "c1",
	}); err != nil {
		t.Fatalf("charge: %v", err)
	}
	if b, _ := led.Balance(ctx, tab.ID); b != -4500 {
		t.Errorf("after $45 charge balance = %s, want -45.00 (owed)", b)
	}

	if _, _, err := led.Payment(ctx, Post{
		TabID: tab.ID, Amount: 4500, ActorUserID: user.ID, IdempotencyKey: "p1",
	}); err != nil {
		t.Fatalf("payment: %v", err)
	}
	if b, _ := led.Balance(ctx, tab.ID); b != 0 {
		t.Errorf("after matching payment balance = %s, want 0.00 (settled)", b)
	}

	if _, _, err := led.Payment(ctx, Post{
		TabID: tab.ID, Amount: 1000, ActorUserID: user.ID, IdempotencyKey: "p2",
	}); err != nil {
		t.Fatalf("advance payment: %v", err)
	}
	if b, _ := led.Balance(ctx, tab.ID); b != 1000 {
		t.Errorf("after paying ahead balance = %s, want 10.00 (credit)", b)
	}
}

// A caller must never apply the sign itself, and a zero charge is a bug.
func TestChargeRejectsNonPositive(t *testing.T) {
	led, _, user, tab := newFixture(t)
	ctx := context.Background()

	for _, amount := range []money.Cents{0, -100} {
		if _, _, err := led.Charge(ctx, Post{
			TabID: tab.ID, Amount: amount, ActorUserID: user.ID,
		}); !errors.Is(err, ErrNonPositive) {
			t.Errorf("Charge(%s) error = %v, want ErrNonPositive", amount, err)
		}
		if _, _, err := led.Payment(ctx, Post{
			TabID: tab.ID, Amount: amount, ActorUserID: user.ID,
		}); !errors.Is(err, ErrNonPositive) {
			t.Errorf("Payment(%s) error = %v, want ErrNonPositive", amount, err)
		}
	}

	if b, _ := led.Balance(ctx, tab.ID); b != 0 {
		t.Errorf("rejected posts changed the balance to %s", b)
	}
}

// LEDGER-02: a correction is a reversing entry; the original stays intact.
func TestReverseLeavesOriginalIntact(t *testing.T) {
	led, db, user, tab := newFixture(t)
	ctx := context.Background()

	original, _, err := led.Charge(ctx, Post{
		TabID: tab.ID, Amount: 7500, Memo: "October", ActorUserID: user.ID, IdempotencyKey: "c1",
	})
	if err != nil {
		t.Fatalf("charge: %v", err)
	}

	reversal, _, err := led.Reverse(ctx, original.Seq, user.ID, "", "r1")
	if err != nil {
		t.Fatalf("reverse: %v", err)
	}

	if reversal.Amount != 7500 {
		t.Errorf("reversal amount = %s, want +75.00 to offset the charge", reversal.Amount)
	}
	if reversal.ReversesSeq == nil || *reversal.ReversesSeq != original.Seq {
		t.Error("reversal does not point at the entry it corrects")
	}
	if b, _ := led.Balance(ctx, tab.ID); b != 0 {
		t.Errorf("balance after reversal = %s, want 0.00", b)
	}

	// The original is still there, unchanged, and still visible in history.
	stillThere, err := db.GetEntry(ctx, original.Seq)
	if err != nil {
		t.Fatalf("original entry disappeared: %v", err)
	}
	if stillThere.Amount != -7500 || stillThere.Memo != "October" {
		t.Errorf("original entry was altered: %s %q", stillThere.Amount, stillThere.Memo)
	}

	entries, _ := led.History(ctx, tab.ID)
	if len(entries) != 2 {
		t.Errorf("history has %d entries, want 2 (original plus its reversal)", len(entries))
	}
}

// An entry may be reversed only once, so a double-tapped undo cannot post two
// offsetting corrections and swing the balance the wrong way.
func TestDoubleReverseRejected(t *testing.T) {
	led, _, user, tab := newFixture(t)
	ctx := context.Background()

	original, _, err := led.Charge(ctx, Post{
		TabID: tab.ID, Amount: 5000, ActorUserID: user.ID, IdempotencyKey: "c1",
	})
	if err != nil {
		t.Fatalf("charge: %v", err)
	}
	if _, _, err := led.Reverse(ctx, original.Seq, user.ID, "", "r1"); err != nil {
		t.Fatalf("first reverse: %v", err)
	}

	// A different idempotency key, so only the reverses_seq constraint can stop it.
	_, _, err = led.Reverse(ctx, original.Seq, user.ID, "", "r2")
	if !errors.Is(err, ErrAlreadyReversed) {
		t.Errorf("second reverse error = %v, want ErrAlreadyReversed", err)
	}

	if b, _ := led.Balance(ctx, tab.ID); b != 0 {
		t.Errorf("balance = %s after a blocked second reversal, want 0.00", b)
	}
}

func TestReversalCannotBeReversed(t *testing.T) {
	led, _, user, tab := newFixture(t)
	ctx := context.Background()

	original, _, _ := led.Charge(ctx, Post{
		TabID: tab.ID, Amount: 2500, ActorUserID: user.ID, IdempotencyKey: "c1",
	})
	reversal, _, err := led.Reverse(ctx, original.Seq, user.ID, "", "r1")
	if err != nil {
		t.Fatalf("reverse: %v", err)
	}

	if _, _, err := led.Reverse(ctx, reversal.Seq, user.ID, "", "r2"); !errors.Is(err, ErrNotReversible) {
		t.Errorf("reversing a reversal error = %v, want ErrNotReversible", err)
	}
}

// CHG-03: an adjustment accepts a signed amount, because a correction moves in
// whichever direction the caller states.
func TestAdjustmentAcceptsBothDirections(t *testing.T) {
	led, _, user, tab := newFixture(t)
	ctx := context.Background()

	if _, _, err := led.Adjustment(ctx, Post{
		TabID: tab.ID, Amount: -500, Memo: "Missed fee", ActorUserID: user.ID, IdempotencyKey: "a1",
	}); err != nil {
		t.Fatalf("negative adjustment: %v", err)
	}
	if _, _, err := led.Adjustment(ctx, Post{
		TabID: tab.ID, Amount: 200, Memo: "Goodwill", ActorUserID: user.ID, IdempotencyKey: "a2",
	}); err != nil {
		t.Fatalf("positive adjustment: %v", err)
	}
	if b, _ := led.Balance(ctx, tab.ID); b != -300 {
		t.Errorf("balance = %s, want -3.00", b)
	}

	if _, _, err := led.Adjustment(ctx, Post{
		TabID: tab.ID, Amount: 0, ActorUserID: user.ID, IdempotencyKey: "a3",
	}); !errors.Is(err, ErrNonPositive) {
		t.Errorf("zero adjustment error = %v, want ErrNonPositive", err)
	}
}

// A generated key must be unique per post, so two identical charges both land.
func TestGeneratedKeysAreDistinct(t *testing.T) {
	led, _, user, tab := newFixture(t)
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		if _, replayed, err := led.Charge(ctx, Post{
			TabID: tab.ID, Amount: 1000, ActorUserID: user.ID,
		}); err != nil || replayed {
			t.Fatalf("post %d: replayed=%v err=%v", i, replayed, err)
		}
	}
	if b, _ := led.Balance(ctx, tab.ID); b != -5000 {
		t.Errorf("balance = %s, want -50.00 from five distinct charges", b)
	}
}

// TAB-04: the item breakdown sums to the charge.
func TestSumItems(t *testing.T) {
	items := []store.EntryItem{
		{Name: "Line 1", Amount: 4500},
		{Name: "Line 2", Amount: 3000},
		{Name: "Insurance", Amount: 799},
	}
	got, ok := SumItems(items)
	if !ok {
		t.Fatal("unexpected overflow")
	}
	if got != 8299 {
		t.Errorf("SumItems = %s, want 82.99", got)
	}
}

// PAY-02: a payment must carry a recognized method.
func TestPaymentRequiresValidMethod(t *testing.T) {
	led, _, user, tab := newFixture(t)
	ctx := context.Background()

	if _, _, err := led.Payment(ctx, Post{
		TabID: tab.ID, Amount: 1000, ActorUserID: user.ID,
		IdempotencyKey: "bad", Method: store.PaymentMethod("gold bars"),
	}); !errors.Is(err, ErrBadMethod) {
		t.Errorf("bogus method error = %v, want ErrBadMethod", err)
	}

	for _, m := range store.PaymentMethods() {
		if _, _, err := led.Payment(ctx, Post{
			TabID: tab.ID, Amount: 100, ActorUserID: user.ID,
			IdempotencyKey: "ok-" + string(m), Method: m,
		}); err != nil {
			t.Errorf("method %q rejected: %v", m, err)
		}
	}
}

// A reversal carries no payment method: undoing a cash payment is not itself
// a cash movement.
func TestReversalCarriesNoMethod(t *testing.T) {
	led, _, user, tab := newFixture(t)
	ctx := context.Background()

	payment, _, err := led.Payment(ctx, Post{
		TabID: tab.ID, Amount: 5000, ActorUserID: user.ID,
		IdempotencyKey: "p1", Method: store.MethodCash,
	})
	if err != nil {
		t.Fatalf("payment: %v", err)
	}

	reversal, _, err := led.Reverse(ctx, payment.Seq, user.ID, "", "r1")
	if err != nil {
		t.Fatalf("reverse: %v", err)
	}
	if reversal.Method != store.MethodNone {
		t.Errorf("reversal method = %q, want empty", reversal.Method)
	}
	if b, _ := led.Balance(ctx, tab.ID); b != 0 {
		t.Errorf("balance = %s after reversing the payment, want 0.00", b)
	}
}

// ReversedSeqs and CanUndo drive whether the undo control is offered.
func TestUndoEligibility(t *testing.T) {
	led, _, user, tab := newFixture(t)
	ctx := context.Background()

	first, _, _ := led.Charge(ctx, Post{
		TabID: tab.ID, Amount: 1000, ActorUserID: user.ID, IdempotencyKey: "c1",
	})
	second, _, _ := led.Charge(ctx, Post{
		TabID: tab.ID, Amount: 2000, ActorUserID: user.ID, IdempotencyKey: "c2",
	})
	if _, _, err := led.Reverse(ctx, first.Seq, user.ID, "", "r1"); err != nil {
		t.Fatalf("reverse: %v", err)
	}

	entries, err := led.History(ctx, tab.ID)
	if err != nil {
		t.Fatal(err)
	}
	reversed := ReversedSeqs(entries)

	if !reversed[first.Seq] {
		t.Error("the reversed entry was not detected as reversed")
	}
	for _, e := range entries {
		switch {
		case e.Seq == first.Seq:
			if CanUndo(e, reversed) {
				t.Error("an already-reversed entry is still offered for undo")
			}
		case e.Seq == second.Seq:
			if !CanUndo(e, reversed) {
				t.Error("an untouched entry is not offered for undo")
			}
		case e.Kind == store.KindReversal:
			if CanUndo(e, reversed) {
				t.Error("a reversal is offered for undo")
			}
		}
	}
}
