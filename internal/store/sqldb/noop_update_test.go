package sqldb

import (
	"context"
	"testing"
	"time"

	"github.com/johnzastrow/bitt/internal/fee"
	"github.com/johnzastrow/bitt/internal/schedule"
	"github.com/johnzastrow/bitt/internal/store"
)

// TestNoOpUpdatesSucceed guards the whole class of bug that reached production:
// an UPDATE that writes a row's existing values back changes no column, MariaDB
// reports zero affected rows, and a "n == 0 -> ErrNotFound" check turns an
// idempotent re-save into a 500. Two of these were hit live (POST /tabs/1/schedule
// and /tabs/1/fee) before the clientFoundRows fix. Every Set/Update method that
// keys on a primary id shares the shape, so each is exercised twice here with
// identical values; the SECOND call is the no-op that must return nil, not
// ErrNotFound. On SQLite these always passed (it counts a matched row as
// affected); the value is the MariaDB run in CI (BITT_TEST_MARIADB_DSN), where
// every one of these would have failed before the fix.
func TestNoOpUpdatesSucceed(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	user := mustUser(t, db, "a@example.com")
	tab := mustTab(t, db, user.ID)

	// A Payoff tab for the loan/interest methods, which reject a Services tab.
	payoff, err := db.CreateTab(ctx, store.Tab{
		Name: "Car loan", Kind: store.TabPayoff, CreatedBy: user.ID,
	}, nil)
	if err != nil {
		t.Fatalf("create payoff tab: %v", err)
	}

	reminders := []store.TabReminder{{Days: 7, Title: "Due soon", Body: "Your payment is due."}}

	// Each closure applies its method once; twice returns the identical values, so
	// the second call is the no-op under test. Naming each keeps a failure legible.
	steps := []struct {
		name string
		do   func() error
	}{
		{"SetSchedule", func() error {
			return db.SetSchedule(ctx, tab.ID, schedule.Schedule{
				Kind: schedule.MonthlyDay, Anchor: day(2026, time.March, 15), Billing: schedule.InAdvance,
			})
		}},
		{"SetFeePolicy", func() error {
			return db.SetFeePolicy(ctx, tab.ID, fee.Policy{Kind: fee.Fixed, Fixed: 2500, GraceDays: 5})
		}},
		{"SetInterestRate", func() error { return db.SetInterestRate(ctx, payoff.ID, 524) }},
		{"SetLoanTerms", func() error { return db.SetLoanTerms(ctx, payoff.ID, 48, 50_565) }},
		{"UpdateTabDetails", func() error {
			return db.UpdateTabDetails(ctx, tab.ID, "Renamed", "a note", store.TabServices)
		}},
		{"SetTabArchived", func() error { return db.SetTabArchived(ctx, tab.ID, true) }},
		{"SetTabReminders", func() error { return db.SetTabReminders(ctx, tab.ID, reminders) }},
		// UpdateItem is deliberately absent: it supersedes (removes the old row,
		// inserts a replacement) rather than updating in place, so re-applying to
		// the old id is a genuine ErrNotFound the handler surfaces as a message,
		// not the no-op-500 class this test guards.
		{"UpdateProfile", func() error {
			_, err := db.UpdateProfile(ctx, user.ID, "A Person", "a@example.com")
			return err
		}},
		{"UpdatePasswordHash", func() error {
			return db.UpdatePasswordHash(ctx, user.ID, "$argon2id$placeholder")
		}},
		{"SetNotifyPrefs", func() error { return db.SetNotifyPrefs(ctx, user.ID, "topic123", true, true) }},
		{"SetUserActive", func() error { return db.SetUserActive(ctx, user.ID, true) }},
		{"SetDelivery", func() error {
			return db.SetDelivery(ctx, store.Delivery{NtfyBaseURL: "https://ntfy.example"})
		}},
		{"SetInstanceReminders", func() error { return db.SetInstanceReminders(ctx, reminders) }},
	}

	for _, s := range steps {
		t.Run(s.name, func(t *testing.T) {
			if err := s.do(); err != nil {
				t.Fatalf("%s first call: %v", s.name, err)
			}
			// The identical second call changes nothing. It must still succeed.
			if err := s.do(); err != nil {
				t.Errorf("%s re-saved with identical values returned %v, want nil", s.name, err)
			}
		})
	}
}
