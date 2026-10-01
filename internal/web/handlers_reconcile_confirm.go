package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/johnzastrow/bitt/internal/auth"
	"github.com/johnzastrow/bitt/internal/ledger"
	"github.com/johnzastrow/bitt/internal/reconcile"
	"github.com/johnzastrow/bitt/internal/schedule"
	"github.com/johnzastrow/bitt/internal/store"
)

// Confirming a match and posting the delta; undoing it (RECON-04). Behind
// requireReconcile. These are the reconciliation writes that the AUTH-05
// exception allows: the delta goes through the ledger, attributed to the
// administrator who confirmed it, and points at its bank line.

// errNotSuggestable is a pair that the current setup would not offer, so it
// cannot be confirmed by hand either: a hand-picked pair must not post an
// outsized difference.
var errNotSuggestable = errors.New("that bank line and payment are outside the date window or amount tolerance")

// confirmPair validates one pair against the current setup and confirms it.
func (s *Server) confirmPair(ctx context.Context, actor *store.User, lineID, seq int64, withNote bool,
	set reconcile.Settings) (store.BankMatch, bool, error) {
	line, err := s.store.GetBankLine(ctx, lineID)
	if err != nil {
		return store.BankMatch{}, false, err
	}
	pay, err := s.store.GetEntry(ctx, seq)
	if err != nil {
		return store.BankMatch{}, false, err
	}
	if pay.Kind != store.KindPayment || pay.Amount <= 0 {
		return store.BankMatch{}, false, store.ErrNotAPayment
	}
	tab, err := s.store.GetTab(ctx, pay.TabID)
	if err != nil {
		return store.BankMatch{}, false, err
	}

	loc := s.location(ctx)
	bankDay, err := time.Parse("2006-01-02", line.PostedOn)
	if err != nil {
		return store.BankMatch{}, false, err
	}
	eff := pay.EffectiveAt.In(loc)
	recDay := time.Date(eff.Year(), eff.Month(), eff.Day(), 0, 0, 0, 0, time.UTC)

	if _, ok := reconcile.Score(
		reconcile.Line{ID: line.ID, PostedOn: bankDay, Amount: line.Amount},
		reconcile.Payment{Seq: pay.Seq, EffectiveOn: recDay, Amount: pay.Amount, Method: string(pay.Method)},
		set); !ok {
		return store.BankMatch{}, false, errNotSuggestable
	}

	flagged, reason := dateFlag(bankDay, recDay, set.DateFlagDays, tab.Schedule)
	// The delta takes effect at noon on the bank date in the instance's
	// timezone: the right day wherever the server is, and clear of midnight.
	bankAt := time.Date(bankDay.Year(), bankDay.Month(), bankDay.Day(), 12, 0, 0, 0, loc).UTC()

	return s.ledger.ConfirmMatch(ctx, ledger.Confirmation{
		Match: store.NewBankMatch{
			LineID: line.ID, EntrySeq: pay.Seq, TabID: pay.TabID,
			BankDate: line.PostedOn, BankAmount: line.Amount,
			RecordedDate: recDay.Format("2006-01-02"), RecordedAmount: pay.Amount,
			DateFlagged: flagged, DateFlagReason: reason, NoteInMemo: withNote,
			ConfirmedBy: actor.ID,
		},
		BankAt: bankAt, Description: line.Description, FileName: line.FileName, Row: line.Row,
		Note: line.Note,
	})
}

// dateFlag decides whether a match's dates are flagged (spec section 8):
// further apart than the setting, or on either side of one of the tab's due
// dates. Dates are flagged, never posted.
func dateFlag(bank, recorded time.Time, flagDays int, sched schedule.Schedule) (bool, string) {
	days := int(bank.Sub(recorded).Hours() / 24)
	var reasons []string
	if abs(days) > flagDays {
		reasons = append(reasons, fmt.Sprintf("bank and recorded dates are %d days apart", abs(days)))
	}
	if due, ok := sched.DueBetween(schedule.DateOf(bank), schedule.DateOf(recorded)); ok {
		reasons = append(reasons, "the dates fall on either side of the due date "+due.Display())
	}
	if len(reasons) == 0 {
		return false, ""
	}
	out := reasons[0]
	if len(reasons) > 1 {
		out += "; " + reasons[1]
	}
	return true, out
}

