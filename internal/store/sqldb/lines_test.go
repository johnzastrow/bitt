package sqldb

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/johnzastrow/bitt/internal/ledger"
	"github.com/johnzastrow/bitt/internal/money"
	"github.com/johnzastrow/bitt/internal/store"
)

// RECON-05 storage: record, unrecord, ignore, unignore, and their history.
// Both backends.

func TestRecordAndUnrecordLine(t *testing.T) {
	f := newReconFixture(t)
	ctx := context.Background()
	f.addLines(t, map[string]money.Cents{"a": 7525})
	l := f.lines["a"]
	start := f.balance(t)
	at := time.Date(2026, 9, 8, 16, 0, 0, 0, time.UTC)

	e, err := f.led.RecordLine(ctx, l.ID, f.tab.ID, f.admin.ID, store.MethodTransfer, "memo", at)
	if err != nil {
		t.Fatal(err)
	}
	if e.Amount != 7525 || e.ActorUserID != f.admin.ID || !e.EffectiveAt.Equal(at) {
		t.Errorf("entry = %+v", e)
	}
	if f.balance(t)-start != 7525 || f.lineState(t, "a") != store.BankLineRecorded {
		t.Error("record did not post or mark the line")
	}
	// Refused: recording again, a cash method.
	if _, err := f.led.RecordLine(ctx, l.ID, f.tab.ID, f.admin.ID, store.MethodTransfer, "m", at); !errors.Is(err, store.ErrLineNotOpen) {
		t.Errorf("record twice: %v", err)
	}
	if _, err := f.led.RecordLine(ctx, l.ID, f.tab.ID, f.admin.ID, store.MethodNone, "m", at); !errors.Is(err, ledger.ErrBadMethod) {
		t.Errorf("no method: %v", err)
	}
	// Ignoring a recorded line is refused.
	if err := f.db.IgnoreBankLine(ctx, l.ID, f.admin.ID, ""); !errors.Is(err, store.ErrLineNotOpen) {
		t.Errorf("ignore recorded: %v", err)
	}

	if err := f.led.UnrecordLine(ctx, l.ID, f.admin.ID); err != nil {
		t.Fatal(err)
	}
	if f.balance(t) != start || f.lineState(t, "a") != store.BankLineOpen {
		t.Error("unrecord did not restore")
	}
	if err := f.led.UnrecordLine(ctx, l.ID, f.admin.ID); !errors.Is(err, store.ErrLineNotOpen) {
		t.Errorf("unrecord twice: %v", err)
	}
	evs, _ := f.db.ListBankLineEvents(ctx, l.ID)
	if len(evs) != 2 || evs[0].Action != "recorded" || evs[0].EntrySeq == nil || *evs[0].EntrySeq != e.Seq ||
		evs[1].Action != "unrecorded" || evs[1].ByName != f.admin.DisplayName || evs[1].At.IsZero() {
		t.Errorf("events = %+v", evs)
	}
	// It can be recorded again, with a new key.
	if _, err := f.led.RecordLine(ctx, l.ID, f.tab.ID, f.admin.ID, store.MethodOther, "again", at); err != nil {
		t.Errorf("record again: %v", err)
	}
}

func TestIgnoreUnignoreLine(t *testing.T) {
	f := newReconFixture(t)
	ctx := context.Background()
	f.addLines(t, map[string]money.Cents{"a": 100})
	id := f.lines["a"].ID
	if err := f.db.IgnoreBankLine(ctx, id, f.admin.ID, "refund"); err != nil {
		t.Fatal(err)
	}
	l, _ := f.db.GetBankLine(ctx, id)
	if l.State != store.BankLineIgnored || l.IgnoreReason != "Not BitTabby" || l.IgnoreNote != "refund" {
		t.Errorf("ignored = %+v", l)
	}
	if err := f.db.IgnoreBankLine(ctx, id, f.admin.ID, ""); !errors.Is(err, store.ErrLineNotOpen) {
		t.Errorf("ignore twice: %v", err)
	}
	if err := f.db.UnignoreBankLine(ctx, id, f.admin.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.db.UnignoreBankLine(ctx, id, f.admin.ID); !errors.Is(err, store.ErrLineNotOpen) {
		t.Errorf("unignore an open line: %v", err)
	}
	if err := f.db.IgnoreBankLine(ctx, 99999, f.admin.ID, ""); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("unknown line: %v", err)
	}
	evs, _ := f.db.ListBankLineEvents(ctx, id)
	if len(evs) != 2 || evs[0].Note != "refund" {
		t.Errorf("events = %+v", evs)
	}
}

// The schema ties "recorded" to its payment, both ways.
func TestRecordedStateChecked(t *testing.T) {
	f := newReconFixture(t)
	ctx := context.Background()
	f.addLines(t, map[string]money.Cents{"a": 100})
	id := f.lines["a"].ID
	if _, err := f.db.db.ExecContext(ctx, `UPDATE bank_lines SET state = 'recorded' WHERE id = ?`, id); err == nil {
		t.Error("recorded without an entry accepted")
	}
	p := f.pay(t, "p", 100)
	if _, err := f.db.db.ExecContext(ctx, `UPDATE bank_lines SET recorded_entry_seq = ? WHERE id = ?`, p.Seq, id); err == nil {
		t.Error("an entry on an open line accepted")
	}
}

// Two people recording the same line at once: one payment, not two.
func TestConcurrentRecord(t *testing.T) {
	f := newReconFixture(t)
	ctx := context.Background()
	f.addLines(t, map[string]money.Cents{"a": 5000})
	start := f.balance(t)
	var wg sync.WaitGroup
	errs := make(chan error, 6)
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := f.led.RecordLine(ctx, f.lines["a"].ID, f.tab.ID, f.admin.ID, store.MethodTransfer, "m",
				time.Date(2026, 9, 1, 16, 0, 0, 0, time.UTC))
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	ok := 0
	for err := range errs {
		if err == nil {
			ok++
		} else if !errors.Is(err, store.ErrLineNotOpen) {
			t.Errorf("unexpected: %v", err)
		}
	}
	if ok != 1 || f.balance(t)-start != 5000 {
		t.Errorf("%d recordings, balance moved %s", ok, f.balance(t)-start)
	}
}
