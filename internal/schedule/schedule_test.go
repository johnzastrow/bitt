package schedule

import (
	"errors"
	"testing"
	"time"
)

// mustLoad fails the test rather than the package if a zone is unavailable,
// which is the useful signal: the binary embeds tzdata, so a missing zone here
// means the embed regressed.
func mustLoad(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatalf("load %s: %v", name, err)
	}
	return loc
}

func d(y int, m time.Month, day int) Date { return Date{Year: y, Month: m, Day: day} }

// ---------------------------------------------------------------------------
// Period boundaries -- SCHED-01, SCHED-02
// ---------------------------------------------------------------------------

// TestNthSequences walks the first several periods of each recurrence. The
// month-end and leap-year cases are the ones that actually break.
func TestNthSequences(t *testing.T) {
	cases := []struct {
		name   string
		sched  Schedule
		want   []Date
		reason string
	}{
		{
			name:  "weekly",
			sched: Schedule{Kind: Weekly, Anchor: d(2026, time.January, 5)},
			want: []Date{
				d(2026, time.January, 5), d(2026, time.January, 12),
				d(2026, time.January, 19), d(2026, time.January, 26),
				d(2026, time.February, 2),
			},
		},
		{
			name:  "weekly across a year boundary",
			sched: Schedule{Kind: Weekly, Anchor: d(2025, time.December, 22)},
			want: []Date{
				d(2025, time.December, 22), d(2025, time.December, 29),
				d(2026, time.January, 5), d(2026, time.January, 12),
			},
		},
		{
			name:  "biweekly",
			sched: Schedule{Kind: Biweekly, Anchor: d(2026, time.January, 5)},
			want: []Date{
				d(2026, time.January, 5), d(2026, time.January, 19),
				d(2026, time.February, 2), d(2026, time.February, 16),
			},
		},
		{
			name:  "biweekly across February in a leap year",
			sched: Schedule{Kind: Biweekly, Anchor: d(2024, time.February, 20)},
			want: []Date{
				d(2024, time.February, 20), d(2024, time.March, 5),
				d(2024, time.March, 19), d(2024, time.April, 2),
			},
			reason: "2024 February has 29 days; 20 Feb + 14 = 5 Mar, not 6 Mar",
		},
		{
			name:  "monthly on the 15th",
			sched: Schedule{Kind: MonthlyDay, Anchor: d(2026, time.January, 15)},
			want: []Date{
				d(2026, time.January, 15), d(2026, time.February, 15),
				d(2026, time.March, 15), d(2026, time.April, 15),
			},
		},
		{
			name:  "monthly on the 31st clamps and recovers",
			sched: Schedule{Kind: MonthlyDay, Anchor: d(2026, time.January, 31)},
			want: []Date{
				d(2026, time.January, 31),
				d(2026, time.February, 28), // clamped
				d(2026, time.March, 31),    // back to the anchor day
				d(2026, time.April, 30),    // clamped
				d(2026, time.May, 31),      // back again
				d(2026, time.June, 30),
				d(2026, time.July, 31),
			},
			reason: "SCHED-02: a clamp must not become permanent",
		},
		{
			name:  "monthly on the 31st in a leap year",
			sched: Schedule{Kind: MonthlyDay, Anchor: d(2024, time.January, 31)},
			want: []Date{
				d(2024, time.January, 31),
				d(2024, time.February, 29), // 2024 is a leap year
				d(2024, time.March, 31),
			},
		},
		{
			name:  "monthly on the 30th clamps only in February",
			sched: Schedule{Kind: MonthlyDay, Anchor: d(2026, time.January, 30)},
			want: []Date{
				d(2026, time.January, 30),
				d(2026, time.February, 28),
				d(2026, time.March, 30),
				d(2026, time.April, 30),
			},
		},
		{
			name:  "monthly on the 29th is a real date only in a leap February",
			sched: Schedule{Kind: MonthlyDay, Anchor: d(2024, time.January, 29)},
			want: []Date{
				d(2024, time.January, 29),
				d(2024, time.February, 29),
				d(2024, time.March, 29),
			},
		},
		{
			name:  "monthly on the 29th clamps in a common year",
			sched: Schedule{Kind: MonthlyDay, Anchor: d(2026, time.January, 29)},
			want: []Date{
				d(2026, time.January, 29),
				d(2026, time.February, 28),
				d(2026, time.March, 29),
			},
		},
		{
			name:  "monthly on the last day",
			sched: Schedule{Kind: MonthlyLast, Anchor: d(2026, time.January, 31)},
			want: []Date{
				d(2026, time.January, 31),
				d(2026, time.February, 28),
				d(2026, time.March, 31),
				d(2026, time.April, 30),
				d(2026, time.May, 31),
			},
		},
		{
			name:  "monthly on the last day through a leap February",
			sched: Schedule{Kind: MonthlyLast, Anchor: d(2024, time.January, 31)},
			want: []Date{
				d(2024, time.January, 31),
				d(2024, time.February, 29),
				d(2024, time.March, 31),
			},
		},
		{
			name:  "monthly across a year boundary",
			sched: Schedule{Kind: MonthlyDay, Anchor: d(2025, time.November, 30)},
			want: []Date{
				d(2025, time.November, 30),
				d(2025, time.December, 30),
				d(2026, time.January, 30),
				d(2026, time.February, 28),
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for i, want := range tc.want {
				got := tc.sched.Nth(i)
				if got != want {
					t.Errorf("Nth(%d) = %s, want %s%s", i, got, want, because(tc.reason))
				}
				if !got.Valid() {
					t.Errorf("Nth(%d) = %s is not a real calendar date", i, got)
				}
			}
		})
	}
}

