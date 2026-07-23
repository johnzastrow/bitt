package fee

import (
	"errors"
	"testing"

	"github.com/johnzastrow/bitt/internal/money"
)

func TestAssessFixed(t *testing.T) {
	p := Policy{Kind: Fixed, Fixed: 2500, GraceDays: 5}

	// A fixed fee ignores the base amount, as long as something was overdue.
	got, ok := p.Assess(10000, 0)
	if !ok || got != 2500 {
		t.Errorf("Assess = %d,%v want 2500,true", got, ok)
	}
	got, _ = p.Assess(50, 0)
	if got != 2500 {
		t.Errorf("a fixed fee on a small overdue amount = %d, want 2500", got)
	}

	// Nothing overdue, no fee.
	if got, _ := p.Assess(0, 0); got != 0 {
		t.Errorf("fee on a zero overdue amount = %d, want 0", got)
	}
}

func TestAssessPercent(t *testing.T) {
	p := Policy{Kind: Percent, PercentBP: 1000, GraceDays: 0} // 10%

	got, ok := p.Assess(25000, 0)
	if !ok || got != 2500 {
		t.Errorf("10%% of $250 = %d,%v want 2500,true", got, ok)
	}

	// FEE-05: the base is the overdue charge, and a caller passing the charge
	// rather than a balance is what keeps fees off fees. The function has no
	// way to compound because it never sees a balance.
	p = Policy{Kind: Percent, PercentBP: 500} // 5%
	if got, _ := p.Assess(12345, 0); got != 617 {
		t.Errorf("5%% of $123.45 = %d, want 617 (617.25 rounds down)", got)
	}
}

// FEE-06: the cap is on the tab's total accrued fees, not on any single fee.
func TestAssessRespectsCap(t *testing.T) {
	p := Policy{Kind: Fixed, Fixed: 3000, Cap: 10000}

	// Under the cap, the whole fee posts.
	if got, _ := p.Assess(0o1, 0); got != 3000 {
		t.Errorf("first fee = %d, want the full 30.00", got)
	}
	if got, _ := p.Assess(1, 9000); got != 1000 {
		t.Errorf("fee near the cap = %d, want only the 10.00 of room left", got)
	}
	// At the cap, nothing more posts.
	if got, _ := p.Assess(1, 10000); got != 0 {
		t.Errorf("fee at the cap = %d, want 0", got)
	}
	if got, _ := p.Assess(1, 12000); got != 0 {
		t.Errorf("fee past the cap = %d, want 0", got)
	}
}

func TestAssessNoCap(t *testing.T) {
	p := Policy{Kind: Fixed, Fixed: 3000, Cap: 0}
	// With no cap, accrued fees never limit a new one.
	if got, _ := p.Assess(1, 1_000_000); got != 3000 {
		t.Errorf("uncapped fee = %d, want 30.00 regardless of accrued", got)
	}
}

func TestAssessUnsetPolicyChargesNothing(t *testing.T) {
	if got, ok := (Policy{}).Assess(10000, 0); !ok || got != 0 {
		t.Errorf("an unset policy assessed %d, want 0", got)
	}
}

func TestAssessReportsOverflow(t *testing.T) {
	p := Policy{Kind: Percent, PercentBP: 1000}
	if _, ok := p.Assess(money.Cents(1<<62), 0); ok {
		t.Error("Assess did not report overflow on a huge base")
	}
}

func TestValidate(t *testing.T) {
	cases := []struct {
		name   string
		policy Policy
		want   error
	}{
		{"unset is fine", Policy{}, nil},
		{"good fixed", Policy{Kind: Fixed, Fixed: 2500, GraceDays: 5}, nil},
		{"good percent", Policy{Kind: Percent, PercentBP: 500, GraceDays: 3, Cap: 10000}, nil},
		{"unknown kind", Policy{Kind: "compound"}, ErrBadKind},
		{"fixed of zero", Policy{Kind: Fixed, Fixed: 0}, ErrBadAmount},
		{"fixed negative", Policy{Kind: Fixed, Fixed: -100}, ErrBadAmount},
		{"percent of zero", Policy{Kind: Percent, PercentBP: 0}, ErrBadRate},
		{"negative grace", Policy{Kind: Fixed, Fixed: 100, GraceDays: -1}, ErrBadGrace},
		{"negative cap", Policy{Kind: Fixed, Fixed: 100, Cap: -1}, ErrBadCap},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.policy.Validate()
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

func TestPercentString(t *testing.T) {
	cases := map[int64]string{
		0:     "0%",
		100:   "1%",
		500:   "5%",
		250:   "2.5%",
		1050:  "10.5%",
		1234:  "12.34%",
		10000: "100%",
		25:    "0.25%",
	}
	for bp, want := range cases {
		if got := PercentString(bp); got != want {
			t.Errorf("PercentString(%d) = %q, want %q", bp, got, want)
		}
	}
}

func TestDescribe(t *testing.T) {
	cases := map[string]Policy{
		"No late fee":                                 {},
		"$25.00 late fee, 5-day grace":                {Kind: Fixed, Fixed: 2500, GraceDays: 5},
		"5% late fee, 3-day grace, capped at $100.00": {Kind: Percent, PercentBP: 500, GraceDays: 3, Cap: 10000},
		"2.5% late fee, 0-day grace":                  {Kind: Percent, PercentBP: 250, GraceDays: 0},
	}
	for want, p := range cases {
		if got := p.Describe(); got != want {
			t.Errorf("Describe() = %q, want %q", got, want)
		}
	}
}

func TestKindValidity(t *testing.T) {
	for _, k := range Kinds() {
		if !k.Valid() {
			t.Errorf("Kinds() offers %q, which is not valid", k)
		}
	}
	if None.Valid() {
		t.Error("None must not be a chargeable kind")
	}
}
