package web

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/johnzastrow/bitt/internal/auth"
	"github.com/johnzastrow/bitt/internal/ledger"
	"github.com/johnzastrow/bitt/internal/money"
	"github.com/johnzastrow/bitt/internal/store"
	"github.com/johnzastrow/bitt/internal/web/views"
)

// ---------------------------------------------------------------------------
// One-tap settle from the dashboard (PAY-01, UI-03)
//
// GET  /tabs/{id}/settle  swaps the card for a prefilled confirmation
// POST /tabs/{id}/settle  records the payment and swaps the updated card back
// GET  /tabs/{id}/card    swaps the plain card back (cancel)
// ---------------------------------------------------------------------------

// getCard returns a dashboard card fragment.
func (s *Server) getCard(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r.Context())
	card, ok := s.cardFor(w, r, user)
	if !ok {
		return
	}
	s.render(w, r, http.StatusOK, views.TabCardView(s.page(w, r, ""), card))
}

// getSettleConfirm returns the confirmation fragment, every field prefilled.
//
// The "custom" variant opens the amount for editing so a partial payment, or a
// payment beyond the balance, can be recorded without leaving the dashboard
// (PAY-01, PAY-05). The default remains one tap plus one confirmation with
// nothing to type (UI-03).
func (s *Server) getSettleConfirm(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r.Context())
	card, ok := s.cardFor(w, r, user)
	if !ok {
		return
	}
	card.Custom = r.URL.Query().Get("custom") != ""

	// Nothing owed: the full-amount confirmation would post a zero payment, so
	// hand back the plain card instead. A custom amount is still allowed, since
	// paying a settled tab ahead is a legitimate way to build credit (PAY-05).
	if !card.CanSettle() && !card.Custom {
		s.render(w, r, http.StatusOK, views.TabCardView(s.page(w, r, ""), card))
		return
	}
	s.render(w, r, http.StatusOK, views.SettleConfirm(s.page(w, r, ""), card))
}

// postSettle records the payment and returns the refreshed card.
func (s *Server) postSettle(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r.Context())

	id, ok := pathID(r, "id")
	if !ok {
		http.NotFound(w, r)
		return
	}
	if err := r.ParseForm(); err != nil {
		s.serverError(w, r, err)
		return
	}
	if !auth.CheckCSRF(r) {
		http.Error(w, "Your session expired. Reload the page and try again.", http.StatusForbidden)
		return
	}

	acc, err := s.authorizeTab(r, id, user)
	if err != nil {
		s.denyTab(w, r, err)
		return
	}
	tab := acc.Tab

	// Moving money is membership only. authorizeTab admits administrators so
	// they can manage a tab they do not belong to, but recording a payment is a
	// statement about something that happened between two people, and an
	// administrator is not one of them.
	if !acc.CanTransact() {
		s.log.Warn("payment denied: not a participant",
			"tab_id", tab.ID, "user_id", user.ID, "path", r.URL.Path)
		http.NotFound(w, r)
		return
	}

	if _, err := s.recordPayment(r, tab, user); err != nil {
		if errors.Is(err, errBadInput) {
			// Re-render the confirmation carrying the message rather than
			// answering 400. htmx does not swap a non-2xx response, so a status
			// code alone would leave the card looking like nothing happened.
			card, ok := s.cardFor(w, r, user)
			if !ok {
				return
			}
			card.Custom = true
			card.Error = err.Error()
			s.render(w, r, http.StatusOK, views.SettleConfirm(s.page(w, r, ""), card))
			return
		}
		s.serverError(w, r, err)
		return
	}

	card, ok := s.cardFor(w, r, user)
	if !ok {
		return
	}
	s.render(w, r, http.StatusOK, views.TabCardView(s.page(w, r, ""), card))
}

// cardFor builds the dashboard card for the tab in the request path, enforcing
// participation. It writes the error response itself and reports whether the
// caller should continue.
func (s *Server) cardFor(w http.ResponseWriter, r *http.Request, user *store.User) (views.TabCard, bool) {
	id, ok := pathID(r, "id")
	if !ok {
		http.NotFound(w, r)
		return views.TabCard{}, false
	}

	role, err := s.store.ParticipantRole(r.Context(), id, user.ID)
	if err != nil {
		s.denyTab(w, r, err)
		return views.TabCard{}, false
	}
	tab, err := s.store.GetTab(r.Context(), id)
	if err != nil {
		s.denyTab(w, r, err)
		return views.TabCard{}, false
	}
	balance, err := s.ledger.Balance(r.Context(), id)
	if err != nil {
		s.serverError(w, r, err)
		return views.TabCard{}, false
	}
	key, err := ledger.NewIdempotencyKey()
	if err != nil {
		s.serverError(w, r, err)
		return views.TabCard{}, false
	}

	period, err := s.cardPeriodPayment(r.Context(), tab, balance)
	if err != nil {
		s.serverError(w, r, err)
		return views.TabCard{}, false
	}

	return views.TabCard{
		Tab:            tab,
		Balance:        balance,
		Role:           role,
		SettleAmount:   settleAmount(balance),
		PeriodPayment:  period,
		IdempotencyKey: key,
	}, true
}

