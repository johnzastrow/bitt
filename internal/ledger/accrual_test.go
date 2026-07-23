package ledger

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/johnzastrow/bitt/internal/money"
	"github.com/johnzastrow/bitt/internal/schedule"
	"github.com/johnzastrow/bitt/internal/store"
	"github.com/johnzastrow/bitt/internal/store/sqlite"
)

// newScheduledFixture builds a tab with a schedule and line items, and a ledger
// whose clock the test controls.
func newScheduledFixture(t *testing.T, sched schedule.Schedule, items []store.TabItem) (*Service, *sqlite.DB, store.Tab) {
	t.Helper()
	db, err := sqlite.Open(sqlite.Options{
		Path:               filepath.Join(t.TempDir(), "accrual.db"),
		AppendOnlyTriggers: true,
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	ctx := context.Background()
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	user, err := db.CreateUser(ctx, store.User{
		Email: "provider@example.com", DisplayName: "Provider", PasswordHash: "$argon2id$x",
	})
	if err != nil {
		t.Fatalf("user: %v", err)
	}

	tab, err := db.CreateTab(ctx, store.Tab{
		Name:      "Rent",
		Kind:      store.TabServices,
		CreatedBy: user.ID,
		Schedule:  sched.Normalize(),
		// Created well before any anchor these tests use, so item reconstruction
		// is exercised on its own terms rather than through the backdating clamp.
		CreatedAt: time.Date(2025, time.December, 1, 0, 0, 0, 0, time.UTC),
	}, items)
	if err != nil {
		t.Fatalf("tab: %v", err)
	}
	return New(db), db, tab
}

// at freezes the ledger's clock at a given UTC date.
func at(led *Service, y int, m time.Month, d int) *Service {
	return led.WithClock(func() time.Time {
		return time.Date(y, m, d, 12, 0, 0, 0, time.UTC)
	})
}

func accrue(t *testing.T, led *Service, db *sqlite.DB, tab store.Tab, loc *time.Location) Accrual {
	t.Helper()
	ctx := context.Background()
	history, err := db.ListItemHistory(ctx, tab.ID)
	if err != nil {
		t.Fatalf("item history: %v", err)
	}
	acc, err := led.Accrue(ctx, tab, history, loc)
	if err != nil {
		t.Fatalf("accrue: %v", err)
	}
	return acc
}

// ---------------------------------------------------------------------------
// SCHED-03: opening a tab posts what is due, and posts each cycle once
// ---------------------------------------------------------------------------

func TestAccruePostsDuePeriods(t *testing.T) {
	led, db, tab := newScheduledFixture(t,
		schedule.Schedule{
			Kind:    schedule.MonthlyDay,
			Anchor:  schedule.NewDate(2026, time.January, 1),
			Billing: schedule.InAdvance,
		},
		[]store.TabItem{{Name: "Rent", Amount: 120000}, {Name: "Utilities", Amount: 4500}},
	)
	ctx := context.Background()

	// Nothing due before the anchor.
	if acc := accrue(t, at(led, 2025, time.December, 15), db, tab, time.UTC); len(acc.Posted) != 0 {
		t.Fatalf("posted %d cycles before the anchor, want 0", len(acc.Posted))
	}
	if b, _ := led.Balance(ctx, tab.ID); b != 0 {
		t.Fatalf("balance %s before the anchor, want 0", b)
	}

	// One month in, one cycle.
	acc := accrue(t, at(led, 2026, time.January, 10), db, tab, time.UTC)
	if len(acc.Posted) != 1 {
		t.Fatalf("posted %d cycles, want 1", len(acc.Posted))
	}
	if b, _ := led.Balance(ctx, tab.ID); b != -124500 {
		t.Errorf("balance %s after one cycle, want -1,245.00", b)
	}
	if acc.Next.Due != schedule.NewDate(2026, time.February, 1) {
		t.Errorf("next cycle due %s, want 2026-02-01", acc.Next.Due)
	}

	// Reading again the same month posts nothing more.
	if acc := accrue(t, at(led, 2026, time.January, 20), db, tab, time.UTC); len(acc.Posted) != 0 {
		t.Fatalf("a second read posted %d cycles, want 0", len(acc.Posted))
	}
	if b, _ := led.Balance(ctx, tab.ID); b != -124500 {
		t.Errorf("balance %s after a repeat read, want -1,245.00", b)
	}
}

// The exit criterion: a tab nobody opened for six months posts six months of
// cycles the moment somebody does, each exactly once (SCHED-03).
func TestAccrueCatchesUpAfterSixMonths(t *testing.T) {
	led, db, tab := newScheduledFixture(t,
		schedule.Schedule{
			Kind:    schedule.MonthlyDay,
			Anchor:  schedule.NewDate(2026, time.January, 15),
			Billing: schedule.InAdvance,
		},
		[]store.TabItem{{Name: "Lawn care", Amount: 8000}},
	)
	ctx := context.Background()

	acc := accrue(t, at(led, 2026, time.June, 20), db, tab, time.UTC)
	if len(acc.Posted) != 6 {
		t.Fatalf("posted %d cycles, want 6 (Jan through Jun)", len(acc.Posted))
	}
	if b, _ := led.Balance(ctx, tab.ID); b != -48000 {
		t.Errorf("balance %s, want -480.00 for six months at $80", b)
	}

	// Oldest first, on the right dates, each with its own breakdown.
	wantDue := []schedule.Date{
		schedule.NewDate(2026, time.January, 15),
		schedule.NewDate(2026, time.February, 15),
		schedule.NewDate(2026, time.March, 15),
		schedule.NewDate(2026, time.April, 15),
		schedule.NewDate(2026, time.May, 15),
		schedule.NewDate(2026, time.June, 15),
	}
	periods, err := db.ListPostedPeriods(ctx, tab.ID)
	if err != nil {
		t.Fatalf("list periods: %v", err)
	}
	if len(periods) != 6 {
		t.Fatalf("claimed %d cycles, want 6", len(periods))
	}
	// ListPostedPeriods returns newest first.
	for i, p := range periods {
		want := wantDue[len(wantDue)-1-i]
		if p.DueOn != want {
			t.Errorf("claim %d due %s, want %s", i, p.DueOn, want)
		}
	}

	// A seventh read changes nothing.
	if acc := accrue(t, at(led, 2026, time.June, 25), db, tab, time.UTC); len(acc.Posted) != 0 {
		t.Fatalf("catch-up ran twice: posted %d more cycles", len(acc.Posted))
	}
	if b, _ := led.Balance(ctx, tab.ID); b != -48000 {
		t.Errorf("balance %s after a repeat read, want -480.00", b)
	}
}

// SCHED-02 end to end: a day-31 anchor bills February on the 28th and returns
// to the 31st, and the entries land on those dates.
func TestAccrueHandlesMonthEndAnchor(t *testing.T) {
	led, db, tab := newScheduledFixture(t,
		schedule.Schedule{
			Kind:    schedule.MonthlyDay,
			Anchor:  schedule.NewDate(2026, time.January, 31),
			Billing: schedule.InAdvance,
		},
		[]store.TabItem{{Name: "Storage", Amount: 5000}},
	)
	ctx := context.Background()

	accrue(t, at(led, 2026, time.May, 1), db, tab, time.UTC)

	periods, err := db.ListPostedPeriods(ctx, tab.ID)
	if err != nil {
		t.Fatalf("list periods: %v", err)
	}
	got := make([]string, 0, len(periods))
	for i := len(periods) - 1; i >= 0; i-- {
		got = append(got, periods[i].DueOn.String())
	}
	want := []string{"2026-01-31", "2026-02-28", "2026-03-31", "2026-04-30"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("due dates %v, want %v", got, want)
	}

	if b, _ := led.Balance(ctx, tab.ID); b != -20000 {
		t.Errorf("balance %s, want -200.00 for four cycles at $50", b)
	}
}

// Billing in arrears is the Provider's other choice: the cycle is charged on
// the day it ends, so at any moment one fewer cycle has posted.
func TestAccrueBillsInArrears(t *testing.T) {
	led, db, tab := newScheduledFixture(t,
		schedule.Schedule{
			Kind:    schedule.MonthlyDay,
			Anchor:  schedule.NewDate(2026, time.January, 1),
			Billing: schedule.InArrears,
		},
		[]store.TabItem{{Name: "Hours", Amount: 25000}},
	)
	ctx := context.Background()

	// Mid-January: the first cycle is still running, so nothing is owed.
	if acc := accrue(t, at(led, 2026, time.January, 20), db, tab, time.UTC); len(acc.Posted) != 0 {
		t.Fatalf("posted %d cycles mid-period, want 0 when billing in arrears", len(acc.Posted))
	}

	// February 1st: January is complete and now billed.
	acc := accrue(t, at(led, 2026, time.February, 1), db, tab, time.UTC)
	if len(acc.Posted) != 1 {
		t.Fatalf("posted %d cycles, want 1", len(acc.Posted))
	}
	if b, _ := led.Balance(ctx, tab.ID); b != -25000 {
		t.Errorf("balance %s, want -250.00", b)
	}

	periods, err := db.ListPostedPeriods(ctx, tab.ID)
	if err != nil {
		t.Fatalf("list periods: %v", err)
	}
	p := periods[0]
	if p.Start != schedule.NewDate(2026, time.January, 1) {
		t.Errorf("cycle starts %s, want 2026-01-01", p.Start)
	}
	if p.DueOn != schedule.NewDate(2026, time.February, 1) {
		t.Errorf("cycle due %s, want 2026-02-01 when billing in arrears", p.DueOn)
	}
	// The key names the cycle, not the day it was billed, so switching the
	// billing rule cannot re-bill it (SCHED-04).
	if p.Key != "2026-01-01" {
		t.Errorf("period key %q, want the cycle's start date", p.Key)
	}
}

// A tab with no schedule accrues nothing and reports so, rather than erroring.
// Billing entirely by hand stays a legitimate way to run a tab (CHG-03).
func TestAccrueIgnoresUnscheduledTabs(t *testing.T) {
	led, db, tab := newScheduledFixture(t, schedule.Schedule{}, []store.TabItem{{Name: "Odd jobs", Amount: 1000}})

	acc := accrue(t, at(led, 2026, time.June, 1), db, tab, time.UTC)
	if acc.Scheduled {
		t.Error("an unscheduled tab reported itself scheduled")
	}
	if len(acc.Posted) != 0 {
		t.Errorf("posted %d cycles on an unscheduled tab", len(acc.Posted))
	}
	if b, _ := led.Balance(context.Background(), tab.ID); b != 0 {
		t.Errorf("balance %s on an unscheduled tab, want 0", b)
	}
}

// SCHED-02 through the accrual path: period boundaries follow the instance
// timezone, so the same instant bills in Tokyo and not yet in New York.
func TestAccrueRespectsInstanceTimezone(t *testing.T) {
	newYork, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatalf("load zone: %v", err)
	}
	tokyo, err := time.LoadLocation("Asia/Tokyo")
	if err != nil {
		t.Fatalf("load zone: %v", err)
	}

	sched := schedule.Schedule{
		Kind:    schedule.MonthlyDay,
		Anchor:  schedule.NewDate(2026, time.March, 1),
		Billing: schedule.InAdvance,
	}
	items := []store.TabItem{{Name: "Service", Amount: 10000}}

	// 2026-03-01T04:00Z: still February 28th in New York, already March 1st in Tokyo.
	frozen := func(led *Service) *Service {
		return led.WithClock(func() time.Time {
			return time.Date(2026, time.March, 1, 4, 0, 0, 0, time.UTC)
		})
	}

	ledNY, dbNY, tabNY := newScheduledFixture(t, sched, items)
	if acc := accrue(t, frozen(ledNY), dbNY, tabNY, newYork); len(acc.Posted) != 0 {
		t.Errorf("New York posted %d cycles, want 0 -- it is still February there", len(acc.Posted))
	}

	ledTokyo, dbTokyo, tabTokyo := newScheduledFixture(t, sched, items)
	if acc := accrue(t, frozen(ledTokyo), dbTokyo, tabTokyo, tokyo); len(acc.Posted) != 1 {
		t.Errorf("Tokyo posted %d cycles, want 1 -- March has started there", len(acc.Posted))
	}
}

// ---------------------------------------------------------------------------
// SCHED-04: concurrent reads of an overdue tab post each cycle exactly once
// ---------------------------------------------------------------------------

func TestConcurrentAccrualPostsEachPeriodOnce(t *testing.T) {
	led, db, tab := newScheduledFixture(t,
		schedule.Schedule{
			Kind:    schedule.Weekly,
			Anchor:  schedule.NewDate(2026, time.January, 5),
			Billing: schedule.InAdvance,
		},
		[]store.TabItem{{Name: "Cleaning", Amount: 6000}},
	)
	ctx := context.Background()

	// Eight weeks overdue, then hit it from many readers at once -- the shape of
	// two people opening the same neglected tab at the same moment.
	frozen := at(led, 2026, time.March, 1)
	history, err := db.ListItemHistory(ctx, tab.ID)
	if err != nil {
		t.Fatalf("item history: %v", err)
	}

	const readers = 12
	var (
		wg     sync.WaitGroup
		mu     sync.Mutex
		posted int
		errs   []error
	)
	start := make(chan struct{})
	for range readers {
		wg.Go(func() {
			<-start
			acc, err := frozen.Accrue(ctx, tab, history, time.UTC)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err)
				return
			}
			posted += len(acc.Posted)
		})
	}
	close(start)
	wg.Wait()

	for _, err := range errs {
		t.Errorf("concurrent accrual failed: %v", err)
	}

	// Jan 5 through Mar 1 inclusive, weekly, is 8 cycles.
	const wantCycles = 8

	// Every cycle was posted, and reported posted by exactly one reader. If the
	// claim leaked, this count would exceed the number of cycles.
	if posted != wantCycles {
		t.Errorf("readers reported %d posted cycles in total, want %d -- a cycle posted more than once", posted, wantCycles)
	}

	periods, err := db.ListPostedPeriods(ctx, tab.ID)
	if err != nil {
		t.Fatalf("list periods: %v", err)
	}
	if len(periods) != wantCycles {
		t.Errorf("claimed %d cycles, want %d", len(periods), wantCycles)
	}
	seen := make(map[string]bool, len(periods))
	for _, p := range periods {
		if seen[p.Key] {
			t.Errorf("cycle %s claimed twice", p.Key)
		}
		seen[p.Key] = true
	}

	// The balance is the real assertion: one charge per cycle, no more.
	balance, err := led.Balance(ctx, tab.ID)
	if err != nil {
		t.Fatalf("balance: %v", err)
	}
	if want := money.Cents(-6000 * wantCycles); balance != want {
		t.Errorf("balance %s after %d concurrent reads, want %s", balance, readers, want)
	}

	entries, err := led.History(ctx, tab.ID)
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(entries) != wantCycles {
		t.Errorf("ledger holds %d entries, want %d -- a duplicate charge survived", len(entries), wantCycles)
	}
}

