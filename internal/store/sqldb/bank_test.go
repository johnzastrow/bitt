package sqldb

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/johnzastrow/bitt/internal/bankcsv"
	"github.com/johnzastrow/bitt/internal/money"
	"github.com/johnzastrow/bitt/internal/store"
)

// RECON-02 storage: layouts, imports, lines, and duplicate detection. Run on
// both backends (BITT_TEST_MARIADB_DSN), since the race and deadlock handling
// only does anything on MariaDB.

func fp(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func refMapping() bankcsv.Mapping {
	return bankcsv.Mapping{Date: 1, DateLayout: "m/d/yyyy", Amount: 5, Debit: bankcsv.None,
		Credit: bankcsv.None, Description: 2, Account: 0, Note: 3, Reference: 4}
}

func mustFormat(t *testing.T, db *DB, by int64, sig string) store.BankFormat {
	t.Helper()
	f, err := db.CreateBankFormat(context.Background(), store.BankFormat{
		Name: "Credit union", Signature: fp(sig), HeaderText: "Account,Date,Description,Note,Check #,Amount,Balance",
		Mapping: refMapping(), CreatedBy: by,
	})
	if err != nil {
		t.Fatalf("create format: %v", err)
	}
	return f
}

func line(row int, key string, cents money.Cents) store.BankLine {
	return store.BankLine{
		Row: row, Raw: "raw " + key, Account: "PRIMARY SHARE", PostedOn: "2026-09-01",
		Amount: cents, Description: "Deposit Transfer\nFROM " + key, Fingerprint: fp(key),
	}
}

func TestBankFormats(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	admin := mustAdmin(t, db, "admin@example.com")

	f := mustFormat(t, db, admin.ID, "header-a")
	got, err := db.BankFormatBySignature(ctx, fp("header-a"))
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != f.ID || got.Mapping != refMapping() || got.Name != "Credit union" ||
		got.HeaderText != f.HeaderText || got.CreatedBy != admin.ID || got.CreatedAt.IsZero() {
		t.Errorf("round trip = %+v", got)
	}
	// Every mapping field survives, including the booleans and None columns.
	m := bankcsv.Mapping{Date: 0, DateLayout: "d.m.yyyy", Amount: bankcsv.None, Debit: 2, Credit: 3,
		IncomingNegative: true, DecimalComma: true, Description: 1, Account: bankcsv.None,
		Note: bankcsv.None, Reference: bankcsv.None}
	f2, err := db.CreateBankFormat(ctx, store.BankFormat{Name: "Euro bank", Signature: fp("header-b"),
		HeaderText: "x", Mapping: m, CreatedBy: admin.ID})
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := db.GetBankFormat(ctx, f2.ID); got.Mapping != m {
		t.Errorf("mapping = %+v, want %+v", got.Mapping, m)
	}

	// The same header cannot be saved twice.
	_, err = db.CreateBankFormat(ctx, store.BankFormat{Name: "Again", Signature: fp("header-a"),
		HeaderText: "x", Mapping: refMapping(), CreatedBy: admin.ID})
	if !errors.Is(err, store.ErrConflict) {
		t.Errorf("duplicate signature: %v, want ErrConflict", err)
	}
	if _, err := db.BankFormatBySignature(ctx, fp("unknown")); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("unknown signature: %v", err)
	}
	list, err := db.ListBankFormats(ctx)
	if err != nil || len(list) != 2 || list[0].Name != "Credit union" {
		t.Errorf("list = %+v, %v", list, err)
	}

	// The schema refuses nonsense the handler should never send.
	for _, bad := range []store.BankFormat{
		{Name: "", Signature: fp("c"), HeaderText: "x", Mapping: refMapping(), CreatedBy: admin.ID},
		{Name: "short sig", Signature: "abc", HeaderText: "x", Mapping: refMapping(), CreatedBy: admin.ID},
		{Name: "no user", Signature: fp("d"), HeaderText: "x", Mapping: refMapping(), CreatedBy: 99999},
	} {
		if _, err := db.CreateBankFormat(ctx, bad); err == nil {
			t.Errorf("accepted %+v", bad.Name)
		}
	}
}