func because(reason string) string {
	if reason == "" {
		return ""
	}
	return " (" + reason + ")"
}

// TestNthIsIndependentOfChaining is the property SCHED-02 actually asks for:
// period N depends only on the anchor and N, so no amount of clamping in
// between can drift the sequence.
func TestNthIsIndependentOfChaining(t *testing.T) {
	s := Schedule{Kind: MonthlyDay, Anchor: d(2026, time.January, 31)}

	// Twenty-four months out, a day-31 anchor must still be on the 31st in
	// every month long enough to have one.
	for n := 0; n < 24; n++ {
		got := s.Nth(n)
		year, month := addMonths(2026, time.January, n)
		wantDay := min(31, daysIn(year, month))
		if got != d(year, month, wantDay) {
			t.Fatalf("Nth(%d) = %s, want %04d-%02d-%02d", n, got, year, int(month), wantDay)
		}
	}
}

// TestPeriodsAreContiguousAndOrdered checks the invariant every consumer
// relies on: period N ends exactly where period N+1 begins, and time only
// moves forward.
func TestPeriodsAreContiguousAndOrdered(t *testing.T) {
	schedules := []Schedule{
		{Kind: Weekly, Anchor: d(2026, time.January, 5)},
		{Kind: Biweekly, Anchor: d(2024, time.February, 20)},
		{Kind: MonthlyDay, Anchor: d(2026, time.January, 31)},
		{Kind: MonthlyDay, Anchor: d(2024, time.January, 29)},
		{Kind: MonthlyLast, Anchor: d(2026, time.January, 31)},
	}

	for _, s := range schedules {
		s = s.Normalize()
		t.Run(s.Describe(), func(t *testing.T) {
			for n := 0; n < 40; n++ {
				p, next := s.Period(n), s.Period(n+1)
				if p.End != next.Start {
					t.Fatalf("period %d ends %s but period %d starts %s", n, p.End, n+1, next.Start)
				}
				if !p.Start.Before(p.End) {
					t.Fatalf("period %d is empty or inverted: %s to %s", n, p.Start, p.End)
				}
				if p.Key() == next.Key() {
					t.Fatalf("periods %d and %d share key %s", n, n+1, p.Key())
				}
			}
		})
	}
}

// TestDueDateFollowsBillingRule covers the Provider's per-tab choice of when a
// charge lands.
func TestDueDateFollowsBillingRule(t *testing.T) {
	anchor := d(2026, time.January, 1)

	advance := Schedule{Kind: MonthlyDay, Anchor: anchor, Billing: InAdvance}
	arrears := Schedule{Kind: MonthlyDay, Anchor: anchor, Billing: InArrears}

	for n := 0; n < 6; n++ {
		a, r := advance.Period(n), arrears.Period(n)
		if a.Due != a.Start {
			t.Errorf("in advance: period %d due %s, want its start %s", n, a.Due, a.Start)
		}
		if r.Due != r.End {
			t.Errorf("in arrears: period %d due %s, want its end %s", n, r.Due, r.End)
		}
		// The key identifies the cycle, not the posting date, so switching the
		// billing rule cannot re-post a cycle under a new key (SCHED-04).
		if a.Key() != r.Key() {
			t.Errorf("period %d key differs by billing rule: %s vs %s", n, a.Key(), r.Key())
		}
	}
}

// ---------------------------------------------------------------------------
// Accrual -- SCHED-03, SCHED-05
// ---------------------------------------------------------------------------

