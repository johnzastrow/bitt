package reconcile

import (
	"fmt"
	"testing"
	"time"

	"github.com/johnzastrow/bitt/internal/money"
)

func day(d int) time.Time { return time.Date(2026, 9, d, 0, 0, 0, 0, time.UTC) }

func ln(id int64, d int, cents money.Cents, text string) Line {
	return Line{ID: id, PostedOn: day(d), Amount: cents, Text: text}
}

func pay(seq int64, d int, cents money.Cents, method string, names ...string) Payment {
	return Payment{Seq: seq, TabID: seq * 10, EffectiveOn: day(d), Amount: cents, Method: method, Names: names}
}

func pairsOf(r Result) string {
	s := ""
	for _, sg := range r.Suggestions {
		s += fmt.Sprintf("[%d~%d]", sg.LineID, sg.PaymentSeq)
	}
	return s
}

func TestDefaultsAreValid(t *testing.T) {
	if err := Defaults().Validate(); err != nil {
		t.Fatal(err)
	}
	d := Defaults()
	if d.DateWindowDays != 7 || d.TolerancePercent != 10 || d.ToleranceCap != 2500 ||
		d.ToleranceFloor != 100 || d.MinLineAmount != 100 || !d.UseNames || d.DateFlagDays != 3 {
		t.Errorf("defaults = %+v", d)
	}
}

func TestValidateRanges(t *testing.T) {
	bad := []func(*Settings){
		func(s *Settings) { s.DateWindowDays = -1 },
		func(s *Settings) { s.DateWindowDays = 61 },
		func(s *Settings) { s.TolerancePercent = 101 },
		func(s *Settings) { s.TolerancePercent = -1 },
		func(s *Settings) { s.ToleranceCap = 1_000_001 },
		func(s *Settings) { s.ToleranceFloor = 10_001 },
		func(s *Settings) { s.ToleranceFloor = -1 },
		func(s *Settings) { s.ToleranceCap, s.ToleranceFloor = 50, 100 },
		func(s *Settings) { s.MinLineAmount = 1_000_001 },
		func(s *Settings) { s.DateFlagDays = 61 },
	}
	for i, f := range bad {
		s := Defaults()
		f(&s)
		if s.Validate() == nil {
			t.Errorf("case %d accepted: %+v", i, s)
		}
	}
	edges := []Settings{
		{DateWindowDays: 0, TolerancePercent: 0, ToleranceCap: 0, ToleranceFloor: 0, MinLineAmount: 0, DateFlagDays: 0},
		{DateWindowDays: 60, TolerancePercent: 100, ToleranceCap: 1_000_000, ToleranceFloor: 10_000, MinLineAmount: 1_000_000, DateFlagDays: 60},
	}
	for _, s := range edges {
		if err := s.Validate(); err != nil {
			t.Errorf("edge %+v refused: %v", s, err)
		}
	}
}

// The tolerance is the percent of the recorded amount, held between the floor
// and the cap.
func TestTolerance(t *testing.T) {
	s := Defaults() // 10%, floor $1, cap $25
	cases := map[money.Cents]money.Cents{
		500:     100,  // 10% of $5 is 50c, raised to the $1 floor
		1000:    100,  // exactly the floor
		15000:   1500, // 10% of $150
		25000:   2500, // exactly the cap
		120000:  2500, // 10% of $1,200 is $120, held to the $25 cap
		0:       100,
		1999999: 2500,
	}
	for rec, want := range cases {
		if got := s.Tolerance(rec); got != want {
			t.Errorf("Tolerance(%d) = %d, want %d", rec, got, want)
		}
	}
	s.TolerancePercent = 0
	if got := s.Tolerance(15000); got != 100 {
		t.Errorf("0%% still has the floor: %d", got)
	}
	// Rounds down: 10% of $0.99... uses integer cents.
	s = Defaults()
	s.ToleranceFloor = 0
	if got := s.Tolerance(999); got != 99 {
		t.Errorf("Tolerance(999) = %d, want 99", got)
	}
}

