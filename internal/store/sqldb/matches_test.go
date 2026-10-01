package sqldb

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/johnzastrow/bitt/internal/ledger"
	"github.com/johnzastrow/bitt/internal/money"
	"github.com/johnzastrow/bitt/internal/store"
)

// RECON-04: confirming a match, posting the delta, undoing it. Driven through
// the ledger service, as the app does, on both backends.

type reconFixture struct {
	db     *DB
	led    *ledger.Service
	admin  store.User
	payer  store.User
	tab    store.Tab
	format store.BankFormat
	imp    store.BankImport
	lines  map[string]store.BankLine
	n      int
}

func newReconFixture(t *testing.T) *reconFixture {
	t.Helper()
	db := newTestDB(t)
	f := &reconFixture{db: db, led: ledger.New(db), lines: map[string]store.BankLine{}}
	f.admin = mustAdmin(t, db, "admin@example.com")
	f.payer = mustUser(t, db, "sam@example.com")
	f.tab = mustTab(t, db, f.payer.ID)
	f.format = mustFormat(t, db, f.admin.ID, "h")
	return f
}

// addLines imports lines keyed by name, each with the given cents.
func (f *reconFixture) addLines(t *testing.T, amounts map[string]money.Cents) {
	t.Helper()
	ctx := context.Background()
	f.n++
	var in []store.BankLine
	row := 2
	for key, c := range amounts {
		l := line(row, fmt.Sprint(f.n, key), c)
		l.Note = "Note for " + key + "\nsecond line"
		in = append(in, l)
		row++
	}
	imp, err := f.db.SaveBankImport(ctx, store.BankImport{FormatID: f.format.ID, FileName: "Sept.csv",
		FileSHA256: fp(fmt.Sprint("file", f.n)), UploadedBy: f.admin.ID}, in)
	if err != nil {
		t.Fatal(err)
	}
	f.imp = imp
	stored, _ := f.db.ListBankLines(ctx, imp.ID)
	for _, l := range stored {
		for key := range amounts {
			if l.Fingerprint == fp(fmt.Sprint(f.n, key)) {
				f.lines[key] = l
			}
		}
	}
}

