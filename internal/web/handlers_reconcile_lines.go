package web

import (
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/johnzastrow/bitt/internal/auth"
	"github.com/johnzastrow/bitt/internal/ledger"
	"github.com/johnzastrow/bitt/internal/reconcile"
	"github.com/johnzastrow/bitt/internal/store"
	"github.com/johnzastrow/bitt/internal/web/views"
)

// One bank line and everything that can be done to it (RECON-05). Behind
// requireReconcile.

func linePath(id int64) string { return "/admin/reconcile/lines/" + strconv.FormatInt(id, 10) }

func (s *Server) getReconcileLine(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, ok := pathID(r, "id")
	if !ok {
		http.NotFound(w, r)
		return
	}
	line, err := s.store.GetBankLine(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	events, err := s.store.ListBankLineEvents(ctx, id)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	all, err := s.store.ListBankMatches(ctx, store.BankMatchFilter{ImportID: line.ImportID})
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	var matches []store.BankMatch
	for _, m := range all {
		if m.LineID == id {
			matches = append(matches, m)
		}
	}
	d := views.LineData{Line: line, Events: events, Matches: matches}
	if line.RecordedEntrySeq != nil {
		if e, err := s.store.GetEntry(ctx, *line.RecordedEntrySeq); err == nil {
			d.Recorded = &e
			if t, err := s.store.GetTab(ctx, e.TabID); err == nil {
				d.RecordedTab = t.Name
			}
		}
	}
	if line.State == store.BankLineOpen {
		if d.Tabs, err = s.recordChoices(r, line); err != nil {
			s.serverError(w, r, err)
			return
		}
		d.Memo = recordMemo(line)
	}
	s.render(w, r, http.StatusOK, views.ReconcileLine(s.page(w, r, "Bank line"), d))
}

// recordChoices lists the tabs a line could be recorded on: every tab not
// archived, those whose name or people appear in the line's text first.
func (s *Server) recordChoices(r *http.Request, line store.OpenBankLine) ([]views.TabChoice, error) {
	tabs, err := s.store.ListAllTabs(r.Context())
	if err != nil {
		return nil, err
	}
	var out []views.TabChoice
	for _, t := range tabs {
		if t.ArchivedAt != nil {
			continue
		}
		ps, err := s.store.ListParticipants(r.Context(), t.ID)
		if err != nil {
			return nil, err
		}
		names := []string{t.Name}
		var people []string
		for _, p := range ps {
			names = append(names, p.DisplayName)
			people = append(people, p.DisplayName)
		}
		out = append(out, views.TabChoice{ID: t.ID, Name: t.Name, People: strings.Join(people, ", "),
			Likely: reconcile.NameHit(line.Description+"\n"+line.Note, names)})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Likely != out[j].Likely {
			return out[i].Likely
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	return out, nil
}

func recordMemo(l store.OpenBankLine) string {
	return fmt.Sprintf("Bank reconciliation: %s (line %d of %s)",
		strings.Join(strings.Fields(l.Description), " "), l.Row, l.FileName)
}

// lineForm reads the line id and checks the CSRF token; false means a
// response has been written.
func (s *Server) lineForm(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, ok := pathID(r, "id")
	if !ok {
		http.NotFound(w, r)
		return 0, false
	}
	if err := r.ParseForm(); err != nil || !auth.CheckCSRF(r) {
		redirectWith(w, r, linePath(id), "err", "Your session expired. Please try again.")
		return 0, false
	}
	return id, true
}

// postRecordLine records an unmatched line as a payment on a chosen tab.
func (s *Server) postRecordLine(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r.Context())
	id, ok := s.lineForm(w, r)
	if !ok {
		return
	}
	back := linePath(id)
	line, err := s.store.GetBankLine(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	tabID, err := strconv.ParseInt(r.PostFormValue("tab_id"), 10, 64)
	if err != nil {
		redirectWith(w, r, back, "err", "Choose the tab the payment belongs to.")
		return
	}
	tab, err := s.store.GetTab(r.Context(), tabID)
	if err != nil || tab.ArchivedAt != nil {
		redirectWith(w, r, back, "err", "Choose the tab the payment belongs to.")
		return
	}
	method := store.PaymentMethod(r.PostFormValue("method"))
	if method != store.MethodTransfer && method != store.MethodOther {
		redirectWith(w, r, back, "err", "Choose how the money arrived.")
		return
	}
	memo := strings.TrimSpace(r.PostFormValue("memo"))
	if memo == "" {
		memo = recordMemo(line)
	}
	if len([]rune(memo)) > 500 {
		redirectWith(w, r, back, "err", "Keep the note under 500 characters.")
		return
	}
	if r.PostFormValue("include_note") == "1" && strings.TrimSpace(line.Note) != "" {
		memo += "\nBank note: " + line.Note
	}
	day, err := time.Parse("2006-01-02", line.PostedOn)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	loc := s.location(r.Context())
	at := time.Date(day.Year(), day.Month(), day.Day(), 12, 0, 0, 0, loc).UTC()

	e, err := s.ledger.RecordLine(r.Context(), id, tab.ID, user.ID, method, memo, at)
	if errors.Is(err, store.ErrLineNotOpen) {
		redirectWith(w, r, back, "err", "That line is no longer open; nothing was posted.")
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.log.Info("bank line recorded", "line_id", id, "entry_seq", e.Seq, "tab_id", tab.ID, "by_user_id", user.ID)
	redirectWith(w, r, back, "ok", "Recorded a payment of "+line.Amount.Display()+" on "+tab.Name+".")
}

func (s *Server) postUnrecordLine(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r.Context())
	id, ok := s.lineForm(w, r)
	if !ok {
		return
	}
	err := s.ledger.UnrecordLine(r.Context(), id, user.ID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		http.NotFound(w, r)
		return
	case errors.Is(err, store.ErrLineNotOpen):
		redirectWith(w, r, linePath(id), "err", "That line is not recorded as a payment.")
		return
	case errors.Is(err, ledger.ErrAlreadyReversed):
		redirectWith(w, r, linePath(id), "err", "That payment was already undone.")
		return
	case err != nil:
		s.serverError(w, r, err)
		return
	}
	s.log.Info("bank line unrecorded", "line_id", id, "by_user_id", user.ID)
	redirectWith(w, r, linePath(id), "ok", "The recorded payment is reversed and the line is open again.")
}

func (s *Server) postIgnoreLine(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r.Context())
	id, ok := s.lineForm(w, r)
	if !ok {
		return
	}
	note := strings.TrimSpace(r.PostFormValue("note"))
	if len([]rune(note)) > 500 {
		redirectWith(w, r, linePath(id), "err", "Keep the note under 500 characters.")
		return
	}
	err := s.store.IgnoreBankLine(r.Context(), id, user.ID, note)
	switch {
	case errors.Is(err, store.ErrNotFound):
		http.NotFound(w, r)
		return
	case errors.Is(err, store.ErrLineNotOpen):
		redirectWith(w, r, linePath(id), "err", "Only an open line can be marked Not BitTabby.")
		return
	case err != nil:
		s.serverError(w, r, err)
		return
	}
	s.log.Info("bank line ignored", "line_id", id, "by_user_id", user.ID)
	redirectWith(w, r, "/admin/reconcile", "ok", "Marked Not BitTabby. It will not be shown again unless un-ignored.")
}

func (s *Server) postUnignoreLine(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r.Context())
	id, ok := s.lineForm(w, r)
	if !ok {
		return
	}
	err := s.store.UnignoreBankLine(r.Context(), id, user.ID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		http.NotFound(w, r)
		return
	case errors.Is(err, store.ErrLineNotOpen):
		redirectWith(w, r, linePath(id), "err", "That line is not ignored.")
		return
	case err != nil:
		s.serverError(w, r, err)
		return
	}
	s.log.Info("bank line unignored", "line_id", id, "by_user_id", user.ID)
	redirectWith(w, r, linePath(id), "ok", "The line is open again and can be matched or recorded.")
}
