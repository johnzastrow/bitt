package bankcsv

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/johnzastrow/bitt/internal/money"
)

func mustRead(t *testing.T, data string) File {
	t.Helper()
	f, err := Read([]byte(data))
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	return f
}

func reference(t *testing.T) File {
	t.Helper()
	data, err := os.ReadFile("testdata/reference.csv")
	if err != nil {
		t.Fatal(err)
	}
	f, err := Read(data)
	if err != nil {
		t.Fatalf("Read reference: %v", err)
	}
	return f
}

func date(s string) time.Time {
	d, err := time.Parse("2006-01-02", s)
	if err != nil {
		panic(err)
	}
	return d
}

// Exit criterion: the synthetic file in the reference layout parses exactly.
func TestReferenceLayout(t *testing.T) {
	f := reference(t)
	if f.Separator != ',' || f.Encoding != "utf-8" {
		t.Errorf("separator %q encoding %s", f.Separator, f.Encoding)
	}
	if len(f.Records) != 10 {
		t.Fatalf("%d records, want 10", len(f.Records))
	}

	m := Guess(f)
	want := Mapping{
		Date: 1, DateLayout: "m/d/yyyy", Amount: 5, Debit: None, Credit: None,
		Description: 2, Account: 0, Note: 3, Reference: 4,
	}
	if m != want {
		t.Fatalf("Guess = %+v\nwant    %+v", m, want)
	}
	if err := m.Validate(len(f.Header)); err != nil {
		t.Fatal(err)
	}

	res := Apply(f, m)
	if len(res.Refused) != 0 {
		t.Fatalf("refused: %+v", res.Refused)
	}
	// ($336.41) and ($1,500.00) are outgoing and dropped.
	if res.Outgoing != 2 {
		t.Errorf("outgoing = %d, want 2", res.Outgoing)
	}
	if len(res.Incoming) != 8 {
		t.Fatalf("%d incoming, want 8", len(res.Incoming))
	}

	first := res.Incoming[0]
	if first.Row != 2 || first.Account != "PRIMARY SHARE" || !first.PostedOn.Equal(date("2026-09-01")) ||
		first.Amount != 15000 || first.Description != "Deposit Transfer\nFROM SAM PAYEE X1234" {
		t.Errorf("first line = %+v", first)
	}
	// The original row text is kept exactly, both lines of the description
	// included, without the line ending.
	if first.Raw != "PRIMARY SHARE,9/1/2026,\"Deposit Transfer\nFROM SAM PAYEE X1234\",,,$150.00,\"$3,239.01\"" {
		t.Errorf("raw = %q", first.Raw)
	}

	// "$1,200.00": thousands separator stripped. Row 4: the outgoing row 3 in
	// between still counts as a row.
	if l := res.Incoming[1]; l.Amount != 120000 || l.Row != 4 {
		t.Errorf("thousands line = %+v", l)
	}
	// Dividends are real incoming money; RECON-03's minimum and ignore rules
	// deal with them, not the parser.
	if l := res.Incoming[2]; l.Amount != 4 || !strings.HasPrefix(l.Description, "Deposit Dividend") {
		t.Errorf("dividend line = %+v", l)
	}
	// The note is kept verbatim, comma and line break included.
	if l := res.Incoming[3]; l.Note != "Phone plan, September\nsplit with Ann" {
		t.Errorf("note = %q", l.Note)
	}
	// Check # is the reference.
	if l := res.Incoming[6]; l.Reference != "1042" || l.Amount != 7525 {
		t.Errorf("check line = %+v", l)
	}
	if l := res.Incoming[5]; l.Account != "SECONDARY SAVINGS" {
		t.Errorf("account = %q", l.Account)
	}
}

func TestReadRowsAndRaw(t *testing.T) {
	// LF endings, a blank line, and a quoted field with an embedded quote.
	f := mustRead(t, "Date,Description,Amount\n9/1/2026,\"He said \"\"hi\"\"\",5.00\n\n9/2/2026,Plain,6.00\n")
	if len(f.Records) != 2 {
		t.Fatalf("%d records", len(f.Records))
	}
	if f.Records[0].Row != 2 || f.Records[1].Row != 3 {
		t.Errorf("rows = %d, %d", f.Records[0].Row, f.Records[1].Row)
	}
	if f.Records[0].Raw != `9/1/2026,"He said ""hi""",5.00` || f.Records[0].Fields[1] != `He said "hi"` {
		t.Errorf("record 0 = %+v", f.Records[0])
	}
	if f.Records[1].Raw != "9/2/2026,Plain,6.00" {
		t.Errorf("raw after a blank line = %q", f.Records[1].Raw)
	}
	// No trailing newline at end of file.
	f = mustRead(t, "Date,Description,Amount\r\n9/1/2026,X,5.00")
	if f.Records[0].Raw != "9/1/2026,X,5.00" {
		t.Errorf("raw at EOF = %q", f.Records[0].Raw)
	}
}

