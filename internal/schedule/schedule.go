// Package schedule computes recurring billing periods.
//
// SCHED-01, SCHED-02: everything here is pure calendar arithmetic over civil
// dates. There is no database dependency and no clock of its own -- the caller
// supplies "now". Keeping it pure is what makes the awkward cases affordable to
// test exhaustively, and the awkward cases are where this phase's risk lives:
// month-end anchors, leap years, and daylight saving transitions.
//
// Civil dates rather than instants, deliberately. A period has come due when
// its due *date* has arrived in the instance timezone. Nothing here ever adds a
// duration to a timestamp, so a 23- or 25-hour day cannot advance or delay a
// period. The only place an instant appears is Date.Time, at the boundary where
// a period start becomes an entry's effective_at.
//
// Every period is computed directly from the anchor rather than chained off its
// predecessor. That is what keeps a day-31 anchor from decaying to the 28th
// permanently after one February (SCHED-02).
package schedule

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// Errors returned by this package.
var (
	// ErrNoSchedule is returned when a tab carries no schedule at all. It is not
	// a failure: a tab may legitimately be billed only by hand (CHG-03).
	ErrNoSchedule = errors.New("schedule: tab has no schedule")
	// ErrBadKind is returned for an unrecognized recurrence.
	ErrBadKind = errors.New("schedule: unrecognized recurrence")
	// ErrBadBilling is returned for an unrecognized billing rule.
	ErrBadBilling = errors.New("schedule: unrecognized billing rule")
	// ErrBadAnchor is returned when the anchor is not a real calendar date.
	ErrBadAnchor = errors.New("schedule: anchor is not a valid date")
)

// MaxPeriods bounds how many periods a single call will produce.
//
// A tab anchored in the distant past would otherwise generate an unbounded
// batch on its first read. The cap truncates instead: the caller posts what it
// gets and the next read continues from where this one stopped, which is the
// same catch-up path a tab left alone for six months already takes.
const MaxPeriods = 600

// ---------------------------------------------------------------------------
// Kind -- the recurrence (SCHED-01)
// ---------------------------------------------------------------------------

// Kind is a tab's recurrence rule.
type Kind string

const (
	// None means the tab has no schedule and is billed only by hand.
	None Kind = ""
	// Weekly repeats every 7 days from the anchor.
	Weekly Kind = "weekly"
	// Biweekly repeats every 14 days from the anchor.
	Biweekly Kind = "biweekly"
	// MonthlyDay repeats on the anchor's day of the month, clamped down in
	// months that are too short (SCHED-02).
	MonthlyDay Kind = "monthly_day"
	// MonthlyLast repeats on the last day of each month, whatever that is.
	MonthlyLast Kind = "monthly_last"
)

// Valid reports whether k is a recurrence that can actually be scheduled. None
// is deliberately not valid: it means "do not schedule this tab".
func (k Kind) Valid() bool {
	switch k {
	case Weekly, Biweekly, MonthlyDay, MonthlyLast:
		return true
	}
	return false
}

// Kinds lists the selectable recurrences in display order.
func Kinds() []Kind { return []Kind{Weekly, Biweekly, MonthlyDay, MonthlyLast} }

// PeriodsPerYear is how many times the recurrence repeats in a year, used to
// turn an annual interest rate into a per-period one. The monthly kinds are 12
// and the weekly kinds are counted by weeks; none of this needs to be exact to
// the day, since interest is applied per posted period, not per elapsed second.
func (k Kind) PeriodsPerYear() int {
	switch k {
	case Weekly:
		return 52
	case Biweekly:
		return 26
	case MonthlyDay, MonthlyLast:
		return 12
	}
	return 0
}

// ---------------------------------------------------------------------------
// Billing -- when the charge lands
// ---------------------------------------------------------------------------

// Billing is the Provider's choice of when a period's charge posts. It is a
// per-tab setting because the answer genuinely differs by what is being billed:
// a retainer is owed up front, metered work is owed after the fact.
type Billing string