func (f *reconFixture) pay(t *testing.T, key string, c money.Cents) store.Entry {
	t.Helper()
	e, _, err := f.led.Payment(context.Background(), ledger.Post{TabID: f.tab.ID, Amount: c,
		ActorUserID: f.payer.ID, Method: store.MethodTransfer, IdempotencyKey: key,
		EffectiveAt: time.Date(2026, 9, 1, 15, 0, 0, 0, time.UTC)})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func (f *reconFixture) confirm(lineKey string, p store.Entry, withNote bool) (store.BankMatch, bool, error) {
	l := f.lines[lineKey]
	return f.led.ConfirmMatch(context.Background(), ledger.Confirmation{
		Match: store.NewBankMatch{
			LineID: l.ID, EntrySeq: p.Seq, TabID: p.TabID, BankDate: l.PostedOn, BankAmount: l.Amount,
			RecordedDate: "2026-09-01", RecordedAmount: p.Amount, NoteInMemo: withNote,
			ConfirmedBy: f.admin.ID,
		},
		BankAt:      time.Date(2026, 9, 1, 16, 0, 0, 0, time.UTC),
		Description: l.Description, FileName: "Sept.csv", Row: l.Row, Note: l.Note,
	})
}

func (f *reconFixture) balance(t *testing.T) money.Cents {
	t.Helper()
	b, err := f.db.SumEntries(context.Background(), f.tab.ID)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func (f *reconFixture) lineState(t *testing.T, key string) store.BankLineState {
	t.Helper()
	ls, _ := f.db.ListBankLines(context.Background(), f.lines[key].ImportID)
	for _, l := range ls {
		if l.ID == f.lines[key].ID {
			return l.State
		}
	}
	t.Fatalf("line %s not found", key)
	return ""
}

// Exit criterion: B > R posts a payment, B < R a debit adjustment, B = R
// nothing; each balance exactly right; confirming twice posts once; undo
// restores the balance exactly.
func TestConfirmPostsTheDelta(t *testing.T) {
	cases := []struct {
		name       string
		bank, rec  money.Cents
		wantKind   store.EntryKind
		wantAmount money.Cents
	}{
		{"bank more", 16000, 15000, store.KindPayment, 1000},
		{"bank less", 14000, 15000, store.KindAdjustment, -1000},
		{"equal", 15000, 15000, "", 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newReconFixture(t)
			ctx := context.Background()
			f.addLines(t, map[string]money.Cents{"a": c.bank})
			p := f.pay(t, "p", c.rec)
			before := f.balance(t)

			m, replayed, err := f.confirm("a", p, false)
			if err != nil || replayed {
				t.Fatalf("confirm: %v replayed=%v", err, replayed)
			}
			if got := f.balance(t) - before; got != c.wantAmount {
				t.Errorf("balance moved by %s, want %s", got, c.wantAmount)
			}
			if c.wantKind == "" {
				if m.DeltaEntrySeq != nil {
					t.Error("equal amounts posted an entry")
				}
			} else {
				if m.DeltaEntrySeq == nil {
					t.Fatal("no delta entry")
				}
				e, err := f.db.GetEntry(ctx, *m.DeltaEntrySeq)
				if err != nil {
					t.Fatal(err)
				}
				if e.Kind != c.wantKind || e.Amount != c.wantAmount || e.ActorUserID != f.admin.ID ||
					e.IdempotencyKey != fmt.Sprint("recon:", m.ID) || e.TabID != f.tab.ID {
					t.Errorf("delta = %+v", e)
				}
				if c.wantKind == store.KindPayment && (e.Method != store.MethodTransfer ||
					!strings.HasPrefix(e.Memo, "Bank reconciliation: Deposit Transfer FROM 1a (line 2 of Sept.csv)")) {
					t.Errorf("payment delta method %q memo %q", e.Method, e.Memo)
				}
				if c.wantKind == store.KindAdjustment && e.Memo != "Bank reconciliation: recorded $150.00, bank shows $140.00 (Deposit Transfer FROM 1a)" {
					t.Errorf("adjustment memo %q", e.Memo)
				}
				if strings.Contains(e.Memo, "Bank note") {
					t.Error("the bank note reached the memo without the checkbox")
				}
				if !e.EffectiveAt.Equal(time.Date(2026, 9, 1, 16, 0, 0, 0, time.UTC)) {
					t.Errorf("delta effective %v, want the bank date", e.EffectiveAt)
				}
			}
			// The recorded payment is never touched.
			if again, _ := f.db.GetEntry(ctx, p.Seq); again.Amount != p.Amount || !again.EffectiveAt.Equal(p.EffectiveAt) {
				t.Error("the recorded payment changed")
			}
			if f.lineState(t, "a") != store.BankLineMatched {
				t.Error("line not marked matched")
			}

			// Confirming the same pair again is a replay: same match, no entry.
			after := f.balance(t)
			m2, replayed, err := f.confirm("a", p, false)
			if err != nil || !replayed || m2.ID != m.ID || f.balance(t) != after {
				t.Errorf("second confirm: %v replayed=%v id %d/%d balance %s/%s", err, replayed, m2.ID, m.ID, f.balance(t), after)
			}

			// Undo restores the balance exactly and frees both sides.
			if err := f.led.UndoMatch(ctx, m.ID, f.admin.ID); err != nil {
				t.Fatal(err)
			}
			if f.balance(t) != before {
				t.Errorf("after undo balance %s, want %s", f.balance(t), before)
			}
			if f.lineState(t, "a") != store.BankLineOpen {
				t.Error("line not reopened")
			}
			um, _ := f.db.GetBankMatch(ctx, m.ID)
			if um.Active() || um.UndoneBy != f.admin.ID {
				t.Errorf("undone match = %+v", um)
			}
			if err := f.led.UndoMatch(ctx, m.ID, f.admin.ID); !errors.Is(err, store.ErrMatchUndone) {
				t.Errorf("undo twice: %v", err)
			}
			// It can be confirmed again: a new match, a new key.
			m3, _, err := f.confirm("a", p, false)
			if err != nil || m3.ID == m.ID {
				t.Fatalf("re-confirm: %v", err)
			}
			if f.balance(t)-before != c.wantAmount {
				t.Error("re-confirm did not post the delta again")
			}
			hist, _ := f.db.ListBankMatches(ctx, store.BankMatchFilter{TabID: f.tab.ID})
			if len(hist) != 2 || !hist[0].Active() || hist[1].Active() {
				t.Errorf("history = %d matches", len(hist))
			}
		})
	}
}

// Exit criterion: a note reaches a ledger memo only when the checkbox is
// ticked, and never otherwise; with equal amounts it never does.
func TestBankNoteInMemoOnlyWhenChosen(t *testing.T) {
	f := newReconFixture(t)
	ctx := context.Background()
	f.addLines(t, map[string]money.Cents{"more": 16000, "same": 5000})
	m, _, err := f.confirm("more", f.pay(t, "p1", 15000), true)
	if err != nil {
		t.Fatal(err)
	}
	e, _ := f.db.GetEntry(ctx, *m.DeltaEntrySeq)
	if !strings.HasSuffix(e.Memo, "\nBank note: Note for more\nsecond line") || !m.NoteInMemo {
		t.Errorf("memo %q, note_in_memo %v", e.Memo, m.NoteInMemo)
	}
	m2, _, err := f.confirm("same", f.pay(t, "p2", 5000), true)
	if err != nil {
		t.Fatal(err)
	}
	if m2.NoteInMemo || m2.DeltaEntrySeq != nil {
		t.Errorf("equal amounts: note_in_memo %v delta %v", m2.NoteInMemo, m2.DeltaEntrySeq)
	}
	// The note is still on the match record.
	if m2.Note != "Note for same\nsecond line" {
		t.Errorf("match note = %q", m2.Note)
	}
}

func TestConfirmRefusals(t *testing.T) {
	f := newReconFixture(t)
	ctx := context.Background()
	f.addLines(t, map[string]money.Cents{"a": 5000, "b": 5000})
	p1 := f.pay(t, "p1", 5000)
	p2 := f.pay(t, "p2", 5000)
	if _, _, err := f.confirm("a", p1, false); err != nil {
		t.Fatal(err)
	}

	// The line is taken by another payment.
	if _, _, err := f.confirm("a", p2, false); !errors.Is(err, store.ErrLineNotOpen) {
		t.Errorf("line taken: %v", err)
	}
	// The payment is taken by another line.
	if _, _, err := f.confirm("b", p1, false); !errors.Is(err, store.ErrPaymentMatched) {
		t.Errorf("payment taken: %v", err)
	}
	if f.lineState(t, "b") != store.BankLineOpen {
		t.Error("a refused confirm changed the line")
	}

	// A reversed payment.
	p3 := f.pay(t, "p3", 5000)
	if _, _, err := f.led.Reverse(ctx, p3.Seq, f.payer.ID, "", ""); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.confirm("b", p3, false); !errors.Is(err, store.ErrNotAPayment) {
		t.Errorf("reversed payment: %v", err)
	}
	// Not a payment at all.
	// (Amount made positive so the store's own check is what refuses it.)
	ch, _, _ := f.led.Charge(ctx, ledger.Post{TabID: f.tab.ID, Amount: 5000, ActorUserID: f.payer.ID})
	ch.Amount = 5000
	if _, _, err := f.confirm("b", ch, false); !errors.Is(err, store.ErrNotAPayment) {
		t.Errorf("charge: %v", err)
	}
	// A payment on another tab than claimed.
	wrong := p2
	wrong.TabID = f.tab.ID + 999
	if _, _, err := f.confirm("b", wrong, false); !errors.Is(err, store.ErrNotAPayment) {
		t.Errorf("wrong tab: %v", err)
	}
	// A reconciliation delta is not itself matchable.
	f.addLines(t, map[string]money.Cents{"c": 6000})
	mc, _, err := f.confirm("c", p2, false)
	if err != nil {
		t.Fatal(err)
	}
	delta, _ := f.db.GetEntry(ctx, *mc.DeltaEntrySeq)
	f.addLines(t, map[string]money.Cents{"d": 1000})
	if _, _, err := f.confirm("d", delta, false); !errors.Is(err, store.ErrNotAPayment) {
		t.Errorf("delta as payment: %v", err)
	}
	// An ignored line.
	f.addLines(t, map[string]money.Cents{"e": 5000})
	eDesc := strings.Split(f.lines["e"].Description, "\n")[1] // "FROM <n>e"
	if _, _, err := f.db.AddIgnoreRule(ctx, store.IgnoreRule{FormatID: f.format.ID, Contains: eDesc, CreatedBy: f.admin.ID}); err != nil {
		t.Fatal(err)
	}
	p4 := f.pay(t, "p4", 5000)
	if _, _, err := f.confirm("e", p4, false); !errors.Is(err, store.ErrLineNotOpen) {
		t.Errorf("ignored line: %v", err)
	}
}

// Candidates exclude matched payments and anything reconciliation posted.
func TestCandidatesExcludeMatchedAndDeltas(t *testing.T) {
	f := newReconFixture(t)
	ctx := context.Background()
	f.addLines(t, map[string]money.Cents{"a": 6000})
	p := f.pay(t, "p", 5000)
	free := f.pay(t, "free", 7000)
	from, to := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)

	m, _, err := f.confirm("a", p, false)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := f.db.ListPaymentCandidates(ctx, from, to)
	if len(got) != 1 || got[0].Seq != free.Seq {
		t.Errorf("candidates after confirm = %+v (delta %v)", got, m.DeltaEntrySeq)
	}
	if err := f.led.UndoMatch(ctx, m.ID, f.admin.ID); err != nil {
		t.Fatal(err)
	}
	got, _ = f.db.ListPaymentCandidates(ctx, from, to)
	if len(got) != 2 {
		t.Errorf("after undo %d candidates, want the payment back and the free one", len(got))
	}
}