func TestReadEncodings(t *testing.T) {
	// UTF-8 with a byte-order mark: the BOM must not leak into the header
	// (it would change the signature and the "date" column name).
	f := mustRead(t, "\xEF\xBB\xBFDate,Description,Amount\n9/1/2026,Café,5.00\n")
	if f.Header[0] != "Date" || f.Encoding != "utf-8" || f.Records[0].Fields[1] != "Café" {
		t.Errorf("BOM file: header %q enc %s field %q", f.Header[0], f.Encoding, f.Records[0].Fields[1])
	}
	if f.Signature != Signature([]string{"Date", "Description", "Amount"}) {
		t.Error("BOM changed the signature")
	}

	// Windows-1252: é is 0xE9, the euro sign 0x80, a curly quote 0x92.
	f = mustRead(t, "Date,Description,Amount\n9/1/2026,Caf\xe9 \x80 O\x92Neil,5.00\n")
	if f.Encoding != "windows-1252" || f.Records[0].Fields[1] != "Café € O’Neil" {
		t.Errorf("1252: enc %s field %q", f.Encoding, f.Records[0].Fields[1])
	}

	// A byte undefined in Windows-1252 means some other encoding: refused.
	if _, err := Read([]byte("Date,Description,Amount\n9/1/2026,X\x81,5.00\n")); !errors.Is(err, ErrRefused) {
		t.Errorf("undefined 1252 byte: %v", err)
	}
}

func TestReadSemicolon(t *testing.T) {
	f := mustRead(t, "Datum;Omschrijving;Bedrag\n01.09.2026;\"Huur; september\";1.234,56\n")
	if f.Separator != ';' || f.Records[0].Fields[1] != "Huur; september" {
		t.Fatalf("sep %q fields %q", f.Separator, f.Records[0].Fields)
	}
	m := Guess(f)
	if !m.DecimalComma {
		t.Error("semicolon file not guessed as decimal comma")
	}
	m.Date, m.DateLayout, m.Description, m.Amount = 0, "d.m.yyyy", 1, 2
	res := Apply(f, m)
	if len(res.Incoming) != 1 || res.Incoming[0].Amount != 123456 || !res.Incoming[0].PostedOn.Equal(date("2026-09-01")) {
		t.Errorf("semicolon line = %+v refused %+v", res.Incoming, res.Refused)
	}
	// A comma inside a quoted header field does not fool the detector.
	f = mustRead(t, "\"Date, posted\";Description;Amount\n9/1/2026;X;5\n")
	if f.Separator != ';' {
		t.Errorf("separator = %q", f.Separator)
	}
}

func TestReadRefusals(t *testing.T) {
	big := "Date,Description,Amount\n" + strings.Repeat("9/1/2026,X,1.00\n", MaxBytes/16+1)
	tooMany := "Date,Description,Amount\n" + strings.Repeat("9/1/2026,X,1.00\n", MaxRows+1)
	cases := map[string]string{
		"empty":         "",
		"whitespace":    " \n\n",
		"binary":        "Date,Amount\n\x00\x01",
		"stray quote":   "Date,Description,Amount\n9/1/2026,Bad \"quote,5\n",
		"unclosed":      "Date,Description,Amount\n9/1/2026,\"never closed,5\n",
		"one column":    "Just a title line\n1\n",
		"over 5 MB":     big,
		"over the rows": tooMany,
	}
	for name, data := range cases {
		if _, err := Read([]byte(data)); !errors.Is(err, ErrRefused) {
			t.Errorf("%s: err = %v, want ErrRefused", name, err)
		}
	}
	// Exactly MaxRows is accepted.
	exact := "Date,Description,Amount\n" + strings.Repeat("9/1/2026,X,1.00\n", MaxRows)
	if f, err := Read([]byte(exact)); err != nil || len(f.Records) != MaxRows {
		t.Errorf("exactly MaxRows: %v, %d", err, len(f.Records))
	}
}

