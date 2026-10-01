package sqldb

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/johnzastrow/bitt/internal/money"
	"github.com/johnzastrow/bitt/internal/reconcile"
	"github.com/johnzastrow/bitt/internal/store"
)

// RECON-03 storage: setup controls, ignore rules, and the reads behind
// suggestions. Both backends.

func TestReconcileSettingsDefaultsAndUpdate(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	admin := mustAdmin(t, db, "admin@example.com")

	got, err := db.GetReconcileSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got.Settings != reconcile.Defaults() || got.UpdatedBy != 0 || !got.UpdatedAt.IsZero() {
		t.Errorf("defaults from the migration = %+v, want %+v", got, reconcile.Defaults())
	}

	s := reconcile.Settings{DateWindowDays: 3, TolerancePercent: 5, ToleranceCap: 1000,
		ToleranceFloor: 50, MinLineAmount: 0, UseNames: false, DateFlagDays: 0}
	for i := 0; i < 2; i++ { // the second save is a no-op on the values
		if err := db.SetReconcileSettings(ctx, s, admin.ID); err != nil {
			t.Fatalf("save #%d: %v", i+1, err)
		}
	}
	got, _ = db.GetReconcileSettings(ctx)
	if got.Settings != s || got.UpdatedBy != admin.ID || got.UpdatedByName != admin.DisplayName || got.UpdatedAt.IsZero() {
		t.Errorf("after save = %+v", got)
	}

	// The schema refuses what Validate would, even if a handler forgot it.
	bad := []reconcile.Settings{
		{DateWindowDays: 61, TolerancePercent: 10, ToleranceCap: 2500, ToleranceFloor: 100},
		{DateWindowDays: 7, TolerancePercent: 101, ToleranceCap: 2500, ToleranceFloor: 100},
		{DateWindowDays: 7, TolerancePercent: 10, ToleranceCap: 50, ToleranceFloor: 100},
		{DateWindowDays: 7, TolerancePercent: 10, ToleranceCap: 2500, ToleranceFloor: -1},
		{DateWindowDays: 7, TolerancePercent: 10, ToleranceCap: 2500, ToleranceFloor: 100, MinLineAmount: 1_000_001},
		{DateWindowDays: 7, TolerancePercent: 10, ToleranceCap: 2500, ToleranceFloor: 100, DateFlagDays: -1},
	}
	for _, b := range bad {
		if err := db.SetReconcileSettings(ctx, b, admin.ID); err == nil {
			t.Errorf("schema accepted %+v", b)
		}
	}
	if got, _ := db.GetReconcileSettings(ctx); got.Settings != s {
		t.Error("a refused save changed the settings")
	}
}

func TestIgnoreRules(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	admin := mustAdmin(t, db, "admin@example.com")
	f := mustFormat(t, db, admin.ID, "h")
	other := mustFormat(t, db, admin.ID, "other")

	div1 := line(5, "div1", 4)
	div1.Description = "Deposit Dividend\nANNUAL PERCENTAGE YIELD"
	div2 := line(11, "div2", 12)
	div2.Description = "DEPOSIT DIVIDEND"
	transfer := line(2, "t", 15000)
	if _, err := db.SaveBankImport(ctx, store.BankImport{FormatID: f.ID, FileName: "a.csv",
		FileSHA256: fp("a"), UploadedBy: admin.ID}, []store.BankLine{transfer, div1, div2}); err != nil {
		t.Fatal(err)
	}
	// The same description through another layout is not touched by f's rule.
	otherDiv := line(2, "otherdiv", 4)
	otherDiv.Description = "Deposit Dividend"
	if _, err := db.SaveBankImport(ctx, store.BankImport{FormatID: other.ID, FileName: "b.csv",
		FileSHA256: fp("b"), UploadedBy: admin.ID}, []store.BankLine{otherDiv}); err != nil {
		t.Fatal(err)
	}

	r, n, err := db.AddIgnoreRule(ctx, store.IgnoreRule{FormatID: f.ID, Contains: "Deposit Dividend", CreatedBy: admin.ID})
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("rule ignored %d open lines, want 2 (case-insensitive)", n)
	}
	open, err := db.ListOpenBankLines(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 2 {
		t.Fatalf("%d open lines, want the transfer and the other layout's dividend", len(open))
	}
	for _, l := range open {
		if l.Description == "DEPOSIT DIVIDEND" || l.Description == div1.Description {
			t.Errorf("a matching line is still open: %+v", l)
		}
	}

	rules, err := db.ListIgnoreRules(ctx)
	if err != nil || len(rules) != 1 || rules[0].ID != r.ID || rules[0].FormatName != "Credit union" ||
		rules[0].Contains != "Deposit Dividend" || rules[0].CreatedAt.IsZero() {
		t.Errorf("rules = %+v, %v", rules, err)
	}

	// The ignored lines carry the rule as their reason.
	imps, _ := db.ListBankImports(ctx, 10)
	var aID int64
	for _, i := range imps {
		if i.FileName == "a.csv" {
			aID = i.ID
		}
	}
	ls, _ := db.ListBankLines(ctx, aID)
	ignored := 0
	for _, l := range ls {
		if l.State == store.BankLineIgnored {
			ignored++
			if l.IgnoreReason != `description contains "Deposit Dividend"` {
				t.Errorf("reason = %q", l.IgnoreReason)
			}
		}
	}
	if ignored != 2 {
		t.Errorf("%d ignored lines, want 2", ignored)
	}

	// The same rule twice is a conflict; an empty one is refused.
	if _, _, err := db.AddIgnoreRule(ctx, store.IgnoreRule{FormatID: f.ID, Contains: "Deposit Dividend", CreatedBy: admin.ID}); !errors.Is(err, store.ErrConflict) {
		t.Errorf("duplicate rule: %v", err)
	}
	if _, _, err := db.AddIgnoreRule(ctx, store.IgnoreRule{FormatID: f.ID, Contains: "", CreatedBy: admin.ID}); err == nil {
		t.Error("empty rule accepted")
	}

	// Deleting a rule leaves its lines ignored.
	if err := db.DeleteIgnoreRule(ctx, r.ID); err != nil {
		t.Fatal(err)
	}
	if open, _ := db.ListOpenBankLines(ctx); len(open) != 2 {
		t.Errorf("deleting the rule reopened lines: %d open", len(open))
	}
	if err := db.DeleteIgnoreRule(ctx, r.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("deleting twice: %v", err)
	}
}