func TestDueThroughCatchesUp(t *testing.T) {
	utc := time.UTC

	cases := []struct {
		name  string
		sched Schedule
		asOf  time.Time
		want  int
		last  Date
	}{
		{
			name:  "nothing due before the anchor",
			sched: Schedule{Kind: MonthlyDay, Anchor: d(2026, time.March, 1), Billing: InAdvance},
			asOf:  time.Date(2026, time.February, 28, 23, 59, 59, 0, utc),
			want:  0,
		},
		{
			name:  "the anchor day itself is due",
			sched: Schedule{Kind: MonthlyDay, Anchor: d(2026, time.March, 1), Billing: InAdvance},
			asOf:  time.Date(2026, time.March, 1, 0, 0, 0, 0, utc),
			want:  1,
			last:  d(2026, time.March, 1),
		},
		{
			name:  "six months untouched posts six periods",
			sched: Schedule{Kind: MonthlyDay, Anchor: d(2026, time.January, 15), Billing: InAdvance},
			asOf:  time.Date(2026, time.June, 20, 12, 0, 0, 0, utc),
			want:  6,
			last:  d(2026, time.June, 15),
		},
		{
			name:  "six months untouched, billed in arrears, posts one fewer",
			sched: Schedule{Kind: MonthlyDay, Anchor: d(2026, time.January, 15), Billing: InArrears},
			asOf:  time.Date(2026, time.June, 20, 12, 0, 0, 0, utc),
			want:  5,
			last:  d(2026, time.June, 15),
		},
		{
			name:  "weekly for a quarter",
			sched: Schedule{Kind: Weekly, Anchor: d(2026, time.January, 1), Billing: InAdvance},
			asOf:  time.Date(2026, time.April, 1, 0, 0, 0, 0, utc),
			want:  13,
			last:  d(2026, time.March, 26),
		},
		{
			name:  "month-end schedule left alone across February",
			sched: Schedule{Kind: MonthlyLast, Anchor: d(2026, time.January, 31), Billing: InAdvance},
			asOf:  time.Date(2026, time.April, 15, 0, 0, 0, 0, utc),
			want:  3,
			last:  d(2026, time.March, 31),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.sched.DueThrough(tc.asOf, utc)
			if err != nil {
				t.Fatalf("DueThrough: %v", err)
			}
			if len(got) != tc.want {
				t.Fatalf("got %d periods, want %d (last was %v)", len(got), tc.want, lastDue(got))
			}
			if tc.want > 0 && got[len(got)-1].Due != tc.last {
				t.Errorf("last due date %s, want %s", got[len(got)-1].Due, tc.last)
			}
			// Oldest first, contiguous indices from zero.
			for i, p := range got {
				if p.Index != i {
					t.Fatalf("period at position %d carries index %d", i, p.Index)
				}
			}
		})
	}
}

func lastDue(ps []Period) any {
	if len(ps) == 0 {
		return "none"
	}
	return ps[len(ps)-1].Due
}

// TestDueThroughKeysAreUnique is the property the (tab, period) constraint
// depends on: no two periods of one schedule ever produce the same key.
func TestDueThroughKeysAreUnique(t *testing.T) {
	s := Schedule{Kind: MonthlyDay, Anchor: d(2020, time.January, 31), Billing: InAdvance}
	periods, err := s.DueThrough(time.Date(2026, time.July, 23, 0, 0, 0, 0, time.UTC), time.UTC)
	if err != nil {
		t.Fatalf("DueThrough: %v", err)
	}
	if len(periods) < 70 {
		t.Fatalf("expected six years of monthly periods, got %d", len(periods))
	}
	seen := make(map[string]int, len(periods))
	for _, p := range periods {
		if prev, dup := seen[p.Key()]; dup {
			t.Fatalf("period %d duplicates the key of period %d: %s", p.Index, prev, p.Key())
		}
		seen[p.Key()] = p.Index
	}
}

func TestDueThroughCapsBatchSize(t *testing.T) {
	// An anchor far enough back to exceed the cap several times over.
	s := Schedule{Kind: Weekly, Anchor: d(1990, time.January, 1), Billing: InAdvance}
	got, err := s.DueThrough(time.Date(2026, time.July, 23, 0, 0, 0, 0, time.UTC), time.UTC)
	if err != nil {
		t.Fatalf("DueThrough: %v", err)
	}
	if len(got) != MaxPeriods {
		t.Fatalf("got %d periods, want the cap of %d", len(got), MaxPeriods)
	}
}

func TestNext(t *testing.T) {
	s := Schedule{Kind: MonthlyDay, Anchor: d(2026, time.January, 15), Billing: InAdvance}

	next, err := s.Next(time.Date(2026, time.March, 20, 0, 0, 0, 0, time.UTC), time.UTC)
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if next.Due != d(2026, time.April, 15) {
		t.Errorf("next due %s, want 2026-04-15", next.Due)
	}

	// Before the anchor, the next period is the first one.
	next, err = s.Next(time.Date(2025, time.December, 1, 0, 0, 0, 0, time.UTC), time.UTC)
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if next.Index != 0 || next.Due != d(2026, time.January, 15) {
		t.Errorf("next = index %d due %s, want index 0 due 2026-01-15", next.Index, next.Due)
	}
}

// ---------------------------------------------------------------------------
// Timezone and daylight saving
// ---------------------------------------------------------------------------

