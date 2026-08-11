package web

import (
	"net/http"

	"github.com/johnzastrow/bitt/internal/ledger"
	"github.com/johnzastrow/bitt/internal/store"
	"github.com/johnzastrow/bitt/internal/web/views"
)

// A screen dedicated to recording one payment: GET /tabs/{id}/pay.
//
// This is where a notification's link goes. The tab page is the right place to
// understand a tab -- history, schedule, people, settings -- and the wrong place
// to land when a reminder has just told you something is due. On a phone it is
// a lot of scrolling to reach one form.
//
// The {url} variable was ALREADY documented as "a link to the tab's payment
// page"; it simply pointed at the whole tab. This makes the documentation true
// rather than redefining it, which is why no new template variable is added and
// no stored template needs editing.
//
// Nothing new is posted. The form targets the existing payment endpoint, so
// every rule about who may record a payment, idempotency and the append-only
// ledger applies unchanged. This screen decides only what is shown.
func (s *Server) getTabPay(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r.Context())

	id, ok := pathID(r, "id")
	if !ok {
		http.NotFound(w, r)
		return
	}
	// The same authorization the tab page uses, so this route cannot become a
	// way to see a tab you could not otherwise see.
	access, err := s.authorizeTab(r, id, user)
	if err != nil {
		s.denyTab(w, r, err)
		return
	}
	tab := access.Tab

	balance, err := s.ledger.Balance(r.Context(), tab.ID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	period, err := s.cardPeriodPayment(r.Context(), tab, balance)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	key, err := ledger.NewIdempotencyKey()
	if err != nil {
		s.serverError(w, r, err)
		return
	}

	data := views.TabPayData{
		Tab:     tab,
		Balance: balance,
		Owed:    settleAmount(balance),
		Period:  period,
		// Recording a payment is membership only, exactly as postPayment
		// enforces. An administrator may reach a tab they do not belong to, but
		// a payment is a statement about something that happened between two
		// people and an administrator is not one of them -- so they see the
		// screen read-only rather than a form that would be refused on submit.
		CanPost:        access.CanTransact(),
		IdempotencyKey: key,
		Methods:        store.PaymentMethods(),
	}
	if due, ok := s.nextUnpaidDue(r.Context(), tab, s.today(r.Context())); ok {
		data.Due = due.Display()
	}

	s.render(w, r, http.StatusOK, views.TabPay(s.page(w, r, "Record a payment"), data))
}

// payPath is the dedicated payment screen for a tab. Notification links point
// here rather than at the tab page.
func payPath(id int64) string { return tabPath(id) + "/pay" }
