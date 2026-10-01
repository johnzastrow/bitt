package sqldb

import (
	"context"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/johnzastrow/bitt/internal/money"
	"github.com/johnzastrow/bitt/internal/store"
)

// A bank description and note are free text up to 16 KB; the memo they go
// into is VARCHAR(1000) on MariaDB. Confirming a match with a long note, and
// recording a line with one, must still post -- with the memo cut to fit --
// on both backends.
func TestLongBankTextFitsTheMemo(t *testing.T) {
	f := newReconFixture(t)
	ctx := context.Background()
	f.addLines(t, map[string]money.Cents{"a": 6000, "b": 7000})
	long := strings.Repeat("é", 3000)
	for _, k := range []string{"a", "b"} {
		l := f.lines[k]
		if _, err := f.db.db.ExecContext(ctx, `UPDATE bank_lines SET description = ?, note = ? WHERE id = ?`,
			long, long, l.ID); err != nil {
			t.Fatal(err)
		}
		l.Description, l.Note = long, long
		f.lines[k] = l
	}

	m, _, err := f.confirm("a", f.pay(t, "p", 5000), true)
	if err != nil {
		t.Fatalf("confirm with long bank text: %v", err)
	}
	e, _ := f.db.GetEntry(ctx, *m.DeltaEntrySeq)
	if n := utf8.RuneCountInString(e.Memo); n > 1000 || !strings.HasPrefix(e.Memo, "Bank reconciliation: ") {
		t.Errorf("delta memo is %d runes", n)
	}

	rec, err := f.led.RecordLine(ctx, f.lines["b"].ID, f.tab.ID, f.admin.ID, store.MethodTransfer,
		"Bank reconciliation: x\nBank note: "+long, time.Date(2026, 9, 1, 16, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("record with long note: %v", err)
	}
	if n := utf8.RuneCountInString(rec.Memo); n > 1000 {
		t.Errorf("recorded memo is %d runes", n)
	}
}