// TestDueThroughUsesInstanceTimezone is the reason period boundaries are
// computed in a configured zone rather than in UTC (SCHED-02). At the same
// instant, a period can be due in one zone and not yet due in another.
func TestDueThroughUsesInstanceTimezone(t *testing.T) {
	newYork := mustLoad(t, "America/New_York")
	tokyo := mustLoad(t, "Asia/Tokyo")

	s := Schedule{Kind: MonthlyDay, Anchor: d(2026, time.March, 1), Billing: InAdvance}

	// 2026-03-01T04:00Z is still 23:00 on February 28th in New York, and is
	// already 13:00 on March 1st in Tokyo.
	asOf := time.Date(2026, time.March, 1, 4, 0, 0, 0, time.UTC)

	inNY, err := s.DueThrough(asOf, newYork)
	if err != nil {
		t.Fatalf("DueThrough New York: %v", err)
	}
	if len(inNY) != 0 {
		t.Errorf("New York: %d periods due, want 0 -- it is still February there", len(inNY))
	}

	inTokyo, err := s.DueThrough(asOf, tokyo)
	if err != nil {
		t.Fatalf("DueThrough Tokyo: %v", err)
	}
	if len(inTokyo) != 1 {
		t.Errorf("Tokyo: %d periods due, want 1 -- March has started there", len(inTokyo))
	}
}

// TestWeeklySurvivesDaylightSaving is the case that breaks any implementation
// that adds 168 hours instead of 7 days.
func TestWeeklySurvivesDaylightSaving(t *testing.T) {
	newYork := mustLoad(t, "America/New_York")

	// Anchored the week before the 2026 spring-forward (March 8) and running
	// past the autumn fall-back (November 1).
	s := Schedule{Kind: Weekly, Anchor: d(2026, time.March, 1), Billing: InAdvance}

	periods, err := s.DueThrough(time.Date(2026, time.December, 1, 12, 0, 0, 0, time.UTC), newYork)
	if err != nil {
		t.Fatalf("DueThrough: %v", err)
	}

	for i, p := range periods {
		// Every period must start on the same weekday as the anchor, in every
		// week of the year, on both sides of both transitions.
		weekday := p.Start.Time(newYork).Weekday()
		if weekday != time.Sunday {
			t.Errorf("period %d starts %s, a %s -- the anchor is a Sunday", i, p.Start, weekday)
		}
		// And exactly seven calendar days after its predecessor.
		if i > 0 {
			if want := periods[i-1].Start.AddDays(7); p.Start != want {
				t.Errorf("period %d starts %s, want %s", i, p.Start, want)
			}
		}
	}

	// The instants those dates resolve to are not evenly spaced, which is
	// correct and is exactly why the arithmetic is not done on instants. In
	// 2026 the US moves its clocks at 02:00 on March 8 and November 1, so it is
	// the week *starting* on each transition date that is short or long.
	spans := []struct {
		name     string
		from, to Date
		want     time.Duration
	}{
		{"the spring-forward day", d(2026, time.March, 8), d(2026, time.March, 9), 23 * time.Hour},
		{"the fall-back day", d(2026, time.November, 1), d(2026, time.November, 2), 25 * time.Hour},
		{"the week starting at spring-forward", d(2026, time.March, 8), d(2026, time.March, 15), 167 * time.Hour},
		{"the week starting at fall-back", d(2026, time.November, 1), d(2026, time.November, 8), 169 * time.Hour},
		{"an ordinary week", d(2026, time.June, 7), d(2026, time.June, 14), 168 * time.Hour},
	}
	for _, s := range spans {
		if got := s.to.Time(newYork).Sub(s.from.Time(newYork)); got != s.want {
			t.Errorf("%s spans %v, want %v", s.name, got, s.want)
		}
	}
}

// TestDateTimeOnAMidnightTransition covers zones whose DST shift happens at
// midnight, where the first instant of a day is 01:00 rather than 00:00.
func TestDateTimeOnAMidnightTransition(t *testing.T) {
	santiago := mustLoad(t, "America/Santiago")

	// Chile moves its clocks forward at midnight, so on the transition date
	// 00:00 does not exist. The requirement is only that the resolved instant
	// still falls on the requested date.
	for _, day := range []Date{
		d(2026, time.September, 5),
		d(2026, time.September, 6),
		d(2026, time.September, 7),
	} {
		got := day.Time(santiago)
		if DateOf(got) != day {
			t.Errorf("%s resolved to %s, a different calendar date", day, got.Format(time.RFC3339))
		}
		if h := got.Hour(); h != 0 && h != 1 {
			t.Errorf("%s resolved to hour %d, want the first instant of the day", day, h)
		}
	}
}

// TestAddDaysIgnoresDaylightSaving pins the helper directly.
func TestAddDaysIgnoresDaylightSaving(t *testing.T) {
	// Across the 2026 US spring-forward.
	if got := d(2026, time.March, 7).AddDays(1); got != d(2026, time.March, 8) {
		t.Errorf("2026-03-07 + 1 = %s, want 2026-03-08", got)
	}
	// Across a leap day.
	if got := d(2024, time.February, 28).AddDays(1); got != d(2024, time.February, 29) {
		t.Errorf("2024-02-28 + 1 = %s, want 2024-02-29", got)
	}
	if got := d(2026, time.February, 28).AddDays(1); got != d(2026, time.March, 1) {
		t.Errorf("2026-02-28 + 1 = %s, want 2026-03-01", got)
	}
	// Across a year boundary, forward and back.
	if got := d(2025, time.December, 31).AddDays(1); got != d(2026, time.January, 1) {
		t.Errorf("2025-12-31 + 1 = %s, want 2026-01-01", got)
	}
	if got := d(2026, time.January, 1).AddDays(-1); got != d(2025, time.December, 31) {
		t.Errorf("2026-01-01 - 1 = %s, want 2025-12-31", got)
	}
}

