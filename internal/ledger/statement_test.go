package ledger

import (
	"testing"
	"time"

	"github.com/johnzastrow/bitt/internal/money"
	"github.com/johnzastrow/bitt/internal/schedule"
	"github.com/johnzastrow/bitt/internal/store"
)

// entry is a terse builder for allocation tests. Charges are negative and
// payments positive, matching what the ledger writes.
func entry(seq int64, amount money.Cents, kind store.EntryKind) store.Entry {
	return store.Entry{Seq: seq, Kind: kind, Amount: amount}
}

func charge(seq int64, amount money.Cents) store.Entry {
	return entry(seq, -amount, store.KindCharge)
}

func payment(seq int64, amount money.Cents) store.Entry {
	return entry(seq, amount, store.KindPayment)
}

func reversalOf(seq, target int64, amount money.Cents) store.Entry {
	e := entry(seq, amount, store.KindReversal)
	e.ReversesSeq = &target
	return e
}

func mustAllocate(t *testing.T, entries []store.Entry) map[int64]money.Cents {
	t.Helper()
	applied, ok := Allocate(entries)
	if !ok {
		t.Fatal("Allocate reported an overflow")
	}
	return applied
}

// CHG-04: credits land on the oldest open charge first, which is the only
// ordering that makes paying ahead behave the way people expect.
func TestAllocateAppliesOldestFirst(t *testing.T) {
	entries := []store.Entry{
		charge(1, 10000),
		charge(2, 10000),
		charge(3, 10000),
		payment(4, 25000),
	}

	applied := mustAllocate(t, entries)

	want := map[int64]money.Cents{1: 10000, 2: 10000, 3: 5000}
	for seq, amount := range want {
		if applied[seq] != amount {
			t.Errorf("charge %d received %s, want %s", seq, applied[seq], amount)
		}
	}
}

func TestAllocatePartialAndUnpaid(t *testing.T) {
	entries := []store.Entry{
		charge(1, 10000),
		charge(2, 10000),
		payment(3, 4000),
	}

	applied := mustAllocate(t, entries)

	if applied[1] != 4000 {
		t.Errorf("first charge received %s, want 40.00", applied[1])
	}
	if applied[2] != 0 {
		t.Errorf("second charge received %s, want nothing", applied[2])
	}
	// Every charge appears, even at zero, so a caller can distinguish "unpaid"
	// from "not a charge".
	if _, present := applied[2]; !present {
		t.Error("an unpaid charge is missing from the allocation")
	}
}

// PAY-05: paying beyond what is owed leaves a credit, and the credit covers the
// next charge without any application step.
func TestAllocateCarriesCreditForward(t *testing.T) {
	entries := []store.Entry{
		charge(1, 5000),
		payment(2, 12000),
		charge(3, 5000),
		charge(4, 5000),
	}

	applied := mustAllocate(t, entries)

	if applied[1] != 5000 || applied[3] != 5000 {
		t.Errorf("early charges received %s and %s, want both fully covered", applied[1], applied[3])
	}
	if applied[4] != 2000 {
		t.Errorf("last charge received %s, want the remaining 20.00", applied[4])
	}
}

// A reversed charge and its reversal both drop out. Counting either would
// misstate this cycle and, through the leftover credit, every later one.
func TestAllocateIgnoresReversedPairs(t *testing.T) {
	entries := []store.Entry{
		charge(1, 10000),
		charge(2, 10000),
		reversalOf(3, 1, 10000), // undoes charge 1
		payment(4, 10000),
	}

	applied := mustAllocate(t, entries)

	if _, present := applied[1]; present {
		t.Error("a reversed charge was allocated against")
	}
	if applied[2] != 10000 {
		t.Errorf("the live charge received %s, want the full 100.00", applied[2])
	}

	// And a reversed payment stops counting as money available.
	withReversedPayment := []store.Entry{
		charge(1, 10000),
		payment(2, 10000),
		reversalOf(3, 2, -10000), // undoes the payment
	}
	applied = mustAllocate(t, withReversedPayment)
	if applied[1] != 0 {
		t.Errorf("charge received %s after its payment was undone, want nothing", applied[1])
	}
}

// Ordering follows the server-assigned sequence, not the order rows arrive in.
// History returns newest first, so this is the shape the caller actually passes.
func TestAllocateUsesSequenceOrderNotSliceOrder(t *testing.T) {
	newestFirst := []store.Entry{
		payment(3, 10000),
		charge(2, 8000),
		charge(1, 8000),
	}

	applied := mustAllocate(t, newestFirst)

	if applied[1] != 8000 {
		t.Errorf("charge 1 received %s, want the full 80.00 -- it is the oldest", applied[1])
	}
	if applied[2] != 2000 {
		t.Errorf("charge 2 received %s, want the remaining 20.00", applied[2])
	}
}

func TestAllocateHandlesNothing(t *testing.T) {
	applied := mustAllocate(t, nil)
	if len(applied) != 0 {
		t.Errorf("an empty ledger allocated %d charges", len(applied))
	}
}

// ---------------------------------------------------------------------------
// Statements -- CHG-04
// ---------------------------------------------------------------------------

func day(y int, m time.Month, d int) schedule.Date { return schedule.NewDate(y, m, d) }

func claim(tabID, seq int64, start, end, due schedule.Date) store.PostedPeriod {
	return store.PostedPeriod{
		TabID: tabID, Key: start.String(), EntrySeq: seq,
		Start: start, End: end, DueOn: due,
	}
}

