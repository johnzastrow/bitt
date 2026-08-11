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
	"github.com/johnzastrow/bitt/internal/schedule"
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
		// Reading a tab is what bills it (SCHED-03). The dashboard is a read of
		// every tab the user has, so opening it brings them all current -- which
		// is why a balance shown here is never stale.
		acc, err := s.accrueTab(r, t)
		if err != nil {
			s.serverError(w, r, err)
			return
		}

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
		period, err := s.cardPeriodPayment(r.Context(), t, balance)
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		card := views.TabCard{
			Tab:            t,
			Balance:        balance,
			Role:           role,
			SettleAmount:   settleAmount(balance),
			PeriodPayment:  period,
			IdempotencyKey: key,
		}
		if acc.Scheduled {
			card.NextDue = acc.Next.Due
			card.JustPosted = len(acc.Posted)
			card.JustFined = len(acc.Fees)
		}
		// A Payoff card leads with loan progress rather than a bare balance
		// (PAYOFF-01), and a fully paid one drops off the active list (PAYOFF-03).
		if t.Kind == store.TabPayoff {
			payoff, err := s.payoffFor(r, t)
			if err != nil {
				s.serverError(w, r, err)
				return
			}
			card.Payoff = &payoff
		}
		cards = append(cards, card)
	}

	s.render(w, r, http.StatusOK, views.Dashboard(s.page(w, r, "Your tabs"), cards))
}

func (s *Server) getNewTab(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, http.StatusOK, views.NewTab(s.page(w, r, "New tab"), views.NewTabForm{}))
}

// newTabFormFrom captures a create-tab submission so a validation error can
// re-render the form with the user's entries rather than an empty one. Schedule
// and fee are parsed best-effort: a valid one pre-fills the shared sub-form, an
// invalid one falls back to blank while every other field is kept as typed.
func (s *Server) newTabFormFrom(r *http.Request) views.NewTabForm {
	f := views.NewTabForm{
		Name:        strings.TrimSpace(r.PostFormValue("name")),
		Description: strings.TrimSpace(r.PostFormValue("description")),
		Kind:        r.PostFormValue("kind"),
		LoanAmount:  strings.TrimSpace(r.PostFormValue("loan_amount")),
		InterestAPR: strings.TrimSpace(r.PostFormValue("interest_apr")),
		LoanTerm:    strings.TrimSpace(r.PostFormValue("loan_term")),
		LoanPayment: strings.TrimSpace(r.PostFormValue("loan_payment")),
	}
	names, amounts := r.PostForm["item_name"], r.PostForm["item_amount"]
	for i := 0; i < len(names) || i < len(amounts); i++ {
		var it views.ItemInput
		if i < len(names) {
			it.Name = names[i]
		}
		if i < len(amounts) {
			it.Amount = amounts[i]
		}
		if strings.TrimSpace(it.Name) == "" && strings.TrimSpace(it.Amount) == "" {
			continue
		}
		f.Items = append(f.Items, it)
	}
	if sched, err := parseSchedule(r, s.location(r.Context())); err == nil {
		f.Schedule = sched
	}
	if pol, err := parseFeePolicy(r); err == nil {
		f.Fee = pol
	}
	return f
}

// renderNewTabError re-renders the create form with the submitted values and an
// error message, so a single mistake does not discard everything the user typed.
func (s *Server) renderNewTabError(w http.ResponseWriter, r *http.Request, msg string) {
	p := s.page(w, r, "New tab")
	p.Error = msg
	s.render(w, r, http.StatusBadRequest, views.NewTab(p, s.newTabFormFrom(r)))
}

