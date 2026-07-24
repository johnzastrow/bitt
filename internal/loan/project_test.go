package loan

import (
	"testing"

	"github.com/johnzastrow/bitt/internal/money"
)

// Project is Simulate with the per-period detail kept, so the two must never
// disagree about how long a loan runs, what the last payment is, or what the
// whole thing costs. This is the test that pins them together.
func TestProjectAgreesWithSimulate(t *testing.T) {
	cases := []struct {
		name    string
		terms   Terms
		payment money.Cents
	}{
		{"car loan, 4.5% over 4 years", monthly(2_200_000, 450, 48), 50_140},
		{"mortgage, 6.5% over 30 years", monthly(35_000_000, 650, 360), 221_240},
		{"interest free", monthly(120_000, 0, 12), 10_000},
		{"one payment clears it", monthly(50_000, 1200, 12), 100_000},
		{"weekly basis", Terms{Principal: 300_000, APRBp: 900, RateNum: 7, RateDen: 365}, 25_000},
		{"with an interest arrears", Terms{
			Principal: 500_000, AccruedUnpaid: 4_000, APRBp: 1200, RateNum: 1, RateDen: 12,
		}, 50_000},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := Simulate(tc.terms, tc.payment)
			rows := Project(tc.terms, tc.payment)

			if !out.Retires {
				if rows != nil {
					t.Fatalf("loan does not retire but Project returned %d rows", len(rows))
				}
				return
			}
			if len(rows) != out.Periods {
				t.Fatalf("Project returned %d rows, Simulate says %d periods", len(rows), out.Periods)
			}

			last := rows[len(rows)-1]
			if last.Payment != out.Final {
				t.Errorf("final payment = %s, Simulate says %s", last.Payment.Display(), out.Final.Display())
			}
			if last.Balance != 0 {
				t.Errorf("balance after the last payment = %s, want zero", last.Balance.Display())
			}

			var total money.Cents
			for _, r := range rows {
				total += r.Payment
			}
			if total != out.Total {
				t.Errorf("payments total %s, Simulate says %s", total.Display(), out.Total.Display())
			}
		})
	}
}

// The schedule must read as a schedule: numbered from one, at the installment
// until the smaller final payment, with the balance falling to zero.
func TestProjectRowsDeclineToZero(t *testing.T) {
	terms := monthly(1_000_000, 600, 24)
	payment, _, ok := SuggestPayment(terms)
	if !ok {
		t.Fatal("SuggestPayment not ok")
	}

	rows := Project(terms, payment)
	if len(rows) != 24 {
		t.Fatalf("got %d payments, want 24", len(rows))
	}

	prev := terms.Principal
	for i, r := range rows {
		if r.Number != i+1 {
			t.Errorf("row %d numbered %d", i, r.Number)
		}
		if r.Balance > prev {
			t.Errorf("payment %d: balance rose from %s to %s",
				r.Number, prev.Display(), r.Balance.Display())
		}
		if r.Balance < 0 {
			t.Errorf("payment %d: balance %s is negative", r.Number, r.Balance.Display())
		}
		if i < len(rows)-1 && r.Payment != payment {
			t.Errorf("payment %d is %s, want the installment %s",
				r.Number, r.Payment.Display(), payment.Display())
		}
		prev = r.Balance
	}
	if last := rows[len(rows)-1]; last.Payment > payment {
		t.Errorf("final payment %s exceeds the installment %s", last.Payment.Display(), payment.Display())
	}
}

// A payment that never gets to principal has no schedule to show, and must not
// produce one that stops at the cap and reads like a payoff.
func TestProjectNonRetiringIsEmpty(t *testing.T) {
	cases := []struct {
		name    string
		terms   Terms
		payment money.Cents
	}{
		{"payment below the interest", monthly(10_000_000, 1200, 360), 50_000},
		{"payment equal to the interest", monthly(1_000_000, 1200, 360), 10_000},
		{"no payment", monthly(500_000, 500, 12), 0},
		{"no principal", Terms{APRBp: 500, RateNum: 1, RateDen: 12}, 10_000},
		{"no rate basis", Terms{Principal: 500_000, APRBp: 500}, 10_000},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if rows := Project(tc.terms, tc.payment); rows != nil {
				t.Errorf("got %d rows, want none", len(rows))
			}
		})
	}
}