// A manual undo on the tab unmakes the match (decided 2026-10-01), in one
// transaction, recording who, when and why; the balance ends exactly as if
// neither the payment nor the match had happened.
func TestTabUndoUnmakesTheMatch(t *testing.T) {
	t.Run("undo the matched payment", func(t *testing.T) {
		f := newReconFixture(t)
		ctx := context.Background()
		f.addLines(t, map[string]money.Cents{"a": 6000})
		start := f.balance(t)
		p := f.pay(t, "p", 5000)
		m, _, err := f.confirm("a", p, false)
		if err != nil {
			t.Fatal(err)
		}
		rev, _, err := f.led.Reverse(ctx, p.Seq, f.payer.ID, "", "")
		if err != nil {
			t.Fatalf("reverse matched payment: %v", err)
		}
		if rev.ReversesSeq == nil || *rev.ReversesSeq != p.Seq || rev.Amount != -5000 {
			t.Errorf("returned reversal = %+v", rev)
		}
		if f.balance(t) != start {
			t.Errorf("balance %s, want %s (payment and its $10 difference both gone)", f.balance(t), start)
		}
		um, _ := f.db.GetBankMatch(ctx, m.ID)
		if um.Active() || um.UndoneBy != f.payer.ID || um.UndoReason != ledger.UnmadePaymentUndone ||
			um.UndoneByName != f.payer.DisplayName || um.UndoneAt == nil {
			t.Errorf("unmade match = %+v", um)
		}
		if f.lineState(t, "a") != store.BankLineOpen {
			t.Error("line not reopened")
		}
		// Reversing again is refused as already reversed.
		if _, _, err := f.led.Reverse(ctx, p.Seq, f.payer.ID, "", ""); !errors.Is(err, ledger.ErrAlreadyReversed) {
			t.Errorf("second reverse: %v", err)
		}
	})
	t.Run("undo the difference", func(t *testing.T) {
		f := newReconFixture(t)
		ctx := context.Background()
		f.addLines(t, map[string]money.Cents{"a": 6000})
		p := f.pay(t, "p", 5000)
		afterPay := f.balance(t)
		m, _, err := f.confirm("a", p, false)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := f.led.Reverse(ctx, *m.DeltaEntrySeq, f.admin.ID, "", ""); err != nil {
			t.Fatalf("reverse the difference: %v", err)
		}
		if f.balance(t) != afterPay {
			t.Errorf("balance %s, want %s (payment stands, difference gone)", f.balance(t), afterPay)
		}
		um, _ := f.db.GetBankMatch(ctx, m.ID)
		if um.Active() || um.UndoReason != ledger.UnmadeDifferenceUndone {
			t.Errorf("unmade = %+v", um)
		}
		// The payment is unmatched again: it can be matched afresh.
		if _, _, err := f.confirm("a", p, false); err != nil {
			t.Errorf("re-confirm after unmaking: %v", err)
		}
	})
	t.Run("equal amounts, no difference", func(t *testing.T) {
		f := newReconFixture(t)
		ctx := context.Background()
		f.addLines(t, map[string]money.Cents{"a": 5000})
		p := f.pay(t, "p", 5000)
		m, _, _ := f.confirm("a", p, false)
		if _, _, err := f.led.Reverse(ctx, p.Seq, f.payer.ID, "", ""); err != nil {
			t.Fatal(err)
		}
		if um, _ := f.db.GetBankMatch(ctx, m.ID); um.Active() {
			t.Error("match still stands after its payment was undone")
		}
	})
	t.Run("from Reconciliation", func(t *testing.T) {
		f := newReconFixture(t)
		ctx := context.Background()
		f.addLines(t, map[string]money.Cents{"a": 5000})
		m, _, _ := f.confirm("a", f.pay(t, "p", 5000), false)
		if err := f.led.UndoMatch(ctx, m.ID, f.admin.ID); err != nil {
			t.Fatal(err)
		}
		um, _ := f.db.GetBankMatch(ctx, m.ID)
		if um.UndoReason != ledger.UnmadeFromReconciliation || um.UndoneByName != f.admin.DisplayName {
			t.Errorf("unmade = %+v", um)
		}
	})
	// Undo on the tab racing undo from Reconciliation: whichever order, the
	// match ends unmade once, the difference reversed once, the payment
	// reversed once.
	t.Run("racing undos", func(t *testing.T) {
		for i := 0; i < 5; i++ {
			f := newReconFixture(t)
			ctx := context.Background()
			f.addLines(t, map[string]money.Cents{"a": 6000})
			start := f.balance(t)
			p := f.pay(t, "p", 5000)
			m, _, _ := f.confirm("a", p, false)
			var wg sync.WaitGroup
			var revErr, undoErr error
			wg.Add(2)
			go func() { defer wg.Done(); _, _, revErr = f.led.Reverse(ctx, p.Seq, f.payer.ID, "", "") }()
			go func() { defer wg.Done(); undoErr = f.led.UndoMatch(ctx, m.ID, f.admin.ID) }()
			wg.Wait()
			if revErr != nil {
				t.Fatalf("tab undo failed: %v", revErr)
			}
			if undoErr != nil && !errors.Is(undoErr, store.ErrMatchUndone) {
				t.Fatalf("reconciliation undo: %v", undoErr)
			}
			if f.balance(t) != start {
				t.Fatalf("run %d: balance %s, want %s", i, f.balance(t), start)
			}
		}
	})
	// An ordinary entry with no tie reverses as before.
	t.Run("untied entry", func(t *testing.T) {
		f := newReconFixture(t)
		p := f.pay(t, "p", 5000)
		start := f.balance(t)
		if _, _, err := f.led.Reverse(context.Background(), p.Seq, f.payer.ID, "", ""); err != nil {
			t.Fatal(err)
		}
		if f.balance(t) != start-5000 {
			t.Error("plain reverse wrong")
		}
	})
}