// confirmMessage reports what a confirm posted.
func confirmMessage(m store.BankMatch) string {
	msg := "Matched to " + m.TabName + "."
	switch m.DeltaKind {
	case store.KindPayment:
		msg += " Posted a payment of " + m.DeltaAmount.Display() + "."
	case store.KindAdjustment:
		msg += " Posted a debit adjustment of " + (-m.DeltaAmount).Display() + "."
	default:
		msg += " Amounts agree; nothing posted."
	}
	if m.DateFlagged {
		msg += " Flagged: " + m.DateFlagReason + "."
	}
	return msg
}

func (s *Server) postReconcileConfirm(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r.Context())
	const back = "/admin/reconcile"
	if err := r.ParseForm(); err != nil || !auth.CheckCSRF(r) {
		redirectWith(w, r, back, "err", "Your session expired. Please try again.")
		return
	}
	lineID, err1 := strconv.ParseInt(r.PostFormValue("line_id"), 10, 64)
	seq, err2 := strconv.ParseInt(r.PostFormValue("entry_seq"), 10, 64)
	if err1 != nil || err2 != nil || lineID <= 0 || seq <= 0 {
		redirectWith(w, r, back, "err", "Could not read that confirmation.")
		return
	}
	set, err := s.store.GetReconcileSettings(r.Context())
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	withNote := r.PostFormValue("include_note") == "1"

	m, replayed, err := s.confirmPair(r.Context(), user, lineID, seq, withNote, set.Settings)
	if msg, ok := confirmRefusal(err); ok {
		redirectWith(w, r, back, "err", msg)
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if replayed {
		redirectWith(w, r, back, "ok", "That match was already confirmed.")
		return
	}
	s.log.Info("bank match confirmed", "match_id", m.ID, "line_id", lineID, "entry_seq", seq,
		"tab_id", m.TabID, "delta_seq", m.DeltaEntrySeq, "date_flagged", m.DateFlagged,
		"note_in_memo", m.NoteInMemo, "by_user_id", user.ID)
	redirectWith(w, r, back, "ok", confirmMessage(m))
}

// confirmRefusal maps a refused confirm to what the person is told.
func confirmRefusal(err error) (string, bool) {
	switch {
	case err == nil:
		return "", false
	case errors.Is(err, store.ErrLineNotOpen):
		return "That bank line is already matched, recorded or ignored.", true
	case errors.Is(err, store.ErrPaymentMatched):
		return "That payment is already matched to another bank line.", true
	case errors.Is(err, store.ErrNotAPayment), errors.Is(err, store.ErrNotFound):
		return "That payment can no longer be matched: it was undone, or is not a payment.", true
	case errors.Is(err, errNotSuggestable):
		return "Not matched: " + err.Error() + ".", true
	}
	return "", false
}

// postReconcileConfirmExact confirms every current suggestion whose amounts
// agree. Each is its own transaction; one refused (taken meanwhile) does not
// stop the rest. Nothing is posted by an exact match, so no note is copied.
func (s *Server) postReconcileConfirmExact(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r.Context())
	const back = "/admin/reconcile"
	if err := r.ParseForm(); err != nil || !auth.CheckCSRF(r) {
		redirectWith(w, r, back, "err", "Your session expired. Please try again.")
		return
	}
	set, err := s.store.GetReconcileSettings(r.Context())
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	sugg, err := s.buildSuggestions(r.Context(), set.Settings)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	done, skipped := 0, 0
	for _, m := range sugg.Matches {
		if !m.Suggestion.Exact {
			continue
		}
		confirmed, replayed, err := s.confirmPair(r.Context(), user, m.Line.ID, m.Payment.Seq, false, set.Settings)
		if _, refused := confirmRefusal(err); refused || replayed {
			skipped++
			continue
		}
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		done++
		s.log.Info("bank match confirmed", "match_id", confirmed.ID, "line_id", m.Line.ID,
			"entry_seq", m.Payment.Seq, "tab_id", confirmed.TabID, "exact", true, "by_user_id", user.ID)
	}
	msg := fmt.Sprintf("Confirmed %d exact matches.", done)
	if skipped > 0 {
		msg += fmt.Sprintf(" %d were taken meanwhile and skipped.", skipped)
	}
	redirectWith(w, r, back, "ok", msg)
}

