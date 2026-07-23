package sqlite

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/johnzastrow/bitt/internal/money"
	"github.com/johnzastrow/bitt/internal/schedule"
	"github.com/johnzastrow/bitt/internal/store"
)

func day(y int, m time.Month, d int) schedule.Date { return schedule.NewDate(y, m, d) }

// period builds the claim for a cycle, the way the ledger does.
func period(tabID int64, start, end, due schedule.Date) store.PostedPeriod {
	return store.PostedPeriod{
		TabID: tabID, Key: start.String(), Start: start, End: end, DueOn: due,
	}
}

func periodCharge(tabID, actor int64, key string, amount int64) store.NewEntry {
	return store.NewEntry{
		TabID:          tabID,
		Kind:           store.KindCharge,
		Amount:         money.Cents(-amount),
		ActorUserID:    actor,
		IdempotencyKey: "period:" + key,
		EffectiveAt:    time.Now().UTC(),
		Items:          []store.EntryItem{{Position: 0, Name: "Service", Amount: money.Cents(amount)}},
	}
}

// ---------------------------------------------------------------------------
// Schedules on tabs (SCHED-01)
// ---------------------------------------------------------------------------

// A schedule survives the round trip through the three stored columns, for
// every recurrence and both billing rules.
func TestScheduleRoundTrips(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	user := mustUser(t, db, "a@example.com")

	cases := []schedule.Schedule{
		{Kind: schedule.Weekly, Anchor: day(2026, time.January, 5), Billing: schedule.InAdvance},
		{Kind: schedule.Biweekly, Anchor: day(2026, time.February, 2), Billing: schedule.InArrears},
		{Kind: schedule.MonthlyDay, Anchor: day(2026, time.January, 31), Billing: schedule.InAdvance},
		{Kind: schedule.MonthlyLast, Anchor: day(2026, time.January, 31), Billing: schedule.InArrears},
	}

	for _, want := range cases {
		t.Run(string(want.Kind)+"/"+string(want.Billing), func(t *testing.T) {
			tab, err := db.CreateTab(ctx, store.Tab{
				Name: "Scheduled", Kind: store.TabServices, CreatedBy: user.ID, Schedule: want,
			}, nil)
			if err != nil {
				t.Fatalf("create tab: %v", err)
			}
			got, err := db.GetTab(ctx, tab.ID)
			if err != nil {
				t.Fatalf("get tab: %v", err)
			}
			if got.Schedule != want {
				t.Errorf("schedule round-tripped as %+v, want %+v", got.Schedule, want)
			}
		})
	}
}

// A tab with no schedule reads back with none, and stores empty strings rather
// than nulls so comparisons need no COALESCE on either backend (DEPLOY-02).
func TestUnscheduledTabStoresEmptyStrings(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	user := mustUser(t, db, "a@example.com")
	tab := mustTab(t, db, user.ID)

	got, err := db.GetTab(ctx, tab.ID)
	if err != nil {
		t.Fatalf("get tab: %v", err)
	}
	if got.Schedule.Set() {
		t.Errorf("an unscheduled tab read back as %+v", got.Schedule)
	}

	var kind, anchor, billing string
	err = db.db.QueryRowContext(ctx,
		`SELECT schedule_kind, schedule_anchor, schedule_billing FROM tabs WHERE id = ?`,
		tab.ID).Scan(&kind, &anchor, &billing)
	if err != nil {
		t.Fatalf("read columns: %v", err)
	}
	if kind != "" || anchor != "" || billing != "" {
		t.Errorf("stored %q/%q/%q, want three empty strings", kind, anchor, billing)
	}
}

func TestSetSchedule(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	user := mustUser(t, db, "a@example.com")
	tab := mustTab(t, db, user.ID)

	want := schedule.Schedule{
		Kind: schedule.MonthlyDay, Anchor: day(2026, time.March, 15), Billing: schedule.InAdvance,
	}
	if err := db.SetSchedule(ctx, tab.ID, want); err != nil {
		t.Fatalf("set schedule: %v", err)
	}
	got, err := db.GetTab(ctx, tab.ID)
	if err != nil {
		t.Fatalf("get tab: %v", err)
	}
	if got.Schedule != want {
		t.Errorf("schedule is %+v, want %+v", got.Schedule, want)
	}

	// Clearing returns the tab to manual billing.
	if err := db.SetSchedule(ctx, tab.ID, schedule.Schedule{}); err != nil {
		t.Fatalf("clear schedule: %v", err)
	}
	if got, _ = db.GetTab(ctx, tab.ID); got.Schedule.Set() {
		t.Errorf("schedule is %+v after clearing, want none", got.Schedule)
	}

	// A malformed schedule is refused rather than stored.
	bad := schedule.Schedule{Kind: "quarterly", Anchor: day(2026, time.March, 15), Billing: schedule.InAdvance}
	if err := db.SetSchedule(ctx, tab.ID, bad); err == nil {
		t.Error("an unrecognized recurrence was accepted")
	}

	// A tab that does not exist is a miss, not a silent success.
	if err := db.SetSchedule(ctx, 9999, want); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("set schedule on a missing tab returned %v, want ErrNotFound", err)
	}
}

