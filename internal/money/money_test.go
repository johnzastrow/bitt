package money

import (
	"errors"
	"testing"
)

func TestParse(t *testing.T) {
	cases := []struct {
		in   string
		want Cents
	}{
		{"0", 0},
		{"0.00", 0},
		{"1", 100},
		{"12.34", 1234},
		{"12.3", 1230},
		{"12.", 1200},
		{".5", 50},
		{".05", 5},
		{"0.07", 7}, // the classic float-rounding trap
		{"0.10", 10},
		{"0.29", 29},
		{"-7", -700},
		{"-0.07", -7},
		{"+3.50", 350},
		{"$45.00", 4500},
		{" $1,234.56 ", 123456},
		{"1,000", 100000},
		{"99999999.99", 9999999999},
	}
	for _, c := range cases {
		got, err := Parse(c.in)
		if err != nil {
			t.Errorf("Parse(%q) unexpected error: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("Parse(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestParseRejects(t *testing.T) {
	cases := []struct {
		in      string
		wantErr error
	}{
		{"", ErrMalformed},
		{"   ", ErrMalformed},
		{"abc", ErrMalformed},
		{"1.2.3", ErrMalformed},
		{"12.345", ErrTooManyDecimals}, // never silently round a ledger amount
		{"0.001", ErrTooManyDecimals},
		{"1e3", ErrMalformed}, // no scientific notation into the money path
		{"NaN", ErrMalformed},
		{"Inf", ErrMalformed},
		{"$", ErrMalformed},
		{"-", ErrMalformed},
		{"99999999999999999999", ErrOutOfRange},
	}
	for _, c := range cases {
		_, err := Parse(c.in)
		if err == nil {
			t.Errorf("Parse(%q) = nil error, want %v", c.in, c.wantErr)
			continue
		}
		if !errors.Is(err, c.wantErr) {
			t.Errorf("Parse(%q) error = %v, want %v", c.in, err, c.wantErr)
		}
	}
}

func TestString(t *testing.T) {
	cases := []struct {
		in   Cents
		want string
	}{
		{0, "0.00"},
		{5, "0.05"},
		{50, "0.50"},
		{100, "1.00"},
		{1234, "12.34"},
		{-7, "-0.07"},
		{-1234, "-12.34"},
		{123456789, "1234567.89"},
	}
	for _, c := range cases {
		if got := c.in.String(); got != c.want {
			t.Errorf("Cents(%d).String() = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestDisplay(t *testing.T) {
	cases := []struct {
		in   Cents
		want string
	}{
		{0, "$0.00"},
		{7, "$0.07"},
		{4500, "$45.00"},
		{123456, "$1,234.56"},
		{100000000, "$1,000,000.00"},
		{-123456, "-$1,234.56"},
		{999, "$9.99"},
		{1000, "$10.00"},
	}
	for _, c := range cases {
		if got := c.in.Display(); got != c.want {
			t.Errorf("Cents(%d).Display() = %q, want %q", c.in, got, c.want)
		}
	}
}

// Round-tripping must be exact for every cent value in a wide range. This is
// the property that a float-backed implementation fails.
func TestRoundTripExact(t *testing.T) {
	for n := Cents(-20000); n <= 20000; n++ {
		s := n.String()
		got, err := Parse(s)
		if err != nil {
			t.Fatalf("Parse(%q) error: %v", s, err)
		}
		if got != n {
			t.Fatalf("round trip %d -> %q -> %d", n, s, got)
		}
	}
}

// Repeatedly accumulating a value that has no exact binary representation must
// stay exact. Adding 0.10 a thousand times is exactly $100.00, not $99.999...
func TestAccumulationExact(t *testing.T) {
	dime, err := Parse("0.10")
	if err != nil {
		t.Fatal(err)
	}
	var total Cents
	for i := 0; i < 1000; i++ {
		var ok bool
		total, ok = Add(total, dime)
		if !ok {
			t.Fatal("unexpected overflow")
		}
	}
	if total.String() != "100.00" {
		t.Errorf("1000 x 0.10 = %s, want 100.00", total)
	}
}

func TestAddOverflow(t *testing.T) {
	const maxCents = Cents(9223372036854775807)
	if _, ok := Add(maxCents, 1); ok {
		t.Error("Add did not report positive overflow")
	}
	const minCents = Cents(-9223372036854775808)
	if _, ok := Add(minCents, -1); ok {
		t.Error("Add did not report negative overflow")
	}
	if v, ok := Add(500, -200); !ok || v != 300 {
		t.Errorf("Add(500,-200) = %d,%v want 300,true", v, ok)
	}
}

func TestSum(t *testing.T) {
	got, ok := Sum([]Cents{1234, -234, 100})
	if !ok || got != 1100 {
		t.Errorf("Sum = %d,%v want 1100,true", got, ok)
	}
	if _, ok := Sum([]Cents{9223372036854775807, 1}); ok {
		t.Error("Sum did not report overflow")
	}
	if got, ok := Sum(nil); !ok || got != 0 {
		t.Errorf("Sum(nil) = %d,%v want 0,true", got, ok)
	}
}

func TestNeg(t *testing.T) {
	if got := Cents(1234).Neg(); got != -1234 {
		t.Errorf("Neg = %d, want -1234", got)
	}
	if got := Cents(-1234).Neg(); got != 1234 {
		t.Errorf("Neg = %d, want 1234", got)
	}
}

func TestPercent(t *testing.T) {
	cases := []struct {
		name string
		base Cents
		bp   int64
		want Cents
	}{
		{"five percent of $100", 10000, 500, 500},
		{"ten percent of $250", 25000, 1000, 2500},
		{"2.5 percent of $250", 25000, 250, 625},
		{"zero rate", 25000, 0, 0},
		{"zero base", 0, 500, 0},
		{"one hundred percent", 4200, 10000, 4200},
		// Rounding: half a cent rounds up, deterministically.
		{"rounds half up", 1, 5000, 1},              // 0.5 cent -> 1
		{"rounds just under half down", 199, 25, 0}, // 0.4975 cent -> 0
		{"rounds just over half up", 201, 25, 1},    // 0.5025 cent -> 1
		{"three percent of $33.33", 3333, 300, 100}, // 99.99 -> 100
		{"seven percent of $14.29", 1429, 700, 100}, // 100.03 -> 100
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := Percent(tc.base, tc.bp)
			if !ok {
				t.Fatalf("Percent(%d, %d) reported overflow", tc.base, tc.bp)
			}
			if got != tc.want {
				t.Errorf("Percent(%d, %d) = %d, want %d", tc.base, tc.bp, got, tc.want)
			}
		})
	}
}

func TestPercentIsDeterministicAndHalfUp(t *testing.T) {
	// The exact half-cent boundary must always round up, and must not depend on
	// any floating-point representation.
	for base := Cents(1); base <= 400; base++ {
		got, ok := Percent(base, 5000) // 50% -> base/2
		if !ok {
			t.Fatalf("overflow at base %d", base)
		}
		// base/2 rounded half up.
		want := Cents((int64(base) + 1) / 2)
		if got != want {
			t.Errorf("Percent(%d, 5000) = %d, want %d", base, got, want)
		}
	}
}

func TestPercentRejectsNegativeAndOverflow(t *testing.T) {
	if _, ok := Percent(-100, 500); ok {
		t.Error("Percent accepted a negative base")
	}
	if _, ok := Percent(100, -500); ok {
		t.Error("Percent accepted a negative rate")
	}
	// A base and rate whose product overflows int64 must be reported, not wrapped.
	if _, ok := Percent(Cents(1<<62), 1000); ok {
		t.Error("Percent did not report overflow on a huge product")
	}
}

func TestInterestOn(t *testing.T) {
	cases := []struct {
		name    string
		balance Cents
		aprBP   int64
		ppy     int
		want    Cents
	}{
		{"6% monthly on $5000", 500000, 600, 12, 2500}, // 0.5%/mo
		{"6% monthly on $4775", 477500, 600, 12, 2388}, // 2387.5 -> 2388 half up
		{"12% weekly on $1000", 100000, 1200, 52, 231}, // 100000*1200/520000 = 230.77 -> 231
		{"0% is nothing", 500000, 0, 12, 0},
		{"zero balance", 0, 600, 12, 0},
		{"annual, one period", 100000, 500, 1, 5000}, // 5% of $1000
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := InterestOn(tc.balance, tc.aprBP, tc.ppy)
			if !ok {
				t.Fatalf("InterestOn reported overflow")
			}
			if got != tc.want {
				t.Errorf("InterestOn(%d, %d, %d) = %d, want %d", tc.balance, tc.aprBP, tc.ppy, got, tc.want)
			}
		})
	}
}

func TestInterestOnRejectsBadInput(t *testing.T) {
	if _, ok := InterestOn(-1, 600, 12); ok {
		t.Error("accepted a negative balance")
	}
	if _, ok := InterestOn(100, -1, 12); ok {
		t.Error("accepted a negative rate")
	}
	if _, ok := InterestOn(100, 600, 0); ok {
		t.Error("accepted zero periods per year")
	}
	if _, ok := InterestOn(Cents(1<<62), 1200, 12); ok {
		t.Error("did not report overflow")
	}
}
