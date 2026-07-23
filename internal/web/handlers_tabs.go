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

// getDashboard lists the user's tabs with derived balances.
func (s *Server) getDashboard(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r.Context())

	// ListTabsForUser joins through participants, so a tab the user does not
	// belong to is never in the result set (AUTH-05).
	tabs, err := s.store.ListTabsForUser(r.Context(), user.ID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}

	cards := make([]views.TabCard, 0, len(tabs))
	for _, t := range tabs {
		balance, err := s.ledger.Balance(r.Context(), t.ID)
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		role, err := s.store.ParticipantRole(r.Context(), t.ID, user.ID)
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		// A key per rendered card, so a double-tapped settle posts once
		// (LEDGER-07).
		key, err := ledger.NewIdempotencyKey()
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		cards = append(cards, views.TabCard{
			Tab:            t,
			Balance:        balance,
			Role:           role,
			SettleAmount:   settleAmount(balance),
			IdempotencyKey: key,
		})
	}

	s.render(w, r, http.StatusOK, views.Dashboard(s.page(w, r, "Your tabs"), cards))
}

func (s *Server) getNewTab(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, http.StatusOK, views.NewTab(s.page(w, r, "New tab")))
}

// postTab creates a Services tab with its line items (TAB-01, TAB-04).
func (s *Server) postTab(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r.Context())

	if err := r.ParseForm(); err != nil {
		redirectWith(w, r, "/tabs/new", "err", "Could not read that form.")
		return
	}
	if !auth.CheckCSRF(r) {
		redirectWith(w, r, "/tabs/new", "err", "Your session expired. Please try again.")
		return
	}

	name := strings.TrimSpace(r.PostFormValue("name"))
	if name == "" {
		redirectWith(w, r, "/tabs/new", "err", "A tab needs a name.")
		return
	}
	if len(name) > 120 {
		redirectWith(w, r, "/tabs/new", "err", "That name is too long.")
		return
	}
	description := strings.TrimSpace(r.PostFormValue("description"))
	if len(description) > 500 {
		redirectWith(w, r, "/tabs/new", "err", "That description is too long.")
		return
	}

	items, err := parseItems(r.PostForm["item_name"], r.PostForm["item_amount"])
	if err != nil {
		redirectWith(w, r, "/tabs/new", "err", err.Error())
		return
	}

	tab, err := s.store.CreateTab(r.Context(), store.Tab{
		Name:        name,
		Kind:        store.TabServices,
		Description: description,
		CreatedBy:   user.ID,
	}, items)
	if err != nil {
		s.serverError(w, r, err)
		return
	}

	s.log.Info("tab created", "tab_id", tab.ID, "user_id", user.ID, "items", len(items))
	redirectWith(w, r, tabPath(tab.ID), "ok", "Tab created.")
}

