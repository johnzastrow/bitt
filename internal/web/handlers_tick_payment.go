package web

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/johnzastrow/bitt/internal/money"
	"github.com/johnzastrow/bitt/internal/notify"
	"github.com/johnzastrow/bitt/internal/store"
)

// NOTIF-01: a notice when a payment is made.
//
// Events are DERIVED FROM THE LEDGER, not queued. The tick lists a tab's
// entries and treats each payment it has not yet announced as an event, with
// the entry's sequence as the claim key.
//
// That shape is forced by the phase's load-bearing rule: delivery "must stay
// off the balance path entirely -- a failed or double send can never affect a
// ledger", and a send inside a ledger transaction "becomes a ledger write. Do
// not." Notifying inline from the payment handler would put SMTP and HTTP on
// the ledger write path. Deriving keeps every property the reminder scan
// already has -- no timer in the binary, idempotent, self-healing after an
// outage, and entirely read-only with respect to the ledger.

// paymentTitle and paymentBody are the built-in notice, in generic third person
// (decision D-E): one message for every recipient, including the payer, whose
// copy reads as a receipt. There is deliberately no per-reader variant and no
// second template to keep in step.
const (
	paymentTitle = "Payment on {tab}"
	paymentBody  = "{payee} made a payment of {paid} on {tab}.\n\n" +
		"Remaining balance: {balance}\n{url}"
)

// runPaymentNotices announces payments that have not been announced yet.
//
// It shares the tick's send budget with the reminder scan, so the ceiling
// bounds the whole run rather than each scan separately.
func (s *Server) runPaymentNotices(ctx context.Context, notifier *notify.Notifier, budget *sendBudget) (sent, skipped int) {
	tabs, err := s.allTabsForNotify(ctx)
	if err != nil {
		s.log.Error("tick: list tabs for payment notices", "error", err)
		return 0, 0
	}
	cutoff := s.paymentCutoff()

	for _, tab := range tabs {
		if tab.ArchivedAt != nil {
			continue
		}
		entries, err := s.store.ListEntries(ctx, tab.ID)
		if err != nil {
			continue
		}
		payments := announceablePayments(entries, cutoff)
		if len(payments) == 0 {
			continue
		}

		participants, err := s.store.ListParticipants(ctx, tab.ID)
		if err != nil {
			continue
		}
		// The balance is the same for every recipient of every payment on this
		// tab, and cannot change mid-scan: read it once.
		balance, err := s.ledger.Balance(ctx, tab.ID)
		if err != nil {
			continue
		}

		for _, e := range payments {
			payer := s.displayName(ctx, e.ActorUserID)
			msg := paymentMessage(tab, payer, e.Amount, balance, s.cfg.BaseURL)
			occasion := "payment:" + strconv.FormatInt(e.Seq, 10)

			// Every party, including the payer (D-A). The Provider is a
			// participant in their own right -- the tab's creator is inserted
			// with RoleProvider -- so no role filter is needed here, unlike the
			// reminder scan which is payees-only.
			for _, p := range participants {
				did := s.notifyUser(ctx, notifier, tab, p.UserID, eventFor(occasion, p.UserID), msg, budget)
				sent += did
				if did == 0 {
					skipped++
				}
			}
		}
	}
	return sent, skipped
}

// paymentCutoff is the oldest entry the scan will announce.
//
// Without a floor, an instance whose cron was down for a month would eventually
// announce a month of history in one go. A week-old payment notice is noise
// rather than news, so past this age an event is dropped permanently and on
// purpose -- it is not deferred, and no later tick will pick it up.
func (s *Server) paymentCutoff() time.Time {
	d := s.cfg.Notify.Lookback()
	if d <= 0 {
		// Zero would make every payment older than "now" ineligible and silence
		// the feature entirely, which is a strange thing to configure by
		// accident. Treat it as no floor.
		return time.Time{}
	}
	return time.Now().Add(-d)
}