func (s *Server) postReconcileUndo(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r.Context())
	const back = "/admin/reconcile"
	id, ok := pathID(r, "id")
	if !ok {
		http.NotFound(w, r)
		return
	}
	if err := r.ParseForm(); err != nil || !auth.CheckCSRF(r) {
		redirectWith(w, r, back, "err", "Your session expired. Please try again.")
		return
	}
	err := s.ledger.UndoMatch(r.Context(), id, user.ID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		http.NotFound(w, r)
		return
	case errors.Is(err, store.ErrMatchUndone):
		redirectWith(w, r, back, "err", "That match was already undone.")
		return
	case errors.Is(err, ledger.ErrAlreadyReversed):
		redirectWith(w, r, back, "err", "Its difference was already reversed elsewhere; nothing changed.")
		return
	case err != nil:
		s.serverError(w, r, err)
		return
	}
	s.log.Info("bank match undone", "match_id", id, "by_user_id", user.ID)
	redirectWith(w, r, back, "ok", "Match undone. Any difference it posted is reversed, and both sides are unmatched again.")
}

// postPaymentNotInBank sets a recorded payment aside as not in the bank --
// cash, another account -- recording who, when and an optional note.
func (s *Server) postPaymentNotInBank(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r.Context())
	const back = "/admin/reconcile"
	seq, ok := pathID(r, "seq")
	if !ok {
		http.NotFound(w, r)
		return
	}
	if err := r.ParseForm(); err != nil || !auth.CheckCSRF(r) {
		redirectWith(w, r, back, "err", "Your session expired. Please try again.")
		return
	}
	note := strings.TrimSpace(r.PostFormValue("note"))
	if len([]rune(note)) > 500 {
		redirectWith(w, r, back, "err", "Keep the note under 500 characters.")
		return
	}
	rev, err := s.store.SetPaymentNotInBank(r.Context(), seq, user.ID, note)
	switch {
	case errors.Is(err, store.ErrNotAPayment):
		redirectWith(w, r, back, "err", "That is not a payment that stands; nothing changed.")
		return
	case errors.Is(err, store.ErrPaymentMatched):
		redirectWith(w, r, back, "err", "That payment is already matched or set aside.")
		return
	case err != nil:
		s.serverError(w, r, err)
		return
	}
	s.log.Info("payment set aside as not in the bank", "review_id", rev.ID, "entry_seq", seq,
		"tab_id", rev.TabID, "by_user_id", user.ID)
	redirectWith(w, r, back, "ok", "Set aside: "+rev.Amount.Display()+" on "+rev.TabName+" is not expected in the bank.")
}

func (s *Server) postUndoPaymentReview(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r.Context())
	const back = "/admin/reconcile"
	id, ok := pathID(r, "id")
	if !ok {
		http.NotFound(w, r)
		return
	}
	if err := r.ParseForm(); err != nil || !auth.CheckCSRF(r) {
		redirectWith(w, r, back, "err", "Your session expired. Please try again.")
		return
	}
	err := s.store.UndoPaymentReview(r.Context(), id, user.ID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		http.NotFound(w, r)
		return
	case errors.Is(err, store.ErrMatchUndone):
		redirectWith(w, r, back, "err", "That was already undone.")
		return
	case err != nil:
		s.serverError(w, r, err)
		return
	}
	s.log.Info("payment review undone", "review_id", id, "by_user_id", user.ID)
	redirectWith(w, r, back, "ok", "The payment is back among those to account for.")
}
