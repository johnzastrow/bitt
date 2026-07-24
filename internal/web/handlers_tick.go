package web

import (
	"context"
	"crypto/subtle"
	"net/http"
	"strconv"
	"strings"

	"github.com/johnzastrow/bitt/internal/config"
	"github.com/johnzastrow/bitt/internal/money"
	"github.com/johnzastrow/bitt/internal/notify"
	"github.com/johnzastrow/bitt/internal/schedule"
	"github.com/johnzastrow/bitt/internal/store"
)

// postTick is the cron-driven delivery entry point.
//
// It is outside requireAuth because an external cron carries no session, so it
// authenticates itself with a shared secret. Three things make that safe, and
// all three are checked before any work:
//
//   - Fail closed. With no secret configured the endpoint refuses every
//     request. It is never open; a deployment that has not set BITT_TICK_SECRET
//     simply cannot drive delivery.
//   - Constant-time comparison, so the secret cannot be recovered by timing.
//   - Rate limited, reusing the avatar limiter's fixed-window shape keyed on a
//     constant, since the caller is a single cron rather than a user.
//
// The scan it runs is READ-ONLY with respect to the ledger: it never posts an
// entry, so this outbound-triggered endpoint stays entirely off the balance
// path, which is the load-bearing rule of the phase.
func (s *Server) postTick(w http.ResponseWriter, r *http.Request) {
	if !s.cfg.Notify.TickEnabled() {
		// No secret configured: fail closed, and say nothing about why.
		s.log.Warn("tick rejected: no tick secret configured")
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	// Authenticate before any work, so an unauthenticated request cannot make
	// the endpoint scan or send at all.
	presented := bearerToken(r.Header.Get("Authorization"))
	want := s.cfg.Notify.TickSecret
	if subtle.ConstantTimeCompare([]byte(presented), []byte(want)) != 1 {
		s.log.Warn("tick rejected: bad secret", "remote", r.RemoteAddr)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	// One cron, so a constant limiter key; this bounds a runaway or hostile
	// caller that already holds the secret.
	if !s.allowTick() {
		http.Error(w, "slow down", http.StatusTooManyRequests)
		return
	}

	if s.notifier == nil || !s.notifier.Enabled() {
		// Authenticated, but nothing to deliver over. Report success with zero
		// work so a cron does not treat it as an error.
		writeTickResult(w, 0, 0)
		return
	}

	sent, skipped := s.runNotifications(r.Context())
	s.log.Info("tick completed", "sent", sent, "skipped", skipped)
	writeTickResult(w, sent, skipped)
}

// runNotifications scans scheduled tabs and delivers due payment reminders.
//
// It is deliberately simple and side-effect-light: for every scheduled tab it
// looks at the next unpaid due date, and if that date falls on a reminder lead
// time it notifies each payee who has not already been notified for that event.
// Every send is send-then-claim (D2): deliver first, and only on confirmed
// success record the claim, so a failure re-sends next tick rather than being
// lost. Before sending it re-derives live state, so a period paid early or a
// tab already settled produces no dunning notice (the one confirmed defect the
// security review named).
func (s *Server) runNotifications(ctx context.Context) (sent, skipped int) {
	tabs, err := s.allTabsForNotify(ctx)
	if err != nil {
		s.log.Error("tick: list tabs", "error", err)
		return 0, 0
	}
	today := s.today(ctx)

	for _, tab := range tabs {
		if tab.ArchivedAt != nil || !tab.Schedule.Set() {
			continue
		}
		due, ok := s.nextUnpaidDue(ctx, tab, today)
		if !ok {
			continue
		}
		lead := daysBetween(today, due)
		spec, ok := s.reminderForTab(ctx, tab.ID, lead)
		if !ok {
			continue
		}
		// Re-derive: nothing is owed means nothing to remind about.
		balance, err := s.ledger.Balance(ctx, tab.ID)
		if err != nil || balance >= 0 {
			continue
		}

		participants, err := s.store.ListParticipants(ctx, tab.ID)
		if err != nil {
			continue
		}
		event := "reminder:" + due.String() + ":d" + strconv.Itoa(lead)
		for _, p := range participants {
			if p.Role != store.RolePayee {
				continue
			}
			did := s.notifyParticipant(ctx, tab, p, event, spec, due, lead, balance)
			sent += did
			if did == 0 {
				skipped++
			}
		}
	}
	return sent, skipped
}

// notifyParticipant delivers one reminder to one payee over each channel they
// have enabled that has not already sent this event. Returns the number of
// channels delivered on.
func (s *Server) notifyParticipant(ctx context.Context, tab store.Tab, p store.Participant, event string, spec config.Reminder, due schedule.Date, lead int, balance money.Cents) int {
	user, err := s.store.GetUser(ctx, p.UserID)
	if err != nil || !user.Active() {
		return 0
	}
	rcpt := notify.Recipient{Email: user.Email, Topic: user.NtfyTopic}
	msg := s.reminderMessage(spec, tab, due, lead, balance)

	var delivered int
	for _, ch := range channelsFor(user, s.notifier, rcpt) {
		already, err := s.store.WasSent(ctx, tab.ID, event, string(ch))
		if err != nil || already {
			continue
		}
		if err := s.notifier.Deliver(ctx, ch, rcpt, msg); err != nil {
			s.log.Warn("notify send failed", "tab_id", tab.ID, "channel", ch, "error", err)
			continue // send-then-claim: a failure is retried next tick
		}
		// Confirmed success: claim it, in its own transaction, off the ledger.
		if _, err := s.store.ClaimSent(ctx, tab.ID, event, string(ch), user.ID); err != nil {
			s.log.Error("notify claim failed after send", "tab_id", tab.ID, "error", err)
		}
		delivered++
	}
	return delivered
}

// channelsFor returns the channels a user wants and that are usable for them.
func channelsFor(u store.User, n *notify.Notifier, r notify.Recipient) []notify.Channel {
	var out []notify.Channel
	if u.NotifyEmail && n.Available(notify.ChannelEmail, r) {
		out = append(out, notify.ChannelEmail)
	}
	if u.NotifyNtfy && n.Available(notify.ChannelNtfy, r) {
		out = append(out, notify.ChannelNtfy)
	}
	return out
}

// reminderMessage renders a reminder's admin-configured templates with the
// per-send variables. A control character that reaches the title via the {tab}
// variable is caught by the sender's header check, which fails the send closed.
func (s *Server) reminderMessage(spec config.Reminder, tab store.Tab, due schedule.Date, lead int, balance money.Cents) notify.Message {
	url := ""
	if s.cfg.BaseURL != "" {
		url = s.cfg.BaseURL + tabPath(tab.ID)
	}
	rep := strings.NewReplacer(
		"{tab}", tab.Name,
		"{amount}", balance.Neg().Display(),
		"{due}", due.Display(),
		"{days}", strconv.Itoa(lead),
		"{when}", leadPhrase(lead),
		"{url}", url,
	)
	return notify.Message{
		Title: strings.TrimSpace(rep.Replace(spec.Title)),
		Body:  strings.TrimRight(rep.Replace(spec.Body), "\n "),
	}
}

// leadPhrase renders a lead time as a human phrase for {when}.
func leadPhrase(days int) string {
	switch days {
	case 0:
		return "today"
	case 1:
		return "tomorrow"
	case 7:
		return "in one week"
	case 14:
		return "in two weeks"
	default:
		return "in " + strconv.Itoa(days) + " days"
	}
}

// reminderForTab returns the rule that fires this many days before the tab's
// due date: the tab's own reminders when the Provider has set any, and the
// instance defaults otherwise.
//
// A customised tab is customised completely -- its list replaces the instance
// one rather than merging with it. Merging would make removal impossible: a
// Provider who drops the 14-day notice from a tab would keep getting it from
// the instance default, and the interface would be lying about what it does.
//
// A read failure sends nothing rather than falling back. The fallback message
// is exactly the one the Provider may have deliberately replaced, and the
// send-then-claim design means a skipped reminder is retried on the next tick
// rather than lost.
func (s *Server) reminderForTab(ctx context.Context, tabID int64, days int) (config.Reminder, bool) {
	own, err := s.store.ListTabReminders(ctx, tabID)
	if err != nil {
		s.log.Error("tick: list tab reminders", "tab_id", tabID, "error", err)
		return config.Reminder{}, false
	}
	for _, r := range own {
		if r.Days == days {
			return config.Reminder{Days: r.Days, Title: r.Title, Body: r.Body}, true
		}
	}
	if len(own) > 0 {
		return config.Reminder{}, false
	}
	return s.reminderFor(days)
}

// reminderFor returns the instance-wide reminder rule whose lead time matches
// the given days-until-due, if any. It is the fallback for a tab the Provider
// has not customised.
func (s *Server) reminderFor(days int) (config.Reminder, bool) {
	for _, r := range s.cfg.Reminders {
		if r.Days == days {
			return r, true
		}
	}
	return config.Reminder{}, false
}

// nextUnpaidDue returns the earliest scheduled due date that has not passed,
// which is what a reminder counts down to. Read-only: it does not accrue.
func (s *Server) nextUnpaidDue(ctx context.Context, tab store.Tab, today schedule.Date) (schedule.Date, bool) {
	for n := 0; n < schedule.MaxPeriods; n++ {
		p := tab.Schedule.Period(n)
		if p.Due.Before(today) {
			continue
		}
		return p.Due, true
	}
	return schedule.Date{}, false
}

// allTabsForNotify lists every tab, for the scan. It is a distinct method so a
// future store can index it; today it reuses the admin listing path.
func (s *Server) allTabsForNotify(ctx context.Context) ([]store.Tab, error) {
	return s.store.ListAllTabs(ctx)
}

// bearerToken extracts the token from an "Authorization: Bearer <token>" header.
func bearerToken(header string) string {
	const prefix = "Bearer "
	if len(header) > len(prefix) && strings.EqualFold(header[:len(prefix)], prefix) {
		return strings.TrimSpace(header[len(prefix):])
	}
	return ""
}

func writeTickResult(w http.ResponseWriter, sent, skipped int) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte("ok sent=" + strconv.Itoa(sent) + " skipped=" + strconv.Itoa(skipped) + "\n"))
}

// allowTick rate-limits the cron endpoint. The caller is a single cron, so the
// limiter is keyed on a constant rather than per-user; it bounds a runaway or a
// hostile caller that already holds the secret. Reuses the fixed-window shape
// of the avatar limiter.
func (s *Server) allowTick() bool {
	const tickKey = int64(-1) // not a real user id
	return s.allowAvatarUpload(tickKey)
}
