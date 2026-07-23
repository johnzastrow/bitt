package web

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/johnzastrow/bitt/internal/auth"
	"github.com/johnzastrow/bitt/internal/money"
	"github.com/johnzastrow/bitt/internal/schedule"
	"github.com/johnzastrow/bitt/internal/store"
)

// anchorWindowDays bounds how far a schedule's start date may sit from today.
//
// An anchor decades back would post hundreds of cycles on the tab's first read,
// and one decades forward is a typo rather than an intent. Five years either way
// leaves backdating a tab by a season or two comfortably available.
const anchorWindowDays = 5 * 366

// postSchedule sets or clears a tab's recurrence (SCHED-01).
//
// Nothing here posts a charge. The next read of the tab does that, through the
// same lazy path every other read uses (SCHED-03), so setting a backdated
// schedule and immediately opening the tab bills the catch-up in one step.
func (s *Server) postSchedule(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r.Context())

	id, ok := pathID(r, "id")
	if !ok {
		http.NotFound(w, r)
		return
	}
	access, ok := s.requireTabManager(w, r, id, user, "Only the provider can change this tab's schedule.")
	if !ok {
		return
	}
	tab := access.Tab

	sched, err := parseSchedule(r, s.location(r.Context()))
	if err != nil {
		redirectWith(w, r, tabPath(id), "err", err.Error())
		return
	}

	if err := s.store.SetSchedule(r.Context(), tab.ID, sched); err != nil {
		s.serverError(w, r, err)
		return
	}

	if !sched.Set() {
		s.log.Info("schedule cleared", "tab_id", tab.ID, "user_id", user.ID, "as_admin", access.Admin)
		redirectWith(w, r, tabPath(id), "ok", "Schedule removed. This tab is billed by hand now.")
		return
	}
	s.log.Info("schedule set", "tab_id", tab.ID, "user_id", user.ID, "as_admin", access.Admin,
		"kind", string(sched.Kind), "anchor", sched.Anchor.String(), "billing", string(sched.Billing))
	redirectWith(w, r, tabPath(id), "ok", sched.Describe()+".")
}

// parseSchedule reads the schedule fields from a submitted form. An empty
// recurrence is not an error: it means the tab is billed by hand (CHG-03).
func parseSchedule(r *http.Request, loc *time.Location) (schedule.Schedule, error) {
	kind := schedule.Kind(strings.TrimSpace(r.PostFormValue("schedule_kind")))
	if kind == schedule.None {
		return schedule.Schedule{}, nil
	}
	if !kind.Valid() {
		return schedule.Schedule{}, errors.New("That is not a recurrence BitTabby offers.")
	}

	raw := strings.TrimSpace(r.PostFormValue("schedule_anchor"))
	if raw == "" {
		return schedule.Schedule{}, errors.New("A schedule needs a start date.")
	}
	anchor, err := schedule.ParseDate(raw)
	if err != nil {
		return schedule.Schedule{}, errors.New("That start date is not a real date.")
	}

	today := schedule.DateOf(time.Now().In(loc))
	if anchor.Before(today.AddDays(-anchorWindowDays)) || anchor.After(today.AddDays(anchorWindowDays)) {
		return schedule.Schedule{}, errors.New("Pick a start date within about five years of today.")
	}

	billing := schedule.Billing(strings.TrimSpace(r.PostFormValue("schedule_billing")))
	if billing == "" {
		billing = schedule.InAdvance
	}
	if !billing.Valid() {
		return schedule.Schedule{}, errors.New("Choose whether the charge lands at the start or the end of each period.")
	}

	sched := schedule.Schedule{Kind: kind, Anchor: anchor, Billing: billing}.Normalize()
	if err := sched.Validate(); err != nil {
		return schedule.Schedule{}, errors.New("That schedule is not usable.")
	}
	return sched, nil
}

// ---------------------------------------------------------------------------
// Line items (CHG-02)
// ---------------------------------------------------------------------------

// postItem adds a line item. It takes effect from the next cycle to post;
// cycles already posted keep the breakdown they captured (CHG-01).
func (s *Server) postItem(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r.Context())

	id, ok := pathID(r, "id")
	if !ok {
		http.NotFound(w, r)
		return
	}
	access, ok := s.requireTabManager(w, r, id, user, "Only the provider can change what this tab covers.")
	if !ok {
		return
	}
	tab := access.Tab

	name, amount, err := parseItemFields(r)
	if err != nil {
		redirectWith(w, r, tabPath(id), "err", err.Error())
		return
	}

	if _, err := s.store.AddItem(r.Context(), store.TabItem{
		TabID: tab.ID, Name: name, Amount: amount,
	}); err != nil {
		s.serverError(w, r, err)
		return
	}

	s.log.Info("item added", "tab_id", tab.ID, "user_id", user.ID, "as_admin", access.Admin)
	redirectWith(w, r, tabPath(id), "ok", "Item added. It applies from the next period.")
}