// ---------------------------------------------------------------------------
// CHG-01, CHG-02: item changes reach the next cycle and never a posted one
// ---------------------------------------------------------------------------

func TestItemChangeTakesEffectNextPeriod(t *testing.T) {
	led, db, tab := newScheduledFixture(t,
		schedule.Schedule{
			Kind:    schedule.MonthlyDay,
			Anchor:  schedule.NewDate(2026, time.January, 1),
			Billing: schedule.InAdvance,
		},
		[]store.TabItem{{Name: "Rent", Amount: 100000}},
	)
	ctx := context.Background()

	// January bills at the original amount.
	accrue(t, at(led, 2026, time.January, 5), db, tab, time.UTC)
	januaryBalance, _ := led.Balance(ctx, tab.ID)
	if januaryBalance != -100000 {
		t.Fatalf("January balance %s, want -1,000.00", januaryBalance)
	}

	entries, err := led.History(ctx, tab.ID)
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	januaryEntry := entries[0]
	januaryItems, err := db.ListEntryItems(ctx, januaryEntry.Seq)
	if err != nil {
		t.Fatalf("entry items: %v", err)
	}

	// The Provider raises the rent partway through January. UpdateItem stamps
	// the supersede with the wall clock, so the test dates it into the frozen
	// timeline the way it would fall in real use: after January posted, before
	// February comes due.
	items, err := db.ListItems(ctx, tab.ID)
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	if _, err := db.UpdateItem(ctx, items[0].ID, "Rent", 110000); err != nil {
		t.Fatalf("update item: %v", err)
	}
	changedAt := time.Date(2026, time.January, 20, 0, 0, 0, 0, time.UTC)
	history, err := db.ListItemHistory(ctx, tab.ID)
	if err != nil {
		t.Fatalf("item history: %v", err)
	}
	if len(history) != 2 {
		t.Fatalf("item history holds %d rows, want 2 -- an update must supersede, not overwrite", len(history))
	}
	history[0].RemovedAt = &changedAt
	history[1].CreatedAt = changedAt

	// January's posted entry is untouched -- amount and breakdown both.
	after, err := db.GetEntry(ctx, januaryEntry.Seq)
	if err != nil {
		t.Fatalf("re-read entry: %v", err)
	}
	if after.Amount != januaryEntry.Amount {
		t.Errorf("January charge became %s after an item change, want %s", after.Amount, januaryEntry.Amount)
	}
	afterItems, err := db.ListEntryItems(ctx, januaryEntry.Seq)
	if err != nil {
		t.Fatalf("entry items: %v", err)
	}
	if len(afterItems) != len(januaryItems) || afterItems[0].Amount != januaryItems[0].Amount {
		t.Errorf("January breakdown became %+v, want %+v", afterItems, januaryItems)
	}

	// February bills at the new amount.
	acc, err := at(led, 2026, time.February, 3).Accrue(ctx, tab, history, time.UTC)
	if err != nil {
		t.Fatalf("accrue: %v", err)
	}
	if len(acc.Posted) != 1 {
		t.Fatalf("February posted %d cycles, want 1", len(acc.Posted))
	}
	if acc.Posted[0].Amount != -110000 {
		t.Errorf("February charge %s, want -1,100.00", acc.Posted[0].Amount)
	}
	if b, _ := led.Balance(ctx, tab.ID); b != -210000 {
		t.Errorf("balance %s, want -2,100.00", b)
	}
}