// settleAmount is the whole outstanding balance -- what "Other amount" prefills
// and what pays off a loan. Zero when the tab is settled or in credit.
func settleAmount(balance money.Cents) money.Cents {
	if balance < 0 {
		return balance.Neg()
	}
	return 0
}

// periodPayment is the expected payment for a single period, capped at what is
// owed. It is what the primary settle button offers.
//
//   - Payoff: the loan's scheduled installment (tab.LoanPayment).
//   - Services: the sum of the current line items -- the recurring charge.
//
// Capping at the balance keeps a near-final payment from overshooting into
// credit: when less than a period remains, paying "the period" means paying
// what is left. Zero when no periodic amount is defined (a hand-billed tab), in
// which case the button falls back to settling the whole balance, and zero when
// nothing is owed.
func periodPayment(base, balance money.Cents) money.Cents {
	owed := settleAmount(balance)
	if base <= 0 || owed <= 0 {
		return 0
	}
	if base > owed {
		return owed
	}
	return base
}

// periodBase returns a tab's per-period expected amount before capping: the
// loan payment for a Payoff tab, the item total for a Services tab. The item
// total is supplied by the caller, which already has the items loaded.
func periodBase(tab store.Tab, itemTotal money.Cents) money.Cents {
	if tab.Kind == store.TabPayoff {
		return tab.LoanPayment
	}
	return itemTotal
}

// cardPeriodPayment computes the primary-button amount for a card, loading the
// line items only when the tab is a Services tab that needs them. A Payoff tab
// reads its installment straight off the tab and touches no items.
func (s *Server) cardPeriodPayment(ctx context.Context, tab store.Tab, balance money.Cents) (money.Cents, error) {
	var itemTotal money.Cents
	if tab.Kind != store.TabPayoff {
		items, err := s.store.ListItems(ctx, tab.ID)
		if err != nil {
			return 0, err
		}
		sum, ok := money.Sum(itemAmounts(items))
		if !ok {
			return 0, errors.New("item total overflow")
		}
		itemTotal = sum
	}
	return periodPayment(periodBase(tab, itemTotal), balance), nil
}

// ---------------------------------------------------------------------------
// Full-page payment form (PAY-01, PAY-02, PAY-03)
// ---------------------------------------------------------------------------

// errBadInput marks a validation failure caused by what the user typed, rather
// than by anything going wrong.
var errBadInput = errors.New("web: invalid input")

// badInput carries a message written for the person who typed it.
//
// It matches errBadInput under errors.Is while its Error text stays clean, so a
// handler can show it verbatim. Wrapping with errors.Join instead put the
// sentinel's own text in front of the message and it reached the screen.
type badInput string

func (e badInput) Error() string { return string(e) }

func (e badInput) Is(target error) bool { return target == errBadInput }

func (s *Server) postPayment(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r.Context())

	id, ok := pathID(r, "id")
	if !ok {
		http.NotFound(w, r)
		return
	}
	if err := r.ParseForm(); err != nil {
		redirectWith(w, r, tabPath(id), "err", "Could not read that form.")
		return
	}
	if !auth.CheckCSRF(r) {
		redirectWith(w, r, tabPath(id), "err", "Your session expired. Please try again.")
		return
	}

	acc, err := s.authorizeTab(r, id, user)
	if err != nil {
		s.denyTab(w, r, err)
		return
	}
	tab := acc.Tab

	// Moving money is membership only. authorizeTab admits administrators so
	// they can manage a tab they do not belong to, but recording a payment is a
	// statement about something that happened between two people, and an
	// administrator is not one of them.
	if !acc.CanTransact() {
		s.log.Warn("payment denied: not a participant",
			"tab_id", tab.ID, "user_id", user.ID, "path", r.URL.Path)
		http.NotFound(w, r)
		return
	}

	entry, err := s.recordPayment(r, tab, user)
	if err != nil {
		if errors.Is(err, errBadInput) {
			redirectWith(w, r, tabPath(id), "err", err.Error())
			return
		}
		s.serverError(w, r, err)
		return
	}

	if entry.Seq == 0 {
		redirectWith(w, r, tabPath(id), "ok", "That payment was already recorded.")
		return
	}
	redirectWith(w, r, tabPath(id), "ok", "Payment recorded.")
}