// Exit criterion: exact, within tolerance, outside tolerance (not suggested).
func TestExactWithinOutside(t *testing.T) {
	s := Defaults()
	payments := []Payment{pay(1, 1, 15000, "transfer")}

	r := Suggest([]Line{ln(10, 1, 15000, "")}, payments, s)
	if pairsOf(r) != "[10~1]" || !r.Suggestions[0].Exact || r.Suggestions[0].AmountDiff != 0 {
		t.Errorf("exact: %+v", r)
	}
	r = Suggest([]Line{ln(10, 3, 16000, "")}, payments, s) // $10 over, tol $15
	if pairsOf(r) != "[10~1]" || r.Suggestions[0].Exact || r.Suggestions[0].AmountDiff != 1000 ||
		r.Suggestions[0].DateDiff != 2 {
		t.Errorf("within: %+v", r)
	}
	r = Suggest([]Line{ln(10, 1, 13400, "")}, payments, s) // $16 under
	if len(r.Suggestions) != 0 || len(r.UnmatchedLines) != 1 || len(r.UnmatchedPayments) != 1 {
		t.Errorf("outside: %+v", r)
	}
	// Exactly at the tolerance is inside.
	r = Suggest([]Line{ln(10, 1, 16500, "")}, payments, s)
	if pairsOf(r) != "[10~1]" {
		t.Errorf("at tolerance: %+v", r)
	}
}

// Exit criterion: a tie is shown, not guessed.
func TestTieIsNotGuessed(t *testing.T) {
	s := Defaults()
	// One $50 line, two $50 payments on different tabs, same date, same method.
	r := Suggest([]Line{ln(10, 5, 5000, "Deposit Transfer")},
		[]Payment{pay(1, 5, 5000, "transfer"), pay(2, 5, 5000, "transfer")}, s)
	if len(r.Suggestions) != 0 || len(r.Ties) != 1 {
		t.Fatalf("result = %+v", r)
	}
	tie := r.Ties[0]
	if fmt.Sprint(tie.LineIDs, tie.PaymentSeqs) != "[10] [1 2]" || len(tie.Pairs) != 2 {
		t.Errorf("tie = %+v", tie)
	}
	if len(r.UnmatchedLines) != 0 || len(r.UnmatchedPayments) != 0 {
		t.Errorf("tied items reported as unmatched: %+v", r)
	}

	// Two identical lines and two identical payments: one group of four pairs.
	r = Suggest([]Line{ln(10, 5, 5000, ""), ln(11, 5, 5000, "")},
		[]Payment{pay(1, 5, 5000, "transfer"), pay(2, 5, 5000, "transfer")}, s)
	if len(r.Ties) != 1 || len(r.Ties[0].Pairs) != 4 || len(r.Suggestions) != 0 {
		t.Errorf("2x2 = %+v", r)
	}

	// A name breaks the tie: Sam's tab wins outright.
	r = Suggest([]Line{ln(10, 5, 5000, "Deposit Transfer\nFROM SAM PAYEE X1234")},
		[]Payment{pay(1, 5, 5000, "transfer", "Phone plan", "Jane Provider", "Sam Payee"),
			pay(2, 5, 5000, "transfer", "Insurance", "Jane Provider", "Alex Other")}, s)
	if pairsOf(r) != "[10~1]" || !r.Suggestions[0].NameHit || len(r.Ties) != 0 {
		t.Errorf("name tiebreak = %+v", r)
	}
}

// A lower-scored pair is not blocked by an unrelated tie, and a tie does not
// swallow items that are not connected to it.
func TestTieDoesNotSpread(t *testing.T) {
	s := Defaults()
	r := Suggest(
		[]Line{ln(10, 5, 5000, ""), ln(20, 9, 9900, "")},
		[]Payment{pay(1, 5, 5000, "transfer"), pay(2, 5, 5000, "transfer"), pay(3, 9, 9900, "cash")},
		s)
	if pairsOf(r) != "[20~3]" || len(r.Ties) != 1 || fmt.Sprint(r.Ties[0].PaymentSeqs) != "[1 2]" {
		t.Errorf("result = %+v", r)
	}
}