const (
	// InAdvance posts a period's charge on the period's first day, owed that
	// same day. Rent, subscriptions, and retainers work this way.
	InAdvance Billing = "advance"
	// InArrears posts a period's charge on the day the period ends, billing for
	// the cycle just completed. Utilities and hourly work work this way.
	InArrears Billing = "arrears"
)

// Valid reports whether b is a recognized billing rule.
func (b Billing) Valid() bool { return b == InAdvance || b == InArrears }

// Billings lists the selectable billing rules in display order.
func Billings() []Billing { return []Billing{InAdvance, InArrears} }

// Label renders the billing rule for display.
func (b Billing) Label() string {
	switch b {
	case InAdvance:
		return "At the start of each period"
	case InArrears:
		return "At the end of each period"
	default:
		return ""
	}
}

// ---------------------------------------------------------------------------
// Date -- a civil calendar date with no timezone of its own
// ---------------------------------------------------------------------------

const dateLayout = "2006-01-02"

// Date is a calendar date: a year, month, and day with no time and no zone.
// It is the unit of all arithmetic here, so that a period boundary is a date
// people would recognize rather than an instant that shifts with the zone.
type Date struct {
	Year  int
	Month time.Month
	Day   int
}

// NewDate builds a date. It does not normalize: an impossible date stays
// impossible, and Valid reports it.
func NewDate(year int, month time.Month, day int) Date {
	return Date{Year: year, Month: month, Day: day}
}

// DateOf returns t's calendar date in t's own location. Callers convert to the
// instance timezone first; this function does not guess.
func DateOf(t time.Time) Date {
	y, m, d := t.Date()
	return Date{Year: y, Month: m, Day: d}
}

// ParseDate reads an ISO-8601 date, "2026-02-28".
func ParseDate(s string) (Date, error) {
	raw := strings.TrimSpace(s)
	t, err := time.ParseInLocation(dateLayout, raw, time.UTC)
	if err != nil {
		return Date{}, fmt.Errorf("%w: %q", ErrBadAnchor, raw)
	}
	d := Date{Year: t.Year(), Month: t.Month(), Day: t.Day()}
	// Round-trip as a second line of defense. time.Parse rejects a day out of
	// range on its own, but a parser that ever started normalizing instead of
	// rejecting would silently turn February 31st into March 3rd.
	if d.String() != raw {
		return Date{}, fmt.Errorf("%w: %q is not a real date", ErrBadAnchor, raw)
	}
	return d, nil
}

// String renders the date as ISO-8601, which is also its period key. The
// format sorts lexicographically, matching how timestamps are stored.
func (d Date) String() string {
	return fmt.Sprintf("%04d-%02d-%02d", d.Year, int(d.Month), d.Day)
}

// Display renders the date for a person to read.
func (d Date) Display() string {
	if !d.Valid() {
		return ""
	}
	return d.Time(time.UTC).Format("Jan 2, 2006")
}

// IsZero reports whether the date is unset.
func (d Date) IsZero() bool { return d == Date{} }

// Valid reports whether the date exists on the calendar, which means February
// 29th is valid in a leap year and not otherwise.
func (d Date) Valid() bool {
	if d.Year < 1 || d.Month < time.January || d.Month > time.December {
		return false
	}
	return d.Day >= 1 && d.Day <= daysIn(d.Year, d.Month)
}

// Compare orders two dates, returning -1, 0, or 1.
func (d Date) Compare(o Date) int {
	switch {
	case d.Year != o.Year:
		return sign(d.Year - o.Year)
	case d.Month != o.Month:
		return sign(int(d.Month) - int(o.Month))
	case d.Day != o.Day:
		return sign(d.Day - o.Day)
	}
	return 0
}

// Before reports whether d falls earlier than o.
func (d Date) Before(o Date) bool { return d.Compare(o) < 0 }

// After reports whether d falls later than o.
func (d Date) After(o Date) bool { return d.Compare(o) > 0 }

// AddDays returns the date n days later, n possibly negative.
//
// The arithmetic runs in UTC purely as a calendar. UTC has no daylight saving
// transition, so adding a day is exactly adding a day; doing this in a zone
// with DST is how "one week later" becomes six days and 23 hours.
func (d Date) AddDays(n int) Date {
	t := time.Date(d.Year, d.Month, d.Day+n, 0, 0, 0, 0, time.UTC)
	return Date{Year: t.Year(), Month: t.Month(), Day: t.Day()}
}