// ---------------------------------------------------------------------------
// Period claims (SCHED-03, SCHED-04)
// ---------------------------------------------------------------------------

func TestPostPeriodEntryClaimsTheCycle(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	user := mustUser(t, db, "a@example.com")
	tab := mustTab(t, db, user.ID)

	p := period(tab.ID, day(2026, time.January, 1), day(2026, time.February, 1), day(2026, time.January, 1))
	entry, replayed, err := db.PostPeriodEntry(ctx, p, periodCharge(tab.ID, user.ID, p.Key, 7500))
	if err != nil {
		t.Fatalf("post period: %v", err)
	}
	if replayed {
		t.Error("the first post of a cycle reported itself replayed")
	}

	claims, err := db.ListPostedPeriods(ctx, tab.ID)
	if err != nil {
		t.Fatalf("list periods: %v", err)
	}
	if len(claims) != 1 {
		t.Fatalf("claimed %d cycles, want 1", len(claims))
	}
	got := claims[0]
	if got.EntrySeq != entry.Seq {
		t.Errorf("claim points at entry %d, want %d", got.EntrySeq, entry.Seq)
	}
	if got.Start != p.Start || got.End != p.End || got.DueOn != p.DueOn {
		t.Errorf("claim dates %s/%s/%s, want %s/%s/%s",
			got.Start, got.End, got.DueOn, p.Start, p.End, p.DueOn)
	}

	// The item snapshot rode along in the same transaction.
	items, err := db.ListEntryItems(ctx, entry.Seq)
	if err != nil {
		t.Fatalf("entry items: %v", err)
	}
	if len(items) != 1 || items[0].Name != "Service" {
		t.Errorf("snapshot %+v, want one Service line", items)
	}
}

// Re-posting a claimed cycle returns the original rather than billing twice,
// and does not leave a stray entry behind (SCHED-04).
func TestPostPeriodEntryIsIdempotent(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	user := mustUser(t, db, "a@example.com")
	tab := mustTab(t, db, user.ID)

	p := period(tab.ID, day(2026, time.January, 1), day(2026, time.February, 1), day(2026, time.January, 1))
	first, _, err := db.PostPeriodEntry(ctx, p, periodCharge(tab.ID, user.ID, p.Key, 7500))
	if err != nil {
		t.Fatalf("first post: %v", err)
	}

	// A second attempt, right down to a different idempotency key, so the claim
	// itself is what refuses rather than the entries index.
	second := periodCharge(tab.ID, user.ID, p.Key, 7500)
	second.IdempotencyKey = "a-completely-different-key"
	got, replayed, err := db.PostPeriodEntry(ctx, p, second)
	if err != nil {
		t.Fatalf("second post: %v", err)
	}
	if !replayed {
		t.Error("re-posting a claimed cycle did not report itself replayed")
	}
	if got.Seq != first.Seq {
		t.Errorf("replay returned entry %d, want the original %d", got.Seq, first.Seq)
	}

	// The rolled-back attempt left nothing: one entry, one claim, one charge.
	balance, err := db.SumEntries(ctx, tab.ID)
	if err != nil {
		t.Fatalf("balance: %v", err)
	}
	if balance != -7500 {
		t.Errorf("balance %s after a refused re-post, want -75.00", balance)
	}
	entries, err := db.ListEntries(ctx, tab.ID)
	if err != nil {
		t.Fatalf("list entries: %v", err)
	}
	if len(entries) != 1 {
		t.Errorf("ledger holds %d entries, want 1 -- the rollback leaked", len(entries))
	}
}