// postTab creates a Services tab with its line items (TAB-01, TAB-04).
func (s *Server) postTab(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r.Context())

	if err := r.ParseForm(); err != nil {
		s.renderNewTabError(w, r, "Could not read that form.")
		return
	}
	if !auth.CheckCSRF(r) {
		s.renderNewTabError(w, r, "Your session expired. Please try again.")
		return
	}

	name := strings.TrimSpace(r.PostFormValue("name"))
	if name == "" {
		s.renderNewTabError(w, r, "A tab needs a name.")
		return
	}
	if len(name) > 120 {
		s.renderNewTabError(w, r, "That name is too long.")
		return
	}
	description := strings.TrimSpace(r.PostFormValue("description"))
	if len(description) > 500 {
		s.renderNewTabError(w, r, "That description is too long.")
		return
	}

	items, err := parseItems(r.PostForm["item_name"], r.PostForm["item_amount"])
	if err != nil {
		s.renderNewTabError(w, r, err.Error())
		return
	}

	// A schedule is optional. Without one the tab is billed by hand, which
	// stays a legitimate way to run one (CHG-03).
	sched, err := parseSchedule(r, s.location(r.Context()))
	if err != nil {
		s.renderNewTabError(w, r, err.Error())
		return
	}

	// Services or Payoff (TAB-01, TAB-02). Services is the default, since a
	// tab created without a stated kind is far more often a recurring one.
	kind, err := parseTabKind(r.PostFormValue("kind"))
	if err != nil {
		s.renderNewTabError(w, r, err.Error())
		return
	}

	// A Payoff loan states its amount at creation, posted as the opening charge
	// -- the principal the payments draw down (TAB-02). It is required, because a
	// loan with no principal cannot track progress and reads as already paid off
	// the moment any payment lands, which is a confusing failure to leave open.
	var principal money.Cents
	if kind == store.TabPayoff {
		raw := strings.TrimSpace(r.PostFormValue("loan_amount"))
		if raw == "" {
			s.renderNewTabError(w, r, "A Payoff loan needs a loan amount -- the balance the payments draw down.")
			return
		}
		if principal, err = money.Parse(raw); err != nil || principal <= 0 {
			s.renderNewTabError(w, r, "The loan amount must be a dollar figure greater than zero.")
			return
		}
	}

	// A late-fee policy is optional and may be set at creation.
	policy, err := parseFeePolicy(r)
	if err != nil {
		s.renderNewTabError(w, r, err.Error())
		return
	}

	// Interest, term, and the scheduled payment apply to Payoff loans only.
	// Ignore them on a Services tab, whose period charge is its line items.
	var (
		interestBP  int64
		loanTerm    int
		loanPayment money.Cents
	)
	if kind == store.TabPayoff {
		if interestBP, err = parseInterestBP(r.PostFormValue("interest_apr")); err != nil {
			s.renderNewTabError(w, r, err.Error())
			return
		}
		if loanTerm, loanPayment, err = parseLoanTerms(r); err != nil {
			s.renderNewTabError(w, r, err.Error())
			return
		}
	}

	// A Payoff tab carries no line items: its loan amount posts as a charge and
	// its payment is the expectation. Items would be a second, silent source of
	// truth for the same figure.
	if kind == store.TabPayoff {
		items = nil
	}

	tab, err := s.store.CreateTab(r.Context(), store.Tab{
		Name:            name,
		Kind:            kind,
		Description:     description,
		CreatedBy:       user.ID,
		Schedule:        sched,
		Fee:             policy,
		InterestAPRBp:   interestBP,
		LoanTermPeriods: loanTerm,
		LoanPayment:     loanPayment,
	}, items)
	if err != nil {
		s.serverError(w, r, err)
		return
	}

	if principal > 0 {
		if _, _, err := s.ledger.Charge(r.Context(), ledger.Post{
			TabID:       tab.ID,
			Amount:      principal,
			Memo:        "Opening balance",
			ActorUserID: user.ID,
			Items:       ledger.ItemsFrom(items),
		}); err != nil {
			// The tab exists; the Provider can post the principal by hand. Report
			// rather than fail the whole creation.
			s.log.Error("opening charge failed", "tab_id", tab.ID, "error", err)
			redirectWith(w, r, tabPath(tab.ID), "err", "Tab created, but the opening balance did not post. Post it as a charge.")
			return
		}
	}

	s.log.Info("tab created", "tab_id", tab.ID, "user_id", user.ID,
		"kind", string(kind), "items", len(items), "schedule", string(sched.Kind),
		"interval", sched.Every(), "fee", string(policy.Kind), "principal", int64(principal),
		"loan_term", loanTerm, "loan_payment", int64(loanPayment))
	redirectWith(w, r, tabPath(tab.ID), "ok", "Tab created.")
}

// parseTabKind reads the Services/Payoff choice, defaulting to Services.
func parseTabKind(raw string) (store.TabKind, error) {
	kind := store.TabKind(strings.TrimSpace(raw))
	if kind == "" {
		return store.TabServices, nil
	}
	if !kind.Valid() {
		return "", errors.New("That is not a kind of tab BitTabby offers.")
	}
	return kind, nil
}