// announceablePayments returns the payment entries eligible for a notice:
// recent enough, and not reversed.
//
// A payment posted and undone between two ticks never happened as far as the
// tab is concerned, and announcing it would be a lie the ledger can disprove.
// This is NOT the stale-state suppression that governs reminders -- a payment
// remains a fact whether or not a balance is still owed, so a settled tab still
// announces its payments. It is only about entries that were retracted.
func announceablePayments(entries []store.Entry, cutoff time.Time) []store.Entry {
	reversed := make(map[int64]bool, len(entries))
	for _, e := range entries {
		if e.ReversesSeq != nil {
			reversed[*e.ReversesSeq] = true
		}
	}
	var out []store.Entry
	for _, e := range entries {
		if e.Kind != store.KindPayment || reversed[e.Seq] {
			continue
		}
		if !cutoff.IsZero() && e.CreatedAt.Before(cutoff) {
			continue
		}
		out = append(out, e)
	}
	return out
}

// paymentMessage renders the notice.
//
// It has its own variable set, separate from the reminder replacer on purpose.
// {amount} means "the balance owed" in every reminder template, including
// per-tab templates a Provider has already customised and saved; giving it a
// second meaning here would silently change stored text. {paid} is a new name
// and cannot misfire.
func paymentMessage(tab store.Tab, payer string, paid, balance money.Cents, baseURL string) notify.Message {
	url := ""
	if baseURL != "" {
		url = baseURL + tabPath(tab.ID)
	}
	// Balance is held negative when money is owed, matching the reminder path's
	// presentation, so it is negated for display.
	owed := balance
	if owed < 0 {
		owed = owed.Neg()
	}
	rep := strings.NewReplacer(
		"{tab}", tab.Name,
		"{payee}", payer,
		"{paid}", paid.Display(),
		"{balance}", owed.Display(),
		"{url}", url,
	)
	return notify.Message{
		Title: strings.TrimSpace(rep.Replace(paymentTitle)),
		Body:  strings.TrimRight(rep.Replace(paymentBody), "\n "),
	}
}

// displayName resolves a user's name for a message body, falling back to
// something neutral rather than failing the send: a missing name is not a
// reason to withhold a notice about money.
func (s *Server) displayName(ctx context.Context, userID int64) string {
	u, err := s.store.GetUser(ctx, userID)
	if err != nil || strings.TrimSpace(u.DisplayName) == "" {
		return "Someone"
	}
	return u.DisplayName
}

// notifyUser delivers one prepared message to one user over each channel they
// have enabled and have not already been sent this event on.
//
// It is the message-agnostic sibling of notifyParticipant: the reminder path
// builds its message from a per-tab reminder spec, while event notices arrive
// with the message already rendered.
func (s *Server) notifyUser(ctx context.Context, notifier *notify.Notifier, tab store.Tab, userID int64, event string, msg notify.Message, budget *sendBudget) int {
	user, err := s.store.GetUser(ctx, userID)
	if err != nil || !user.Active() {
		return 0
	}
	rcpt := notify.Recipient{Email: user.Email, Topic: user.NtfyTopic}

	var delivered int
	for _, ch := range channelsFor(user, notifier, rcpt) {
		already, err := s.store.WasSent(ctx, tab.ID, event, string(ch))
		if err != nil || already {
			continue
		}
		if !budget.take() {
			continue
		}
		if err := notifier.Deliver(ctx, ch, rcpt, msg); err != nil {
			s.log.Warn("notify send failed", "tab_id", tab.ID, "event", event, "channel", ch, "error", err)
			continue // send-then-claim: a failure is retried next tick
		}
		if _, err := s.store.ClaimSent(ctx, tab.ID, event, string(ch), user.ID); err != nil {
			s.log.Error("notify claim failed after send", "tab_id", tab.ID, "event", event, "error", err)
		}
		delivered++
	}
	return delivered
}