func TestIgnoreRuleMatches(t *testing.T) {
	r := store.IgnoreRule{Contains: "Deposit Dividend"}
	for desc, want := range map[string]bool{
		"Deposit Dividend\nANNUAL": true,
		"deposit dividend":         true,
		"Deposit Transfer":         false,
		"XDEPOSIT DIVIDENDX":       true, // "contains", as the spec says
		"Deposit\nDividend":        false,
	} {
		if got := r.Matches(desc); got != want {
			t.Errorf("Matches(%q) = %v", desc, got)
		}
	}
}

func TestListPaymentCandidates(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	jane := mustAdmin(t, db, "jane@example.com")
	sam := mustUser(t, db, "sam@example.com")
	tab := mustTab(t, db, jane.ID)
	if err := db.AddParticipant(ctx, store.Participant{TabID: tab.ID, UserID: sam.ID, Role: store.RolePayee}); err != nil {
		t.Fatal(err)
	}

	at := func(d int) time.Time { return time.Date(2026, 9, d, 12, 0, 0, 0, time.UTC) }
	post := func(key string, kind store.EntryKind, cents money.Cents, d int, method store.PaymentMethod, rev *int64) store.Entry {
		t.Helper()
		e, _, err := db.PostEntry(ctx, store.NewEntry{TabID: tab.ID, Kind: kind, Amount: cents,
			EffectiveAt: at(d), ActorUserID: sam.ID, IdempotencyKey: key, Method: method, ReversesSeq: rev})
		if err != nil {
			t.Fatalf("post %s: %v", key, err)
		}
		return e
	}
	post("charge", store.KindCharge, -7500, 1, "", nil)
	kept := post("p1", store.KindPayment, 5000, 3, store.MethodTransfer, nil)
	undone := post("p2", store.KindPayment, 2500, 4, store.MethodCash, nil)
	post("undo", store.KindReversal, -2500, 4, "", &undone.Seq)
	post("adj", store.KindAdjustment, 300, 5, "", nil)
	post("late", store.KindPayment, 9900, 28, store.MethodOther, nil)

	got, err := db.ListPaymentCandidates(ctx, at(1).Add(-12*time.Hour), at(20))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("%d candidates, want only the unreversed payment in range: %+v", len(got), got)
	}
	p := got[0]
	if p.Seq != kept.Seq || p.Amount != 5000 || p.Method != store.MethodTransfer || p.TabName != "Phone plan" ||
		p.ActorName != sam.DisplayName || !p.EffectiveAt.Equal(at(3)) {
		t.Errorf("candidate = %+v", p)
	}
	if len(p.Participants) != 2 {
		t.Errorf("participants = %v", p.Participants)
	}

	// The range is [from, to): the late payment is in only when to passes it.
	if got, _ := db.ListPaymentCandidates(ctx, at(1), at(28)); len(got) != 1 {
		t.Errorf("to is not exclusive: %d", len(got))
	}
	if got, _ := db.ListPaymentCandidates(ctx, at(1), at(29)); len(got) != 2 {
		t.Errorf("late payment not included: %d", len(got))
	}
	if got, _ := db.ListPaymentCandidates(ctx, at(29), at(30)); len(got) != 0 {
		t.Errorf("empty range returned %d", len(got))
	}
}
