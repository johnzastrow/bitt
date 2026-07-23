package ledger

import (
	"context"
	"strconv"
	"time"

	"github.com/johnzastrow/bitt/internal/money"
	"github.com/johnzastrow/bitt/internal/schedule"
	"github.com/johnzastrow/bitt/internal/store"
)

// accrueInterest posts declining-balance interest for every schedule period
// that has come due and not yet accrued (Payoff loans only).
//
// It runs lazily in the read path like charges and fees, and claims each period
// through PostInterestEntry so interest is charged at most once per period
// however many readers pass over it at once. Each period's interest is computed
// on the loan's outstanding balance as of that period's date, so as the loan is
// paid down the interest falls; unpaid interest is part of that balance, so it
// compounds -- which is how a loan is meant to behave, and is exactly the thing
// a late fee must never do.
func (s *Service) accrueInterest(ctx context.Context, tab store.Tab, loc *time.Location) ([]store.Entry, error) {
	ppy := tab.Schedule.Kind.PeriodsPerYear()
	if ppy <= 0 {
		return nil, nil
	}

	entries, err := s.store.ListEntries(ctx, tab.ID)
	if err != nil {
		return nil, err
	}
	claimed, err := s.store.ListPostedInterest(ctx, tab.ID)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool, len(claimed))
	for _, in := range claimed {
		seen[in.Key] = true
	}

	today := schedule.DateOf(s.now().In(loc))

	var posted []store.Entry
	for n := 0; n < schedule.MaxPeriods; n++ {
		due := tab.Schedule.Period(n).Due
		if due.After(today) {
			break
		}
		key := due.String()
		if seen[key] {
			continue
		}

		// The loan balance as of this period: principal plus interest already
		// charged, less payments made by now, excluding late fees. Interest is
		// on the loan, not on penalties.
		base := loanBalanceThrough(entries, due, loc)
		if base <= 0 {
			// Nothing owed on the loan this period, so no interest. Do not claim
			// the period: a later charge could legitimately make it owe again.
			continue
		}
		amount, ok := money.InterestOn(base, tab.InterestAPRBp, ppy)
		if !ok {
			return posted, ErrOverflow
		}
		if amount <= 0 {
			continue
		}

		entry, replayed, err := s.postInterest(ctx, tab, due, key, base, amount, loc)
		if err != nil {
			return posted, err
		}
		if !replayed {
			posted = append(posted, entry)
			// Reflect this interest in the running entries slice so the next
			// period compounds on it without a re-read.
			entries = append(entries, entry)
		}
	}
	return posted, nil
}

// postInterest writes one interest charge and claims its period.
func (s *Service) postInterest(ctx context.Context, tab store.Tab, due schedule.Date, key string, base, amount money.Cents, loc *time.Location) (store.Entry, bool, error) {
	return s.store.PostInterestEntry(ctx,
		store.PostedInterest{
			TabID:      tab.ID,
			Key:        key,
			AccruedFor: due,
			Base:       base,
		},
		store.NewEntry{
			TabID: tab.ID,
			// Interest is a charge -- it increases what is owed -- carrying the
			// interest category so it can be told apart from principal.
			Kind:           store.KindCharge,
			Category:       store.CategoryInterest,
			Amount:         amount.Neg(),
			Memo:           InterestMemo(due),
			EffectiveAt:    due.Time(loc),
			ActorUserID:    tab.CreatedBy,
			IdempotencyKey: InterestIdempotencyKey(tab.ID, key),
			Method:         store.MethodNone,
		})
}

// InterestIdempotencyKey derives the key a periodic interest charge posts under.
func InterestIdempotencyKey(tabID int64, periodKey string) string {
	return "interest:" + strconv.FormatInt(tabID, 10) + ":" + periodKey
}

// InterestMemo describes the period a charge accrued interest for.
func InterestMemo(due schedule.Date) string {
	return "Interest for the period ending " + due.Display()
}

// loanBalanceThrough is the loan's outstanding balance as of a date: principal
// and interest charged, less payments, all effective on or before the date, and
// excluding late fees. Floored at zero.
//
// It is what the next slice of interest is computed on, so it deliberately omits
// fees (interest never accrues on a penalty) and includes prior interest (a loan
// compounds on unpaid interest).
func loanBalanceThrough(entries []store.Entry, asOf schedule.Date, loc *time.Location) money.Cents {
	reversed := ReversedSeqs(entries)
	var owed money.Cents
	for _, e := range entries {
		if reversed[e.Seq] {
			continue
		}
		if schedule.DateOf(e.EffectiveAt.In(loc)).After(asOf) {
			continue
		}
		switch {
		case e.Kind == store.KindCharge:
			owed += e.Amount.Neg() // principal and prior interest are both charges
		case e.Kind == store.KindPayment:
			owed -= e.Amount
		}
	}
	if owed < 0 {
		return 0
	}
	return owed
}
