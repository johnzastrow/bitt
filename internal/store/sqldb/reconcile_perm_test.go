package sqldb

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/johnzastrow/bitt/internal/store"
)

// RECON-01: the "Can reconcile" permission and the instance switch. These run
// on SQLite by default and on MariaDB when BITT_TEST_MARIADB_DSN is set; the
// CHECK constraint is written differently on each, so both runs matter.

func mustAdmin(t *testing.T, db *DB, email string) store.User {
	t.Helper()
	u, err := db.CreateUser(context.Background(), store.User{
		Email: email, DisplayName: email, PasswordHash: "$argon2id$placeholder", IsAdmin: true,
	})
	if err != nil {
		t.Fatalf("create admin: %v", err)
	}
	return u
}

func canReconcile(t *testing.T, db *DB, id int64) bool {
	t.Helper()
	u, err := db.GetUser(context.Background(), id)
	if err != nil {
		t.Fatalf("get user: %v", err)
	}
	return u.CanReconcile
}

func TestCanReconcileDefaultsOff(t *testing.T) {
	db := newTestDB(t)
	admin := mustAdmin(t, db, "admin@example.com")
	plain := mustUser(t, db, "plain@example.com")

	for _, u := range []store.User{admin, plain} {
		if canReconcile(t, db, u.ID) {
			t.Errorf("%s holds Can reconcile by default", u.Email)
		}
	}
}

func TestSetCanReconcile(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	admin := mustAdmin(t, db, "admin@example.com")
	plain := mustUser(t, db, "plain@example.com")

	// Grant, then grant again: the second is a no-op UPDATE, which MariaDB
	// reports as zero affected rows unless found-rows is on. It must not turn
	// into ErrNotAdmin or ErrNotFound.
	for i := 0; i < 2; i++ {
		if err := db.SetCanReconcile(ctx, admin.ID, true); err != nil {
			t.Fatalf("grant #%d: %v", i+1, err)
		}
	}
	if !canReconcile(t, db, admin.ID) {
		t.Fatal("grant did not stick")
	}

	// Remove, and remove again.
	for i := 0; i < 2; i++ {
		if err := db.SetCanReconcile(ctx, admin.ID, false); err != nil {
			t.Fatalf("remove #%d: %v", i+1, err)
		}
	}
	if canReconcile(t, db, admin.ID) {
		t.Fatal("remove did not stick")
	}

	// A non-administrator cannot be granted it; removing from one is harmless.
	if err := db.SetCanReconcile(ctx, plain.ID, true); !errors.Is(err, store.ErrNotAdmin) {
		t.Errorf("grant to non-admin: got %v, want ErrNotAdmin", err)
	}
	if canReconcile(t, db, plain.ID) {
		t.Error("non-admin holds the permission after a refused grant")
	}
	if err := db.SetCanReconcile(ctx, plain.ID, false); err != nil {
		t.Errorf("remove from non-admin: %v", err)
	}

	// Unknown accounts are not found either way.
	for _, on := range []bool{true, false} {
		if err := db.SetCanReconcile(ctx, 999999, on); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("set(%v) on unknown account: got %v, want ErrNotFound", on, err)
		}
	}
}

// The schema itself refuses a non-administrator holding the permission, so a
// future "remove administrator" action cannot forget to clear it: demoting
// alone fails, demoting and clearing in one statement succeeds.
func TestCanReconcileRequiresAdminInSchema(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	admin := mustAdmin(t, db, "admin@example.com")
	plain := mustUser(t, db, "plain@example.com")
	if err := db.SetCanReconcile(ctx, admin.ID, true); err != nil {
		t.Fatal(err)
	}

	if _, err := db.db.ExecContext(ctx,
		`UPDATE users SET is_admin = 0 WHERE id = ?`, admin.ID); err == nil {
		t.Error("demoting a holder without clearing the permission was accepted")
	}
	if !canReconcile(t, db, admin.ID) {
		t.Error("a refused demotion still changed the row")
	}

	if _, err := db.db.ExecContext(ctx,
		`UPDATE users SET can_reconcile = 1 WHERE id = ?`, plain.ID); err == nil {
		t.Error("granting a non-admin directly was accepted by the schema")
	}
	if _, err := db.db.ExecContext(ctx,
		`UPDATE users SET can_reconcile = 2 WHERE id = ?`, admin.ID); err == nil {
		t.Error("a can_reconcile value other than 0 or 1 was accepted")
	}

	if _, err := db.db.ExecContext(ctx,
		`UPDATE users SET is_admin = 0, can_reconcile = 0 WHERE id = ?`, admin.ID); err != nil {
		t.Fatalf("demote and clear together: %v", err)
	}
	u, err := db.GetUser(ctx, admin.ID)
	if err != nil {
		t.Fatal(err)
	}
	if u.IsAdmin || u.CanReconcile {
		t.Errorf("after demotion: admin=%v reconcile=%v", u.IsAdmin, u.CanReconcile)
	}
}

// The authenticated user on every request comes from GetSession, which lists
// its columns separately; the permission must arrive through it.
func TestSessionUserCarriesCanReconcile(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	admin := mustAdmin(t, db, "admin@example.com")
	if err := db.SetCanReconcile(ctx, admin.ID, true); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := db.CreateSession(ctx, store.Session{
		TokenHash: "h1", UserID: admin.ID, CreatedAt: now, ExpiresAt: now.Add(time.Hour), LastSeenAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	_, u, err := db.GetSession(ctx, "h1")
	if err != nil {
		t.Fatal(err)
	}
	if !u.CanReconcile {
		t.Error("session user lost CanReconcile")
	}

	users, err := db.ListUsers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(users) != 1 || !users[0].CanReconcile {
		t.Errorf("ListUsers lost CanReconcile: %+v", users)
	}
}

// Concurrent grants and removals on one account leave it in one of the two
// states, never an error; this is mostly for MariaDB's parallel writers.
func TestSetCanReconcileConcurrent(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	admin := mustAdmin(t, db, "admin@example.com")

	var wg sync.WaitGroup
	errs := make(chan error, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(on bool) {
			defer wg.Done()
			errs <- db.SetCanReconcile(ctx, admin.ID, on)
		}(i%2 == 0)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("concurrent set: %v", err)
		}
	}
}

func TestMayReconcile(t *testing.T) {
	holder := store.User{IsAdmin: true, CanReconcile: true}
	gone := holder
	gone.DeactivatedAt = new(time.Time)
	cases := []struct {
		name string
		u    store.User
		want bool
	}{
		{"holder", holder, true},
		{"admin without permission", store.User{IsAdmin: true}, false},
		{"permission without admin (impossible in schema)", store.User{CanReconcile: true}, false},
		{"deactivated holder", gone, false},
		{"plain account", store.User{}, false},
	}
	for _, c := range cases {
		if got := c.u.MayReconcile(); got != c.want {
			t.Errorf("%s: MayReconcile = %v, want %v", c.name, got, c.want)
		}
	}
}