// Catching up must bill each cycle for the items the tab carried at the time,
// not for the items it carries now (CHG-02).
func TestCatchUpBillsHistoricalItems(t *testing.T) {
	led, db, tab := newScheduledFixture(t,
		schedule.Schedule{
			Kind:    schedule.MonthlyDay,
			Anchor:  schedule.NewDate(2026, time.January, 1),
			Billing: schedule.InAdvance,
		},
		[]store.TabItem{{Name: "Rent", Amount: 100000}},
	)
	ctx := context.Background()

	// The amount changes in March, while nobody has opened the tab since
	// December. UpdateItem stamps the change with the wall clock, so the change
	// is dated by hand to sit between the January and April cycles.
	items, err := db.ListItems(ctx, tab.ID)
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	if _, err := db.UpdateItem(ctx, items[0].ID, "Rent", 130000); err != nil {
		t.Fatalf("update item: %v", err)
	}
	history, err := db.ListItemHistory(ctx, tab.ID)
	if err != nil {
		t.Fatalf("item history: %v", err)
	}
	if len(history) != 2 {
		t.Fatalf("item history holds %d rows, want 2 -- the update should supersede, not overwrite", len(history))
	}
	// Re-date the supersede to March 1st so the reconstruction has something to
	// distinguish. The rows are ordered oldest first within a position.
	changedAt := time.Date(2026, time.March, 1, 0, 0, 0, 0, time.UTC)
	history[0].RemovedAt = &changedAt
	history[1].CreatedAt = changedAt

	acc, err := at(led, 2026, time.April, 10).Accrue(ctx, tab, history, time.UTC)
	if err != nil {
		t.Fatalf("accrue: %v", err)
	}
	if len(acc.Posted) != 4 {
		t.Fatalf("posted %d cycles, want 4 (January through April)", len(acc.Posted))
	}

	want := []money.Cents{-100000, -100000, -130000, -130000} // Jan, Feb, Mar, Apr
	for i, e := range acc.Posted {
		if e.Amount != want[i] {
			t.Errorf("cycle %d charged %s, want %s -- catch-up billed the wrong era's items", i, e.Amount, want[i])
		}
	}
}