// Best score first, each line and payment used once: the closer pair wins even
// when the other line comes first in the input.
func TestGreedyBestFirst(t *testing.T) {
	s := Defaults()
	lines := []Line{ln(10, 6, 5000, ""), ln(11, 1, 5000, "")}
	payments := []Payment{pay(1, 1, 5000, "transfer"), pay(2, 7, 5000, "transfer")}
	r := Suggest(lines, payments, s)
	if pairsOf(r) != "[10~2][11~1]" && pairsOf(r) != "[11~1][10~2]" {
		t.Errorf("pairs = %s", pairsOf(r))
	}
	// An exact amount beats a near one even with a worse date.
	r = Suggest([]Line{ln(10, 1, 5000, "")},
		[]Payment{pay(1, 7, 5000, "cash"), pay(2, 1, 5100, "transfer")}, s)
	if pairsOf(r) != "[10~1]" {
		t.Errorf("exact over near: %s %+v", pairsOf(r), r.Suggestions)
	}
	if len(r.UnmatchedPayments) != 1 || r.UnmatchedPayments[0] != 2 {
		t.Errorf("unmatched payments = %v", r.UnmatchedPayments)
	}
}

// Exit criterion: each setup control changes suggestions as stated.
func TestControlsChangeSuggestions(t *testing.T) {
	base := Defaults()

	// Date window: 7 days matches, 3 does not, 0 only the same day.
	l := []Line{ln(10, 8, 5000, "")}
	p := []Payment{pay(1, 1, 5000, "transfer")}
	if pairsOf(Suggest(l, p, base)) != "[10~1]" {
		t.Error("7-day window missed a 7-day gap")
	}
	s := base
	s.DateWindowDays = 3
	if pairsOf(Suggest(l, p, s)) != "" {
		t.Error("3-day window matched a 7-day gap")
	}
	s.DateWindowDays = 0
	if pairsOf(Suggest([]Line{ln(10, 1, 5000, "")}, p, s)) != "[10~1]" {
		t.Error("0-day window missed the same day")
	}

	// Percent between floor and cap: $150 recorded, $162 in the bank.
	l = []Line{ln(10, 1, 16200, "")}
	p = []Payment{pay(1, 1, 15000, "transfer")}
	if pairsOf(Suggest(l, p, base)) != "[10~1]" { // 10% = $15
		t.Error("10% missed a $12 difference")
	}
	s = base
	s.TolerancePercent = 5 // $7.50
	if pairsOf(Suggest(l, p, s)) != "" {
		t.Error("5% matched a $12 difference")
	}
	s.ToleranceFloor = 1200 // the floor lifts it back to $12
	s.ToleranceCap = 2500
	if pairsOf(Suggest(l, p, s)) != "[10~1]" {
		t.Error("the floor did not apply")
	}
	s = base
	s.TolerancePercent, s.ToleranceCap = 100, 1000 // cap $10 holds 100% down
	if pairsOf(Suggest(l, p, s)) != "" {
		t.Error("the cap did not apply")
	}

	// Minimum line amount: the dividend.
	l = []Line{ln(10, 3, 4, "Deposit Dividend")}
	p = []Payment{pay(1, 3, 4, "transfer")}
	r := Suggest(l, p, base)
	if pairsOf(r) != "" || fmt.Sprint(r.Small) != "[10]" || len(r.UnmatchedLines) != 0 {
		t.Errorf("minimum: %+v", r)
	}
	s = base
	s.MinLineAmount = 0
	if pairsOf(Suggest(l, p, s)) != "[10~1]" {
		t.Error("minimum 0 still hid the line")
	}

	// Names on and off: on, the name breaks a tie; off, it is a tie again.
	l = []Line{ln(10, 5, 5000, "FROM SAM PAYEE")}
	p = []Payment{pay(1, 5, 5000, "transfer", "Sam Payee"), pay(2, 5, 5000, "transfer", "Alex")}
	if pairsOf(Suggest(l, p, base)) != "[10~1]" {
		t.Error("names on did not pick Sam")
	}
	s = base
	s.UseNames = false
	if r := Suggest(l, p, s); len(r.Ties) != 1 || r.Suggestions != nil {
		t.Errorf("names off: %+v", r)
	}
}