// A row with the wrong number of fields is refused on its own; the rest of
// the file still reads.
func TestWrongFieldCountRefusesOnlyThatRow(t *testing.T) {
	f := mustRead(t, "Date,Description,Amount\n9/1/2026,A,5.00\n9/2/2026,B,6.00,extra\n9/3/2026,C,7.00\n")
	res := Apply(f, Mapping{Date: 0, DateLayout: "m/d/yyyy", Description: 1, Amount: 2,
		Debit: None, Credit: None, Account: None, Note: None, Reference: None})
	if len(res.Incoming) != 2 || len(res.Refused) != 1 || res.Refused[0].Row != 3 {
		t.Errorf("incoming %d refused %+v", len(res.Incoming), res.Refused)
	}
	if !strings.Contains(res.Refused[0].Reason, "4 fields") {
		t.Errorf("reason = %q", res.Refused[0].Reason)
	}
}

func TestParseAmount(t *testing.T) {
	ok := []struct {
		in    string
		comma bool
		want  money.Cents
	}{
		{"$150.00", false, 15000},
		{"$3,239.01", false, 323901},
		{"($336.41)", false, -33641},
		{"($1,500.00)", false, -150000},
		{"-12.5", false, -1250},
		{"+7", false, 700},
		{"-$5.00", false, -500},
		{"$-5.00", false, -500},
		{" 0.04 ", false, 4},
		{"€1.234,56", true, 123456},
		{"-1.234,5", true, -123450},
		{"(12,00)", true, -1200},
		{"1 234,56", true, 123456},
		{"1 234.56", false, 123456},
		{"£0.07", false, 7},
		{"0", false, 0},
	}
	for _, c := range ok {
		got, err := ParseAmount(c.in, c.comma)
		if err != nil || got != c.want {
			t.Errorf("ParseAmount(%q, %v) = %d, %v; want %d", c.in, c.comma, got, err, c.want)
		}
	}
	bad := []struct {
		in    string
		comma bool
	}{
		{"", false}, {"abc", false}, {"1.234", false}, // three decimals
		{"(-5)", false}, {"--5", false}, {"5-", false}, {"1.2.3", false},
		{"$", false}, {"()", false}, {"1e3", false}, {"=1+1", false},
		{"12,345", true}, // three decimals under decimal comma
	}
	for _, c := range bad {
		if got, err := ParseAmount(c.in, c.comma); err == nil {
			t.Errorf("ParseAmount(%q, %v) = %d, want an error", c.in, c.comma, got)
		}
	}
}

func TestParseDate(t *testing.T) {
	ok := map[[2]string]string{
		{"9/1/2026", "m/d/yyyy"}:             "2026-09-01",
		{"09/01/2026", "m/d/yyyy"}:           "2026-09-01",
		{"9/1/2026 12:00:00 AM", "m/d/yyyy"}: "2026-09-01",
		{"1/9/2026", "d/m/yyyy"}:             "2026-09-01",
		{"2026-09-01", "yyyy-mm-dd"}:         "2026-09-01",
		{"1.9.2026", "d.m.yyyy"}:             "2026-09-01",
		{"9/1/26", "m/d/yy"}:                 "2026-09-01",
		{"9-1-2026", "m-d-yyyy"}:             "2026-09-01",
	}
	for in, want := range ok {
		got, err := ParseDate(in[0], in[1])
		if err != nil || got.Format("2006-01-02") != want {
			t.Errorf("ParseDate(%q, %s) = %v, %v; want %s", in[0], in[1], got, err, want)
		}
	}
	bad := [][2]string{
		{"", "m/d/yyyy"}, {"13/1/2026", "m/d/yyyy"}, {"2/30/2026", "m/d/yyyy"},
		{"9/1/26", "m/d/yyyy"}, {"9/1/2026", "m/d/yy"}, {"9/1/1890", "m/d/yyyy"},
		{"9/1/2026", "nonsense"}, {"tomorrow", "m/d/yyyy"},
	}
	for _, in := range bad {
		if got, err := ParseDate(in[0], in[1]); err == nil {
			t.Errorf("ParseDate(%q, %s) = %v, want an error", in[0], in[1], got)
		}
	}
}