// A cycle that came due before the tab existed is billed for the items the tab
// was created with. Without the clamp a backdated anchor posts a run of zeroes.
func TestBackdatedAnchorBillsCreationItems(t *testing.T) {
	led, db, tab := newScheduledFixture(t,
		schedule.Schedule{
			Kind:    schedule.MonthlyDay,
			Anchor:  schedule.NewDate(2025, time.October, 1),
			Billing: schedule.InAdvance,
		},
		[]store.TabItem{{Name: "Backdated service", Amount: 7500}},
	)

	// The fixture creates the tab on 2025-12-01, two cycles after the anchor.
	acc := accrue(t, at(led, 2025, time.December, 5), db, tab, time.UTC)
	if len(acc.Posted) != 3 {
		t.Fatalf("posted %d cycles, want 3 (October through December)", len(acc.Posted))
	}
	for i, e := range acc.Posted {
		if e.Amount != -7500 {
			t.Errorf("cycle %d charged %s, want -75.00 -- a backdated cycle billed nothing", i, e.Amount)
		}
	}
}

// ItemsAsOf is the pure part of item reconstruction, so it gets pinned directly.
func TestItemsAsOf(t *testing.T) {
	stamp := func(y int, m time.Month, d int) time.Time {
		return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
	}
	removed := stamp(2026, time.March, 1)

	history := []store.TabItem{
		{Name: "Always", Amount: 100, CreatedAt: stamp(2026, time.January, 1)},
		{Name: "Old", Amount: 200, CreatedAt: stamp(2026, time.January, 1), RemovedAt: &removed},
		{Name: "New", Amount: 300, CreatedAt: removed},
	}

	cases := []struct {
		at    time.Time
		names []string
	}{
		{stamp(2025, time.December, 1), nil},
		{stamp(2026, time.February, 1), []string{"Always", "Old"}},
		// Removal takes effect at its own instant: "Old" is already gone and
		// "New" is already present.
		{removed, []string{"Always", "New"}},
		{stamp(2026, time.April, 1), []string{"Always", "New"}},
	}

	for _, tc := range cases {
		got := ItemsAsOf(history, tc.at)
		names := make([]string, 0, len(got))
		for _, it := range got {
			names = append(names, it.Name)
		}
		if fmt.Sprint(names) != fmt.Sprint(tc.names) {
			t.Errorf("ItemsAsOf(%s) = %v, want %v", tc.at.Format(time.DateOnly), names, tc.names)
		}
	}
}

