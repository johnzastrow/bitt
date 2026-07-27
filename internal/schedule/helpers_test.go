package schedule

import (
	"testing"
	"time"
)

// Small pure helpers on Date and Unit that the UI leans on for labels and
// blank-state handling. Cheap to pin, and a silent change to any is a visible
// change in the interface.

func TestUnitLabel(t *testing.T) {
	if UnitWeek.Label() != "weeks" {
		t.Errorf("UnitWeek.Label() = %q, want weeks", UnitWeek.Label())
	}
	if UnitMonth.Label() != "months" {
		t.Errorf("UnitMonth.Label() = %q, want months", UnitMonth.Label())
	}
	if UnitNone.Label() != "" {
		t.Errorf("UnitNone.Label() = %q, want empty", UnitNone.Label())
	}
}

func TestNewDate(t *testing.T) {
	d := NewDate(2026, time.March, 15)
	if d.Year != 2026 || d.Month != time.March || d.Day != 15 {
		t.Errorf("NewDate built %+v", d)
	}
	// It does not normalize: an impossible date stays impossible for Valid to catch.
	bad := NewDate(2026, time.February, 31)
	if bad.Valid() {
		t.Error("NewDate normalized an impossible date instead of leaving it invalid")
	}
}

func TestDateDisplayAndIsZero(t *testing.T) {
	if !(Date{}).IsZero() {
		t.Error("the zero Date should report IsZero")
	}
	d := NewDate(2026, time.March, 15)
	if d.IsZero() {
		t.Error("a set date should not report IsZero")
	}
	if got := d.Display(); got != "Mar 15, 2026" {
		t.Errorf("Display() = %q, want \"Mar 15, 2026\"", got)
	}
	// An invalid date displays as empty rather than a guessed calendar date.
	if got := NewDate(2026, time.February, 31).Display(); got != "" {
		t.Errorf("Display() of an invalid date = %q, want empty", got)
	}
}