// recordPayment validates and posts a payment.
//
// PAY-03: either participant may record. The acting user is stored on the
// entry, so a Provider recording on the Payee's behalf is attributed to the
// Provider rather than silently appearing as the Payee's own action.
func (s *Server) recordPayment(r *http.Request, tab store.Tab, user *store.User) (store.Entry, error) {
	amount, err := money.Parse(r.PostFormValue("amount"))
	if err != nil {
		return store.Entry{}, badInput("That amount is not a valid dollar figure.")
	}
	if amount <= 0 {
		return store.Entry{}, badInput("A payment must be greater than zero.")
	}

	method := store.PaymentMethod(strings.TrimSpace(r.PostFormValue("method")))
	if method == store.MethodNone {
		method = store.MethodOther
	}
	if !method.Valid() {
		return store.Entry{}, badInput("That is not a recognized payment method.")
	}

	memo := strings.TrimSpace(r.PostFormValue("memo"))
	if len(memo) > 300 {
		memo = memo[:300]
	}

	entry, replayed, err := s.ledger.Payment(r.Context(), ledger.Post{
		TabID:          tab.ID,
		Amount:         amount,
		Memo:           memo,
		Method:         method,
		ActorUserID:    user.ID,
		IdempotencyKey: strings.TrimSpace(r.PostFormValue("idempotency_key")),
	})
	if err != nil {
		return store.Entry{}, err
	}
	if replayed {
		s.log.Info("payment replay ignored", "tab_id", tab.ID, "entry_seq", entry.Seq)
		return store.Entry{}, nil
	}

	s.log.Info("payment recorded",
		"tab_id", tab.ID, "entry_seq", entry.Seq, "actor_user_id", user.ID, "method", method)
	return entry, nil
}

// ---------------------------------------------------------------------------
// Undo (PAY-04, LEDGER-02)
// ---------------------------------------------------------------------------

func (s *Server) postUndo(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r.Context())

	id, ok := pathID(r, "id")
	if !ok {
		http.NotFound(w, r)
		return
	}
	seq, err := strconv.ParseInt(r.PathValue("seq"), 10, 64)
	if err != nil || seq <= 0 {
		http.NotFound(w, r)
		return
	}

	if err := r.ParseForm(); err != nil {
		redirectWith(w, r, tabPath(id), "err", "Could not read that form.")
		return
	}
	if !auth.CheckCSRF(r) {
		redirectWith(w, r, tabPath(id), "err", "Your session expired. Please try again.")
		return
	}

	acc, err := s.authorizeTab(r, id, user)
	if err != nil {
		s.denyTab(w, r, err)
		return
	}
	tab := acc.Tab

	// The entry must belong to this tab. Without this check, a participant on
	// one tab could reverse an entry on another by guessing a sequence number.
	original, err := s.store.GetEntry(r.Context(), seq)
	if err != nil || original.TabID != tab.ID {
		s.log.Warn("undo denied: entry not on tab",
			"tab_id", tab.ID, "entry_seq", seq, "user_id", user.ID)
		http.NotFound(w, r)
		return
	}

	role, err := s.store.ParticipantRole(r.Context(), tab.ID, user.ID)
	if err != nil {
		s.denyTab(w, r, err)
		return
	}
	// A Provider may undo anything on their tab. A Payee may undo only what
	// they recorded themselves, so one participant cannot silently reverse the
	// other's entries.
	if role != store.RoleProvider && original.ActorUserID != user.ID {
		s.log.Warn("undo denied: not the author",
			"tab_id", tab.ID, "entry_seq", seq, "user_id", user.ID)
		redirectWith(w, r, tabPath(id), "err", "You can only undo entries you recorded.")
		return
	}

	// Undoing a fee is a waiver, and a waiver carries a reason (FEE-07). The
	// reason rides in the reversal's memo, so it stays with the entry that
	// records the waiver rather than living in a side note.
	memo := ""
	if original.Kind == store.KindFee {
		reason := strings.TrimSpace(r.PostFormValue("reason"))
		if len(reason) > 200 {
			reason = reason[:200]
		}
		if reason != "" {
			memo = "Waived: " + reason
		} else {
			memo = "Waived"
		}
	}

	_, _, err = s.ledger.Reverse(r.Context(), seq, user.ID, memo, "")
	switch {
	case errors.Is(err, ledger.ErrAlreadyReversed):
		redirectWith(w, r, tabPath(id), "err", "That entry was already undone.")
		return
	case errors.Is(err, ledger.ErrNotReversible):
		redirectWith(w, r, tabPath(id), "err", "A reversal cannot itself be undone.")
		return
	case err != nil:
		s.serverError(w, r, err)
		return
	}

	if original.Kind == store.KindFee {
		s.log.Info("fee waived", "tab_id", tab.ID, "fee_seq", seq, "user_id", user.ID)
		redirectWith(w, r, tabPath(id), "ok", "Late fee waived. The original stays in the history.")
		return
	}
	s.log.Info("entry reversed", "tab_id", tab.ID, "reversed_seq", seq, "user_id", user.ID)
	redirectWith(w, r, tabPath(id), "ok", "Undone. The original entry stays in the history.")
}

