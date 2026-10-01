package reconcile

import (
	"testing"

	"github.com/johnzastrow/bitt/internal/money"
)

// FuzzSuggest: for any inputs, each line and payment is in exactly one list,
// every suggestion is within the limits, and none shares a line or payment.
func FuzzSuggest(f *testing.F) {
	f.Add([]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12}, uint8(7), uint8(10))
	f.Add([]byte{50, 50, 50, 50, 50, 50, 50, 50}, uint8(0), uint8(0))
	f.Fuzz(func(t *testing.T, data []byte, window, pct uint8) {
		s := Defaults()
		s.DateWindowDays = int(window % 61)
		s.TolerancePercent = int(pct % 101)
		var lines []Line
		var pays []Payment
		methods := []string{"transfer", "cash", "other"}
		for i := 0; i+2 < len(data) && i < 120; i += 3 {
			cents := money.Cents(int(data[i+1])*100 + int(data[i+2]%3))
			d := 1 + int(data[i])%28
			if data[i]%2 == 0 {
				lines = append(lines, ln(int64(1000+i), d, cents, "FROM SAM"))
			} else {
				names := []string{}
				if data[i+2]%2 == 0 {
					names = []string{"Sam"}
				}
				pays = append(pays, pay(int64(1+i), d, cents, methods[int(data[i+2])%3], names...))
			}
		}
		r := Suggest(lines, pays, s)

		seenL := map[int64]int{}
		seenP := map[int64]int{}
		byL := map[int64]Line{}
		byP := map[int64]Payment{}
		for _, l := range lines {
			byL[l.ID] = l
		}
		for _, p := range pays {
			byP[p.Seq] = p
		}
		for _, sg := range r.Suggestions {
			seenL[sg.LineID]++
			seenP[sg.PaymentSeq]++
			if again, ok := Score(byL[sg.LineID], byP[sg.PaymentSeq], s); !ok || again != sg {
				t.Fatalf("suggestion %v is outside the limits or rescored differently", sg)
			}
		}
		for _, tie := range r.Ties {
			if len(tie.Pairs) < 2 {
				t.Fatalf("a tie of %d pairs", len(tie.Pairs))
			}
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
				t.Fatalf("line %d appears %d times", l.ID, seenL[l.ID])
			}
		}
		for _, p := range pays {
			if seenP[p.Seq] != 1 {
				t.Fatalf("payment %d appears %d times", p.Seq, seenP[p.Seq])
			}
		}
	})
}