// ---------------------------------------------------------------------------
// Calendar helpers
// ---------------------------------------------------------------------------

func TestDaysIn(t *testing.T) {
	cases := []struct {
		year  int
		month time.Month
		want  int
	}{
		{2026, time.January, 31},
		{2026, time.February, 28},
		{2024, time.February, 29}, // divisible by 4
		{2000, time.February, 29}, // divisible by 400
		{1900, time.February, 28}, // divisible by 100 but not 400
		{2100, time.February, 28}, // the next century that is not a leap year
		{2026, time.April, 30},
		{2026, time.December, 31},
	}
	for _, tc := range cases {
		if got := daysIn(tc.year, tc.month); got != tc.want {
			t.Errorf("daysIn(%d, %s) = %d, want %d", tc.year, tc.month, got, tc.want)
		}
	}
}

func TestAddMonths(t *testing.T) {
	cases := []struct {
		year      int
		month     time.Month
		n         int
		wantYear  int
		wantMonth time.Month
	}{
		{2026, time.January, 0, 2026, time.January},
		{2026, time.January, 1, 2026, time.February},
		{2026, time.January, 11, 2026, time.December},
		{2026, time.January, 12, 2027, time.January},
		{2026, time.December, 1, 2027, time.January},
		{2026, time.January, 25, 2028, time.February},
		{2026, time.January, -1, 2025, time.December},
		{2026, time.January, -13, 2024, time.December},
	}
	for _, tc := range cases {
		y, m := addMonths(tc.year, tc.month, tc.n)
		if y != tc.wantYear || m != tc.wantMonth {
			t.Errorf("addMonths(%d, %s, %d) = %d %s, want %d %s",
				tc.year, tc.month, tc.n, y, m, tc.wantYear, tc.wantMonth)
		}
	}
}

// ---------------------------------------------------------------------------
// Dates, validation, and parsing
// ---------------------------------------------------------------------------

func TestDateValid(t *testing.T) {
	valid := []Date{
		d(2026, time.January, 1),
		d(2026, time.December, 31),
		d(2024, time.February, 29),
	}
	for _, c := range valid {
		if !c.Valid() {
			t.Errorf("%s should be valid", c)
		}
	}

	invalid := []Date{
		{},                         // zero
		d(2026, time.February, 29), // not a leap year
		d(2026, time.April, 31),    // April has 30
		d(2026, time.January, 0),   // day zero
		d(2026, time.January, 32),  // past the end
		d(2026, time.Month(0), 1),  // month zero
		d(2026, time.Month(13), 1), // month thirteen
		d(0, time.January, 1),      // year zero
	}
	for _, c := range invalid {
		if c.Valid() {
			t.Errorf("%v should be invalid", c)
		}
	}
}