// getTab renders one tab: its people, items, derived balance, and history.
func (s *Server) getTab(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r.Context())

	id, ok := pathID(r, "id")
	if !ok {
		http.NotFound(w, r)
		return
	}

	tab, err := s.authorizeTab(r, id, user.ID)
	if err != nil {
		s.denyTab(w, r, err)
		return
	}

	role, err := s.store.ParticipantRole(r.Context(), tab.ID, user.ID)
	if err != nil {
		s.denyTab(w, r, err)
		return
	}

	items, err := s.store.ListItems(r.Context(), tab.ID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	itemTotal, ok := money.Sum(itemAmounts(items))
	if !ok {
		s.serverError(w, r, errors.New("item total overflow"))
		return
	}

	balance, err := s.ledger.Balance(r.Context(), tab.ID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	entries, err := s.ledger.History(r.Context(), tab.ID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	actors, err := s.actorNames(r, entries)
	if err != nil {
		s.serverError(w, r, err)
		return
	}

	// Which entries already carry a reversal, so the undo control is not shown
	// where it would be refused. Computed from the entries already loaded.
	reversed := ledger.ReversedSeqs(entries)

	rows := make([]views.HistoryRow, 0, len(entries))
	for _, e := range entries {
		canUndo := ledger.CanUndo(e, reversed) &&
			(role == store.RoleProvider || e.ActorUserID == user.ID)
		rows = append(rows, views.HistoryRow{
			Entry:   e,
			Actor:   actorName(actors, e.ActorUserID),
			CanUndo: canUndo,
		})
	}

	participants, err := s.store.ListParticipants(r.Context(), tab.ID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}

	var attachable []store.User
	if role == store.RoleProvider {
		if attachable, err = s.attachableUsers(r, tab.ID); err != nil {
			s.serverError(w, r, err)
			return
		}
	}

	key, err := ledger.NewIdempotencyKey()
	if err != nil {
		s.serverError(w, r, err)
		return
	}

	s.render(w, r, http.StatusOK, views.TabDetail(s.page(w, r, tab.Name), views.TabDetailData{
		Tab:            tab,
		Items:          items,
		ItemTotal:      itemTotal,
		Balance:        balance,
		Rows:           rows,
		Participants:   participants,
		Attachable:     attachable,
		Role:           role,
		IdempotencyKey: key,
		SettleAmount:   settleAmount(balance),
	}))
}

// postCharge posts a one-off charge or adjustment (CHG-03).
func (s *Server) postCharge(w http.ResponseWriter, r *http.Request) {
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

	// Only the Provider bills. The role check is per-tab, not global.
	role, err := s.store.ParticipantRole(r.Context(), tab.ID, user.ID)
	if err != nil || role != store.RoleProvider {
		s.log.Warn("charge denied", "tab_id", tab.ID, "user_id", user.ID, "role", role)
		redirectWith(w, r, tabPath(id), "err", "Only the provider can post a charge on this tab.")
		return
	}

	amount, err := money.Parse(r.PostFormValue("amount"))
	if err != nil {
		redirectWith(w, r, tabPath(id), "err", "That amount is not a valid dollar figure.")
		return
	}
	if amount <= 0 {
		redirectWith(w, r, tabPath(id), "err", "A charge must be greater than zero.")
		return
	}

	memo := strings.TrimSpace(r.PostFormValue("memo"))
	if len(memo) > 300 {
		memo = memo[:300]
	}

	// Snapshot the items so a later edit cannot rewrite what was charged (CHG-01).
	items, err := s.store.ListItems(r.Context(), tab.ID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}

	// The idempotency key comes from the form when the client supplies one, and
	// is otherwise generated. Phase 2 puts a per-render key in every form; this
	// path already honors it (LEDGER-07).
	key := strings.TrimSpace(r.PostFormValue("idempotency_key"))

	entry, replayed, err := s.ledger.Charge(r.Context(), ledger.Post{
		TabID:          tab.ID,
		Amount:         amount,
		Memo:           memo,
		ActorUserID:    user.ID,
		IdempotencyKey: key,
		Items:          ledger.ItemsFrom(items),
	})
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if replayed {
		redirectWith(w, r, tabPath(id), "ok", "That charge was already recorded.")
		return
	}

	s.log.Info("charge posted", "tab_id", tab.ID, "entry_seq", entry.Seq, "user_id", user.ID)
	redirectWith(w, r, tabPath(id), "ok", "Charge posted.")
}

// ---------------------------------------------------------------------------
// Authorization and helpers
// ---------------------------------------------------------------------------

// authorizeTab loads a tab only if the user participates in it (AUTH-05).
func (s *Server) authorizeTab(r *http.Request, tabID, userID int64) (store.Tab, error) {
	if _, err := s.store.ParticipantRole(r.Context(), tabID, userID); err != nil {
		return store.Tab{}, err
	}
	return s.store.GetTab(r.Context(), tabID)
}

// denyTab answers 404 for both "no such tab" and "not yours", so the response
// cannot be used to enumerate which tab ids exist. The distinction is logged.
func (s *Server) denyTab(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, store.ErrNotFound) {
		s.log.Warn("tab access denied", "path", r.URL.Path, "remote", clientIP(r))
		http.NotFound(w, r)
		return
	}
	s.serverError(w, r, err)
}

// actorNames resolves the display names shown against history rows.
func (s *Server) actorNames(r *http.Request, entries []store.Entry) (map[int64]string, error) {
	names := make(map[int64]string)
	for _, e := range entries {
		if _, seen := names[e.ActorUserID]; seen {
			continue
		}
		u, err := s.store.GetUser(r.Context(), e.ActorUserID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				names[e.ActorUserID] = "(removed)"
				continue
			}
			return nil, err
		}
		names[e.ActorUserID] = u.DisplayName
	}
	return names, nil
}

// parseItems reads the parallel name and amount fields from the tab form.
// A row with neither a name nor an amount is skipped; a half-filled row is an
// error rather than a silently dropped line, since a dropped line item would
// quietly change what a tab charges.
func parseItems(names, amounts []string) ([]store.TabItem, error) {
	var items []store.TabItem
	n := len(names)
	if len(amounts) > n {
		n = len(amounts)
	}

	for i := 0; i < n; i++ {
		var name, amountRaw string
		if i < len(names) {
			name = strings.TrimSpace(names[i])
		}
		if i < len(amounts) {
			amountRaw = strings.TrimSpace(amounts[i])
		}

		if name == "" && amountRaw == "" {
			continue
		}
		if name == "" {
			return nil, errors.New("Every line item needs a name.")
		}
		if len(name) > 120 {
			return nil, errors.New("That line item name is too long.")
		}
		if amountRaw == "" {
			return nil, errors.New("Every line item needs an amount.")
		}

		amount, err := money.Parse(amountRaw)
		if err != nil {
			return nil, errors.New("One of the line item amounts is not a valid dollar figure.")
		}
		if amount < 0 {
			return nil, errors.New("Line item amounts cannot be negative.")
		}
		items = append(items, store.TabItem{Name: name, Amount: amount, Position: len(items)})
	}
	return items, nil
}

func actorName(actors map[int64]string, id int64) string {
	if n, ok := actors[id]; ok {
		return n
	}
	return "--"
}

func itemAmounts(items []store.TabItem) []money.Cents {
	out := make([]money.Cents, 0, len(items))
	for _, it := range items {
		out = append(out, it.Amount)
	}
	return out
}

func tabPath(id int64) string {
	return "/tabs/" + strconv.FormatInt(id, 10)
}
