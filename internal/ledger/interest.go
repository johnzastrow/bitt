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
// however many readers pass over it at once.
//
// Each period's interest is computed on the outstanding *principal* as of that
// period's date, and on nothing else. As the loan is paid down the interest
// falls. Interest that a short payment did not cover does not join that base:
// it sits in the unpaid-interest bucket AllocateLoan maintains, where it is
// owed and is repaid before principal, but never itself accrues.
//
// That is the U.S. Rule, which Regulation Z permits for closed-end credit and
// which consumer lenders run. It matters here for a practical reason rather
// than a legal one: a borrower who misses a payment and then catches up must
// end up where their bank says they are. Capitalizing the missed interest into
// principal would compound it, and the two figures would separate permanently
// after the first missed payment, with no way back.
func (s *Service) accrueInterest(ctx context.Context, tab store.Tab, loc *time.Location) ([]store.Entry, error) {
	// The fraction of a year one period covers. Monthly is 1/12, the APR/12 a
	// borrower can check by hand; a three-week cycle is 21/365. A fraction
	// rather than a periods-per-year count is what lets an arbitrary interval
	// stay exact.
	rateNum, rateDen := tab.Schedule.RateBasis()
	if rateNum <= 0 || rateDen <= 0 {
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

		// The base is the outstanding principal as of this period and nothing
		// else: not the unpaid interest, which never accrues, and not late
		// fees, which are a penalty rather than part of the loan.
		base := AllocateLoan(entries, due, loc).Principal
		if base <= 0 {
			// No principal outstanding this period, so no interest. Do not claim
			// the period: a later charge could legitimately make it owe again.
			continue
		}
		amount, ok := money.InterestFor(base, tab.InterestAPRBp, rateNum, rateDen)
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