// ---------------------------------------------------------------------------
// Participants (TAB-03)
// ---------------------------------------------------------------------------

func (s *Server) postParticipant(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r.Context())

	id, ok := pathID(r, "id")
	if !ok {
		http.NotFound(w, r)
		return
	}
	if err := r.ParseForm(); err != nil {
		redirectWith(w, r, tabPath(id), "err", "Could not read that form.")
		return
	}
	if !auth.CheckCSRF(r) {
		redirectWith(w, r, tabPath(id), "err", "Your session expired. Please try again.")
		return
	}

	acc, err := s.authorizeTab(r, id, user)
	if err != nil {
		s.denyTab(w, r, err)
		return
	}
	tab := acc.Tab

	// Only a Provider decides who is on their tab.
	role, err := s.store.ParticipantRole(r.Context(), tab.ID, user.ID)
	if err != nil || role != store.RoleProvider {
		s.log.Warn("attach denied", "tab_id", tab.ID, "user_id", user.ID, "role", role)
		redirectWith(w, r, tabPath(id), "err", "Only the provider can attach someone to this tab.")
		return
	}

	targetID, err := strconv.ParseInt(strings.TrimSpace(r.PostFormValue("user_id")), 10, 64)
	if err != nil || targetID <= 0 {
		redirectWith(w, r, tabPath(id), "err", "Choose someone to attach.")
		return
	}

	target, err := s.store.GetUser(r.Context(), targetID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			redirectWith(w, r, tabPath(id), "err", "That person no longer has an account.")
			return
		}
		s.serverError(w, r, err)
		return
	}
	if !target.Active() {
		redirectWith(w, r, tabPath(id), "err", "That account is deactivated.")
		return
	}

	err = s.store.AddParticipant(r.Context(), store.Participant{
		TabID: tab.ID, UserID: target.ID, Role: store.RolePayee,
	})
	if err != nil {
		if errors.Is(err, store.ErrConflict) {
			redirectWith(w, r, tabPath(id), "err", "They are already on this tab.")
			return
		}
		s.serverError(w, r, err)
		return
	}

	s.log.Info("payee attached", "tab_id", tab.ID, "payee_user_id", target.ID, "by_user_id", user.ID)
	redirectWith(w, r, tabPath(id), "ok", target.DisplayName+" is now on this tab.")
}

// postParticipantRemove detaches someone from a tab (TAB-03).
//
// The store refuses to remove a tab's last Provider inside the write
// transaction, so a tab cannot be left unbillable and unreachable by any
// provider-role check. Nothing about the ledger changes: entries the removed
// person recorded stay, still attributed to them (LEDGER-05).
func (s *Server) postParticipantRemove(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r.Context())

	id, ok := pathID(r, "id")
	if !ok {
		http.NotFound(w, r)
		return
	}
	targetID, ok := pathID(r, "userID")
	if !ok {
		http.NotFound(w, r)
		return
	}

	access, ok := s.requireTabManager(w, r, id, user, "Only the provider can change who is on this tab.")
	if !ok {
		return
	}

	if err := s.store.RemoveParticipant(r.Context(), access.Tab.ID, targetID); err != nil {
		switch {
		case errors.Is(err, store.ErrNotFound):
			redirectWith(w, r, tabPath(id), "err", "They are not on this tab.")
		case errors.Is(err, store.ErrConflict):
			redirectWith(w, r, tabPath(id), "err", "A tab must keep at least one provider.")
		default:
			s.serverError(w, r, err)
		}
		return
	}

	s.log.Info("participant removed",
		"tab_id", access.Tab.ID, "removed_user_id", targetID,
		"by_user_id", user.ID, "as_admin", access.Admin)
	redirectWith(w, r, tabPath(id), "ok", "Removed from this tab. Their entries stay in the history.")
}