func TestParseDate(t *testing.T) {
	good := map[string]Date{
		"2026-01-05":  d(2026, time.January, 5),
		"2024-02-29":  d(2024, time.February, 29),
		" 2026-12-31": d(2026, time.December, 31),
	}
	for in, want := range good {
		got, err := ParseDate(in)
		if err != nil {
			t.Errorf("ParseDate(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("ParseDate(%q) = %s, want %s", in, got, want)
		}
	}

	bad := []string{
		"", "not a date", "2026-13-01", "2026-02-30", "2026-02-31",
		"2026/01/05", "01-05-2026", "2026-1-5", "20260105",
	}
	for _, in := range bad {
		if got, err := ParseDate(in); err == nil {
			t.Errorf("ParseDate(%q) = %s, want an error", in, got)
		} else if !errors.Is(err, ErrBadAnchor) {
			t.Errorf("ParseDate(%q) returned %v, want ErrBadAnchor", in, err)
		}
	}
}

func TestDateStringRoundTrips(t *testing.T) {
	for _, c := range []Date{
		d(2026, time.January, 5),
		d(2024, time.February, 29),
		d(999, time.December, 31),
	} {
		got, err := ParseDate(c.String())
		if err != nil {
			t.Fatalf("round trip %s: %v", c, err)
		}
		if got != c {
			t.Errorf("round trip %s produced %s", c, got)
		}
	}
}

func TestDateCompare(t *testing.T) {
	a := d(2026, time.March, 15)
	cases := []struct {
		other Date
		want  int
	}{
		{d(2026, time.March, 15), 0},
		{d(2026, time.March, 16), -1},
		{d(2026, time.March, 14), 1},
		{d(2026, time.April, 1), -1},
		{d(2026, time.February, 28), 1},
		{d(2027, time.January, 1), -1},
		{d(2025, time.December, 31), 1},
	}
	for _, tc := range cases {
		if got := a.Compare(tc.other); got != tc.want {
			t.Errorf("%s.Compare(%s) = %d, want %d", a, tc.other, got, tc.want)
		}
		if got := a.Before(tc.other); got != (tc.want < 0) {
			t.Errorf("%s.Before(%s) = %v", a, tc.other, got)
		}
		if got := a.After(tc.other); got != (tc.want > 0) {
			t.Errorf("%s.After(%s) = %v", a, tc.other, got)
		}
	}
}

func TestValidate(t *testing.T) {
	anchor := d(2026, time.January, 15)

	cases := []struct {
		name  string
		sched Schedule
		want  error
	}{
		{"unscheduled", Schedule{}, ErrNoSchedule},
		{"unknown kind", Schedule{Kind: "quarterly", Anchor: anchor, Billing: InAdvance}, ErrBadKind},
		{"unknown billing", Schedule{Kind: Weekly, Anchor: anchor, Billing: "eventually"}, ErrBadBilling},
		{"missing billing", Schedule{Kind: Weekly, Anchor: anchor}, ErrBadBilling},
		{"impossible anchor", Schedule{Kind: Weekly, Anchor: d(2026, time.February, 30), Billing: InAdvance}, ErrBadAnchor},
		{"zero anchor", Schedule{Kind: Weekly, Billing: InAdvance}, ErrBadAnchor},
		{"good", Schedule{Kind: Weekly, Anchor: anchor, Billing: InAdvance}, nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.sched.Validate()
			if tc.want == nil {
				if err != nil {
					t.Fatalf("Validate = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("Validate = %v, want %v", err, tc.want)
			}
		})
	}
}

// TestDueThroughRefusesAnInvalidSchedule keeps a malformed schedule from
// silently accruing nothing, which would look like a working tab that never
// bills.
func TestDueThroughRefusesAnInvalidSchedule(t *testing.T) {
	bad := Schedule{Kind: "quarterly", Anchor: d(2026, time.January, 1), Billing: InAdvance}
	if _, err := bad.DueThrough(time.Now(), time.UTC); !errors.Is(err, ErrBadKind) {
		t.Fatalf("DueThrough on a bad kind = %v, want ErrBadKind", err)
	}
	if _, err := (Schedule{}).DueThrough(time.Now(), time.UTC); !errors.Is(err, ErrNoSchedule) {
		t.Fatalf("DueThrough on no schedule = %v, want ErrNoSchedule", err)
	}
}

func TestNormalize(t *testing.T) {
	// A mid-month anchor under "last day of the month" snaps to the end of
	// that month, so the stored anchor agrees with period zero.
	got := Schedule{Kind: MonthlyLast, Anchor: d(2026, time.March, 10)}.Normalize()
	if got.Anchor != d(2026, time.March, 31) {
		t.Errorf("anchor normalized to %s, want 2026-03-31", got.Anchor)
	}
	if got.Anchor != got.Nth(0) {
		t.Errorf("anchor %s disagrees with period zero %s", got.Anchor, got.Nth(0))
	}
	if got.Billing != InAdvance {
		t.Errorf("billing defaulted to %q, want %q", got.Billing, InAdvance)
	}

	// February, where the last day depends on the year.
	if got := (Schedule{Kind: MonthlyLast, Anchor: d(2024, time.February, 1)}).Normalize(); got.Anchor != d(2024, time.February, 29) {
		t.Errorf("leap February normalized to %s, want 2024-02-29", got.Anchor)
	}

	// Other kinds keep their anchor exactly. Only the interval is filled in,
	// from the zero value to the every-period default.
	for _, k := range []Kind{Weekly, MonthlyDay} {
		in := Schedule{Kind: k, Anchor: d(2026, time.March, 10), Billing: InArrears}
		got := in.Normalize()
		if got.Anchor != in.Anchor || got.Kind != in.Kind || got.Billing != in.Billing {
			t.Errorf("%s normalization changed %+v to %+v", k, in, got)
		}
		if got.Interval != 1 {
			t.Errorf("%s normalized to interval %d, want 1", k, got.Interval)
		}
	}

	// An explicit interval survives normalization untouched.
	in := Schedule{Kind: Weekly, Anchor: d(2026, time.March, 10), Billing: InArrears, Interval: 3}
	if got := in.Normalize(); got != in {
		t.Errorf("normalization changed %+v to %+v", in, got)
	}
}

// TestNormalizeRewritesBiweekly pins the migration's central claim: biweekly
// and weekly-every-two-weeks are the same dates, so rewriting one to the other
// cannot move a period boundary and cannot make a posted cycle re-post under a
// new key. If this ever fails, the 0006 migration is unsafe.
func TestNormalizeRewritesBiweekly(t *testing.T) {
	anchor := d(2026, time.March, 10)
	legacy := Schedule{Kind: Biweekly, Anchor: anchor, Billing: InArrears}

	got := legacy.Normalize()
	if got.Kind != Weekly || got.Interval != 2 {
		t.Fatalf("biweekly normalized to %+v, want weekly interval 2", got)
	}
	if got.Anchor != anchor || got.Billing != InArrears {
		t.Errorf("normalization disturbed the anchor or billing: %+v", got)
	}

	// Every period, and so every period key, must be identical.
	for n := range 200 {
		if a, b := legacy.Nth(n), got.Nth(n); a != b {
			t.Fatalf("period %d: biweekly %s != weekly-by-2 %s", n, a, b)
		}
		if a, b := legacy.Period(n).Key(), got.Period(n).Key(); a != b {
			t.Fatalf("period %d key: %q != %q", n, a, b)
		}
	}
}

func TestKindAndBillingValidity(t *testing.T) {
	for _, k := range Kinds() {
		if !k.Valid() {
			t.Errorf("Kinds() offers %q, which is not valid", k)
		}
	}
	if None.Valid() {
		t.Error("None must not be a schedulable kind")
	}
	if Kind("monthly").Valid() {
		t.Error("an unknown kind must not validate")
	}

	for _, b := range Billings() {
		if !b.Valid() {
			t.Errorf("Billings() offers %q, which is not valid", b)
		}
		if b.Label() == "" {
			t.Errorf("billing %q has no label", b)
		}
	}
	if Billing("").Valid() {
		t.Error("an empty billing rule must not validate")
	}
}

func TestDescribe(t *testing.T) {
	cases := map[string]Schedule{
		"No schedule": {},
		"Weekly on Mondays, billed at period start":        {Kind: Weekly, Anchor: d(2026, time.January, 5), Billing: InAdvance},
		"Every two weeks on Fridays, billed at period end": {Kind: Biweekly, Anchor: d(2026, time.January, 2), Billing: InArrears},
		"Monthly on the 1st, billed at period start":       {Kind: MonthlyDay, Anchor: d(2026, time.January, 1), Billing: InAdvance},
		"Monthly on the 22nd, billed at period start":      {Kind: MonthlyDay, Anchor: d(2026, time.January, 22), Billing: InAdvance},
		"Monthly on the 3rd, billed at period start":       {Kind: MonthlyDay, Anchor: d(2026, time.January, 3), Billing: InAdvance},
		"Monthly on the last day, billed at period end":    {Kind: MonthlyLast, Anchor: d(2026, time.January, 31), Billing: InArrears},
	}
	for want, s := range cases {
		if got := s.Describe(); got != want {
			t.Errorf("Describe() = %q, want %q", got, want)
		}
	}
}

func TestOrdinal(t *testing.T) {
	cases := map[int]string{
		1: "1st", 2: "2nd", 3: "3rd", 4: "4th",
		11: "11th", 12: "12th", 13: "13th",
		21: "21st", 22: "22nd", 23: "23rd",
		30: "30th", 31: "31st",
	}
	for n, want := range cases {
		if got := ordinal(n); got != want {
			t.Errorf("ordinal(%d) = %q, want %q", n, got, want)
		}
	}
}

// ---------------------------------------------------------------------------
// Intervals
// ---------------------------------------------------------------------------

func TestIntervalSteps(t *testing.T) {
	cases := []struct {
		name     string
		schedule Schedule
		want     []Date
	}{
		{
			"every week",
			Schedule{Kind: Weekly, Anchor: d(2026, time.March, 2), Interval: 1},
			[]Date{d(2026, time.March, 2), d(2026, time.March, 9), d(2026, time.March, 16)},
		},
		{
			"every three weeks",
			Schedule{Kind: Weekly, Anchor: d(2026, time.March, 2), Interval: 3},
			[]Date{d(2026, time.March, 2), d(2026, time.March, 23), d(2026, time.April, 13)},
		},
		{
			"every two months on the 15th",
			Schedule{Kind: MonthlyDay, Anchor: d(2026, time.January, 15), Interval: 2},
			[]Date{d(2026, time.January, 15), d(2026, time.March, 15), d(2026, time.May, 15)},
		},
		{
			"every quarter on the last day",
			Schedule{Kind: MonthlyLast, Anchor: d(2026, time.January, 31), Interval: 3},
			[]Date{d(2026, time.January, 31), d(2026, time.April, 30), d(2026, time.July, 31)},
		},
		{
			"a zero interval behaves as every period",
			Schedule{Kind: Weekly, Anchor: d(2026, time.March, 2), Interval: 0},
			[]Date{d(2026, time.March, 2), d(2026, time.March, 9), d(2026, time.March, 16)},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for n, want := range tc.want {
				if got := tc.schedule.Nth(n); got != want {
					t.Errorf("period %d = %s, want %s", n, got, want)
				}
			}
		})
	}
}

// TestIntervalKeepsTheAnchorRule is SCHED-02 under an interval: every period is
// computed from the anchor, so a day-31 anchor still recovers after a short
// month rather than decaying permanently.
func TestIntervalKeepsTheAnchorRule(t *testing.T) {
	s := Schedule{Kind: MonthlyDay, Anchor: d(2026, time.January, 31), Interval: 1}
	want := []Date{
		d(2026, time.January, 31),
		d(2026, time.February, 28), // clamped
		d(2026, time.March, 31),    // and back again
	}
	for n, w := range want {
		if got := s.Nth(n); got != w {
			t.Errorf("period %d = %s, want %s", n, got, w)
		}
	}

	// The same under an interval of 2: Jan 31 -> Mar 31 -> May 31, never
	// clamping because it never lands on a short month.
	every2 := Schedule{Kind: MonthlyDay, Anchor: d(2026, time.January, 31), Interval: 2}
	for n, w := range []Date{
		d(2026, time.January, 31), d(2026, time.March, 31), d(2026, time.May, 31),
	} {
		if got := every2.Nth(n); got != w {
			t.Errorf("every-2 period %d = %s, want %s", n, got, w)
		}
	}
}

func TestIntervalValidation(t *testing.T) {
	base := Schedule{Kind: Weekly, Anchor: d(2026, time.March, 2), Billing: InAdvance}

	for _, interval := range []int{0, 1, 2, 3, MaxInterval} {
		s := base
		s.Interval = interval
		if err := s.Validate(); err != nil {
			t.Errorf("interval %d rejected: %v", interval, err)
		}
	}
	for _, interval := range []int{-1, MaxInterval + 1, 10000} {
		s := base
		s.Interval = interval
		if err := s.Validate(); !errors.Is(err, ErrBadInterval) {
			t.Errorf("interval %d gave %v, want ErrBadInterval", interval, err)
		}
	}
}

// TestRateBasis pins the day-count conventions, which are the numbers a
// borrower can check against a lender's statement.
func TestRateBasis(t *testing.T) {
	cases := []struct {
		name     string
		schedule Schedule
		num, den int
	}{
		// Month-stepping schedules are months/12, the 30/360 basis a US
		// installment loan is quoted on. Plain monthly is exactly APR/12.
		{"monthly", Schedule{Kind: MonthlyDay, Interval: 1}, 1, 12},
		{"monthly, last day", Schedule{Kind: MonthlyLast, Interval: 1}, 1, 12},
		{"every two months", Schedule{Kind: MonthlyDay, Interval: 2}, 2, 12},
		{"quarterly", Schedule{Kind: MonthlyLast, Interval: 3}, 3, 12},
		// Week-stepping schedules are days/365, actual/365, because there is
		// no whole number of weeks in a year.
		{"weekly", Schedule{Kind: Weekly, Interval: 1}, 7, 365},
		{"every two weeks", Schedule{Kind: Weekly, Interval: 2}, 14, 365},
		{"every three weeks", Schedule{Kind: Weekly, Interval: 3}, 21, 365},
		{"a zero interval is every period", Schedule{Kind: Weekly, Interval: 0}, 7, 365},
		// A legacy row that never went through Normalize still answers.
		{"unnormalized biweekly", Schedule{Kind: Biweekly}, 14, 365},
		// No schedule, no basis.
		{"unscheduled", Schedule{}, 0, 0},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			num, den := tc.schedule.RateBasis()
			if num != tc.num || den != tc.den {
				t.Errorf("RateBasis() = %d/%d, want %d/%d", num, den, tc.num, tc.den)
			}
		})
	}
}