// Time materializes the date as the first instant of that day in loc.
//
// This is the only place a civil date becomes an instant, and it is the one
// place daylight saving can still do damage. A handful of zones move their
// clocks at midnight -- Chile, and historically Brazil -- so on a transition
// date 00:00 simply does not exist. Handed a time in that gap, time.Date
// resolves it *backwards*: asking for 2026-09-06 00:00 in America/Santiago
// returns 2026-09-05 23:00, the previous calendar day. An entry stamped that
// way would carry the wrong date for its own period.
//
// So the result is checked and walked forward to the first hour that genuinely
// falls on the requested date. Four hours is well past the largest transition
// any zone has ever applied.
func (d Date) Time(loc *time.Location) time.Time {
	if loc == nil {
		loc = time.UTC
	}
	t := time.Date(d.Year, d.Month, d.Day, 0, 0, 0, 0, loc)
	if DateOf(t) == d {
		return t
	}
	for hour := 1; hour <= 4; hour++ {
		if shifted := time.Date(d.Year, d.Month, d.Day, hour, 0, 0, 0, loc); DateOf(shifted) == d {
			return shifted
		}
	}
	return t
}

// ---------------------------------------------------------------------------
// Schedule
// ---------------------------------------------------------------------------

// Schedule is a tab's recurrence: an anchor date plus a rule, plus the
// Provider's choice of when in each period the charge lands (SCHED-01).
type Schedule struct {
	Kind    Kind
	Anchor  Date
	Billing Billing
}

// Set reports whether the tab is scheduled at all.
func (s Schedule) Set() bool { return s.Kind != None }

// Validate reports why the schedule cannot be used, or nil.
func (s Schedule) Validate() error {
	if s.Kind == None {
		return ErrNoSchedule
	}
	if !s.Kind.Valid() {
		return fmt.Errorf("%w: %q", ErrBadKind, s.Kind)
	}
	if !s.Billing.Valid() {
		return fmt.Errorf("%w: %q", ErrBadBilling, s.Billing)
	}
	if !s.Anchor.Valid() {
		return fmt.Errorf("%w: %s", ErrBadAnchor, s.Anchor)
	}
	return nil
}

// Normalize snaps a schedule onto its own grid so that the stored anchor is
// always period zero's start, and fills in the default billing rule.
//
// Only MonthlyLast moves: an anchor of March 10th under "last day of the month"
// means the 31st onward, and storing the 10th would leave the anchor
// disagreeing with every period computed from it.
func (s Schedule) Normalize() Schedule {
	if s.Billing == "" {
		s.Billing = InAdvance
	}
	if s.Kind == MonthlyLast && s.Anchor.Valid() {
		s.Anchor.Day = daysIn(s.Anchor.Year, s.Anchor.Month)
	}
	return s
}

// Describe renders the schedule for a person to read.
func (s Schedule) Describe() string {
	if !s.Set() {
		return "No schedule"
	}
	var when string
	switch s.Kind {
	case Weekly:
		when = "Weekly on " + s.Anchor.Time(time.UTC).Format("Monday") + "s"
	case Biweekly:
		when = "Every two weeks on " + s.Anchor.Time(time.UTC).Format("Monday") + "s"
	case MonthlyDay:
		when = "Monthly on the " + ordinal(s.Anchor.Day)
	case MonthlyLast:
		when = "Monthly on the last day"
	default:
		return "No schedule"
	}
	if s.Billing == InArrears {
		return when + ", billed at period end"
	}
	return when + ", billed at period start"
}