// The claim and the entry share a transaction, so losing the race on the claim
// must take the entry with it. Many writers, one cycle, one charge.
func TestConcurrentPostPeriodEntry(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	user := mustUser(t, db, "a@example.com")
	tab := mustTab(t, db, user.ID)

	p := period(tab.ID, day(2026, time.January, 1), day(2026, time.February, 1), day(2026, time.January, 1))

	const writers = 16
	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		fresh int
		errs  []error
	)
	start := make(chan struct{})
	for i := range writers {
		wg.Go(func() {
			<-start
			e := periodCharge(tab.ID, user.ID, p.Key, 7500)
			// Distinct keys, so only the period claim can prevent a double post.
			e.IdempotencyKey = "writer-" + string(rune('a'+i))
			_, replayed, err := db.PostPeriodEntry(ctx, p, e)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err)
				return
			}
			if !replayed {
				fresh++
			}
		})
	}
	close(start)
	wg.Wait()

	for _, err := range errs {
		t.Errorf("concurrent post failed: %v", err)
	}
	if fresh != 1 {
		t.Errorf("%d writers posted fresh entries, want exactly 1", fresh)
	}

	balance, err := db.SumEntries(ctx, tab.ID)
	if err != nil {
		t.Fatalf("balance: %v", err)
	}
	if balance != -7500 {
		t.Errorf("balance %s after %d concurrent posts, want -75.00", balance, writers)
	}
	entries, err := db.ListEntries(ctx, tab.ID)
	if err != nil {
		t.Fatalf("list entries: %v", err)
	}
	if len(entries) != 1 {
		t.Errorf("ledger holds %d entries, want 1", len(entries))
	}
}

// Period claims are as immutable as the entries they point at: repointing one,
// or deleting one so a cycle could bill again, would defeat SCHED-04.
func TestPostedPeriodsAreAppendOnly(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	user := mustUser(t, db, "a@example.com")
	tab := mustTab(t, db, user.ID)

	p := period(tab.ID, day(2026, time.January, 1), day(2026, time.February, 1), day(2026, time.January, 1))
	if _, _, err := db.PostPeriodEntry(ctx, p, periodCharge(tab.ID, user.ID, p.Key, 7500)); err != nil {
		t.Fatalf("post period: %v", err)
	}

	for _, sql := range []string{
		`UPDATE posted_periods SET entry_seq = 999 WHERE tab_id = ?`,
		`DELETE FROM posted_periods WHERE tab_id = ?`,
	} {
		_, err := db.db.ExecContext(ctx, sql, tab.ID)
		if err == nil {
			t.Errorf("%q succeeded; the append-only trigger is not active", sql)
			continue
		}
		if !strings.Contains(err.Error(), "append-only") {
			t.Errorf("%q blocked by an unexpected error: %v", sql, err)
		}
	}
}

// A claim whose tab disagrees with its entry's tab is a caller bug, and is
// refused rather than written.
func TestPostPeriodEntryRejectsMismatchedTab(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	user := mustUser(t, db, "a@example.com")
	tab := mustTab(t, db, user.ID)

	p := period(tab.ID+1, day(2026, time.January, 1), day(2026, time.February, 1), day(2026, time.January, 1))
	if _, _, err := db.PostPeriodEntry(ctx, p, periodCharge(tab.ID, user.ID, p.Key, 7500)); err == nil {
		t.Error("a claim for a different tab than its entry was accepted")
	}

	empty := period(tab.ID, day(2026, time.January, 1), day(2026, time.February, 1), day(2026, time.January, 1))
	empty.Key = ""
	if _, _, err := db.PostPeriodEntry(ctx, empty, periodCharge(tab.ID, user.ID, "x", 7500)); err == nil {
		t.Error("a claim with no period key was accepted")
	}
}

// ---------------------------------------------------------------------------
// Items (CHG-01, CHG-02)
// ---------------------------------------------------------------------------

func TestUpdateItemSupersedes(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	user := mustUser(t, db, "a@example.com")
	tab := mustTab(t, db, user.ID)

	items, err := db.ListItems(ctx, tab.ID)
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	original := items[0]

	replacement, err := db.UpdateItem(ctx, original.ID, "Line 1 renamed", 5500)
	if err != nil {
		t.Fatalf("update item: %v", err)
	}
	if replacement.ID == original.ID {
		t.Error("update overwrote the row in place; it should supersede")
	}
	if replacement.Position != original.Position {
		t.Errorf("replacement sits at position %d, want %d -- ordering must survive an edit",
			replacement.Position, original.Position)
	}

	// The active list shows the replacement and not the original.
	active, err := db.ListItems(ctx, tab.ID)
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	if len(active) != len(items) {
		t.Errorf("active items number %d, want %d", len(active), len(items))
	}
	for _, it := range active {
		if it.ID == original.ID {
			t.Error("the superseded item is still active")
		}
	}

	// The history keeps both, and the old one is dated.
	history, err := db.ListItemHistory(ctx, tab.ID)
	if err != nil {
		t.Fatalf("item history: %v", err)
	}
	if len(history) != len(items)+1 {
		t.Fatalf("history holds %d rows, want %d", len(history), len(items)+1)
	}
	var found bool
	for _, it := range history {
		if it.ID == original.ID {
			found = true
			if it.RemovedAt == nil {
				t.Error("the superseded item carries no removal time")
			}
			if it.Amount != original.Amount {
				t.Errorf("the superseded row now reads %s, want the %s it was", it.Amount, original.Amount)
			}
		}
	}
	if !found {
		t.Error("the superseded item is missing from the history")
	}

	// Updating an already-superseded item is a miss.
	if _, err := db.UpdateItem(ctx, original.ID, "again", 1); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("updating a superseded item returned %v, want ErrNotFound", err)
	}
}