func TestNameHit(t *testing.T) {
	cases := []struct {
		text  string
		names []string
		want  bool
	}{
		{"Deposit Transfer\nFROM SAM PAYEE X1234", []string{"Sam Payee"}, true},
		{"from sam", []string{"Sam"}, true},
		{"from samantha", []string{"Sam"}, false}, // whole words only
		{"from al", []string{"Al Smith"}, false},  // "al" is too short; smith absent
		{"ZELLE FROM O'NEIL", []string{"Pat O'Neil"}, true},
		{"phone plan sept", []string{"Phone plan"}, true},
		{"", []string{"Sam"}, false},
		{"Café payment from José", []string{"José García"}, true},
	}
	for _, c := range cases {
		if got := nameHit(c.text, c.names); got != c.want {
			t.Errorf("nameHit(%q, %v) = %v", c.text, c.names, got)
		}
	}
}

func TestMethodAndDateScoring(t *testing.T) {
	s := Defaults()
	transfer, _ := Score(ln(1, 1, 5000, ""), pay(1, 1, 5000, "transfer"), s)
	cash, _ := Score(ln(1, 1, 5000, ""), pay(1, 1, 5000, "cash"), s)
	other, _ := Score(ln(1, 1, 5000, ""), pay(1, 1, 5000, "other"), s)
	if !(transfer.Score > other.Score && other.Score > cash.Score) {
		t.Errorf("method order: transfer %d other %d cash %d", transfer.Score, other.Score, cash.Score)
	}
	near, _ := Score(ln(1, 2, 5000, ""), pay(1, 1, 5000, "cash"), s)
	far, _ := Score(ln(1, 7, 5000, ""), pay(1, 1, 5000, "cash"), s)
	if !(cash.Score > near.Score && near.Score > far.Score) {
		t.Errorf("date order: %d %d %d", cash.Score, near.Score, far.Score)
	}
	// Bank before the recorded date counts the same as after.
	before, _ := Score(ln(1, 1, 5000, ""), pay(1, 3, 5000, "cash"), s)
	after, _ := Score(ln(1, 5, 5000, ""), pay(1, 3, 5000, "cash"), s)
	if before.Score != after.Score || before.DateDiff != -2 || after.DateDiff != 2 {
		t.Errorf("before %+v after %+v", before, after)
	}
	// Zero tolerance: only exact amounts.
	s.TolerancePercent, s.ToleranceFloor = 0, 0
	if _, ok := Score(ln(1, 1, 5001, ""), pay(1, 1, 5000, "cash"), s); ok {
		t.Error("zero tolerance accepted a 1c difference")
	}
}

// Every line and payment appears exactly once across the result's lists.
func TestResultPartitionsInputs(t *testing.T) {
	s := Defaults()
	var lines []Line
	var pays []Payment
	for i := 0; i < 30; i++ {
		lines = append(lines, ln(int64(100+i), 1+i%28, money.Cents(50+(i%7)*1000), ""))
		pays = append(pays, pay(int64(1+i), 1+(i*3)%28, money.Cents(50+(i%5)*1000), "transfer"))
	}
	r := Suggest(lines, pays, s)
	seenL := map[int64]int{}
	seenP := map[int64]int{}
	for _, sg := range r.Suggestions {
		seenL[sg.LineID]++
		seenP[sg.PaymentSeq]++
	}
	for _, tie := range r.Ties {
		for _, id := range tie.LineIDs {
			seenL[id]++
		}
		for _, seq := range tie.PaymentSeqs {
			seenP[seq]++
		}
	}
	for _, id := range append(r.UnmatchedLines, r.Small...) {
		seenL[id]++
	}
	for _, seq := range r.UnmatchedPayments {
		seenP[seq]++
	}
	for _, l := range lines {
		if seenL[l.ID] != 1 {
			t.Errorf("line %d appears %d times", l.ID, seenL[l.ID])
		}
	}
	for _, p := range pays {
		if seenP[p.Seq] != 1 {
			t.Errorf("payment %d appears %d times", p.Seq, seenP[p.Seq])
		}
	}
	// Deterministic.
	if fmt.Sprint(Suggest(lines, pays, s)) != fmt.Sprint(r) {
		t.Error("two runs differ")
	}
}
