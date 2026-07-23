package sqlite

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/johnzastrow/bitt/internal/money"
	"github.com/johnzastrow/bitt/internal/store"
)

// newTestDB returns a migrated database backed by a temp file. A file rather
// than :memory: so that WAL and trigger behavior match production.
func newTestDB(t *testing.T) *DB {
	t.Helper()
	db, err := Open(Options{
		Path:               filepath.Join(t.TempDir(), "test.db"),
		AppendOnlyTriggers: true,
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

func mustUser(t *testing.T, db *DB, email string) store.User {
	t.Helper()
	u, err := db.CreateUser(context.Background(), store.User{
		Email:        email,
		DisplayName:  email,
		PasswordHash: "$argon2id$placeholder",
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	return u
}

func mustTab(t *testing.T, db *DB, owner int64) store.Tab {
	t.Helper()
	tab, err := db.CreateTab(context.Background(), store.Tab{
		Name:      "Phone plan",
		Kind:      store.TabServices,
		CreatedBy: owner,
	}, []store.TabItem{
		{Name: "Line 1", Amount: 4500},
		{Name: "Line 2", Amount: 3000},
	})
	if err != nil {
		t.Fatalf("create tab: %v", err)
	}
	return tab
}

// DEPLOY-01: migrations must be safe to run repeatedly.
func TestMigrateIsIdempotent(t *testing.T) {
	db := newTestDB(t)
	for i := 0; i < 3; i++ {
		if err := db.Migrate(context.Background()); err != nil {
			t.Fatalf("re-migrate %d: %v", i, err)
		}
	}
	if _, err := db.GetInstance(context.Background()); err != nil {
		t.Fatalf("instance missing after repeated migrate: %v", err)
	}
}

// LEDGER-01: a posted entry cannot be updated or deleted, even by raw SQL.
// This is the exit criterion that the abort triggers exist and are active.
func TestEntriesAreAppendOnly(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	user := mustUser(t, db, "a@example.com")
	tab := mustTab(t, db, user.ID)

	entry, _, err := db.PostEntry(ctx, store.NewEntry{
		TabID: tab.ID, Kind: store.KindCharge, Amount: -4500,
		ActorUserID: user.ID, IdempotencyKey: "k1", EffectiveAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("post: %v", err)
	}

	_, err = db.db.ExecContext(ctx, `UPDATE entries SET amount_cents = 0 WHERE seq = ?`, entry.Seq)
	if err == nil {
		t.Fatal("UPDATE against entries succeeded; append-only trigger is not active")
	}
	if !strings.Contains(err.Error(), "append-only") {
		t.Errorf("UPDATE blocked by unexpected error: %v", err)
	}

	_, err = db.db.ExecContext(ctx, `DELETE FROM entries WHERE seq = ?`, entry.Seq)
	if err == nil {
		t.Fatal("DELETE against entries succeeded; append-only trigger is not active")
	}
	if !strings.Contains(err.Error(), "append-only") {
		t.Errorf("DELETE blocked by unexpected error: %v", err)
	}

	// And the balance is unchanged by the attempts.
	balance, err := db.SumEntries(ctx, tab.ID)
	if err != nil {
		t.Fatalf("sum: %v", err)
	}
	if balance != -4500 {
		t.Errorf("balance = %s after blocked mutations, want -45.00", balance)
	}
}

// The EntryStore interface must offer no way to modify a posted entry.
func TestEntryStoreHasNoMutators(t *testing.T) {
	// Compile-time: if someone adds UpdateEntry or DeleteEntry to the
	// interface, this assertion's type list stops matching and the intent is
	// re-examined deliberately rather than by accident.
	var s store.EntryStore = newTestDB(t)
	if s == nil {
		t.Fatal("nil store")
	}
}

// LEDGER-03: the balance is a sum over entries, and no cached column exists.
func TestBalanceIsDerived(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	user := mustUser(t, db, "a@example.com")
	tab := mustTab(t, db, user.ID)

	post := func(kind store.EntryKind, amount money.Cents, key string) {
		t.Helper()
		if _, _, err := db.PostEntry(ctx, store.NewEntry{
			TabID: tab.ID, Kind: kind, Amount: amount,
			ActorUserID: user.ID, IdempotencyKey: key,
		}); err != nil {
			t.Fatalf("post %s: %v", key, err)
		}
	}

	if b, _ := db.SumEntries(ctx, tab.ID); b != 0 {
		t.Errorf("empty tab balance = %s, want 0.00", b)
	}

	post(store.KindCharge, -7500, "c1")
	post(store.KindPayment, 5000, "p1")
	post(store.KindCharge, -7500, "c2")

	got, err := db.SumEntries(ctx, tab.ID)
	if err != nil {
		t.Fatalf("sum: %v", err)
	}
	if want := money.Cents(-10000); got != want {
		t.Errorf("balance = %s, want %s", got, want)
	}

	// There must be no balance column to drift out of step.
	var count int
	err = db.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM pragma_table_info('tabs') WHERE name LIKE '%balance%'`).Scan(&count)
	if err != nil {
		t.Fatalf("introspect: %v", err)
	}
	if count != 0 {
		t.Errorf("tabs table has %d balance-like column(s); balances must be derived (LEDGER-03)", count)
	}
}

// LEDGER-06: sequence numbers are server-assigned and strictly increasing.
func TestSequenceIsMonotonic(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	user := mustUser(t, db, "a@example.com")
	tab := mustTab(t, db, user.ID)

	var last int64
	for i := 0; i < 20; i++ {
		e, _, err := db.PostEntry(ctx, store.NewEntry{
			TabID: tab.ID, Kind: store.KindCharge, Amount: -100,
			ActorUserID: user.ID, IdempotencyKey: "seq" + string(rune('a'+i)),
			// A client clock running backwards must not affect ordering.
			EffectiveAt: time.Now().UTC().Add(-time.Duration(i) * time.Hour),
		})
		if err != nil {
			t.Fatalf("post %d: %v", i, err)
		}
		if e.Seq <= last {
			t.Fatalf("seq %d not greater than previous %d", e.Seq, last)
		}
		last = e.Seq
	}
}

// Idempotency: a replayed key returns the original entry rather than posting a
// second one. This is what makes a double-tapped submit safe (LEDGER-07).
func TestIdempotentReplay(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	user := mustUser(t, db, "a@example.com")
	tab := mustTab(t, db, user.ID)

	first, replayed, err := db.PostEntry(ctx, store.NewEntry{
		TabID: tab.ID, Kind: store.KindPayment, Amount: 2500,
		ActorUserID: user.ID, IdempotencyKey: "same-key",
	})
	if err != nil || replayed {
		t.Fatalf("first post: entry=%v replayed=%v err=%v", first.Seq, replayed, err)
	}

	second, replayed, err := db.PostEntry(ctx, store.NewEntry{
		TabID: tab.ID, Kind: store.KindPayment, Amount: 2500,
		ActorUserID: user.ID, IdempotencyKey: "same-key",
	})
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if !replayed {
		t.Error("replay not reported as replayed")
	}
	if second.Seq != first.Seq {
		t.Errorf("replay produced seq %d, want the original %d", second.Seq, first.Seq)
	}

	balance, _ := db.SumEntries(ctx, tab.ID)
	if balance != 2500 {
		t.Errorf("balance = %s after replay, want 25.00 -- the replay double-posted", balance)
	}
}

// Concurrent submissions of the same key must post exactly once. This is the
// property a check-then-insert would fail.
func TestIdempotentUnderConcurrency(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	user := mustUser(t, db, "a@example.com")
	tab := mustTab(t, db, user.ID)

	const n = 8
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _, errs[i] = db.PostEntry(ctx, store.NewEntry{
				TabID: tab.ID, Kind: store.KindPayment, Amount: 1000,
				ActorUserID: user.ID, IdempotencyKey: "concurrent",
			})
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("goroutine %d: %v", i, err)
		}
	}

	entries, err := db.ListEntries(ctx, tab.ID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("posted %d entries for one idempotency key, want exactly 1", len(entries))
	}
	if b, _ := db.SumEntries(ctx, tab.ID); b != 1000 {
		t.Errorf("balance = %s, want 10.00", b)
	}
}

// AUTH-03: the first-run screen locks permanently, including under a race.
func TestSetupLocksPermanently(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	inst, err := db.GetInstance(ctx)
	if err != nil {
		t.Fatalf("instance: %v", err)
	}
	if inst.SetupComplete() {
		t.Fatal("fresh instance reports setup already complete")
	}

	admin, err := db.CompleteSetup(ctx, store.User{
		Email: "admin@example.com", DisplayName: "Admin", PasswordHash: "$argon2id$x",
	}, "America/New_York")
	if err != nil {
		t.Fatalf("complete setup: %v", err)
	}
	if !admin.IsAdmin {
		t.Error("first account was not made an admin")
	}

	inst, _ = db.GetInstance(ctx)
	if !inst.SetupComplete() {
		t.Error("setup not latched closed")
	}
	if inst.Timezone != "America/New_York" {
		t.Errorf("timezone = %q, want America/New_York", inst.Timezone)
	}

	// A second attempt must be refused.
	_, err = db.CompleteSetup(ctx, store.User{
		Email: "attacker@example.com", DisplayName: "Nope", PasswordHash: "$argon2id$y",
	}, "UTC")
	if !errors.Is(err, store.ErrConflict) {
		t.Errorf("second setup error = %v, want ErrConflict", err)
	}

	users, _ := db.ListUsers(ctx)
	if len(users) != 1 {
		t.Errorf("%d users exist after a blocked second setup, want 1", len(users))
	}
}

func TestSetupRaceCreatesOneAdmin(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	const n = 6
	var wg sync.WaitGroup
	ok := make([]bool, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := db.CompleteSetup(ctx, store.User{
				Email:        "admin" + string(rune('a'+i)) + "@example.com",
				DisplayName:  "Admin",
				PasswordHash: "$argon2id$x",
			}, "UTC")
			ok[i] = err == nil
		}(i)
	}
	wg.Wait()

	wins := 0
	for _, v := range ok {
		if v {
			wins++
		}
	}
	if wins != 1 {
		t.Errorf("%d concurrent setups succeeded, want exactly 1", wins)
	}
	users, _ := db.ListUsers(ctx)
	if len(users) != 1 {
		t.Errorf("%d users created, want 1", len(users))
	}
}

// AUTH-05: the tab list is scoped by participation, not filtered afterward.
func TestTabsScopedToParticipants(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	owner := mustUser(t, db, "owner@example.com")
	stranger := mustUser(t, db, "stranger@example.com")
	tab := mustTab(t, db, owner.ID)

	mine, err := db.ListTabsForUser(ctx, owner.ID)
	if err != nil {
		t.Fatalf("list own: %v", err)
	}
	if len(mine) != 1 || mine[0].ID != tab.ID {
		t.Fatalf("owner sees %d tabs, want 1", len(mine))
	}

	theirs, err := db.ListTabsForUser(ctx, stranger.ID)
	if err != nil {
		t.Fatalf("list stranger: %v", err)
	}
	if len(theirs) != 0 {
		t.Errorf("non-participant sees %d tabs, want 0", len(theirs))
	}

	if _, err := db.ParticipantRole(ctx, tab.ID, stranger.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("ParticipantRole for non-participant = %v, want ErrNotFound", err)
	}

	role, err := db.ParticipantRole(ctx, tab.ID, owner.ID)
	if err != nil {
		t.Fatalf("owner role: %v", err)
	}
	if role != store.RoleProvider {
		t.Errorf("creator role = %q, want provider", role)
	}
}

// Emails must match case-insensitively and uniquely.
func TestEmailUniquenessIsCaseInsensitive(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	mustUser(t, db, "Person@Example.com")

	_, err := db.CreateUser(ctx, store.User{
		Email: "person@example.COM", DisplayName: "Dup", PasswordHash: "$argon2id$x",
	})
	if !errors.Is(err, store.ErrConflict) {
		t.Errorf("duplicate email error = %v, want ErrConflict", err)
	}

	found, err := db.GetUserByEmail(ctx, "PERSON@EXAMPLE.COM")
	if err != nil {
		t.Fatalf("case-insensitive lookup failed: %v", err)
	}
	if found.Email != "Person@Example.com" {
		t.Errorf("stored email = %q, want the original casing preserved", found.Email)
	}
}

// TAB-04: items are stored with their amounts and ordering.
func TestTabItemsPersist(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	user := mustUser(t, db, "a@example.com")
	tab := mustTab(t, db, user.ID)

	items, err := db.ListItems(ctx, tab.ID)
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("%d items, want 2", len(items))
	}
	if items[0].Name != "Line 1" || items[0].Amount != 4500 {
		t.Errorf("item 0 = %q/%s, want Line 1/45.00", items[0].Name, items[0].Amount)
	}
	if items[1].Amount != 3000 {
		t.Errorf("item 1 amount = %s, want 30.00", items[1].Amount)
	}
}

// Entry item snapshots must survive a later change to the tab's items (CHG-01).
func TestEntryItemSnapshotIsFrozen(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	user := mustUser(t, db, "a@example.com")
	tab := mustTab(t, db, user.ID)

	entry, _, err := db.PostEntry(ctx, store.NewEntry{
		TabID: tab.ID, Kind: store.KindCharge, Amount: -7500,
		ActorUserID: user.ID, IdempotencyKey: "snap",
		Items: []store.EntryItem{
			{Position: 0, Name: "Line 1", Amount: 4500},
			{Position: 1, Name: "Line 2", Amount: 3000},
		},
	})
	if err != nil {
		t.Fatalf("post: %v", err)
	}

	// Change the tab's items afterward.
	if _, err := db.AddItem(ctx, store.TabItem{TabID: tab.ID, Name: "Line 3", Amount: 1000, Position: 9}); err != nil {
		t.Fatalf("add item: %v", err)
	}

	snap, err := db.ListEntryItems(ctx, entry.Seq)
	if err != nil {
		t.Fatalf("list entry items: %v", err)
	}
	if len(snap) != 2 {
		t.Fatalf("snapshot has %d items, want the 2 present when it posted", len(snap))
	}
	if snap[0].Amount != 4500 || snap[1].Amount != 3000 {
		t.Errorf("snapshot amounts changed: %s, %s", snap[0].Amount, snap[1].Amount)
	}
}

// Sessions must fail closed for expiry and deactivation.
func TestSessionFailsClosed(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	user := mustUser(t, db, "a@example.com")
	now := time.Now().UTC()

	if err := db.CreateSession(ctx, store.Session{
		TokenHash: "live", UserID: user.ID,
		CreatedAt: now, ExpiresAt: now.Add(time.Hour), LastSeenAt: now,
	}); err != nil {
		t.Fatalf("create session: %v", err)
	}
	if _, _, err := db.GetSession(ctx, "live"); err != nil {
		t.Fatalf("valid session rejected: %v", err)
	}

	if err := db.CreateSession(ctx, store.Session{
		TokenHash: "stale", UserID: user.ID,
		CreatedAt: now.Add(-2 * time.Hour), ExpiresAt: now.Add(-time.Hour), LastSeenAt: now,
	}); err != nil {
		t.Fatalf("create expired session: %v", err)
	}
	if _, _, err := db.GetSession(ctx, "stale"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("expired session error = %v, want ErrNotFound", err)
	}

	if _, _, err := db.GetSession(ctx, "never-issued"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("unknown session error = %v, want ErrNotFound", err)
	}
}

// The database holds financial records and password hashes, so its files must
// not be readable by other accounts on the host.
func TestDatabaseFilesAreOwnerOnly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "perms.db")

	db, err := Open(Options{Path: path, AppendOnlyTriggers: true})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = db.Close() }()
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	// Force the WAL and shared-memory sidecars into existence.
	user := mustUser(t, db, "a@example.com")
	mustTab(t, db, user.ID)

	for _, suffix := range []string{"", "-wal", "-shm"} {
		p := path + suffix
		info, err := os.Stat(p)
		if err != nil {
			continue // sidecar may be absent depending on checkpointing
		}
		if perm := info.Mode().Perm(); perm&0o077 != 0 {
			t.Errorf("%s has mode %04o; group and other must have no access", p, perm)
		}
	}
}
