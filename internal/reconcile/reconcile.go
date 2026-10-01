// Package reconcile suggests which bank line is which recorded payment
// (SPEC-BANK-RECONCILE, RECON-03).
//
// It is a pure function over values: no database, no clock. It proposes and
// never decides -- nothing here reaches the ledger. A person confirms each
// match (RECON-04), and where two candidates score the same the package says
// so instead of picking one.
package reconcile

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/johnzastrow/bitt/internal/money"
)

// Settings are the Reconciliation setup controls (spec section 7).
type Settings struct {
	DateWindowDays   int         // 0..60: how far a bank date may be from the recorded one
	TolerancePercent int         // 0..100: relative difference allowed
	ToleranceCap     money.Cents // $0..$10,000: at most
	ToleranceFloor   money.Cents // $0..$100: at least
	MinLineAmount    money.Cents // $0..$10,000: smaller lines are not offered
	UseNames         bool
	DateFlagDays     int // 0..60: used when confirming (RECON-04)
}

// Defaults are the spec's starting values.
func Defaults() Settings {
	return Settings{
		DateWindowDays: 7, TolerancePercent: 10, ToleranceCap: 2500, ToleranceFloor: 100,
		MinLineAmount: 100, UseNames: true, DateFlagDays: 3,
	}
}

// Validate refuses anything outside the stated ranges. The floor may not
// exceed the cap, or "between the floor and the cap" means nothing.
func (s Settings) Validate() error {
	switch {
	case s.DateWindowDays < 0 || s.DateWindowDays > 60:
		return errors.New("the date window must be 0 to 60 days")
	case s.TolerancePercent < 0 || s.TolerancePercent > 100:
		return errors.New("the tolerance percent must be 0 to 100")
	case s.ToleranceCap < 0 || s.ToleranceCap > 1_000_000:
		return errors.New("the tolerance cap must be $0 to $10,000")
	case s.ToleranceFloor < 0 || s.ToleranceFloor > 10_000:
		return errors.New("the tolerance floor must be $0 to $100")
	case s.ToleranceFloor > s.ToleranceCap:
		return errors.New("the tolerance floor cannot be more than the cap")
	case s.MinLineAmount < 0 || s.MinLineAmount > 1_000_000:
		return errors.New("the minimum line amount must be $0 to $10,000")
	case s.DateFlagDays < 0 || s.DateFlagDays > 60:
		return errors.New("the date flag must be 0 to 60 days")
	}
	return nil
}

// Tolerance is the amount difference allowed for a recorded payment: the
// percent of it, held between the floor and the cap. Integer cents, rounded
// down, so the allowance is never more than stated.
func (s Settings) Tolerance(recorded money.Cents) money.Cents {
	t := money.Cents(int64(recorded) * int64(s.TolerancePercent) / 100)
	if t < s.ToleranceFloor {
		t = s.ToleranceFloor
	}
	if t > s.ToleranceCap {
		t = s.ToleranceCap
	}
	return t
}

// Line is an open bank line, as matching sees it.
type Line struct {
	ID       int64
	PostedOn time.Time // date, UTC midnight
	Amount   money.Cents
	// Text is what names are searched in: the description and the note.
	Text string
}

// Payment is an unmatched, unreversed payment entry.
type Payment struct {
	Seq         int64
	TabID       int64
	EffectiveOn time.Time // date, UTC midnight, in the instance's timezone
	Amount      money.Cents
	Method      string // "transfer", "cash", "other"
	// Names are the tab's name and its participants' display names.
	Names []string
}

// Suggestion is one proposed match and why.
type Suggestion struct {
	LineID     int64
	PaymentSeq int64
	Score      int
	Exact      bool        // same amount
	AmountDiff money.Cents // bank minus recorded
	DateDiff   int         // bank minus recorded, days
	NameHit    bool
}

// Tie is a group a person must resolve: lines and payments whose best pairs
// scored the same, so any choice would be a guess.
type Tie struct {
	LineIDs     []int64
	PaymentSeqs []int64
	Pairs       []Suggestion
}

// Result is the outcome of one Suggest run.
type Result struct {
	Suggestions []Suggestion
	Ties        []Tie
	// UnmatchedLines are offered lines with no candidate; Small are lines
	// below the minimum amount, not offered at all.
	UnmatchedLines []int64
	Small          []int64
	// UnmatchedPayments had no line in range; information only.
	UnmatchedPayments []int64
}

const (
	scoreExact    = 100
	scoreAmountUp = 60
	scoreDateUp   = 30
	scoreName     = 40
	scoreTransfer = 10
	scoreOther    = 5
)