// Nth returns the first day of period n, counting from zero at the anchor.
//
// Monthly kinds compute the month offset from the anchor and clamp the day
// into it. Computing from the anchor rather than from the previous period is
// what makes a day-31 anchor land on February 28th and return to March 31st,
// instead of sticking at the 28th forever (SCHED-02).
func (s Schedule) Nth(n int) Date {
	switch s.Kind {
	case Weekly:
		return s.Anchor.AddDays(7 * n)
	case Biweekly:
		return s.Anchor.AddDays(14 * n)
	case MonthlyDay:
		y, m := addMonths(s.Anchor.Year, s.Anchor.Month, n)
		return Date{Year: y, Month: m, Day: min(s.Anchor.Day, daysIn(y, m))}
	case MonthlyLast:
		y, m := addMonths(s.Anchor.Year, s.Anchor.Month, n)
		return Date{Year: y, Month: m, Day: daysIn(y, m)}
	}
	return Date{}
}

// Period is one billing cycle.
type Period struct {
	// Index counts from zero at the anchor.
	Index int
	// Start is the cycle's first day. End is the day the next cycle starts, so
	// the cycle covers [Start, End).
	Start Date
	End   Date
	// Due is when the charge posts and is owed (SCHED-05). Under InAdvance it
	// is Start; under InArrears it is End. It is derived from the schedule
	// alone -- no invoice record supplies it.
	Due Date
}

// Key identifies the period within its tab, and is what the unique constraint
// on (tab, period) is built from (SCHED-04).
//
// It is the period's start date, not its due date, so that a Provider changing
// the billing rule cannot cause an already-posted cycle to post a second time
// under a different key.
func (p Period) Key() string { return p.Start.String() }

// Period returns cycle n.
func (s Schedule) Period(n int) Period {
	start, end := s.Nth(n), s.Nth(n+1)
	due := start
	if s.Billing == InArrears {
		due = end
	}
	return Period{Index: n, Start: start, End: end, Due: due}
}

// DueThrough returns every period whose due date has arrived as of asOf,
// oldest first.
//
// The comparison is between calendar dates in loc, so a tab left alone for six
// months simply reports six months of periods and catch-up needs no separate
// code path (SCHED-03). At most MaxPeriods are returned; a caller that posts a
// full batch should call again.
func (s Schedule) DueThrough(asOf time.Time, loc *time.Location) ([]Period, error) {
	if err := s.Validate(); err != nil {
		return nil, err
	}
	if loc == nil {
		loc = time.UTC
	}
	today := DateOf(asOf.In(loc))

	out := make([]Period, 0, 8)
	for n := 0; n < MaxPeriods; n++ {
		p := s.Period(n)
		if p.Due.After(today) {
			break
		}
		out = append(out, p)
	}
	return out, nil
}

// Next returns the first period that has not yet come due, which is what a tab
// shows as its upcoming charge.
func (s Schedule) Next(asOf time.Time, loc *time.Location) (Period, error) {
	due, err := s.DueThrough(asOf, loc)
	if err != nil {
		return Period{}, err
	}
	return s.Period(len(due)), nil
}

// ---------------------------------------------------------------------------
// Calendar helpers
// ---------------------------------------------------------------------------

// daysIn returns the length of a month, accounting for leap years.
//
// Day zero of the following month normalizes to the last day of this one,
// which is both correct and cheaper than restating the leap-year rule.
func daysIn(year int, month time.Month) int {
	return time.Date(year, month+1, 0, 0, 0, 0, 0, time.UTC).Day()
}

// addMonths shifts a year and month by n months, carrying across year
// boundaries. It works in absolute months so a negative n behaves.
func addMonths(year int, month time.Month, n int) (int, time.Month) {
	total := year*12 + (int(month) - 1) + n
	y := total / 12
	m := total % 12
	if m < 0 {
		m += 12
		y--
	}
	return y, time.Month(m + 1)
}

func sign(n int) int {
	switch {
	case n < 0:
		return -1
	case n > 0:
		return 1
	}
	return 0
}

func ordinal(n int) string {
	suffix := "th"
	// 11th, 12th, and 13th are the exceptions to the digit rule.
	if n%100 < 11 || n%100 > 13 {
		switch n % 10 {
		case 1:
			suffix = "st"
		case 2:
			suffix = "nd"
		case 3:
			suffix = "rd"
		}
	}
	return fmt.Sprintf("%d%s", n, suffix)
}