func TestDebitCreditColumns(t *testing.T) {
	f := mustRead(t, "Date,Payee,Debit,Credit\n"+
		"2026-09-01,A,,50.00\n"+ // in
		"2026-09-02,B,20.00,\n"+ // out
		"2026-09-03,C,-20.00,\n"+ // out, sign ignored: the column decides
		"2026-09-04,D,,(30.00)\n"+ // in, parentheses ignored likewise
		"2026-09-05,E,0.00,40.00\n"+ // in, zero debit
		"2026-09-06,F,10.00,40.00\n"+ // both: refused
		"2026-09-07,G,,\n") // neither: refused
	m := Guess(f)
	if m.Amount != None || m.Debit != 2 || m.Credit != 3 || m.Description != 1 || m.DateLayout != "yyyy-mm-dd" {
		t.Fatalf("guess = %+v", m)
	}
	res := Apply(f, m)
	var got []money.Cents
	for _, l := range res.Incoming {
		got = append(got, l.Amount)
	}
	if len(got) != 3 || got[0] != 5000 || got[1] != 3000 || got[2] != 4000 {
		t.Errorf("incoming = %v", got)
	}
	if res.Outgoing != 2 || len(res.Refused) != 2 {
		t.Errorf("outgoing %d refused %+v", res.Outgoing, res.Refused)
	}
}

func TestIncomingNegative(t *testing.T) {
	// Some exports write money received as negative.
	f := mustRead(t, "Date,Description,Amount\n9/1/2026,In,-50.00\n9/2/2026,Out,25.00\n")
	m := Guess(f)
	m.IncomingNegative = true
	res := Apply(f, m)
	if len(res.Incoming) != 1 || res.Incoming[0].Amount != 5000 || res.Outgoing != 1 {
		t.Errorf("res = %+v", res)
	}
}

func TestBadCellsAreRefusedRows(t *testing.T) {
	f := mustRead(t, "Date,Description,Amount\nnot a date,A,5\n9/2/2026,B,five\n9/3/2026,C,1.234\n9/4/2026,D,\n9/5/2026,E,5\n")
	res := Apply(f, Guess(f))
	if len(res.Incoming) != 1 || len(res.Refused) != 4 {
		t.Fatalf("incoming %d refused %+v", len(res.Incoming), res.Refused)
	}
	for i, want := range []string{"date", "not a number", "two decimal", "no amount"} {
		if !strings.Contains(res.Refused[i].Reason, want) {
			t.Errorf("refusal %d = %q, want it to mention %q", i, res.Refused[i].Reason, want)
		}
	}
}

func TestValidate(t *testing.T) {
	base := Mapping{Date: 0, DateLayout: "m/d/yyyy", Description: 1, Amount: 2,
		Debit: None, Credit: None, Account: None, Note: None, Reference: None}
	if err := base.Validate(3); err != nil {
		t.Fatalf("base: %v", err)
	}
	mut := []func(*Mapping){
		func(m *Mapping) { m.Date = None },
		func(m *Mapping) { m.Date = 3 },
		func(m *Mapping) { m.DateLayout = "" },
		func(m *Mapping) { m.Description = None },
		func(m *Mapping) { m.Amount = 7 },
		func(m *Mapping) { m.Debit = 0 }, // amount and debit both
		func(m *Mapping) { m.Amount = None },
		func(m *Mapping) { m.Amount, m.Debit, m.Credit = None, 2, 2 },
		func(m *Mapping) { m.Note = 9 },
		func(m *Mapping) { m.Account = -2 },
	}
	for i, f := range mut {
		m := base
		f(&m)
		if err := m.Validate(3); err == nil {
			t.Errorf("mutation %d accepted: %+v", i, m)
		}
	}
}

func TestSignature(t *testing.T) {
	a := Signature([]string{"Date", " Amount ", "DESCRIPTION"})
	if a != Signature([]string{"date", "amount", "description"}) {
		t.Error("signature is not case- and space-insensitive")
	}
	if a == Signature([]string{"amount", "date", "description"}) {
		t.Error("column order does not change the signature")
	}
	// Joining must not be ambiguous: ["a,b"] is not ["a","b"].
	if Signature([]string{"a,b", "c"}) == Signature([]string{"a", "b,c"}) {
		t.Error("signature collides across a cell boundary")
	}
}