// postTabDetails renames a tab and changes what it covers or what kind it is.
//
// None of these touches an entry, so no balance can move here. The change is
// logged with its before and after: a tab's name and kind are how people refer
// to it and how it is measured, and a silent change to either is the kind of
// thing that needs explaining months later.
func (s *Server) postTabDetails(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r.Context())

	id, ok := pathID(r, "id")
	if !ok {
		http.NotFound(w, r)
		return
	}
	access, ok := s.requireTabManager(w, r, id, user, "Only the provider can change this tab's details.")
	if !ok {
		return
	}
	before := access.Tab

	name := strings.TrimSpace(r.PostFormValue("name"))
	switch {
	case name == "":
		redirectWith(w, r, tabPath(id), "err", "A tab needs a name.")
		return
	case len(name) > 120:
		redirectWith(w, r, tabPath(id), "err", "That name is too long.")
		return
	}

	description := strings.TrimSpace(r.PostFormValue("description"))
	if len(description) > 500 {
		redirectWith(w, r, tabPath(id), "err", "That description is too long.")
		return
	}

	kind, err := parseTabKind(r.PostFormValue("kind"))
	if err != nil {
		redirectWith(w, r, tabPath(id), "err", err.Error())
		return
	}

	if err := s.store.UpdateTabDetails(r.Context(), id, name, description, kind); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		s.serverError(w, r, err)
		return
	}

	s.log.Info("tab details changed",
		"tab_id", id, "user_id", user.ID, "as_admin", access.Admin,
		"name_from", before.Name, "name_to", name,
		"kind_from", string(before.Kind), "kind_to", string(kind))

	if before.Kind != kind {
		redirectWith(w, r, tabPath(id), "ok",
			"Saved. This is now a "+kind.Label()+" tab; nothing already posted has changed.")
		return
	}
	redirectWith(w, r, tabPath(id), "ok", "Tab details saved.")
}

// postTabArchive retires a tab from the active dashboard, or brings it back.
//
// Archiving is not deletion. Every entry, the balance, and the whole history
// stay exactly as they are; what stops is scheduled accrual, so an archived tab
// does not quietly keep billing.
func (s *Server) postTabArchive(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r.Context())

	id, ok := pathID(r, "id")
	if !ok {
		http.NotFound(w, r)
		return
	}
	access, ok := s.requireTabManager(w, r, id, user, "Only the provider can archive this tab.")
	if !ok {
		return
	}

	archive := access.Tab.ArchivedAt == nil
	if err := s.store.SetTabArchived(r.Context(), id, archive); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		s.serverError(w, r, err)
		return
	}

	s.log.Info("tab archive state changed",
		"tab_id", id, "user_id", user.ID, "as_admin", access.Admin, "archived", archive)

	if archive {
		redirectWith(w, r, tabPath(id), "ok",
			"Archived. It stops billing and leaves the active list; the history is untouched.")
		return
	}
	redirectWith(w, r, tabPath(id), "ok", "Reactivated. Any periods that came due while it was archived will post on the next read.")
}

