package ledger

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"time"

	"github.com/johnzastrow/bitt/internal/money"
	"github.com/johnzastrow/bitt/internal/schedule"
	"github.com/johnzastrow/bitt/internal/store"
)

// expectedPoint is one date on which the Payee was expected to pay a certain
// amount. It is the unit late-fee assessment walks over, and it means the same
// thing for both tab kinds even though the two build it from different sources.
type expectedPoint struct {
	// Date is the due date the expectation attaches to.
	Date schedule.Date
	// Base is the amount expected for this one period -- the period charge on a
	// Services tab, the installment on a Payoff tab. A percentage fee is taken
	// on this, never on a running balance (FEE-05).
	Base money.Cents
}

// assessFees posts a late fee for every due date past its grace period whose
// expected amount was not met (FEE-03).
//
// It runs lazily in the read path, exactly like charge accrual, and claims each
// date through PostFeeEntry so a date is assessed at most once however many
// readers pass over it at once (FEE-04). Fees never compound: the percentage is
// taken on the period's own charge, and the cap bounds the tab's total.
func (s *Service) assessFees(ctx context.Context, tab store.Tab, history []store.TabItem, loc *time.Location) ([]store.Entry, error) {
	entries, err := s.store.ListEntries(ctx, tab.ID)
	if err != nil {
		return nil, err
	}

	points, err := s.expectedSchedule(ctx, tab, history, entries, loc)
	if err != nil {
		return nil, err
	}
	if len(points) == 0 {
		return nil, nil
	}

	assessed, err := s.store.ListPostedFees(ctx, tab.ID)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool, len(assessed))
	for _, f := range assessed {
		seen[f.Key] = true
	}

	reversed := ReversedSeqs(entries)
	accrued := accruedFees(entries, reversed)
	today := schedule.DateOf(s.now().In(loc))

	// Each installment is judged on the payments made in its own window, which
	// is what makes a fee independent per period: pay this period's installment
	// and this period is clear, even while an earlier miss stays fined and does
	// not drag later periods down with it. The windows partition the timeline at
	// the grace deadlines, so a payment counts toward exactly one installment
	// and never toward two.
	var (
		posted   []store.Entry
		prev     schedule.Date
		havePrev bool
	)
	for _, pt := range points {
		// Grace of N days means the payment is due by date+N, so a fee falls
		// only once that deadline is strictly past -- a payment made on the
		// deadline day itself is on time. Points are ordered by date and the
		// grace is constant, so once one date's deadline is not yet past, none
		// after it are either.
		graceDeadline := pt.Date.AddDays(tab.Fee.GraceDays)
		if !graceDeadline.Before(today) {
			break
		}

		// Payments in (previous deadline, this deadline]. Only dates on or
		// before this deadline count, so a late catch-up cannot retroactively
		// clear a period that was short when its own deadline passed -- the
		// assessment is deterministic regardless of when the tab is read.
		lower := prev
		paid := paidWindow(entries, reversed, lower, havePrev, graceDeadline, loc)
		prev, havePrev = graceDeadline, true

		key := pt.Date.String()
		if seen[key] {
			continue
		}
		if paid >= pt.Base {
			continue
		}

		amount, ok := tab.Fee.Assess(pt.Base, accrued)
		if !ok {
			return posted, fmt.Errorf("%w: fee on tab %d for %s", ErrOverflow, tab.ID, key)
		}
		if amount <= 0 {
			// The cap is reached, or the policy sized this to nothing. Do not
			// claim the date: if a later waiver frees cap room, it can still
			// post on a subsequent read.
			continue
		}

		entry, replayed, err := s.postFee(ctx, tab, pt, key, amount, graceDeadline, loc)
		if err != nil {
			return posted, err
		}
		if !replayed {
			posted = append(posted, entry)
			accrued += amount
		}
	}
	return posted, nil
}

// expectedSchedule builds the list of dated expectations for a tab, oldest
// first. The two kinds differ only here.
func (s *Service) expectedSchedule(ctx context.Context, tab store.Tab, history []store.TabItem, entries []store.Entry, loc *time.Location) ([]expectedPoint, error) {
	today := schedule.DateOf(s.now().In(loc))
	if tab.Kind == store.TabPayoff {
		return payoffExpectations(tab, entries, today), nil
	}
	return s.servicesExpectations(ctx, tab, entries)
}

// servicesExpectations derives expectations from the charges the schedule has
// already posted. A Services period that was reversed creates no expectation.
func (s *Service) servicesExpectations(ctx context.Context, tab store.Tab, entries []store.Entry) ([]expectedPoint, error) {
	periods, err := s.store.ListPostedPeriods(ctx, tab.ID)
	if err != nil {
		return nil, err
	}
	reversed := ReversedSeqs(entries)
	amountBySeq := make(map[int64]money.Cents, len(entries))
	for _, e := range entries {
		amountBySeq[e.Seq] = e.Amount
	}

	// Oldest first, so the cumulative total builds in date order.
	ordered := append([]store.PostedPeriod(nil), periods...)
	slices.SortFunc(ordered, func(a, b store.PostedPeriod) int { return a.DueOn.Compare(b.DueOn) })

	var points []expectedPoint
	for _, p := range ordered {
		if reversed[p.EntrySeq] {
			continue
		}
		// Charges are stored negative; the expected amount is the magnitude.
		base := amountBySeq[p.EntrySeq].Neg()
		if base <= 0 {
			continue
		}
		points = append(points, expectedPoint{Date: p.DueOn, Base: base})
	}
	return points, nil
}

