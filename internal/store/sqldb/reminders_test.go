package sqldb

import (
	"testing"

	"github.com/johnzastrow/bitt/internal/store"
)

// Overdue notices are negative lead times, so a negative day MUST persist.
//
// This is the test that was missing when NOTIF-02 shipped. The parser was
// tested as a pure function and accepted negatives happily, but nothing
// exercised the round trip to storage -- where CHECK (days > 0) rejected them.
// The result: overdue worked from the built-in defaults, which never touch the
// database, and silently did not work for any instance that had ever saved its
// reminders. Migration 0012 relaxes the constraint.
func TestReminderDaysAcceptNegativeLeadTimes(t *testing.T) {
	db := newTestDB(t)
	ctx := t.Context()

	want := []store.TabReminder{
		{Days: 14, Title: "T", Body: "B"},
		{Days: 1, Title: "T", Body: "B"},
		{Days: -1, Title: "Overdue", Body: "B"},
		{Days: -7, Title: "Overdue", Body: "B"},
	}
	if err := db.SetInstanceReminders(ctx, want); err != nil {
		t.Fatalf("storing a negative lead time failed -- overdue cannot be configured: %v", err)
	}
	got, err := db.ListInstanceReminders(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("stored %d rules, read back %d", len(want), len(got))
	}
	var neg int
	for _, r := range got {
		if r.Days < 0 {
			neg++
		}
	}
	if neg != 2 {
		t.Errorf("read back %d negative rules, want 2 -- overdue rules did not survive", neg)
	}
}

// The same for a tab's own rules, which are a separate table with its own CHECK.
func TestTabReminderDaysAcceptNegativeLeadTimes(t *testing.T) {
	db := newTestDB(t)
	ctx := t.Context()
	owner := mustUser(t, db, "owner@example.com")
	tab := mustTab(t, db, owner.ID)

	if err := db.SetTabReminders(ctx, tab.ID, []store.TabReminder{
		{Days: 7, Title: "T", Body: "B"},
		{Days: -1, Title: "Overdue", Body: "B"},
	}); err != nil {
		t.Fatalf("storing a negative lead time on a tab failed: %v", err)
	}
	got, err := db.ListTabReminders(ctx, tab.ID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var neg int
	for _, r := range got {
		if r.Days < 0 {
			neg++
		}
	}
	if neg != 1 {
		t.Errorf("read back %d negative rules, want 1", neg)
	}
}

// Zero stays refused: "on the due date" is ambiguous between a reminder and an
// overdue notice, and the constraint is what enforces that at the storage layer.
func TestReminderDaysRefusesZero(t *testing.T) {
	db := newTestDB(t)
	if err := db.SetInstanceReminders(t.Context(), []store.TabReminder{
		{Days: 0, Title: "T", Body: "B"},
	}); err == nil {
		t.Error("a zero lead time was stored; it should be refused")
	}
}
