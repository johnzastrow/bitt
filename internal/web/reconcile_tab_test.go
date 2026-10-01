package web

import (
	"net/url"
	"strings"
	"testing"

	"github.com/johnzastrow/bitt/internal/store"
)

// RECON-06: what a tab shows of bank reconciliation. Everyone on the tab sees
// a mark beside a reconciled payment with the bank date and amount; the bank
// text -- description, original row, note -- is for holders of Can reconcile
// only (spec section 11).

func TestTabShowsBankMarksAndHistory(t *testing.T) {
	h := scenario(t)
	phone := tabByName(t, h, "Phone plan")
	ins := paymentOn(t, h, "Insurance", 119000)
	h.confirmPair(t, lineByRow(t, h, 4).ID, ins.Seq, false) // $10 difference
	h.confirmPair(t, lineByRow(t, h, 2).ID, paymentOn(t, h, "Phone plan", 15000).Seq, false)
	h.lineAction(t, lineByRow(t, h, 6).ID, "record", url.Values{"tab_id": {itoa(phone.ID)}, "method": {"transfer"}})

	// The holder sees marks and the full history.
	_, body := h.get(tabPath(ins.TabID))
	for _, want := range []string{
		"matched to bank: Sep 2, $1,200.00", "bank reconciliation difference",
		"<h2>Bank reconciliation</h2>", "Row 4 of TransactionHistory.csv", "Confirmed by Jane Provider",
		"Original row", "FROM ALEX OTHER",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("holder's Insurance tab lacks %q", want)
		}
	}
	_, body = h.get(tabPath(phone.ID))
	for _, want := range []string{"matched to bank: Sep 1, $150.00", "recorded from bank: Sep 5",
		"recorded from bank", "Bank note:", "Phone plan, September\nsplit with Ann"} {
		if !strings.Contains(body, want) {
			t.Errorf("holder's Phone tab lacks %q", want)
		}
	}

	// A payee sees the marks, and none of the bank text.
	h.loginAs("sam@example.com", "a-long-enough-password")
	_, body = h.get(tabPath(phone.ID))
	if !strings.Contains(body, "matched to bank: Sep 1, $150.00") || !strings.Contains(body, "recorded from bank: Sep 5") {
		t.Error("the payee does not see the bank marks")
	}
	// The memo carries the description and the file row by design (spec
	// section 8); what stays private is the history, the original row, the
	// account, and the note unless someone chose to include it.
	for _, private := range []string{"<h2>Bank reconciliation</h2>", "Original row", "Bank note:",
		"split with Ann", "PRIMARY SHARE"} {
		if strings.Contains(body, private) {
			t.Errorf("the payee sees bank text %q", private)
		}
	}
}

// An administrator without the permission sees the marks but not the history.
func TestTabHistoryNeedsThePermission(t *testing.T) {
	h := scenario(t)
	ins := paymentOn(t, h, "Insurance", 119000)
	h.confirmPair(t, lineByRow(t, h, 4).ID, ins.Seq, false)
	h.setReconcile(1, false)
	_, body := h.get(tabPath(ins.TabID))
	if !strings.Contains(body, "matched to bank: Sep 2") {
		t.Error("marks hidden from an administrator")
	}
	if strings.Contains(body, "<h2>Bank reconciliation</h2>") || strings.Contains(body, "Original row") {
		t.Error("the history shows without the permission")
	}
}

// Once a match is unmade, the mark goes; the holder's history keeps it, marked.
func TestUnmadeMatchLosesItsMark(t *testing.T) {
	h := scenario(t)
	ins := paymentOn(t, h, "Insurance", 119000)
	h.confirmPair(t, lineByRow(t, h, 4).ID, ins.Seq, false)
	ms, _ := h.db.ListBankMatches(t.Context(), storeFilterTab(ins.TabID))
	h.post("/admin/reconcile/matches/"+itoa(ms[0].ID)+"/undo", url.Values{"csrf_token": {h.csrfToken("/admin/reconcile")}})
	_, body := h.get(tabPath(ins.TabID))
	if strings.Contains(body, "matched to bank:") {
		t.Error("an unmade match still marks the payment")
	}
	if !strings.Contains(body, ">unmade<") || !strings.Contains(body, "undone from Reconciliation") {
		t.Error("the holder's history lost the unmade match or its reason")
	}
}

func storeFilterTab(id int64) store.BankMatchFilter { return store.BankMatchFilter{TabID: id} }
