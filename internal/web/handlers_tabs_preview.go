package web

import (
	"context"
	"strings"

	"github.com/johnzastrow/bitt/internal/config"
	"github.com/johnzastrow/bitt/internal/notify"
	"github.com/johnzastrow/bitt/internal/schedule"
	"github.com/johnzastrow/bitt/internal/store"
	"github.com/johnzastrow/bitt/internal/web/views"
)

// REM-01: render each of a tab's reminder rules as it would actually send.
//
// The motivating failure: a car loan reminder went out headed "$21,877.58 due
// tomorrow" -- the whole loan rather than the installment. Nothing on the setup
// screen could have shown that, because the screen displays the TEMPLATE
// ("{amount}") and the mistake only exists in what the template renders to.
//
// So the preview uses the tab's LIVE figures, not sample values. Sample data
// would have produced a plausible "$450.00" and hidden the bug completely.
//
// It answers "who" as well as "what". A payee who cannot receive anything is
// listed with the reason, because a silently unreachable recipient is the other
// way a reminder goes missing -- and it is invisible everywhere else.

// reminderPreviews renders every rule in force for a tab.
func (s *Server) reminderPreviews(ctx context.Context, tab store.Tab) []views.ReminderPreview {
	rules := s.effectiveRules(ctx, tab)
	if len(rules) == 0 {
		return nil
	}

	today := s.today(ctx)
	notifier := s.notifierFor(ctx)

	// Derived once: they are the same for every rule on this tab.
	balance, balErr := s.ledger.Balance(ctx, tab.ID)
	participants, partErr := s.store.ListParticipants(ctx, tab.ID)

	out := make([]views.ReminderPreview, 0, len(rules))
	for _, r := range rules {
		p := views.ReminderPreview{Days: r.Days, When: leadPhrase(r.Days)}

		// A rule cannot be rendered without a date to count from. Say so rather
		// than substituting a zero date, which would read as a real reminder
		// for 1 January year one.
		due, ok := s.previewDue(ctx, tab, today, r.Days)
		switch {
		case !tab.Schedule.Set():
			p.Problem = "This tab has no schedule, so there is no due date to count from and this rule cannot fire."
		case !ok && r.Days < 0:
			p.Problem = "No due date has passed yet, so there is nothing overdue to report."
		case !ok:
			p.Problem = "The schedule has no upcoming due date left, so this rule has nothing to count down to."
		case balErr != nil:
			p.Problem = "The balance could not be read, so the amounts cannot be shown."
		}
		if p.Problem != "" {
			out = append(out, p)
			continue
		}

		msg := s.reminderMessage(ctx, config.Reminder{Days: r.Days, Title: r.Title, Body: r.Body},
			tab, due, r.Days, balance)
		p.Title = msg.Title
		p.Body = msg.Body
		p.Due = due.Display()

		// Nothing owed means the scan skips this tab entirely. The message
		// still renders, so it is shown, but with the reason it would not go.
		if balance >= 0 {
			p.Note = "Nothing is owed on this tab right now, so this reminder would not be sent."
		}

		if partErr == nil {
			p.Recipients = s.previewRecipients(ctx, notifier, participants, r.Days)
		}
		out = append(out, p)
	}
	return out
}

// effectiveRules is what this tab actually sends: its own rules when it has
// any, the instance defaults otherwise. The same replace-not-merge rule the
// scan applies -- a tab with one rule of its own ignores the instance set
// completely, which is a common surprise and worth mirroring exactly.
func (s *Server) effectiveRules(ctx context.Context, tab store.Tab) []store.TabReminder {
	own, err := s.store.ListTabReminders(ctx, tab.ID)
	if err == nil && len(own) > 0 {
		return own
	}
	// The instance set is held as config.Reminder; the two carry the same three
	// fields and differ only in where they came from.
	def := s.instanceReminders(ctx)
	out := make([]store.TabReminder, 0, len(def))
	for _, r := range def {
		out = append(out, store.TabReminder{Days: r.Days, Title: r.Title, Body: r.Body})
	}
	return out
}

// previewDue picks the date a rule would count from: the next unpaid due date
// for a reminder, the most recent past one for an overdue notice.
func (s *Server) previewDue(ctx context.Context, tab store.Tab, today schedule.Date, days int) (schedule.Date, bool) {
	if days < 0 {
		return s.lastPastDue(tab, today)
	}
	return s.nextUnpaidDue(ctx, tab, today)
}

// previewRecipients lists who a rule reaches, and why anyone is missed.
//
// The role filter mirrors the scans exactly: a reminder goes to payees, an
// overdue notice to the payee and the Provider. Getting this wrong in the
// preview would be worse than having no preview, since it would state something
// confidently untrue.
func (s *Server) previewRecipients(ctx context.Context, n *notify.Notifier, ps []store.Participant, days int) []views.PreviewRecipient {
	var out []views.PreviewRecipient
	for _, p := range ps {
		if days < 0 {
			if p.Role != store.RolePayee && p.Role != store.RoleProvider {
				continue
			}
		} else if p.Role != store.RolePayee {
			continue
		}

		u, err := s.store.GetUser(ctx, p.UserID)
		if err != nil {
			continue
		}
		r := views.PreviewRecipient{Name: u.DisplayName, Role: string(p.Role)}
		if !u.Active() {
			r.Reason = "account is deactivated"
			out = append(out, r)
			continue
		}
		rcpt := notify.Recipient{Email: u.Email, Topic: u.NtfyTopic}
		for _, ch := range channelsFor(u, n, rcpt) {
			r.Channels = append(r.Channels, string(ch))
		}
		if len(r.Channels) == 0 {
			r.Reason = unreachableReason(u, n)
		}
		out = append(out, r)
	}
	return out
}

// unreachableReason explains, in one line an operator can act on, why a person
// would receive nothing. "Skipped" without a reason is what made the original
// problem take a database query to diagnose.
func unreachableReason(u store.User, n *notify.Notifier) string {
	var why []string
	switch {
	case !u.NotifyEmail:
		why = append(why, "email switched off")
	case u.Email == "":
		why = append(why, "no email address")
	case n != nil && !n.Available(notify.ChannelEmail, notify.Recipient{Email: u.Email}):
		why = append(why, "email delivery not configured on this instance")
	}
	switch {
	case !u.NotifyNtfy:
		why = append(why, "ntfy switched off")
	case !notify.ValidTopic(u.NtfyTopic):
		why = append(why, "no ntfy topic set on their profile")
	case n != nil && !n.Available(notify.ChannelNtfy, notify.Recipient{Topic: u.NtfyTopic}):
		why = append(why, "ntfy not configured on this instance")
	}
	if len(why) == 0 {
		return "no usable channel"
	}
	return strings.Join(why, "; ")
}