// Score rates one pair, or reports that it is outside a limit.
func Score(l Line, p Payment, s Settings) (Suggestion, bool) {
	days := int(l.PostedOn.Sub(p.EffectiveOn).Hours() / 24)
	if abs(days) > s.DateWindowDays {
		return Suggestion{}, false
	}
	diff := l.Amount - p.Amount
	tol := s.Tolerance(p.Amount)
	if absC(diff) > tol {
		return Suggestion{}, false
	}

	sg := Suggestion{LineID: l.ID, PaymentSeq: p.Seq, AmountDiff: diff, DateDiff: days}
	if diff == 0 {
		sg.Exact = true
		sg.Score += scoreExact
	} else {
		// tol > 0 here, or diff would have been out of range.
		sg.Score += int(int64(scoreAmountUp) * int64(tol-absC(diff)) / int64(tol))
	}
	if s.DateWindowDays == 0 {
		sg.Score += scoreDateUp
	} else {
		sg.Score += scoreDateUp * (s.DateWindowDays - abs(days)) / s.DateWindowDays
	}
	if s.UseNames && nameHit(l.Text, p.Names) {
		sg.NameHit = true
		sg.Score += scoreName
	}
	switch p.Method {
	case "transfer":
		sg.Score += scoreTransfer
	case "other":
		sg.Score += scoreOther
	}
	return sg, true
}

// Suggest pairs lines with payments, best score first, each used once.
//
// The top remaining pair is taken only when it is the single best for both its
// line and its payment. When another unused pair with the same score shares
// the line or the payment, the whole connected group at that score becomes a
// Tie and nobody in it is assigned: "two possible payments" for a person, not
// a coin toss.
func Suggest(lines []Line, payments []Payment, s Settings) Result {
	var res Result
	var offered []Line
	for _, l := range lines {
		if l.Amount < s.MinLineAmount {
			res.Small = append(res.Small, l.ID)
			continue
		}
		offered = append(offered, l)
	}

	var pairs []Suggestion
	for _, l := range offered {
		for _, p := range payments {
			if sg, ok := Score(l, p, s); ok {
				pairs = append(pairs, sg)
			}
		}
	}
	// Deterministic: by score (which already weighs exactness and the date
	// gap), then by ids.
	sort.SliceStable(pairs, func(i, j int) bool {
		a, b := pairs[i], pairs[j]
		if a.Score != b.Score {
			return a.Score > b.Score
		}
		if a.LineID != b.LineID {
			return a.LineID < b.LineID
		}
		return a.PaymentSeq < b.PaymentSeq
	})

	usedL := map[int64]bool{}
	usedP := map[int64]bool{}
	for i, top := range pairs {
		if usedL[top.LineID] || usedP[top.PaymentSeq] {
			continue
		}
		group := tieGroup(pairs[i:], top, usedL, usedP)
		if len(group.Pairs) == 1 {
			res.Suggestions = append(res.Suggestions, top)
			usedL[top.LineID], usedP[top.PaymentSeq] = true, true
			continue
		}
		for _, id := range group.LineIDs {
			usedL[id] = true
		}
		for _, seq := range group.PaymentSeqs {
			usedP[seq] = true
		}
		res.Ties = append(res.Ties, group)
	}

	for _, l := range offered {
		if !usedL[l.ID] {
			res.UnmatchedLines = append(res.UnmatchedLines, l.ID)
		}
	}
	for _, p := range payments {
		if !usedP[p.Seq] {
			res.UnmatchedPayments = append(res.UnmatchedPayments, p.Seq)
		}
	}
	return res
}

// tieGroup gathers every unused pair with top's score that is connected to it
// through a shared line or payment. rest is sorted with top first.
func tieGroup(rest []Suggestion, top Suggestion, usedL, usedP map[int64]bool) Tie {
	var same []Suggestion
	for _, p := range rest {
		if p.Score != top.Score {
			break
		}
		if !usedL[p.LineID] && !usedP[p.PaymentSeq] {
			same = append(same, p)
		}
	}
	inL := map[int64]bool{top.LineID: true}
	inP := map[int64]bool{top.PaymentSeq: true}
	in := make([]bool, len(same))
	for changed := true; changed; {
		changed = false
		for i, p := range same {
			if !in[i] && (inL[p.LineID] || inP[p.PaymentSeq]) {
				in[i], changed = true, true
				inL[p.LineID], inP[p.PaymentSeq] = true, true
			}
		}
	}
	var t Tie
	for i, p := range same {
		if in[i] {
			t.Pairs = append(t.Pairs, p)
		}
	}
	t.LineIDs = sortedKeys(inL)
	t.PaymentSeqs = sortedKeys(inP)
	return t
}

func sortedKeys(m map[int64]bool) []int64 {
	out := make([]int64, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// nameHit reports whether any word of three or more letters from the names
// appears as a whole word in the text, ignoring case and punctuation.
func nameHit(text string, names []string) bool {
	words := map[string]bool{}
	for _, w := range splitWords(text) {
		words[w] = true
	}
	for _, n := range names {
		for _, w := range splitWords(n) {
			if len([]rune(w)) >= 3 && words[w] {
				return true
			}
		}
	}
	return false
}

func splitWords(s string) []string {
	return strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

func absC(c money.Cents) money.Cents {
	if c < 0 {
		return -c
	}
	return c
}

// String is for logs and test failures.
func (sg Suggestion) String() string {
	return fmt.Sprintf("line %d ~ payment %d (score %d)", sg.LineID, sg.PaymentSeq, sg.Score)
}