// CHG-01: a posted cycle carries its own breakdown, so both parties can see
// which line changed and when.
func TestPostedPeriodSnapshotsItems(t *testing.T) {
	led, db, tab := newScheduledFixture(t,
		schedule.Schedule{
			Kind:    schedule.MonthlyDay,
			Anchor:  schedule.NewDate(2026, time.January, 1),
			Billing: schedule.InAdvance,
		},
		[]store.TabItem{
			{Name: "Base", Amount: 5000},
			{Name: "Extras", Amount: 1250},
		},
	)
	ctx := context.Background()

	acc := accrue(t, at(led, 2026, time.January, 2), db, tab, time.UTC)
	if len(acc.Posted) != 1 {
		t.Fatalf("posted %d cycles, want 1", len(acc.Posted))
	}

	items, err := db.ListEntryItems(ctx, acc.Posted[0].Seq)
	if err != nil {
		t.Fatalf("entry items: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("snapshot holds %d items, want 2", len(items))
	}
	if items[0].Name != "Base" || items[0].Amount != 5000 {
		t.Errorf("first snapshot item %+v, want Base at 50.00", items[0])
	}
	if items[1].Name != "Extras" || items[1].Amount != 1250 {
		t.Errorf("second snapshot item %+v, want Extras at 12.50", items[1])
	}

	total, ok := SumItems(items)
	if !ok {
		t.Fatal("snapshot total overflowed")
	}
	if total.Neg() != acc.Posted[0].Amount {
		t.Errorf("snapshot totals %s but the charge is %s", total.Neg(), acc.Posted[0].Amount)
	}
}

func TestPeriodIdempotencyKeyIsDerived(t *testing.T) {
	// The same cycle always posts under the same key, which is what makes the
	// entries table a second guard against a double post.
	a := PeriodIdempotencyKey(7, "2026-01-01")
	b := PeriodIdempotencyKey(7, "2026-01-01")
	if a != b {
		t.Errorf("key is not deterministic: %q then %q", a, b)
	}
	if PeriodIdempotencyKey(8, "2026-01-01") == a {
		t.Error("different tabs share a period key")
	}
	if PeriodIdempotencyKey(7, "2026-02-01") == a {
		t.Error("different cycles share a period key")
	}
}
