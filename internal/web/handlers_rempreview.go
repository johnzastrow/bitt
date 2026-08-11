package web

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/johnzastrow/bitt/internal/config"
	"github.com/johnzastrow/bitt/internal/notify"
	"github.com/johnzastrow/bitt/internal/store"
)

// REM-02: send one of a tab's reminders to yourself, now.
//
// It is a REHEARSAL, not an operational "notify everyone" button. The message
// is the real one, built from the tab's live figures over the real channels,
// but it is addressed to the administrator who pressed it and to nobody else.
//
// Two gates the scheduled scan enforces are deliberately bypassed:
//
//   - the already-sent claim, because a button that silently does nothing for
//     an occasion already delivered is the exact failure this feature exists to
//     remove;
//   - the lead-day match, because a preview that only works on the one day the
//     reminder would have fired anyway is useless for checking configuration.
//
// Both are safe ONLY because the message goes to the requester. An operational
// send-to-everyone button is a different feature and is deliberately not this.

const (
	previewLimit  = 6
	previewWindow = time.Minute
)

// postReminderPreview delivers a tab's reminder to the requesting administrator.
func (s *Server) postReminderPreview(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r.Context())

	id, ok := pathID(r, "id")
	if !ok {
		http.NotFound(w, r)
		return
	}
	// The same authority that edits the tab's reminders, since this sends what
	// those settings produce.
	if _, ok := s.requireTabManager(w, r, id, user,
		"Only the provider can send a test reminder for this tab."); !ok {
		return
	}
	if !s.allowReminderPreview(user.ID) {
		redirectWith(w, r, tabPath(id), "err",
			"That is a lot of test reminders. Give it a minute.")
		return
	}

	ctx := r.Context()
	tab, err := s.store.GetTab(ctx, id)
	if err != nil {
		s.serverError(w, r, err)
		return
	}

	days, _ := strconv.Atoi(r.PostFormValue("days"))
	rule, ok := s.ruleForDays(ctx, tab, days)
	if !ok {
		redirectWith(w, r, tabPath(id), "err", "That reminder is no longer configured on this tab.")
		return
	}

	notifier := s.notifierFor(ctx)
	if notifier == nil || !notifier.Enabled() {
		redirectWith(w, r, tabPath(id), "err",
			"No notification delivery is configured on this instance, so there is nothing to send over.")
		return
	}

	msg, ok := s.previewMessage(ctx, tab, rule)
	if !ok {
		redirectWith(w, r, tabPath(id), "err",
			"This reminder cannot be rendered right now -- see the note under it for why.")
		return
	}

	rcpt := notify.Recipient{Email: user.Email, Topic: user.NtfyTopic}
	channels := channelsFor(*user, notifier, rcpt)
	if len(channels) == 0 {
		redirectWith(w, r, tabPath(id), "err",
			"Your own account has no usable channel: "+unreachableReason(*user, notifier)+".")
		return
	}

	var sent []string
	for _, ch := range channels {
		// NO CLAIM IS WRITTEN, and this is the load-bearing line of the whole
		// feature. A claim is what suppresses a later send of the same
		// occasion, so recording one here would silently cancel the real
		// reminder to the real payee. A rehearsal is not the event.
		if err := notifier.Deliver(ctx, ch, rcpt, msg); err != nil {
			s.log.Warn("reminder preview send failed",
				"tab_id", tab.ID, "channel", ch, "error", err)
			continue
		}
		sent = append(sent, string(ch))
	}

	if len(sent) == 0 {
		redirectWith(w, r, tabPath(id), "err",
			"The send failed on every channel. Check the notification settings.")
		return
	}
	s.log.Info("reminder preview sent",
		"tab_id", tab.ID, "user_id", user.ID, "days", days, "channels", sent,
		"note", "preview only: addressed to the requester, no claim written, "+
			"the scheduled reminder is unaffected")

	redirectWith(w, r, tabPath(id), "ok",
		"Sent to you over "+joinAnd(sent)+". Nobody else was notified, and the "+
			"scheduled reminder still goes out as normal.")
}

// ruleForDays finds the rule in force for a lead time, whether it is the tab's
// own or inherited from the instance.
func (s *Server) ruleForDays(ctx context.Context, tab store.Tab, days int) (store.TabReminder, bool) {
	for _, r := range s.effectiveRules(ctx, tab) {
		if r.Days == days {
			return r, true
		}
	}
	return store.TabReminder{}, false
}

// previewMessage renders a rule the way the scan would, or reports that it
// cannot be rendered -- no schedule, or no date to count from.
func (s *Server) previewMessage(ctx context.Context, tab store.Tab, r store.TabReminder) (notify.Message, bool) {
	if !tab.Schedule.Set() {
		return notify.Message{}, false
	}
	due, ok := s.previewDue(ctx, tab, s.today(ctx), r.Days)
	if !ok {
		return notify.Message{}, false
	}
	balance, err := s.ledger.Balance(ctx, tab.ID)
	if err != nil {
		return notify.Message{}, false
	}
	return s.reminderMessage(ctx,
		config.Reminder{Days: r.Days, Title: r.Title, Body: r.Body},
		tab, due, r.Days, balance), true
}

// allowReminderPreview bounds the button. Even addressed to oneself, an
// unbounded send is a way to mail yourself into a spam folder.
func (s *Server) allowReminderPreview(userID int64) bool {
	now := time.Now()
	v, _ := s.previewRate.LoadOrStore(userID, &rateEntry{})
	e := v.(*rateEntry)

	s.previewRateMu.Lock()
	defer s.previewRateMu.Unlock()
	if now.After(e.reset) {
		e.count, e.reset = 0, now.Add(previewWindow)
	}
	if e.count >= previewLimit {
		return false
	}
	e.count++
	return true
}

// joinAnd renders a short list the way a sentence wants it.
func joinAnd(xs []string) string {
	switch len(xs) {
	case 0:
		return ""
	case 1:
		return xs[0]
	default:
		return strings.Join(xs[:len(xs)-1], ", ") + " and " + xs[len(xs)-1]
	}
}