func TestFingerprints(t *testing.T) {
	f := reference(t)
	res := Apply(f, Guess(f))
	fps := Fingerprints(1, res.Incoming)

	seen := map[string]bool{}
	for i, fp := range fps {
		if seen[fp] {
			t.Errorf("line %d shares a fingerprint", i)
		}
		seen[fp] = true
	}
	// The two identical $50 lines of 9/5 in PRIMARY SHARE (one with a note)
	// are two lines; the note is not part of the key, so the occurrence
	// number is what keeps them apart.
	// Re-reading the same lines gives the same fingerprints.
	again := Fingerprints(1, Apply(f, Guess(f)).Incoming)
	for i := range fps {
		if fps[i] != again[i] {
			t.Fatalf("fingerprint %d not stable", i)
		}
	}
	// A different layout id gives different fingerprints.
	if Fingerprints(2, res.Incoming)[0] == fps[0] {
		t.Error("layout id is not part of the fingerprint")
	}

	// An overlapping later file (the 9/5 lines onward, description spacing
	// changed) matches the same fingerprints for the shared lines.
	overlap := []Line{res.Incoming[3], res.Incoming[4], res.Incoming[5]}
	overlap[0].Description = "deposit   transfer FROM SAM PAYEE x1234"
	ofp := Fingerprints(1, overlap)
	for i := range ofp {
		if ofp[i] != fps[3+i] {
			t.Errorf("overlap line %d not recognised", i)
		}
	}
}

func TestGuessPrefersEarlierNames(t *testing.T) {
	f := mustRead(t, "Memo,Description,Posted Date,Transaction Date,Amount\nm,d,9/1/2026,9/2/2026,5\n")
	m := Guess(f)
	if m.Description != 1 || m.Date != 2 {
		t.Errorf("guess = %+v (want description col 1, date col 2: posted date ranks above transaction date)", m)
	}
	// D/M dates with a day above 12 are guessed as D/M.
	f = mustRead(t, "Date,Description,Amount\n25/9/2026,X,5\n")
	if Guess(f).DateLayout != "d/m/yyyy" {
		t.Errorf("layout = %s", Guess(f).DateLayout)
	}
}

func TestOverlongRowRefused(t *testing.T) {
	f := mustRead(t, "Date,Description,Amount\n9/1/2026,"+strings.Repeat("x", MaxRowBytes)+",5\n9/2/2026,ok,6\n")
	res := Apply(f, Guess(f))
	if len(res.Incoming) != 1 || len(res.Refused) != 1 || res.Refused[0].Row != 2 {
		t.Errorf("incoming %d refused %+v", len(res.Incoming), res.Refused)
	}
}

func TestPreview(t *testing.T) {
	f := reference(t)
	m := Guess(f)
	p := Preview(f, m, 3)
	if len(p) != 3 {
		t.Fatalf("%d preview rows", len(p))
	}
	if p[0].Outcome != "in" || p[0].Line.Amount != 15000 || p[0].Row != 2 {
		t.Errorf("row 2 = %+v", p[0])
	}
	if p[1].Outcome != "out" || p[1].Row != 3 {
		t.Errorf("row 3 = %+v", p[1])
	}
	// A wrong date layout refuses rows with the reason, which is what makes
	// the preview useful before saving.
	m.DateLayout = "yyyy-mm-dd"
	p = Preview(f, m, 10)
	if len(p) != 10 || p[0].Outcome != "refused" || !strings.Contains(p[0].Reason, "YYYY-MM-DD") {
		t.Errorf("wrong layout row = %+v", p[0])
	}
	// Fewer rows than asked.
	small := mustRead(t, "Date,Description,Amount\n9/1/2026,A,5\n")
	if got := Preview(small, Guess(small), 10); len(got) != 1 || got[0].Outcome != "in" {
		t.Errorf("small preview = %+v", got)
	}
	// A mapping for a wider file refuses every row instead of panicking.
	wide := Guess(f)
	if got := Preview(small, wide, 10); len(got) != 1 || got[0].Outcome != "refused" {
		t.Errorf("mismatched preview = %+v", got)
	}
	if res := Apply(small, wide); len(res.Refused) != 1 || len(res.Incoming) != 0 {
		t.Errorf("mismatched apply = %+v", res)
	}
}
