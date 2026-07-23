package ledger

import (
	"slices"

	"github.com/johnzastrow/bitt/internal/money"
	"github.com/johnzastrow/bitt/internal/schedule"
	"github.com/johnzastrow/bitt/internal/store"
)

// Statement is one billing cycle as a person reads it: what was charged, what
// it was made of, when it was owed, and what has been paid against it (CHG-04).
//
// Nothing here is stored. Every field is derived from ledger entries on each
// render, which is the point -- a stored statement is a second copy of the
// truth, free to drift from the entries that produce the balance.
type Statement struct {
	Period store.PostedPeriod
	Entry  store.Entry
	// Items is the breakdown captured when the cycle posted, so a later change
	// to the tab cannot rewrite what this cycle was billed for (CHG-01).
	Items []store.EntryItem
	// Charge is the magnitude billed for the cycle.
	Charge money.Cents
	// Paid is how much credit has been applied to it.
	Paid money.Cents
	// Reversed reports that the cycle's charge was undone (LEDGER-02).
	Reversed bool
	// Overdue reports an unsettled cycle whose due date has passed. Phase 4's
	// late fees key off the same condition.
	Overdue bool
}

// Outstanding is what remains owed on the cycle.
func (s Statement) Outstanding() money.Cents { return s.Charge - s.Paid }

// Settled reports whether the cycle is fully covered.
func (s Statement) Settled() bool { return s.Outstanding() <= 0 }

// Allocate applies credits against charges, oldest charge first, and reports
// how much landed on each (CHG-04).
//
// Payments are deliberately not allocated in the database. Items and periods
// carry no balance of their own (TAB-05), so "what has this cycle been paid"
// is a question answered by walking the same entries that produce the balance,
// on every render. There is no allocation table to fall out of step, and a
// payment recorded against the tab needs no decision about which cycle it
// belongs to.
//
// Oldest first is the only ordering that makes a credit behave the way people
// expect: money paid ahead covers the earliest thing still open, and a credit
// left over simply carries.
//
// A reversed entry and its reversal are both dropped. The pair nets to zero, so
// treating either as a real charge or a real payment would misstate both this
// cycle and every later one the leftover credit reaches.
//
// The second return is false if the credits overflow int64 cents, which the
// caller should treat as a failure rather than render.
func Allocate(entries []store.Entry) (map[int64]money.Cents, bool) {
	reversed := ReversedSeqs(entries)

	live := make([]store.Entry, 0, len(entries))
	credits := make([]money.Cents, 0, len(entries))
	for _, e := range entries {
		if e.Kind == store.KindReversal || reversed[e.Seq] {
			continue
		}
		live = append(live, e)
		if e.Amount > 0 {
			credits = append(credits, e.Amount)
		}
	}

	pool, ok := money.Sum(credits)
	if !ok {
		return nil, false
	}

	// Authoritative order is the server-assigned sequence, never a client clock
	// (LEDGER-06).
	slices.SortFunc(live, func(a, b store.Entry) int { return int(a.Seq - b.Seq) })

	applied := make(map[int64]money.Cents, len(live))
	for _, e := range live {
		if e.Amount >= 0 {
			continue
		}
		owed := e.Amount.Neg()
		take := min(owed, pool)
		if take < 0 {
			take = 0
		}
		applied[e.Seq] = take
		pool -= take
	}
	return applied, true
}

// Statements assembles the per-cycle view of a tab, newest cycle first.
//
// periods and entries should both come straight from the store; today is the
// current date in the instance timezone, and decides which cycles read as
// overdue. The second return is false if the ledger's credits overflow.
func Statements(
	periods []store.PostedPeriod,
	entries []store.Entry,
	items map[int64][]store.EntryItem,
	today schedule.Date,
) ([]Statement, bool) {
	applied, ok := Allocate(entries)
	if !ok {
		return nil, false
	}
	reversed := ReversedSeqs(entries)

	bySeq := make(map[int64]store.Entry, len(entries))
	for _, e := range entries {
		bySeq[e.Seq] = e
	}

	out := make([]Statement, 0, len(periods))
	for _, p := range periods {
		entry, found := bySeq[p.EntrySeq]
		if !found {
			// A claim whose entry is missing cannot happen through any write
			// path here -- they share a transaction -- but rendering a
			// half-statement would be worse than omitting it.
			continue
		}

		st := Statement{
			Period: p,
			Entry:  entry,
			Items:  items[entry.Seq],
			Charge: entry.Amount.Neg(),
			Paid:   applied[entry.Seq],
		}
		if reversed[entry.Seq] {
			// An undone cycle owes nothing, whatever it was billed at. The
			// charge stays visible so the history reads honestly.
			st.Reversed = true
			st.Paid = st.Charge
		}
		st.Overdue = !st.Settled() && p.DueOn.Before(today)
		out = append(out, st)
	}
	return out, true
}