// Exit criterion: a payment and a line can each be in one active match, under
// concurrent confirms, on SQLite and on MariaDB.
func TestConcurrentConfirms(t *testing.T) {
	f := newReconFixture(t)
	ctx := context.Background()

	// Many payments racing for one line.
	f.addLines(t, map[string]money.Cents{"one": 5000})
	var pays []store.Entry
	for i := 0; i < 8; i++ {
		pays = append(pays, f.pay(t, fmt.Sprint("race", i), 5000+money.Cents(i)))
	}
	before := f.balance(t)
	var wg sync.WaitGroup
	results := make(chan error, len(pays))
	for _, p := range pays {
		wg.Add(1)
		go func(p store.Entry) {
			defer wg.Done()
			_, _, err := f.confirm("one", p, false)
			results <- err
		}(p)
	}
	wg.Wait()
	close(results)
	ok := 0
	for err := range results {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, store.ErrLineNotOpen):
		default:
			t.Errorf("unexpected error: %v", err)
		}
	}
	if ok != 1 {
		t.Fatalf("%d confirms of one line succeeded, want 1", ok)
	}
	ms, _ := f.db.ListBankMatches(ctx, store.BankMatchFilter{})
	if len(ms) != 1 {
		t.Fatalf("%d match rows, want 1", len(ms))
	}
	// Exactly one delta was posted: the winner's.
	if got := f.balance(t) - before; got != ms[0].DeltaAmount {
		t.Errorf("balance moved %s, the one delta is %s", got, ms[0].DeltaAmount)
	}

	// Many lines racing for one payment.
	keys := map[string]money.Cents{}
	for i := 0; i < 8; i++ {
		keys[fmt.Sprint("l", i)] = 7000
	}
	f.addLines(t, keys)
	target := f.pay(t, "target", 7000)
	results = make(chan error, len(keys))
	for k := range keys {
		wg.Add(1)
		go func(k string) {
			defer wg.Done()
			_, _, err := f.confirm(k, target, false)
			results <- err
		}(k)
	}
	wg.Wait()
	close(results)
	ok = 0
	for err := range results {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, store.ErrPaymentMatched):
		default:
			t.Errorf("unexpected error: %v", err)
		}
	}
	if ok != 1 {
		t.Errorf("%d confirms of one payment succeeded, want 1", ok)
	}

	// Concurrent undos of one match: exactly one reverses the delta.
	ms, _ = f.db.ListBankMatches(ctx, store.BankMatchFilter{})
	var withDelta store.BankMatch
	for _, m := range ms {
		if m.Active() && m.DeltaEntrySeq != nil {
			withDelta = m
		}
	}
	if withDelta.ID == 0 {
		t.Fatal("no match with a delta to undo")
	}
	before = f.balance(t)
	results = make(chan error, 6)
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results <- f.led.UndoMatch(ctx, withDelta.ID, f.admin.ID)
		}()
	}
	wg.Wait()
	close(results)
	ok = 0
	for err := range results {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, store.ErrMatchUndone), errors.Is(err, ledger.ErrAlreadyReversed):
		default:
			t.Errorf("unexpected undo error: %v", err)
		}
	}
	if ok != 1 {
		t.Errorf("%d undos succeeded, want 1", ok)
	}
	if got := f.balance(t) - before; got != -withDelta.DeltaAmount {
		t.Errorf("undo moved the balance %s, want %s", got, -withDelta.DeltaAmount)
	}
}

// The schema keeps the active columns honest even against a direct write.
func TestMatchActiveColumnsChecked(t *testing.T) {
	f := newReconFixture(t)
	ctx := context.Background()
	f.addLines(t, map[string]money.Cents{"a": 5000})
	m, _, err := f.confirm("a", f.pay(t, "p", 5000), false)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`UPDATE bank_matches SET active_line_id = NULL WHERE id = ?`,
		`UPDATE bank_matches SET undone_at = '2026-10-01T00:00:00.000Z' WHERE id = ?`,
		`UPDATE bank_matches SET active_entry_seq = active_entry_seq + 1 WHERE id = ?`,
	} {
		if _, err := f.db.db.ExecContext(ctx, q, m.ID); err == nil {
			t.Errorf("schema accepted: %s", q)
		}
	}
}

// The ledger finds the match store by type assertion; this fails to compile if
// the two drift apart, instead of the ledger silently going without it.
var _ ledger.MatchStore = (*DB)(nil)
