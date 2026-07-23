// Package money provides an integer-cents monetary type.
//
// LEDGER-04: all monetary amounts are stored and computed as integer USD cents.
// No floating-point arithmetic appears anywhere in this package, including
// parsing and display formatting. Nothing here converts to or from float32,
// float64, or any type that would round through one.
package money

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Cents is a USD amount in integer minor units. Positive values are credits to
// the party holding the tab; negative values are amounts owed. Callers should
// use the ledger's entry kinds rather than relying on sign conventions here.
type Cents int64

// Zero is the additive identity, provided so callers need not write money.Cents(0).
const Zero Cents = 0

var (
	// ErrMalformed is returned when input is not a well-formed decimal amount.
	ErrMalformed = errors.New("money: malformed amount")
	// ErrTooManyDecimals is returned when input carries more than two decimal places.
	// Amounts are never silently rounded, since a rounded amount in a ledger is a
	// wrong amount.
	ErrTooManyDecimals = errors.New("money: more than two decimal places")
	// ErrOutOfRange is returned when input does not fit in an int64 cent count.
	ErrOutOfRange = errors.New("money: amount out of range")
)

// maxWholeUnits bounds the integer part so that the units-to-cents
// multiplication cannot overflow int64. int64 holds roughly 9.22e18 cents.
const maxWholeUnits = 92_233_720_368_547_757

// Parse converts a decimal string such as "12.34", "-7", or "0.05" into Cents.
//
// It accepts an optional leading sign, an optional leading "$", digit group
// separators, and zero, one, or two decimal places. It parses digits directly
// into integers and never routes through a float, so "0.07" is exactly 7 cents
// rather than the nearest representable double.
func Parse(s string) (Cents, error) {
	raw := strings.TrimSpace(s)
	if raw == "" {
		return 0, fmt.Errorf("%w: empty", ErrMalformed)
	}

	neg := false
	switch raw[0] {
	case '+':
		raw = raw[1:]
	case '-':
		neg = true
		raw = raw[1:]
	}
	raw = strings.TrimPrefix(strings.TrimSpace(raw), "$")
	raw = strings.ReplaceAll(raw, ",", "")
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, fmt.Errorf("%w: no digits", ErrMalformed)
	}

	whole, frac, hasFrac := strings.Cut(raw, ".")
	if hasFrac && strings.Contains(frac, ".") {
		return 0, fmt.Errorf("%w: multiple decimal points", ErrMalformed)
	}
	// ".50" and "12." are both accepted as shorthand.
	if whole == "" {
		whole = "0"
	}
	if hasFrac && frac == "" {
		frac = "0"
	}

	if !allDigits(whole) || (hasFrac && !allDigits(frac)) {
		return 0, fmt.Errorf("%w: %q", ErrMalformed, s)
	}
	if len(frac) > 2 {
		return 0, fmt.Errorf("%w: %q", ErrTooManyDecimals, s)
	}

	units, err := strconv.ParseInt(whole, 10, 64)
	if err != nil || units > maxWholeUnits {
		return 0, fmt.Errorf("%w: %q", ErrOutOfRange, s)
	}

	// Pad so that "5" and "50" in the fractional position both mean 50 cents.
	var subunits int64
	switch len(frac) {
	case 0:
	case 1:
		d, _ := strconv.ParseInt(frac, 10, 64)
		subunits = d * 10
	case 2:
		d, _ := strconv.ParseInt(frac, 10, 64)
		subunits = d
	}

	total := units*100 + subunits
	if total < 0 {
		return 0, fmt.Errorf("%w: %q", ErrOutOfRange, s)
	}
	if neg {
		total = -total
	}
	return Cents(total), nil
}

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// String renders the amount as a plain decimal with exactly two places, such as
// "12.34" or "-0.07". Division and remainder are integer operations.
func (c Cents) String() string {
	neg := c < 0
	n := int64(c)
	if neg {
		// Negating math.MinInt64 overflows; handle it via unsigned arithmetic.
		if n == -9223372036854775808 {
			return "-92233720368547758.08"
		}
		n = -n
	}
	units := n / 100
	sub := n % 100
	var b strings.Builder
	if neg {
		b.WriteByte('-')
	}
	b.WriteString(strconv.FormatInt(units, 10))
	b.WriteByte('.')
	if sub < 10 {
		b.WriteByte('0')
	}
	b.WriteString(strconv.FormatInt(sub, 10))
	return b.String()
}

// Display renders the amount for the interface, with a currency symbol and
// thousands separators: "$1,234.56" or "-$0.07". USD only (see PROJECT.md).
func (c Cents) Display() string {
	s := c.String()
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")

	units, frac, _ := strings.Cut(s, ".")
	var b strings.Builder
	if neg {
		b.WriteByte('-')
	}
	b.WriteByte('$')
	b.WriteString(group(units))
	b.WriteByte('.')
	b.WriteString(frac)
	return b.String()
}

// group inserts commas every three digits from the right.
func group(digits string) string {
	n := len(digits)
	if n <= 3 {
		return digits
	}
	var b strings.Builder
	lead := n % 3
	if lead > 0 {
		b.WriteString(digits[:lead])
	}
	for i := lead; i < n; i += 3 {
		if b.Len() > 0 {
			b.WriteByte(',')
		}
		b.WriteString(digits[i : i+3])
	}
	return b.String()
}

// Add returns the sum, reporting whether it overflowed int64. Callers in the
// money path must check the boolean rather than ignoring it: a silently wrapped
// balance is the precise failure this project exists to prevent.
func Add(a, b Cents) (Cents, bool) {
	sum := a + b
	if (a > 0 && b > 0 && sum < 0) || (a < 0 && b < 0 && sum > 0) {
		return 0, false
	}
	return sum, true
}

// Sum totals a slice, reporting whether the running total overflowed.
func Sum(values []Cents) (Cents, bool) {
	var total Cents
	for _, v := range values {
		var ok bool
		total, ok = Add(total, v)
		if !ok {
			return 0, false
		}
	}
	return total, true
}

// Neg returns the additive inverse, used when writing reversing entries.
func (c Cents) Neg() Cents { return -c }

// IsZero reports whether the amount is exactly zero.
func (c Cents) IsZero() bool { return c == 0 }
