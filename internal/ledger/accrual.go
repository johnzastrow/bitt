package ledger

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/johnzastrow/bitt/internal/schedule"
	"github.com/johnzastrow/bitt/internal/store"
)

// Accrual reports what reading a tab caused to be posted.
type Accrual struct {
	// Posted holds the entries this call wrote, oldest cycle first. It is empty
	// on the overwhelmingly common read where nothing has come due.
	Posted []store.Entry
	// Next is the first cycle that has not yet come due, which is what a tab
	// shows as its upcoming charge.
	Next schedule.Period
	// Scheduled reports whether the tab has a schedule at all.
	Scheduled bool
}

// Accrue posts every billing cycle that has come due (SCHED-03).
//
// This runs inside the read path -- opening a tab, or listing tabs on the
// dashboard, is what makes charges appear. There is no scheduler process, no
// timer, and no cron entry. That is not a simplification to be undone later: a
// scheduler is a second thing that has to be running for balances to be right,
// and the whole commitment of this application is that the balance is right.
//
// Catch-up is inherent rather than engineered. What is due is always the
// schedule's cycles minus the ones already claimed, recomputed from scratch, so
// a tab nobody has opened in six months posts six months of cycles the moment
// somebody does, and one nobody ever opens simply owes nothing yet.
//
// Concurrency is handled underneath, by the claim in PostPeriodEntry. Two
// simultaneous readers both compute the same due list here and both try to post
// it; the database lets exactly one of each cycle through and reports the other
// as replayed (SCHED-04). Nothing in this function needs a lock.
//
// history must be the tab's full item history, including superseded rows: each
// cycle is billed for the items the tab carried when that cycle came due, not
// for the items it carries now (CHG-02).
func (s *Service) Accrue(ctx context.Context, tab store.Tab, history []store.TabItem, loc *time.Location) (Accrual, error) {
	acc := Accrual{Scheduled: tab.Schedule.Set()}
	if !acc.Scheduled {
		return acc, nil
	}
	if loc == nil {
		loc = time.UTC
	}

	due, err := tab.Schedule.DueThrough(s.now(), loc)
	if err != nil {
		return acc, err
	}
	acc.Next = tab.Schedule.Period(len(due))
	if len(due) == 0 {
		return acc, nil
	}

	claimed, err := s.store.ListPostedPeriods(ctx, tab.ID)
	if err != nil {
		return acc, err
	}
	seen := make(map[string]bool, len(claimed))
	for _, p := range claimed {
		seen[p.Key] = true
	}

	for _, p := range due {
		if seen[p.Key()] {
			continue
		}
		entry, replayed, err := s.postPeriod(ctx, tab, p, history, loc)
		if err != nil {
			return acc, err
		}
		// A replay means another reader claimed the cycle first. That is a
		// success, not a conflict -- the charge exists either way.
		if !replayed {
			acc.Posted = append(acc.Posted, entry)
		}
	}
	return acc, nil
}

// postPeriod bills one cycle.
func (s *Service) postPeriod(ctx context.Context, tab store.Tab, p schedule.Period, history []store.TabItem, loc *time.Location) (store.Entry, bool, error) {
	effective := p.Due.Time(loc)

	// Which items to bill. A cycle that came due before the tab existed --
	// possible whenever a Provider backdates the anchor -- is billed for the
	// items the tab was created with, since there were no others. Without this
	// clamp a backdated anchor would post a run of zero-amount cycles.
	itemsAt := effective
	if itemsAt.Before(tab.CreatedAt) {
		itemsAt = tab.CreatedAt
	}

	items := ItemsFrom(ItemsAsOf(history, itemsAt))
	total, ok := SumItems(items)
	if !ok {
		return store.Entry{}, false, fmt.Errorf("%w: tab %d, period %s", ErrOverflow, tab.ID, p.Key())
	}

	// A cycle with no items posts a zero charge rather than being skipped. It
	// still gets claimed, so a Provider who adds items later bills them from the
	// next cycle forward instead of retroactively across every cycle that was
	// passed over (CHG-02).
	return s.store.PostPeriodEntry(ctx,
		store.PostedPeriod{
			TabID: tab.ID,
			Key:   p.Key(),
			Start: p.Start,
			End:   p.End,
			DueOn: p.Due,
		},
		store.NewEntry{
			TabID: tab.ID,
			Kind:  store.KindCharge,
			// Charges move the balance negative; the ledger owns the sign.
			Amount:      total.Neg(),
			Memo:        PeriodMemo(p),
			EffectiveAt: effective,
			// The tab's creator is the Provider on whose behalf the schedule
			// bills. It is a stable id that cannot be detached from the tab.
			ActorUserID: tab.CreatedBy,
			// Derived rather than random, so the entries table's own uniqueness
			// constraint is a second guard against a cycle posting twice, even
			// if the period claim were somehow bypassed (LEDGER-07).
			IdempotencyKey: PeriodIdempotencyKey(tab.ID, p.Key()),
			Method:         store.MethodNone,
			Items:          items,
		})
}

// PeriodIdempotencyKey derives the key a scheduled charge posts under. It is a
// pure function of the tab and cycle, so a retry of the same cycle always
// carries the same key.
func PeriodIdempotencyKey(tabID int64, periodKey string) string {
	return "period:" + strconv.FormatInt(tabID, 10) + ":" + periodKey
}

// PeriodMemo describes the cycle a scheduled charge covers. End is exclusive,
// so the memo names the last day actually inside the cycle.
func PeriodMemo(p schedule.Period) string {
	return fmt.Sprintf("Scheduled charge for %s to %s", p.Start.Display(), p.End.AddDays(-1).Display())
}

// ItemsAsOf returns the items a tab carried at a given moment (CHG-02).
//
// An item counts if it had been created by then and had not yet been superseded
// or retired. Superseding writes a removal time on the old row and creates a
// new one, so this reconstructs the breakdown of any past cycle from rows that
// are all still present.
func ItemsAsOf(history []store.TabItem, at time.Time) []store.TabItem {
	out := make([]store.TabItem, 0, len(history))
	for _, it := range history {
		if it.CreatedAt.After(at) {
			continue
		}
		// Removal is effective from its instant onward, so an item removed at
		// exactly this moment is already gone.
		if it.RemovedAt != nil && !it.RemovedAt.After(at) {
			continue
		}
		out = append(out, it)
	}
	return out
}
