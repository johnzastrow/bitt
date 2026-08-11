package web

import (
	"context"
	"strconv"

	"github.com/johnzastrow/bitt/internal/notify"
	"github.com/johnzastrow/bitt/internal/schedule"
	"github.com/johnzastrow/bitt/internal/store"
)

// NOTIF-02: a notice when a payment is missed.
//
// Overdue was not merely unimplemented before this, it was structurally
// unreachable: nextUnpaidDue discards past periods, so daysBetween(today, due)
// was never negative and no past-due date could match a rule. Once a due date
// passed, that period went permanently silent -- a missed payment produced a
// silence indistinguishable from a healthy tab.
//
// The cadence is the existing reminder rules with NEGATIVE lead times (decision
// D-B), so there is one mental model rather than two, and the cap falls out of
// the configuration: rules at -1 and -7 produce exactly two notices and then
// silence. There is no separate stop-nagging rule to get wrong.

// runOverdueNotices announces periods whose due date has passed while money is
// still owed. It shares the tick's send budget with the other scans.
func (s *Server) runOverdueNotices(ctx context.Context, notifier *notify.Notifier, budget *sendBudget) (sent, skipped int) {
	tabs, err := s.allTabsForNotify(ctx)
	if err != nil {
		s.log.Error("tick: list tabs for overdue notices", "error", err)
		return 0, 0
	}
	today := s.today(ctx)
	defaults := s.instanceReminders(ctx)

	for _, tab := range tabs {
		if tab.ArchivedAt != nil || !tab.Schedule.Set() {
			continue
		}
		due, ok := s.lastPastDue(tab, today)
		if !ok {
			continue
		}
		// Negative, because the due date is behind us. daysBetween is signed,
		// so the rule lookup is the same one the reminder scan uses.
		lead := daysBetween(today, due)
		spec, ok := s.reminderForTab(ctx, tab.ID, lead, defaults)
		if !ok {
			continue
		}
		// Overdue DOES re-derive live state, unlike a payment notice. A payment
		// is a fact that happened; "overdue" is a claim about the present, and
		// a period settled late must not still be chased.
		balance, err := s.ledger.Balance(ctx, tab.ID)
		if err != nil || balance >= 0 {
			continue
		}

		participants, err := s.store.ListParticipants(ctx, tab.ID)
		if err != nil {
			continue
		}
		msg := s.reminderMessage(ctx, spec, tab, due, lead, balance)
		occasion := "overdue:" + due.String() + ":d" + strconv.Itoa(-lead)

		for _, p := range participants {
			// The payee is chased and the Provider is told the money has not
			// arrived (D-C). Tab administrators are deliberately excluded: they
			// are not owed it, and a tab may have several.
			if p.Role != store.RolePayee && p.Role != store.RoleProvider {
				continue
			}
			did := s.notifyUser(ctx, notifier, tab, p.UserID, eventFor(occasion, p.UserID), msg, budget)
			sent += did
			if did == 0 {
				skipped++
			}
		}
	}
	return sent, skipped
}

// lastPastDue returns the most recent period due before today.
//
// The sibling of nextUnpaidDue rather than a change to it: the reminder scan
// depends on that function returning a FUTURE date, and giving one function two
// meanings by a flag is how a scan starts dunning people it meant to remind.
//
// It takes the latest past due date rather than assuming the periods arrive in
// order, so only the newest missed period is chased. An old unpaid period does
// not keep generating notices of its own -- one overdue conversation at a time.
func (s *Server) lastPastDue(tab store.Tab, today schedule.Date) (schedule.Date, bool) {
	var last schedule.Date
	var found bool
	for n := 0; n < schedule.MaxPeriods; n++ {
		p := tab.Schedule.Period(n)
		if !p.Due.Before(today) {
			continue
		}
		if !found || last.Before(p.Due) {
			last, found = p.Due, true
		}
	}
	return last, found
}

// abs is the magnitude of a lead time, for rendering.
func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}