func TestSaveBankImportAndDuplicates(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	admin := mustAdmin(t, db, "admin@example.com")
	f := mustFormat(t, db, admin.ID, "h")

	note := "Phone plan, September\nsplit with Ann"
	first := []store.BankLine{line(2, "a", 15000), line(4, "b", 120000), line(6, "c", 5000)}
	first[2].Note = note
	ignored := line(5, "dividend", 4)
	ignored.State, ignored.IgnoreReason = store.BankLineIgnored, "Deposit Dividend"
	first = append(first, ignored)

	imp, err := db.SaveBankImport(ctx, store.BankImport{
		FormatID: f.ID, FileName: "TransactionHistory.csv", FileSHA256: fp("file1"),
		UploadedBy: admin.ID, Outgoing: 2, Refused: 1, FirstDate: "2026-09-01", LastDate: "2026-09-30",
		RefusedDetail: "Row 9: amount \"x\" is not a number",
	}, first)
	if err != nil {
		t.Fatal(err)
	}
	if imp.Kept != 3 || imp.Ignored != 1 || imp.Duplicates != 0 || imp.Outgoing != 2 || imp.Refused != 1 {
		t.Errorf("first import counts = %+v", imp)
	}

	// Read back: counts, joins, and the lines in file order, note verbatim.
	got, err := db.GetBankImport(ctx, imp.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Kept != 3 || got.Ignored != 1 || got.FormatName != "Credit union" ||
		got.UploaderName != admin.DisplayName || got.FileName != "TransactionHistory.csv" ||
		got.FirstDate != "2026-09-01" || got.LastDate != "2026-09-30" || got.RefusedDetail == "" {
		t.Errorf("read import = %+v", got)
	}
	lines, err := db.ListBankLines(ctx, imp.ID)
	if err != nil || len(lines) != 4 {
		t.Fatalf("lines = %d, %v", len(lines), err)
	}
	rows := []int{lines[0].Row, lines[1].Row, lines[2].Row, lines[3].Row}
	if fmt.Sprint(rows) != "[2 4 5 6]" {
		t.Errorf("rows in order = %v", rows)
	}
	if lines[3].Note != note || lines[3].Amount != 5000 || lines[3].State != store.BankLineOpen ||
		lines[0].Description != "Deposit Transfer\nFROM a" || lines[0].Raw != "raw a" {
		t.Errorf("line = %+v", lines[3])
	}
	if lines[2].State != store.BankLineIgnored || lines[2].IgnoreReason != "Deposit Dividend" {
		t.Errorf("ignored line = %+v", lines[2])
	}

	// The same file again: everything is a duplicate, nothing is added, and
	// the import is still recorded (an audit of what was uploaded).
	again, err := db.SaveBankImport(ctx, store.BankImport{
		FormatID: f.ID, FileName: "TransactionHistory.csv", FileSHA256: fp("file1"), UploadedBy: admin.ID,
	}, first)
	if err != nil {
		t.Fatal(err)
	}
	if again.Kept != 0 || again.Ignored != 0 || again.Duplicates != 4 {
		t.Errorf("re-import counts = %+v", again)
	}

	// An overlapping file: two old lines, one new.
	overlap, err := db.SaveBankImport(ctx, store.BankImport{
		FormatID: f.ID, FileName: "October.csv", FileSHA256: fp("file2"), UploadedBy: admin.ID,
	}, []store.BankLine{line(2, "b", 120000), line(3, "c", 5000), line(4, "d", 2500)})
	if err != nil {
		t.Fatal(err)
	}
	if overlap.Kept != 1 || overlap.Duplicates != 2 {
		t.Errorf("overlap counts = %+v", overlap)
	}
	if l, _ := db.ListBankLines(ctx, overlap.ID); len(l) != 1 || l[0].Fingerprint != fp("d") {
		t.Errorf("overlap kept lines = %+v", l)
	}

	// A fingerprint repeated within one call is stored once.
	dup, err := db.SaveBankImport(ctx, store.BankImport{
		FormatID: f.ID, FileName: "x.csv", FileSHA256: fp("file3"), UploadedBy: admin.ID,
	}, []store.BankLine{line(2, "e", 100), line(3, "e", 100)})
	if err != nil || dup.Kept != 1 || dup.Duplicates != 1 {
		t.Errorf("in-call duplicate: %+v, %v", dup, err)
	}

	list, err := db.ListBankImports(ctx, 10)
	if err != nil || len(list) != 4 || list[0].ID != dup.ID {
		t.Errorf("recent imports = %d (first %d), %v", len(list), list[0].ID, err)
	}
	if l, _ := db.ListBankImports(ctx, 2); len(l) != 2 {
		t.Errorf("limit not applied: %d", len(l))
	}
}

// A line that breaks a schema rule fails the whole import: no import row and
// no partial lines are left behind.
func TestSaveBankImportIsAtomic(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	admin := mustAdmin(t, db, "admin@example.com")
	f := mustFormat(t, db, admin.ID, "h")

	bad := line(3, "neg", -5) // amounts must be positive
	_, err := db.SaveBankImport(ctx, store.BankImport{
		FormatID: f.ID, FileName: "x.csv", FileSHA256: fp("x"), UploadedBy: admin.ID,
	}, []store.BankLine{line(2, "ok", 100), bad})
	if err == nil {
		t.Fatal("negative amount accepted")
	}
	if list, _ := db.ListBankImports(ctx, 10); len(list) != 0 {
		t.Errorf("%d imports left behind", len(list))
	}
	// The good line was rolled back with it, so it imports cleanly later.
	imp, err := db.SaveBankImport(ctx, store.BankImport{
		FormatID: f.ID, FileName: "x.csv", FileSHA256: fp("x"), UploadedBy: admin.ID,
	}, []store.BankLine{line(2, "ok", 100)})
	if err != nil || imp.Kept != 1 {
		t.Errorf("retry: %+v, %v", imp, err)
	}

	for name, l := range map[string]store.BankLine{
		"bad state":  func() store.BankLine { x := line(2, "s", 1); x.State = "weird"; return x }(),
		"bad date":   func() store.BankLine { x := line(2, "t", 1); x.PostedOn = "9/1/2026"; return x }(),
		"header row": func() store.BankLine { x := line(1, "u", 1); return x }(),
	} {
		if _, err := db.SaveBankImport(ctx, store.BankImport{
			FormatID: f.ID, FileName: "y.csv", FileSHA256: fp("y"), UploadedBy: admin.ID,
		}, []store.BankLine{l}); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if _, err := db.SaveBankImport(ctx, store.BankImport{
		FormatID: 99999, FileName: "z.csv", FileSHA256: fp("z"), UploadedBy: admin.ID,
	}, nil); err == nil {
		t.Error("import with an unknown format accepted")
	}
}

// Overlapping files uploaded at the same moment: every distinct line is stored
// exactly once, and the counts add up across the imports. On MariaDB this
// exercises the unique-key wait, and the deadlock retry when two files list
// the same lines in opposite orders.
func TestSaveBankImportConcurrent(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	admin := mustAdmin(t, db, "admin@example.com")
	f := mustFormat(t, db, admin.ID, "h")

	var forward, backward []store.BankLine
	for i := 0; i < 40; i++ {
		forward = append(forward, line(i+2, fmt.Sprint("k", i), money.Cents(100+i)))
	}
	for i := len(forward) - 1; i >= 0; i-- {
		backward = append(backward, forward[i])
	}

	const uploads = 6
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		results []store.BankImport
		errs    []error
	)
	for u := 0; u < uploads; u++ {
		wg.Add(1)
		go func(u int) {
			defer wg.Done()
			lines := forward
			if u%2 == 1 {
				lines = backward
			}
			imp, err := db.SaveBankImport(ctx, store.BankImport{
				FormatID: f.ID, FileName: fmt.Sprint("f", u, ".csv"), FileSHA256: fp(fmt.Sprint("f", u)),
				UploadedBy: admin.ID,
			}, lines)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err)
				return
			}
			results = append(results, imp)
		}(u)
	}
	wg.Wait()
	for _, err := range errs {
		t.Errorf("concurrent import: %v", err)
	}

	kept, dups := 0, 0
	for _, r := range results {
		kept += r.Kept
		dups += r.Duplicates
	}
	if kept != len(forward) || kept+dups != len(forward)*len(results) {
		t.Errorf("kept %d (want %d), duplicates %d across %d imports", kept, len(forward), dups, len(results))
	}
	var n int
	if err := db.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM bank_lines`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != len(forward) {
		t.Errorf("%d lines stored, want %d", n, len(forward))
	}
}
