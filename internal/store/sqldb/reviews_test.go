package sqldb

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/johnzastrow/bitt/internal/money"
	"github.com/johnzastrow/bitt/internal/store"
)

// Payments with no bank transaction (migration 0018). Both backends.

func TestPaymentNotInBank(t *testing.T) {
	f := newReconFixture(t)
	ctx := context.Background()
	from, to := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	cash := f.pay(t, "cash", 2000)
	other := f.pay(t, "other", 3000)

	r, err := f.db.SetPaymentNotInBank(ctx, cash.Seq, f.admin.ID, "paid in cash at dinner")
	if err != nil {
		t.Fatal(err)
	}
	if r.EntrySeq != cash.Seq || r.Note != "paid in cash at dinner" || r.ByName != f.admin.DisplayName ||
		r.At.IsZero() || !r.Active() || r.Amount != 2000 || r.TabID != f.tab.ID {
		t.Errorf("review = %+v", r)
	}
	// It is no longer unaddressed, nor offered for matching.
	un, _ := f.db.ListUnaddressedPayments(ctx, from, to)
	if len(un) != 1 || un[0].Seq != other.Seq {
		t.Errorf("unaddressed = %+v", un)
	}
	f.addLines(t, map[string]money.Cents{"a": 2000})
	if _, _, err := f.confirm("a", cash, false); !errors.Is(err, store.ErrPaymentMatched) {
		t.Errorf("confirming a set-aside payment: %v", err)
	}
	// Twice is refused.
	if _, err := f.db.SetPaymentNotInBank(ctx, cash.Seq, f.admin.ID, ""); !errors.Is(err, store.ErrPaymentMatched) {
		t.Errorf("set aside twice: %v", err)
	}
	// Undo: back among the unaddressed, and matchable.
	if err := f.db.UndoPaymentReview(ctx, r.ID, f.admin.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.db.UndoPaymentReview(ctx, r.ID, f.admin.ID); !errors.Is(err, store.ErrMatchUndone) {
		t.Errorf("undo twice: %v", err)
	}
	if err := f.db.UndoPaymentReview(ctx, 99999, f.admin.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("unknown review: %v", err)
	}
	if un, _ := f.db.ListUnaddressedPayments(ctx, from, to); len(un) != 2 {
		t.Errorf("after undo %d unaddressed", len(un))
	}
	if _, _, err := f.confirm("a", cash, false); err != nil {
		t.Errorf("confirm after undo: %v", err)
	}
	// A matched payment cannot be set aside.
	if _, err := f.db.SetPaymentNotInBank(ctx, cash.Seq, f.admin.ID, ""); !errors.Is(err, store.ErrPaymentMatched) {
		t.Errorf("set aside a matched payment: %v", err)
	}
	// History keeps both reviews.
	rs, _ := f.db.ListPaymentReviews(ctx, f.tab.ID, 10)
	if len(rs) != 1 || rs[0].Active() || rs[0].UndoneByName != f.admin.DisplayName {
		t.Errorf("reviews = %+v", rs)
	}
}

func TestPaymentNotInBankRefusals(t *testing.T) {
	f := newReconFixture(t)
	ctx := context.Background()
	p := f.pay(t, "p", 2000)
	if _, _, err := f.led.Reverse(ctx, p.Seq, f.payer.ID, "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.SetPaymentNotInBank(ctx, p.Seq, f.admin.ID, ""); err == nil {
		t.Error("a reversed payment was set aside")
	}
	if _, err := f.db.SetPaymentNotInBank(ctx, 99999, f.admin.ID, ""); !errors.Is(err, store.ErrNotAPayment) {
		t.Errorf("unknown entry: %v", err)
	}
}

// Confirming and setting aside the same payment at once: exactly one wins.
func TestConfirmRacesSetAside(t *testing.T) {
	for i := 0; i < 6; i++ {
		f := newReconFixture(t)
		ctx := context.Background()
		f.addLines(t, map[string]money.Cents{"a": 2000})
		p := f.pay(t, "p", 2000)
		var wg sync.WaitGroup
		var cErr, sErr error
		wg.Add(2)
		go func() { defer wg.Done(); _, _, cErr = f.confirm("a", p, false) }()
		go func() { defer wg.Done(); _, sErr = f.db.SetPaymentNotInBank(ctx, p.Seq, f.admin.ID, "") }()
		wg.Wait()
		if (cErr == nil) == (sErr == nil) {
			t.Fatalf("run %d: confirm %v, set aside %v -- want exactly one to win", i, cErr, sErr)
		}
	}
}

func TestBankCoverage(t *testing.T) {
	f := newReconFixture(t)
	ctx := context.Background()
	if a, b, err := f.db.BankCoverage(ctx); err != nil || a != "" || b != "" {
		t.Errorf("empty coverage = %q %q %v", a, b, err)
	}
	f.addLines(t, map[string]money.Cents{"a": 100})
	if a, b, _ := f.db.BankCoverage(ctx); a != "2026-09-01" || b != "2026-09-01" {
		t.Errorf("coverage = %q %q", a, b)
	}
}