// ---------------------------------------------------------------------------
// Adjustments (CHG-03)
// ---------------------------------------------------------------------------

// adjustmentReasonMax bounds the stored reason. It matches the memo limit used
// by charges and payments.
const adjustmentReasonMax = 300

// postAdjustment records a correction to what is owed, in either direction.
//
// This is the honest way to reduce a balance after a reconciliation. The
// alternative people reach for -- recording a payment that never happened --
// misstates three things at once: it claims money changed hands, it satisfies
// the period's late-fee window, and on a Payoff tab it is allocated to interest
// before principal, so a credit meant to shrink the debt quietly pays interest
// instead. An adjustment is its own entry kind and says what it is.
//
// The form asks for a direction and a positive amount rather than a signed
// figure. A minus sign is easy to omit and impossible to see afterward, and the
// two directions are not equally common; making the choice explicit means a
// slip produces a validation error rather than a correction that runs backward.
//
// Provider only, like posting a charge: deciding that part of a debt should not
// exist is the same authority as deciding it should.
func (s *Server) postAdjustment(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r.Context())

	id, ok := pathID(r, "id")
	if !ok {
		http.NotFound(w, r)
		return
	}

	if err := r.ParseForm(); err != nil {
		redirectWith(w, r, tabPath(id), "err", "Could not read that form.")
		return
	}
	if !auth.CheckCSRF(r) {
		redirectWith(w, r, tabPath(id), "err", "Your session expired. Please try again.")
		return
	}

	acc, err := s.authorizeTab(r, id, user)
	if err != nil {
		s.denyTab(w, r, err)
		return
	}
	tab := acc.Tab

	// Membership, and the Provider role within it. An administrator reaching a
	// tab they are not party to must not move what two other people owe each
	// other, which is why this checks the participant role rather than
	// stopping at authorizeTab.
	role, err := s.store.ParticipantRole(r.Context(), tab.ID, user.ID)
	if err != nil || role != store.RoleProvider {
		s.log.Warn("adjustment denied", "tab_id", tab.ID, "user_id", user.ID, "role", role)
		redirectWith(w, r, tabPath(id), "err", "Only the provider can adjust what is owed on this tab.")
		return
	}

	amount, err := money.Parse(r.PostFormValue("amount"))
	if err != nil {
		redirectWith(w, r, tabPath(id), "err", "That amount is not a valid dollar figure.")
		return
	}
	if amount <= 0 {
		redirectWith(w, r, tabPath(id), "err", "An adjustment must be greater than zero. Choose a direction below to say which way it goes.")
		return
	}

	// A reason is required. An unexplained correction to a shared balance is
	// the thing both parties will be trying to reconstruct months later, and
	// this is the only record of why it happened.
	reason := strings.TrimSpace(r.PostFormValue("reason"))
	if reason == "" {
		redirectWith(w, r, tabPath(id), "err", "An adjustment needs a reason. Both people will want to know why the balance moved.")
		return
	}
	if len(reason) > adjustmentReasonMax {
		reason = reason[:adjustmentReasonMax]
	}

	// Direction. A credit reduces what is owed, which is a positive amount in
	// the ledger's sign convention; a debit increases it.
	signed := amount
	direction := strings.TrimSpace(r.PostFormValue("direction"))
	switch direction {
	case "", "credit":
		direction = "credit"
	case "debit":
		signed = amount.Neg()
	default:
		redirectWith(w, r, tabPath(id), "err", "Choose whether this reduces or increases what is owed.")
		return
	}

	key := strings.TrimSpace(r.PostFormValue("idempotency_key"))

	entry, replayed, err := s.ledger.Adjustment(r.Context(), ledger.Post{
		TabID:          tab.ID,
		Amount:         signed,
		Memo:           reason,
		ActorUserID:    user.ID,
		IdempotencyKey: key,
		Method:         store.MethodNone,
	})
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if replayed {
		redirectWith(w, r, tabPath(tab.ID), "ok", "That adjustment was already recorded.")
		return
	}

	s.log.Info("adjustment posted", "tab_id", tab.ID, "user_id", user.ID,
		"seq", entry.Seq, "direction", direction, "amount", int64(signed))

	if direction == "credit" {
		redirectWith(w, r, tabPath(tab.ID), "ok", amount.Display()+" credited. The balance is lower by that much.")
		return
	}
	redirectWith(w, r, tabPath(tab.ID), "ok", amount.Display()+" added to what is owed.")
}