func TestRemoveItem(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	user := mustUser(t, db, "a@example.com")
	tab := mustTab(t, db, user.ID)

	items, _ := db.ListItems(ctx, tab.ID)
	if err := db.RemoveItem(ctx, items[0].ID); err != nil {
		t.Fatalf("remove item: %v", err)
	}

	active, _ := db.ListItems(ctx, tab.ID)
	if len(active) != len(items)-1 {
		t.Errorf("%d items remain active, want %d", len(active), len(items)-1)
	}
	history, _ := db.ListItemHistory(ctx, tab.ID)
	if len(history) != len(items) {
		t.Errorf("history holds %d rows, want %d -- removal must not delete", len(history), len(items))
	}

	// Removing twice is a miss rather than a silent success.
	if err := db.RemoveItem(ctx, items[0].ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("removing twice returned %v, want ErrNotFound", err)
	}
	if err := db.RemoveItem(ctx, 9999); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("removing a missing item returned %v, want ErrNotFound", err)
	}
}

func TestGetItem(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	user := mustUser(t, db, "a@example.com")
	tab := mustTab(t, db, user.ID)

	items, _ := db.ListItems(ctx, tab.ID)
	got, err := db.GetItem(ctx, items[0].ID)
	if err != nil {
		t.Fatalf("get item: %v", err)
	}
	if got.ID != items[0].ID || got.TabID != tab.ID {
		t.Errorf("got item %+v, want %+v", got, items[0])
	}
	// The tab id is what callers authorize against, so it must be populated.
	if got.TabID == 0 {
		t.Error("item carries no tab id")
	}

	if _, err := db.GetItem(ctx, 9999); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("get missing item returned %v, want ErrNotFound", err)
	}
}

// One query returns every entry's breakdown for a tab, so a statement page
// does not issue one per period (CHG-04).
func TestListEntryItemsForTab(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	user := mustUser(t, db, "a@example.com")
	tab := mustTab(t, db, user.ID)
	other := mustTab(t, db, user.ID)

	var seqs []int64
	for i, key := range []string{"2026-01-01", "2026-02-01"} {
		p := period(tab.ID, day(2026, time.Month(i+1), 1), day(2026, time.Month(i+2), 1), day(2026, time.Month(i+1), 1))
		e, _, err := db.PostPeriodEntry(ctx, p, periodCharge(tab.ID, user.ID, key, 7500))
		if err != nil {
			t.Fatalf("post %s: %v", key, err)
		}
		seqs = append(seqs, e.Seq)
	}
	// An entry on a different tab must not appear in the result.
	if _, _, err := db.PostEntry(ctx, store.NewEntry{
		TabID: other.ID, Kind: store.KindCharge, Amount: -100, ActorUserID: user.ID,
		IdempotencyKey: "other", Items: []store.EntryItem{{Name: "Elsewhere", Amount: 100}},
	}); err != nil {
		t.Fatalf("post on the other tab: %v", err)
	}

	got, err := db.ListEntryItemsForTab(ctx, tab.ID)
	if err != nil {
		t.Fatalf("list entry items: %v", err)
	}
	if len(got) != len(seqs) {
		t.Fatalf("returned breakdowns for %d entries, want %d", len(got), len(seqs))
	}
	for _, seq := range seqs {
		items := got[seq]
		if len(items) != 1 || items[0].Name != "Service" {
			t.Errorf("entry %d breakdown %+v, want one Service line", seq, items)
		}
	}
	for _, items := range got {
		for _, it := range items {
			if it.Name == "Elsewhere" {
				t.Error("another tab's breakdown leaked into the result")
			}
		}
	}
}

// A tab with no periods reads back empty rather than erroring.
func TestListPostedPeriodsOnAnUnscheduledTab(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	user := mustUser(t, db, "a@example.com")
	tab := mustTab(t, db, user.ID)

	got, err := db.ListPostedPeriods(ctx, tab.ID)
	if err != nil {
		t.Fatalf("list periods: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("an unscheduled tab reported %d claims", len(got))
	}
}