// postItemUpdate changes an item's name or amount (CHG-02).
func (s *Server) postItemUpdate(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r.Context())

	tab, item, ok := s.authorizeItem(w, r, user)
	if !ok {
		return
	}

	name, amount, err := parseItemFields(r)
	if err != nil {
		redirectWith(w, r, tabPath(tab.ID), "err", err.Error())
		return
	}

	if _, err := s.store.UpdateItem(r.Context(), item.ID, name, amount); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			redirectWith(w, r, tabPath(tab.ID), "err", "That item has already been changed or removed.")
			return
		}
		s.serverError(w, r, err)
		return
	}

	s.log.Info("item changed", "tab_id", tab.ID, "item_id", item.ID, "user_id", user.ID)
	redirectWith(w, r, tabPath(tab.ID), "ok", "Item updated. It applies from the next period; posted charges are unchanged.")
}

// postItemRemove retires an item from future cycles (CHG-02).
func (s *Server) postItemRemove(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r.Context())

	tab, item, ok := s.authorizeItem(w, r, user)
	if !ok {
		return
	}

	if err := s.store.RemoveItem(r.Context(), item.ID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			redirectWith(w, r, tabPath(tab.ID), "err", "That item has already been removed.")
			return
		}
		s.serverError(w, r, err)
		return
	}

	s.log.Info("item removed", "tab_id", tab.ID, "item_id", item.ID, "user_id", user.ID)
	redirectWith(w, r, tabPath(tab.ID), "ok", "Item removed from future periods.")
}

// parseItemFields validates the shared name and amount inputs.
func parseItemFields(r *http.Request) (string, money.Cents, error) {
	name := strings.TrimSpace(r.PostFormValue("name"))
	if name == "" {
		return "", 0, errors.New("An item needs a name.")
	}
	if len(name) > 120 {
		return "", 0, errors.New("That item name is too long.")
	}

	amount, err := money.Parse(r.PostFormValue("amount"))
	if err != nil {
		return "", 0, errors.New("That amount is not a valid dollar figure.")
	}
	if amount < 0 {
		return "", 0, errors.New("An item amount cannot be negative.")
	}
	return name, amount, nil
}

// ---------------------------------------------------------------------------
// Authorization helpers
// ---------------------------------------------------------------------------

// requireTabManager loads a tab and admits only someone allowed to change its
// settings: its Provider, or a global administrator. It parses and CSRF-checks
// the form first, since every caller is a POST.
//
// A non-participant who is not an administrator gets 404 rather than 403, so
// tab ids cannot be enumerated. A Payee gets a message instead, because they
// can already see the tab and a 404 would tell them nothing true.
func (s *Server) requireTabManager(w http.ResponseWriter, r *http.Request, tabID int64, user *store.User, refusal string) (tabAccess, bool) {
	if err := r.ParseForm(); err != nil {
		redirectWith(w, r, tabPath(tabID), "err", "Could not read that form.")
		return tabAccess{}, false
	}
	if !auth.CheckCSRF(r) {
		redirectWith(w, r, tabPath(tabID), "err", "Your session expired. Please try again.")
		return tabAccess{}, false
	}

	access, err := s.authorizeTab(r, tabID, user)
	if err != nil {
		s.denyTab(w, r, err)
		return tabAccess{}, false
	}
	if !access.CanManage() {
		s.log.Warn("tab settings change denied",
			"path", r.URL.Path, "tab_id", tabID, "user_id", user.ID, "role", access.Role)
		redirectWith(w, r, tabPath(tabID), "err", refusal)
		return tabAccess{}, false
	}
	return access, true
}

// authorizeItem resolves the tab and item from the path and admits only the
// tab's Provider.
//
// The item's own tab_id is checked against the path's tab, so an item id
// belonging to someone else's tab cannot be operated on by pairing it with a
// tab the caller does participate in.
func (s *Server) authorizeItem(w http.ResponseWriter, r *http.Request, user *store.User) (store.Tab, store.TabItem, bool) {
	tabID, ok := pathID(r, "id")
	if !ok {
		http.NotFound(w, r)
		return store.Tab{}, store.TabItem{}, false
	}
	itemID, ok := pathID(r, "itemID")
	if !ok {
		http.NotFound(w, r)
		return store.Tab{}, store.TabItem{}, false
	}

	access, ok := s.requireTabManager(w, r, tabID, user, "Only the provider can change what this tab covers.")
	if !ok {
		return store.Tab{}, store.TabItem{}, false
	}
	tab := access.Tab

	item, err := s.store.GetItem(r.Context(), itemID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			redirectWith(w, r, tabPath(tabID), "err", "That item no longer exists.")
			return store.Tab{}, store.TabItem{}, false
		}
		s.serverError(w, r, err)
		return store.Tab{}, store.TabItem{}, false
	}
	if item.TabID != tab.ID {
		s.log.Warn("item does not belong to the named tab",
			"tab_id", tab.ID, "item_id", itemID, "user_id", user.ID, "remote", clientIP(r))
		http.NotFound(w, r)
		return store.Tab{}, store.TabItem{}, false
	}
	return tab, item, true
}
