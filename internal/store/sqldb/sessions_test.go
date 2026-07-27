package sqldb

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/johnzastrow/bitt/internal/store"
)

// Sessions are the whole of "who is signed in", so their store methods are
// security-load-bearing: a session must resolve only while it is unexpired and
// its user active, logout must remove it, a password change must be able to end
// every other session, and expiry must be prunable. This exercises that
// lifecycle end to end on whichever backend is under test.
func TestSessionLifecycle(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	u := mustUser(t, db, "s@example.com")

	// A fixed clock so expiry comparisons are deterministic. The store compares
	// against its own now(), so "past"/"future" are relative to real time.
	// Truncated to seconds because the store persists times at that granularity;
	// a nanosecond-precise value would not round-trip equal.
	now := time.Now().UTC().Truncate(time.Second)
	live := store.Session{
		TokenHash: "live-hash", UserID: u.ID,
		CreatedAt: now, ExpiresAt: now.Add(time.Hour), LastSeenAt: now,
	}
	if err := db.CreateSession(ctx, live); err != nil {
		t.Fatalf("create session: %v", err)
	}

	// A live session resolves to its user.
	got, gotUser, err := db.GetSession(ctx, "live-hash")
	if err != nil {
		t.Fatalf("get live session: %v", err)
	}
	if got.UserID != u.ID || gotUser.ID != u.ID || gotUser.Email != "s@example.com" {
		t.Errorf("session/user mismatch: %+v / %+v", got, gotUser)
	}

	// TouchSession advances last_seen without disturbing the rest.
	later := now.Add(30 * time.Minute)
	if err := db.TouchSession(ctx, "live-hash", later); err != nil {
		t.Fatalf("touch: %v", err)
	}
	if got, _, _ = db.GetSession(ctx, "live-hash"); !got.LastSeenAt.Equal(later) {
		t.Errorf("last_seen = %v, want %v", got.LastSeenAt, later)
	}

	// An expired session fails closed -- ErrNotFound, not a stale-but-valid hit.
	expired := store.Session{
		TokenHash: "expired-hash", UserID: u.ID,
		CreatedAt: now.Add(-2 * time.Hour), ExpiresAt: now.Add(-time.Hour), LastSeenAt: now.Add(-2 * time.Hour),
	}
	if err := db.CreateSession(ctx, expired); err != nil {
		t.Fatalf("create expired: %v", err)
	}
	if _, _, err := db.GetSession(ctx, "expired-hash"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("expired session resolved (err=%v), must fail closed", err)
	}

	// A deactivated user's live session also fails closed.
	if err := db.SetUserActive(ctx, u.ID, false); err != nil {
		t.Fatalf("deactivate: %v", err)
	}
	if _, _, err := db.GetSession(ctx, "live-hash"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("deactivated user's session resolved (err=%v), must fail closed", err)
	}
	if err := db.SetUserActive(ctx, u.ID, true); err != nil {
		t.Fatalf("reactivate: %v", err)
	}
}

// DeleteSessionsForUserExcept is what a password change runs to log out every
// other device while keeping the one making the request.
func TestDeleteSessionsForUserExcept(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	u := mustUser(t, db, "s@example.com")
	now := time.Now().UTC()

	for _, h := range []string{"keep", "other-1", "other-2"} {
		if err := db.CreateSession(ctx, store.Session{
			TokenHash: h, UserID: u.ID, CreatedAt: now, ExpiresAt: now.Add(time.Hour), LastSeenAt: now,
		}); err != nil {
			t.Fatalf("create %s: %v", h, err)
		}
	}

	n, err := db.DeleteSessionsForUserExcept(ctx, u.ID, "keep")
	if err != nil {
		t.Fatalf("delete others: %v", err)
	}
	if n != 2 {
		t.Errorf("deleted %d, want 2", n)
	}
	if _, _, err := db.GetSession(ctx, "keep"); err != nil {
		t.Errorf("the kept session was removed: %v", err)
	}
	if _, _, err := db.GetSession(ctx, "other-1"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("other-1 survived (err=%v)", err)
	}
}

// DeleteSession is logout; DeleteExpiredSessions is the pruner.
func TestDeleteAndPruneSessions(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	u := mustUser(t, db, "s@example.com")
	now := time.Now().UTC()

	_ = db.CreateSession(ctx, store.Session{TokenHash: "logout-me", UserID: u.ID, CreatedAt: now, ExpiresAt: now.Add(time.Hour), LastSeenAt: now})
	if err := db.DeleteSession(ctx, "logout-me"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, _, err := db.GetSession(ctx, "logout-me"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("session survived logout (err=%v)", err)
	}

	// Two expired and one live; pruning removes exactly the two expired.
	_ = db.CreateSession(ctx, store.Session{TokenHash: "exp-1", UserID: u.ID, CreatedAt: now.Add(-2 * time.Hour), ExpiresAt: now.Add(-time.Hour), LastSeenAt: now})
	_ = db.CreateSession(ctx, store.Session{TokenHash: "exp-2", UserID: u.ID, CreatedAt: now.Add(-2 * time.Hour), ExpiresAt: now.Add(-time.Minute), LastSeenAt: now})
	_ = db.CreateSession(ctx, store.Session{TokenHash: "still-live", UserID: u.ID, CreatedAt: now, ExpiresAt: now.Add(time.Hour), LastSeenAt: now})

	pruned, err := db.DeleteExpiredSessions(ctx, now)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if pruned != 2 {
		t.Errorf("pruned %d, want 2", pruned)
	}
	if _, _, err := db.GetSession(ctx, "still-live"); err != nil {
		t.Errorf("the live session was pruned: %v", err)
	}
}

// CountUsers underlies first-run detection (no users -> show setup).
func TestCountUsers(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	if n, err := db.CountUsers(ctx); err != nil || n != 0 {
		t.Fatalf("fresh CountUsers = %d, err=%v, want 0", n, err)
	}
	mustUser(t, db, "a@example.com")
	mustUser(t, db, "b@example.com")
	if n, err := db.CountUsers(ctx); err != nil || n != 2 {
		t.Errorf("CountUsers = %d, err=%v, want 2", n, err)
	}
}