// A period statement renders the charge, its breakdown, its due date, and the
// payments applied to it -- all computed, none stored.
func TestStatementsRenderAPeriod(t *testing.T) {
	// Two monthly cycles, and one payment covering the first plus part of the second.
	periods := []store.PostedPeriod{
		claim(1, 2, day(2026, time.February, 1), day(2026, time.March, 1), day(2026, time.February, 1)),
		claim(1, 1, day(2026, time.January, 1), day(2026, time.February, 1), day(2026, time.January, 1)),
	}
	entries := []store.Entry{
		payment(3, 12000),
		charge(2, 10000),
		charge(1, 10000),
	}
	items := map[int64][]store.EntryItem{
		1: {{Position: 0, Name: "Rent", Amount: 10000}},
		2: {{Position: 0, Name: "Rent", Amount: 10000}},
	}

	got, ok := Statements(periods, entries, items, day(2026, time.February, 10))
	if !ok {
		t.Fatal("Statements reported an overflow")
	}
	if len(got) != 2 {
		t.Fatalf("built %d statements, want 2", len(got))
	}

	// Newest first, matching the order the periods arrive in.
	february, january := got[0], got[1]

	if january.Charge != 10000 {
		t.Errorf("January charged %s, want 100.00", january.Charge)
	}
	if january.Paid != 10000 || !january.Settled() {
		t.Errorf("January paid %s, outstanding %s -- want fully settled", january.Paid, january.Outstanding())
	}
	if january.Period.DueOn != day(2026, time.January, 1) {
		t.Errorf("January due %s, want 2026-01-01", january.Period.DueOn)
	}
	if len(january.Items) != 1 || january.Items[0].Name != "Rent" {
		t.Errorf("January breakdown %+v, want one Rent line", january.Items)
	}
	if january.Overdue {
		t.Error("a settled cycle was flagged overdue")
	}

	if february.Paid != 2000 {
		t.Errorf("February paid %s, want the remaining 20.00", february.Paid)
	}
	if february.Outstanding() != 8000 {
		t.Errorf("February outstanding %s, want 80.00", february.Outstanding())
	}
	if !february.Overdue {
		t.Error("February is unpaid and past its due date but was not flagged overdue")
	}
}

func TestStatementsFlagOverdueOnlyAfterTheDueDate(t *testing.T) {
	periods := []store.PostedPeriod{
		claim(1, 1, day(2026, time.March, 1), day(2026, time.April, 1), day(2026, time.March, 1)),
	}
	entries := []store.Entry{charge(1, 5000)}

	// On the due date itself, nothing is late yet.
	got, _ := Statements(periods, entries, nil, day(2026, time.March, 1))
	if got[0].Overdue {
		t.Error("a cycle was flagged overdue on its own due date")
	}

	// The next day it is.
	got, _ = Statements(periods, entries, nil, day(2026, time.March, 2))
	if !got[0].Overdue {
		t.Error("an unpaid cycle past its due date was not flagged overdue")
	}
}

// An undone cycle owes nothing, but what it was billed stays visible.
func TestStatementsHandleAReversedPeriod(t *testing.T) {
	periods := []store.PostedPeriod{
		claim(1, 1, day(2026, time.January, 1), day(2026, time.February, 1), day(2026, time.January, 1)),
	}
	entries := []store.Entry{
		reversalOf(2, 1, 10000),
		charge(1, 10000),
	}

	got, ok := Statements(periods, entries, nil, day(2026, time.June, 1))
	if !ok {
		t.Fatal("Statements reported an overflow")
	}
	if len(got) != 1 {
		t.Fatalf("built %d statements, want 1", len(got))
	}
	st := got[0]

	if !st.Reversed {
		t.Error("a reversed cycle was not flagged")
	}
	if st.Charge != 10000 {
		t.Errorf("charge %s, want the original 100.00 to stay visible", st.Charge)
	}
	if st.Outstanding() != 0 {
		t.Errorf("outstanding %s on an undone cycle, want nothing", st.Outstanding())
	}
	if st.Overdue {
		t.Error("an undone cycle was flagged overdue")
	}
}

// A one-off charge is not a cycle, but it still absorbs credit in sequence, so
// a period statement cannot claim money that went to it (CHG-03, CHG-04).
func TestStatementsAccountForOneOffCharges(t *testing.T) {
	periods := []store.PostedPeriod{
		claim(1, 3, day(2026, time.February, 1), day(2026, time.March, 1), day(2026, time.February, 1)),
		claim(1, 1, day(2026, time.January, 1), day(2026, time.February, 1), day(2026, time.January, 1)),
	}
	entries := []store.Entry{
		payment(4, 15000),
		charge(3, 10000), // February cycle
		charge(2, 5000),  // a one-off, mid-January
		charge(1, 10000), // January cycle
	}

	got, ok := Statements(periods, entries, nil, day(2026, time.February, 10))
	if !ok {
		t.Fatal("Statements reported an overflow")
	}
	february, january := got[0], got[1]

	if january.Paid != 10000 {
		t.Errorf("January paid %s, want 100.00", january.Paid)
	}
	// The one-off took 50.00 next, leaving nothing for February.
	if february.Paid != 0 {
		t.Errorf("February paid %s, want nothing -- the one-off charge came first", february.Paid)
	}
}

// A claim whose entry is missing is omitted rather than rendered half-built.
func TestStatementsSkipClaimsWithoutEntries(t *testing.T) {
	periods := []store.PostedPeriod{
		claim(1, 99, day(2026, time.January, 1), day(2026, time.February, 1), day(2026, time.January, 1)),
	}
	got, ok := Statements(periods, []store.Entry{charge(1, 100)}, nil, day(2026, time.June, 1))
	if !ok {
		t.Fatal("Statements reported an overflow")
	}
	if len(got) != 0 {
		t.Errorf("built %d statements from a dangling claim, want 0", len(got))
	}
}