// getTab renders one tab: its people, items, derived balance, and history.
func (s *Server) getTab(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r.Context())

	id, ok := pathID(r, "id")
	if !ok {
		http.NotFound(w, r)
		return
	}

	access, err := s.authorizeTab(r, id, user)
	if err != nil {
		s.denyTab(w, r, err)
		return
	}
	tab, role := access.Tab, access.Role

	// Post whatever has come due before anything is read off the tab, so the
	// balance, the history, and the statements below all describe the same
	// moment (SCHED-03).
	acc, err := s.accrueTab(r, tab)
	if err != nil {
		s.serverError(w, r, err)
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

	// The running balance beside each entry. Entries arrive newest first, so the
	// walk starts at the balance the page shows and subtracts each entry back
	// out: the top row is the current balance by construction, and the column
	// cannot disagree with the figure above it.
	running := balance

	rows := make([]views.HistoryRow, 0, len(entries))
	for _, e := range entries {
		canUndo := ledger.CanUndo(e, reversed) &&
			(billsTab(role) || e.ActorUserID == user.ID)
		actor := actorFor(actors, e.ActorUserID)
		rows = append(rows, views.HistoryRow{
			Entry:       e,
			Actor:       actor.Name,
			ActorID:     e.ActorUserID,
			ActorAvatar: actor.AvatarKey,
			CanUndo:     canUndo,
			Balance:     running,
		})
		running -= e.Amount
	}

	participants, err := s.store.ListParticipants(r.Context(), tab.ID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}

	var attachable []store.User
	if access.CanManage() {
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

	statements, err := s.statements(r, tab, entries)
	if err != nil {
		s.serverError(w, r, err)
		return
	}

	// The tab's own reminders, and the instance defaults it falls back to when
	// it has none. Only the Provider can see or set them, so the read is
	// skipped entirely for a payee.
	var reminders []store.TabReminder
	if access.CanManage() {
		if reminders, err = s.store.ListTabReminders(r.Context(), tab.ID); err != nil {
			s.serverError(w, r, err)
			return
		}
	}

	data := views.TabDetailData{
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
		PeriodPayment:  periodPayment(periodBase(tab, itemTotal), balance),
		Statements:     statements,
		Scheduled:      acc.Scheduled,
		NextDue:        acc.Next.Due,
		JustPosted:     len(acc.Posted),
		JustFined:      len(acc.Fees),
		JustInterest:   len(acc.Interest),
		Today:          s.today(r.Context()),
		CanManage:      access.CanManage(),
		CanTransact:    access.CanTransact(),
		AsAdmin:        access.Admin,
		Upcoming:       s.upcoming(tab, acc, itemTotal),
		// REM-01: what each reminder would actually say, and who would get it.
		ReminderPreviews: s.reminderPreviews(r.Context(), tab),

		Reminders:        reminders,
		DefaultReminders: s.defaultReminders(r.Context()),
		NotifyReady:      s.notifyReady(r.Context()),
	}
	if tab.Kind == store.TabPayoff {
		payoff, err := s.payoffFor(r, tab)
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		data.Payoff = &payoff
		// Advisory only: the suggested payment and the true-up against it.
		// Nothing here posts, and the Provider's entered payment is unchanged.
		data.Plan = ledger.ComputePlan(tab, payoff)
		// The payments still to come, at the payment being made now.
		data.Forecast = ledger.ComputeForecast(tab, payoff, data.Today)
	}

	s.render(w, r, http.StatusOK, views.TabDetail(s.page(w, r, tab.Name), data))
}

// noticeWindowDays is how far ahead an upcoming payment or charge is surfaced.
// The user asked for the request to appear two weeks before it is due.
const noticeWindowDays = 14

// upcoming describes the tab's next scheduled event when it falls within the
// notice window, so a payment request appears well before its due date.
//
// This is the in-app half of the reminder. The pushed email/ntfy version is a
// later phase; this half is pure computation on the read path and needs no new
// machinery, so it ships now.
func (s *Server) upcoming(tab store.Tab, acc ledger.Accrual, itemTotal money.Cents) views.Upcoming {
	// What is coming differs by kind: a Services tab is about to be charged the
	// sum of its items, while a Payoff tab expects the loan's scheduled
	// payment, which posts nothing and lives in its own field.
	amount := itemTotal
	if tab.Kind == store.TabPayoff {
		amount = tab.LoanPayment
	}
	if !acc.Scheduled || tab.ArchivedAt != nil || amount <= 0 {
		return views.Upcoming{}
	}
	due := acc.Next.Due
	if due.IsZero() {
		return views.Upcoming{}
	}
	today := s.today(context.Background())
	days := daysBetween(today, due)
	if days < 0 || days > noticeWindowDays {
		return views.Upcoming{}
	}
	return views.Upcoming{
		Due:       due,
		Amount:    amount,
		DaysUntil: days,
		// On a Payoff tab the schedule describes an expected payment; on a
		// Services tab it is the charge that is about to post.
		IsPayment: tab.Kind == store.TabPayoff,
	}
}

// daysBetween counts whole days from a to b, computed as a calendar difference
// in UTC so no daylight saving transition can shift it.
func daysBetween(a, b schedule.Date) int {
	return int(b.Time(nil).Sub(a.Time(nil)).Hours()) / 24
}

// statements builds the per-period view from entries already loaded (CHG-04).
//
// Nothing is read back that the page does not already have except the item
// snapshots, which come in one query rather than one per period.
func (s *Server) statements(r *http.Request, tab store.Tab, entries []store.Entry) ([]ledger.Statement, error) {
	periods, err := s.store.ListPostedPeriods(r.Context(), tab.ID)
	if err != nil {
		return nil, err
	}
	if len(periods) == 0 {
		return nil, nil
	}

	items, err := s.store.ListEntryItemsForTab(r.Context(), tab.ID)
	if err != nil {
		return nil, err
	}

	built, ok := ledger.Statements(periods, entries, items, s.today(r.Context()))
	if !ok {
		return nil, errors.New("statement allocation overflow")
	}
	return built, nil
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

	acc, err := s.authorizeTab(r, id, user)
	if err != nil {
		s.denyTab(w, r, err)
		return
	}
	tab := acc.Tab

	// The Provider bills, and so may a per-tab administrator. The role check is
	// per-tab, not global.
	role, err := s.store.ParticipantRole(r.Context(), tab.ID, user.ID)
	if err != nil || !billsTab(role) {
		s.log.Warn("charge denied", "tab_id", tab.ID, "user_id", user.ID, "role", role)
		redirectWith(w, r, tabPath(id), "err", "Only the provider or a tab administrator can post a charge on this tab.")
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

// tabAccess describes what the signed-in user may do with one tab.
//
// Two ways in. Participation is the ordinary one and is what AUTH-05 describes.
// The global administrator role is a deliberate second one, so that a tab whose
// Provider has left the household can still be renamed, archived, or repaired
// without someone editing the database by hand.
//
// The two are kept distinct rather than collapsed into a single boolean,
// because they do not grant the same things: an administrator who is not on a
// tab may manage what the tab *is*, and may not move money on it. Recording a
// payment is a statement about something that happened between two people, and
// an administrator is not one of them.
type tabAccess struct {
	Tab store.Tab
	// Role is the user's role on the tab, empty when they do not participate.
	Role store.Role
	// Participant reports membership. Only participants transact.
	Participant bool
	// Admin reports that access came from the administrator role rather than
	// from membership.
	Admin bool
}

// IsProvider reports whether the user bills on this tab as a participant.
func (a tabAccess) IsProvider() bool { return a.Participant && a.Role == store.RoleProvider }

// billsTab reports whether a per-tab role may bill and correct the tab -- post
// charges and adjustments, and undo any entry on it. The Provider and a per-tab
// administrator may; a Payee may not (a Payee still records their own payments
// and undoes only their own entries). This is deliberately a role check, not a
// membership one: the instance administrator, who is no party to the money, is
// never a biller even where they may manage settings.
func billsTab(role store.Role) bool {
	return role == store.RoleProvider || role == store.RoleAdmin
}

// IsTabAdmin reports whether the user is a per-tab administrator: a member added
// with the admin role, distinct from the instance-wide administrator (a.Admin).
func (a tabAccess) IsTabAdmin() bool { return a.Participant && a.Role == store.RoleAdmin }

// CanManage covers a tab's own settings: its name, kind, schedule, items, and
// people. The Provider always may, as does a per-tab administrator; the
// instance administrator may on any tab.
func (a tabAccess) CanManage() bool { return a.IsProvider() || a.IsTabAdmin() || a.Admin }

// CanTransact covers moving money: charges, payments, and undo. Membership
// only -- an administrator looking after the instance is not a party to what
// two other people owe each other.
func (a tabAccess) CanTransact() bool { return a.Participant }

// authorizeTab loads a tab the user is entitled to see.
//
// A non-participant who is not an administrator gets ErrNotFound, so foreign
// tab ids still cannot be enumerated. An administrator reaching a tab they do
// not belong to is admitted and logged: the log line is what makes the
// exception to AUTH-05 auditable rather than invisible.
func (s *Server) authorizeTab(r *http.Request, tabID int64, user *store.User) (tabAccess, error) {
	role, roleErr := s.store.ParticipantRole(r.Context(), tabID, user.ID)
	switch {
	case roleErr == nil:
	case errors.Is(roleErr, store.ErrNotFound) && user.IsAdmin:
	default:
		return tabAccess{}, roleErr
	}

	tab, err := s.store.GetTab(r.Context(), tabID)
	if err != nil {
		return tabAccess{}, err
	}

	acc := tabAccess{
		Tab:         tab,
		Role:        role,
		Participant: roleErr == nil,
		Admin:       roleErr != nil,
	}
	if acc.Admin {
		s.log.Warn("administrator reached a tab they do not participate in",
			"tab_id", tabID, "user_id", user.ID, "method", r.Method, "path", r.URL.Path)
	}
	return acc, nil
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
// actorInfo is what the history needs to name and picture whoever recorded an
// entry: the display name and the avatar timestamp, resolved once per actor.
type actorInfo struct {
	Name      string
	AvatarKey string
}

func (s *Server) actorNames(r *http.Request, entries []store.Entry) (map[int64]actorInfo, error) {
	actors := make(map[int64]actorInfo)
	for _, e := range entries {
		if _, seen := actors[e.ActorUserID]; seen {
			continue
		}
		u, err := s.store.GetUser(r.Context(), e.ActorUserID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				// A deactivated account keeps its entries, but a genuinely
				// removed one leaves a dangling id. Name it plainly and show no
				// picture.
				actors[e.ActorUserID] = actorInfo{Name: "(removed)"}
				continue
			}
			return nil, err
		}
		actors[e.ActorUserID] = actorInfo{Name: u.DisplayName, AvatarKey: u.AvatarUpdatedAt}
	}
	return actors, nil
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

func actorFor(actors map[int64]actorInfo, id int64) actorInfo {
	if a, ok := actors[id]; ok {
		return a
	}
	return actorInfo{Name: "--"}
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