func TestDescribeIntervals(t *testing.T) {
	cases := []struct {
		schedule Schedule
		want     string
	}{
		{Schedule{Kind: Weekly, Anchor: d(2026, time.March, 2), Interval: 1, Billing: InAdvance},
			"Weekly on Mondays, billed at period start"},
		{Schedule{Kind: Weekly, Anchor: d(2026, time.March, 2), Interval: 2, Billing: InAdvance},
			"Every two weeks on Mondays, billed at period start"},
		{Schedule{Kind: Weekly, Anchor: d(2026, time.March, 2), Interval: 3, Billing: InAdvance},
			"Every 3 weeks on Mondays, billed at period start"},
		{Schedule{Kind: MonthlyDay, Anchor: d(2026, time.March, 15), Interval: 1, Billing: InArrears},
			"Monthly on the 15th, billed at period end"},
		{Schedule{Kind: MonthlyDay, Anchor: d(2026, time.March, 15), Interval: 2, Billing: InArrears},
			"Every 2 months on the 15th, billed at period end"},
		{Schedule{Kind: MonthlyLast, Anchor: d(2026, time.March, 31), Interval: 3, Billing: InAdvance},
			"Every 3 months on the last day, billed at period start"},
	}
	for _, tc := range cases {
		if got := tc.schedule.Describe(); got != tc.want {
			t.Errorf("Describe() = %q, want %q", got, tc.want)
		}
	}
}