// payoffExpectations derives expectations from the schedule and the loan's
// scheduled payment, bounded by its term.
//
// The payment comes from the tab's own field, which is the figure the Provider
// took off the lender's paperwork. It used to be the sum of the tab's line
// items -- the same field a Services tab bills from -- and one field meaning
// both "what is charged" and "what is expected" is what made a mis-set loan
// read as settled.
//
// The bound is the term. It used to be the principal, which stopped expecting
// payments once they added up to the loan amount; on an interest-bearing loan
// that is sooner than the loan can actually be repaid, so a borrower paying
// exactly on time would stop being judged part-way through and the last months
// of the schedule went unfined. With no term the loan is open-ended and the
// principal remains the only honest bound available.
func payoffExpectations(tab store.Tab, entries []store.Entry, today schedule.Date) []expectedPoint {
	installment := tab.LoanPayment
	if installment <= 0 {
		return nil
	}
	principal := chargedPrincipal(entries)
	if principal <= 0 {
		return nil
	}

	var (
		points []expectedPoint
		cum    money.Cents
	)
	for n := 0; n < schedule.MaxPeriods; n++ {
		if tab.LoanTermPeriods > 0 && n >= tab.LoanTermPeriods {
			break
		}
		due := tab.Schedule.Period(n).Due
		if due.After(today) {
			break
		}
		base := installment
		if tab.LoanTermPeriods > 0 {
			// A termed loan expects a full installment every period of its
			// term. The last one is smaller in practice, but the fee is judged
			// on what was asked for, and the schedule asks for the installment.
			points = append(points, expectedPoint{Date: due, Base: base})
			continue
		}
		// Open-ended: expectations stop once the scheduled payments would have
		// covered the principal, since nothing states how long the loan runs.
		if cum >= principal {
			break
		}
		if cum+base > principal {
			base = principal - cum // the final, partial installment
		}
		cum += base
		points = append(points, expectedPoint{Date: due, Base: base})
	}
	return points
}

// postFee writes one late-fee entry and claims its date.
func (s *Service) postFee(ctx context.Context, tab store.Tab, pt expectedPoint, key string, amount money.Cents, deadline schedule.Date, loc *time.Location) (store.Entry, bool, error) {
	return s.store.PostFeeEntry(ctx,
		store.PostedFee{
			TabID:       tab.ID,
			Key:         key,
			AssessedFor: pt.Date,
			Base:        pt.Base,
		},
		store.NewEntry{
			TabID: tab.ID,
			Kind:  store.KindFee,
			// A fee increases what is owed, so it moves the balance negative,
			// like a charge. The ledger owns the sign.
			Amount:      amount.Neg(),
			Memo:        FeeMemo(pt.Date),
			EffectiveAt: deadline.Time(loc),
			ActorUserID: tab.CreatedBy,
			// Derived, so the entries index is a second guard against a date
			// being assessed twice even if the fee claim were bypassed.
			IdempotencyKey: FeeIdempotencyKey(tab.ID, key),
			Method:         store.MethodNone,
		})
}

// FeeIdempotencyKey derives the key a late fee posts under.
func FeeIdempotencyKey(tabID int64, dateKey string) string {
	return "fee:" + strconv.FormatInt(tabID, 10) + ":" + dateKey
}

// FeeMemo describes the date a late fee answers to.
func FeeMemo(due schedule.Date) string {
	return "Late fee for the amount due " + due.Display()
}

// paidWindow totals the payments effective within (lower, upper], excluding any
// that were reversed. When hasLower is false the window is open at the bottom,
// (-infinity, upper], which is the first installment's window.
//
// The half-open shape -- exclusive lower, inclusive upper -- is what partitions
// the timeline cleanly at the grace deadlines, so a payment is attributed to
// exactly one installment.
func paidWindow(entries []store.Entry, reversed map[int64]bool, lower schedule.Date, hasLower bool, upper schedule.Date, loc *time.Location) money.Cents {
	var total money.Cents
	for _, e := range entries {
		if e.Kind != store.KindPayment || reversed[e.Seq] {
			continue
		}
		on := schedule.DateOf(e.EffectiveAt.In(loc))
		if on.After(upper) {
			continue
		}
		if hasLower && !on.After(lower) {
			continue // on or before the previous deadline: a prior window's
		}
		total += e.Amount
	}
	return total
}

// accruedFees totals the late fees standing on a tab, excluding waived ones, so
// the cap is measured against fees that still count (FEE-06, FEE-07).
func accruedFees(entries []store.Entry, reversed map[int64]bool) money.Cents {
	var total money.Cents
	for _, e := range entries {
		if e.Kind != store.KindFee || reversed[e.Seq] {
			continue
		}
		total += e.Amount.Neg()
	}
	return total
}

// chargedPrincipal totals the charges on a tab, excluding reversed ones. On a
// Payoff tab this is the loan amount drawn down by payments.
func chargedPrincipal(entries []store.Entry) money.Cents {
	reversed := ReversedSeqs(entries)
	var total money.Cents
	for _, e := range entries {
		if e.Kind != store.KindCharge || reversed[e.Seq] {
			continue
		}
		total += e.Amount.Neg()
	}
	return total
}

// activeItems returns the items a tab currently carries, dropping superseded
// and removed rows.
func activeItems(history []store.TabItem) []store.TabItem {
	out := make([]store.TabItem, 0, len(history))
	for _, it := range history {
		if it.RemovedAt == nil {
			out = append(out, it)
		}
	}
	return out
}
