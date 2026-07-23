package web

import (
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
func (s *Server) getSettleConfirm(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r.Context())
	card, ok := s.cardFor(w, r, user)
	if !ok {
		return
	}
	if !card.CanSettle() {
		// Nothing owed: hand back the plain card rather than a confirmation
		// that would post a zero payment.
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

	tab, err := s.authorizeTab(r, id, user.ID)
	if err != nil {
		s.denyTab(w, r, err)
		return
	}

	if _, err := s.recordPayment(r, tab, user); err != nil {
		if errors.Is(err, errBadInput) {
			http.Error(w, "That payment could not be recorded.", http.StatusBadRequest)
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

	return views.TabCard{
		Tab:            tab,
		Balance:        balance,
		Role:           role,
		SettleAmount:   settleAmount(balance),
		IdempotencyKey: key,
	}, true
}

// settleAmount is what a one-tap settle pays: the outstanding balance, or zero
// when the tab is settled or in credit.
func settleAmount(balance money.Cents) money.Cents {
	if balance < 0 {
		return balance.Neg()
	}
	return 0
}

// ---------------------------------------------------------------------------
// Full-page payment form (PAY-01, PAY-02, PAY-03)
// ---------------------------------------------------------------------------

// errBadInput marks a validation failure the caller should report as 400.
var errBadInput = errors.New("web: invalid input")

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

	tab, err := s.authorizeTab(r, id, user.ID)
	if err != nil {
		s.denyTab(w, r, err)
		return
	}

	entry, err := s.recordPayment(r, tab, user)
	if err != nil {
		if errors.Is(err, errBadInput) {
			redirectWith(w, r, tabPath(id), "err", strings.TrimPrefix(err.Error(), "web: invalid input: "))
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
		return store.Entry{}, errors.Join(errBadInput, errors.New("That amount is not a valid dollar figure."))
	}
	if amount <= 0 {
		return store.Entry{}, errors.Join(errBadInput, errors.New("A payment must be greater than zero."))
	}

	method := store.PaymentMethod(strings.TrimSpace(r.PostFormValue("method")))
	if method == store.MethodNone {
		method = store.MethodOther
	}
	if !method.Valid() {
		return store.Entry{}, errors.Join(errBadInput, errors.New("That is not a recognized payment method."))
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

	tab, err := s.authorizeTab(r, id, user.ID)
	if err != nil {
		s.denyTab(w, r, err)
		return
	}

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

	_, _, err = s.ledger.Reverse(r.Context(), seq, user.ID, "", "")
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

	tab, err := s.authorizeTab(r, id, user.ID)
	if err != nil {
		s.denyTab(w, r, err)
		return
	}

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
