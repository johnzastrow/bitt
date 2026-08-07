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

	// The effective configuration, which is the environment plus whatever an
	// administrator has set through the interface -- resolved now rather than at
	// startup, so a setting changed in the app takes effect on the next tick
	// without a restart.
	notifier := s.notifierFor(r.Context())
	if notifier == nil || !notifier.Enabled() {
		// Authenticated, but nothing to deliver over. Report success with zero
		// work so a cron does not treat it as an error.
		writeTickResult(w, 0, 0, 0)
		return
	}

	sent, skipped, deferred := s.runNotifications(r.Context(), notifier)
	s.log.Info("tick completed", "sent", sent, "skipped", skipped, "deferred", deferred)
	if deferred > 0 {
		// Never let a cap look like quiet. A truncated run that reports only
		// "sent=0" reads exactly like a run with nothing to do, and telling
		// those two apart from the outside is expensive.
		s.log.Warn("tick hit the per-tick send ceiling",
			"deferred", deferred, "ceiling", s.cfg.Notify.MaxPerTick,
			"note", "the next tick will deliver these; no claim was written")
	}
	writeTickResult(w, sent, skipped, deferred)
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
//
// A per-tick send ceiling (NOTIF-03) bounds the run: nothing is lost when it
// bites, because a claim is only written after a delivery, so an event that was
// not reached this tick is reached by the next one.
func (s *Server) runNotifications(ctx context.Context, notifier *notify.Notifier) (sent, skipped, deferred int) {
	budget := newSendBudget(s.cfg.Notify.MaxPerTick)
	defer func() { deferred = budget.deferred }()

	tabs, err := s.allTabsForNotify(ctx)
	if err != nil {
		s.log.Error("tick: list tabs", "error", err)
		return
	}
	today := s.today(ctx)

	// The instance-wide defaults, read once for the whole scan: they are the
	// same for every tab, and a per-tab read would be a query per tab for an
	// answer that cannot change mid-tick.
	defaults := s.instanceReminders(ctx)

	for _, tab := range tabs {
		if tab.ArchivedAt != nil || !tab.Schedule.Set() {
			continue
		}
		due, ok := s.nextUnpaidDue(ctx, tab, today)
		if !ok {
			continue
		}
		lead := daysBetween(today, due)
		spec, ok := s.reminderForTab(ctx, tab.ID, lead, defaults)
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
		occasion := "reminder:" + due.String() + ":d" + strconv.Itoa(lead)
		for _, p := range participants {
			if p.Role != store.RolePayee {
				continue
			}
			// The recipient belongs IN the key. The claim table is keyed
			// (tab_id, event_key, channel) with no user column in the key, by
			// design -- migration 0008 spells out the intended shape,
			// "req:2026-08-01:u7". Without the suffix the first payee notified
			// claims the occasion for the whole tab and every other payee is
			// skipped in silence, so a tab with two payees notifies one person.
			event := eventFor(occasion, p.UserID)
			did := s.notifyParticipant(ctx, notifier, tab, p, event, spec, due, lead, balance, budget)
			sent += did
			if did == 0 {
				skipped++
			}
		}
	}
	return
}

// sendBudget bounds the deliveries one tick may perform.
//
// Exceeding it is not an error and nothing is dropped: a claim is written only
// after a confirmed delivery, so an event this tick did not reach carries no
// claim and the next tick picks it up. What the budget prevents is a single run
// after a long outage turning into a burst across every tab at once.
type sendBudget struct {
	remaining int // negative means unbounded
	deferred  int
}

func newSendBudget(max int) *sendBudget {
	if max <= 0 {
		return &sendBudget{remaining: -1}
	}
	return &sendBudget{remaining: max}
}

// take reserves one delivery, reporting whether the caller may proceed. A
// refusal is counted so the tick can say how much it left behind -- a silent
// cap is indistinguishable from having nothing to send.
func (b *sendBudget) take() bool {
	if b.remaining < 0 {
		return true
	}
	if b.remaining == 0 {
		b.deferred++
		return false
	}
	b.remaining--
	return true
}

// eventFor scopes an occasion to one recipient, producing the claim key.
//
// Every event key in this package MUST go through here. The claim table's
// primary key is (tab_id, event_key, channel): the recipient is not a column in
// that key, so an unscoped key silently means "this tab has been told", not
// "this person has been told".
func eventFor(occasion string, userID int64) string {
	return occasion + ":u" + strconv.FormatInt(userID, 10)
}

// notifyParticipant delivers one reminder to one payee over each channel they
// have enabled that has not already sent this event. Returns the number of
// channels delivered on.
func (s *Server) notifyParticipant(ctx context.Context, notifier *notify.Notifier, tab store.Tab, p store.Participant, event string, spec config.Reminder, due schedule.Date, lead int, balance money.Cents, budget *sendBudget) int {
	user, err := s.store.GetUser(ctx, p.UserID)
	if err != nil || !user.Active() {
		return 0
	}
	rcpt := notify.Recipient{Email: user.Email, Topic: user.NtfyTopic}
	msg := s.reminderMessage(spec, tab, due, lead, balance)

	var delivered int
	for _, ch := range channelsFor(user, notifier, rcpt) {
		already, err := s.store.WasSent(ctx, tab.ID, event, string(ch))
		if err != nil || already {
			continue
		}
		// Budget is taken only for work actually about to happen: an
		// already-claimed channel must not consume the ceiling, or a large
		// backlog of settled events would starve the ones still to send.
		if !budget.take() {
			continue
		}
		if err := notifier.Deliver(ctx, ch, rcpt, msg); err != nil {
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
func (s *Server) reminderForTab(ctx context.Context, tabID int64, days int, defaults []config.Reminder) (config.Reminder, bool) {
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
	return reminderAt(defaults, days)
}

// reminderAt returns the rule in a set whose lead time matches the given
// days-until-due, if any.
func reminderAt(rs []config.Reminder, days int) (config.Reminder, bool) {
	for _, r := range rs {
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

func writeTickResult(w http.ResponseWriter, sent, skipped, deferred int) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	body := "ok sent=" + strconv.Itoa(sent) + " skipped=" + strconv.Itoa(skipped)
	// Only present when it happened, so the usual line a cron logs every hour
	// stays as short as it has always been, and a deferred= appearing in it is
	// itself the signal that something needs attention.
	if deferred > 0 {
		body += " deferred=" + strconv.Itoa(deferred)
	}
	_, _ = w.Write([]byte(body + "\n"))
}

// allowTick rate-limits the cron endpoint. The caller is a single cron, so the
// limiter is keyed on a constant rather than per-user; it bounds a runaway or a
// hostile caller that already holds the secret. Reuses the fixed-window shape
// of the avatar limiter.
func (s *Server) allowTick() bool {
	const tickKey = int64(-1) // not a real user id
	return s.allowAvatarUpload(tickKey)
}
