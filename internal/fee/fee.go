// Package fee computes late-fee amounts.
//
// Like internal/schedule, this is pure: no database, no clock, no ledger. It
// answers one question -- given an overdue amount and what has already been
// charged in fees, how much fee posts now -- and answers it in integer cents
// with deterministic rounding (FEE-05). When a fee posts, and against which
// dates, is decided in the ledger's accrual path; this package only sizes it.
package fee

import (
	"errors"
	"fmt"

	"github.com/johnzastrow/bitt/internal/money"
)

// Errors returned by this package.
var (
	// ErrBadKind is returned for an unrecognized fee kind.
	ErrBadKind = errors.New("fee: unrecognized fee kind")
	// ErrBadAmount is returned when a fixed fee is not positive.
	ErrBadAmount = errors.New("fee: a fixed fee must be greater than zero")
	// ErrBadRate is returned when a percentage rate is not positive.
	ErrBadRate = errors.New("fee: a percentage rate must be greater than zero")
	// ErrBadGrace is returned when the grace period is negative.
	ErrBadGrace = errors.New("fee: grace days cannot be negative")
	// ErrBadCap is returned when the cap is negative.
	ErrBadCap = errors.New("fee: the cap cannot be negative")
)

// Kind is how a late fee is sized.
type Kind string

const (
	// None means the tab charges no late fee.
	None Kind = ""
	// Fixed is a flat amount per overdue period.
	Fixed Kind = "fixed"
	// Percent is a rate applied to the overdue period charge.
	Percent Kind = "percent"
)

// Valid reports whether k is a fee kind that actually charges something.
func (k Kind) Valid() bool { return k == Fixed || k == Percent }

// Kinds lists the selectable kinds in display order.
func Kinds() []Kind { return []Kind{Fixed, Percent} }

// Policy is a tab's late-fee configuration (FEE-01, FEE-02, FEE-06).
type Policy struct {
	Kind Kind
	// Fixed is the flat amount, used when Kind is Fixed.
	Fixed money.Cents
	// PercentBP is the rate in basis points, used when Kind is Percent.
	// 100 basis points is one percent.
	PercentBP int64
	// GraceDays is how long after a due date a period may stay unpaid before a
	// fee is assessed (FEE-02).
	GraceDays int
	// Cap bounds the total late fees a tab may accrue (FEE-06). Zero means no
	// cap.
	Cap money.Cents
}

// Set reports whether the tab charges a late fee at all.
func (p Policy) Set() bool { return p.Kind.Valid() }

// Validate reports why the policy cannot be used, or nil. An unset policy is
// valid: charging no late fee is a legitimate choice.
func (p Policy) Validate() error {
	if p.Kind == None {
		return nil
	}
	if !p.Kind.Valid() {
		return fmt.Errorf("%w: %q", ErrBadKind, p.Kind)
	}
	switch p.Kind {
	case Fixed:
		if p.Fixed <= 0 {
			return ErrBadAmount
		}
	case Percent:
		if p.PercentBP <= 0 {
			return ErrBadRate
		}
	}
	if p.GraceDays < 0 {
		return ErrBadGrace
	}
	if p.Cap < 0 {
		return ErrBadCap
	}
	return nil
}

// Assess sizes the fee for one overdue point.
//
// base is the overdue period charge -- the amount that was due and went unpaid,
// never a running balance -- so a percentage fee is computed on the charge
// alone and cannot compound on earlier fees (FEE-05).
//
// accrued is the total late fee already standing on the tab. The cap is applied
// against it, so the fee returned is clamped so that the tab's total fees never
// exceed the cap (FEE-06). A point that would push past the cap returns whatever
// room is left, and returns zero once the cap is reached.
//
// The boolean reports an arithmetic overflow rather than letting a wrapped
// amount through. A zero result means no fee posts for this point.
func (p Policy) Assess(base, accrued money.Cents) (money.Cents, bool) {
	if !p.Set() {
		return 0, true
	}
	if base <= 0 {
		return 0, true
	}

	var raw money.Cents
	switch p.Kind {
	case Fixed:
		raw = p.Fixed
	case Percent:
		var ok bool
		if raw, ok = money.Percent(base, p.PercentBP); !ok {
			return 0, false
		}
	default:
		return 0, false
	}

	if raw <= 0 {
		return 0, true
	}

	// The cap is on the tab's total accrued fees, not on any single fee.
	if p.Cap > 0 {
		if accrued >= p.Cap {
			return 0, true
		}
		if room := p.Cap - accrued; raw > room {
			raw = room
		}
	}
	return raw, true
}

// Describe renders the policy for a person to read.
func (p Policy) Describe() string {
	if !p.Set() {
		return "No late fee"
	}
	var base string
	switch p.Kind {
	case Fixed:
		base = p.Fixed.Display() + " late fee"
	case Percent:
		base = PercentString(p.PercentBP) + " late fee"
	}
	base += fmt.Sprintf(", %d-day grace", p.GraceDays)
	if p.Cap > 0 {
		base += ", capped at " + p.Cap.Display()
	}
	return base
}

// PercentString renders a basis-point rate as a percentage, without a float.
// 250 basis points renders as "2.5%", 500 as "5%".
func PercentString(bp int64) string {
	whole := bp / 100
	frac := bp % 100
	if frac == 0 {
		return fmt.Sprintf("%d%%", whole)
	}
	if frac%10 == 0 {
		return fmt.Sprintf("%d.%d%%", whole, frac/10)
	}
	return fmt.Sprintf("%d.%02d%%", whole, frac)
}
