package sqlite

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/johnzastrow/bitt/internal/fee"
	"github.com/johnzastrow/bitt/internal/money"
	"github.com/johnzastrow/bitt/internal/schedule"
	"github.com/johnzastrow/bitt/internal/store"
)

func feeClaim(tabID int64, key string, base int64) store.PostedFee {
	return store.PostedFee{
		TabID: tabID, Key: key, AssessedFor: mustDate(key), Base: money.Cents(base),
	}
}

func mustDate(s string) schedule.Date {
	d, err := schedule.ParseDate(s)
	if err != nil {
		panic(err)
	}
	return d
}

func feeEntry(tabID, actor int64, key string, amount int64) store.NewEntry {
	return store.NewEntry{
		TabID:          tabID,
		Kind:           store.KindFee,
		Amount:         money.Cents(-amount),
		ActorUserID:    actor,
		IdempotencyKey: "fee:" + key,
		EffectiveAt:    time.Now().UTC(),
	}
}

// A fee policy survives the round trip through its five columns.
func TestFeePolicyRoundTrips(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	user := mustUser(t, db, "a@example.com")

	cases := []fee.Policy{
		{Kind: fee.Fixed, Fixed: 2500, GraceDays: 5},
		{Kind: fee.Percent, PercentBP: 250, GraceDays: 3, Cap: 10000},
	}
	for _, want := range cases {
		t.Run(string(want.Kind), func(t *testing.T) {
			tab, err := db.CreateTab(ctx, store.Tab{
				Name: "Loan", Kind: store.TabPayoff, CreatedBy: user.ID, Fee: want,
			}, nil)
			if err != nil {
				t.Fatalf("create tab: %v", err)
			}
			got, err := db.GetTab(ctx, tab.ID)
			if err != nil {
				t.Fatalf("get tab: %v", err)
			}
			if got.Fee != want {
				t.Errorf("fee policy round-tripped as %+v, want %+v", got.Fee, want)
			}
		})
	}
}

func TestSetFeePolicy(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	user := mustUser(t, db, "a@example.com")
	tab := mustTab(t, db, user.ID)

	want := fee.Policy{Kind: fee.Percent, PercentBP: 500, GraceDays: 7, Cap: 20000}
	if err := db.SetFeePolicy(ctx, tab.ID, want); err != nil {
		t.Fatalf("set fee policy: %v", err)
	}
	if got, _ := db.GetTab(ctx, tab.ID); got.Fee != want {
		t.Errorf("fee policy is %+v, want %+v", got.Fee, want)
	}

	// Clearing.
	if err := db.SetFeePolicy(ctx, tab.ID, fee.Policy{}); err != nil {
		t.Fatalf("clear fee policy: %v", err)
	}
	if got, _ := db.GetTab(ctx, tab.ID); got.Fee.Set() {
		t.Errorf("fee policy still set after clearing: %+v", got.Fee)
	}

	// A malformed policy is refused.
	if err := db.SetFeePolicy(ctx, tab.ID, fee.Policy{Kind: fee.Fixed, Fixed: 0}); err == nil {
		t.Error("a fixed fee of zero was accepted")
	}
	// A missing tab is a miss.
	if err := db.SetFeePolicy(ctx, 9999, want); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("set fee policy on a missing tab returned %v, want ErrNotFound", err)
	}
}

func TestPostFeeEntryClaimsDate(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	user := mustUser(t, db, "a@example.com")
	tab := mustTab(t, db, user.ID)

	entry, replayed, err := db.PostFeeEntry(ctx,
		feeClaim(tab.ID, "2026-01-01", 25000), feeEntry(tab.ID, user.ID, "2026-01-01", 2500))
	if err != nil {
		t.Fatalf("post fee: %v", err)
	}
	if replayed {
		t.Error("the first fee reported itself replayed")
	}
	if entry.Kind != store.KindFee || entry.Amount != -2500 {
		t.Errorf("fee entry %+v, want a -25.00 fee", entry)
	}

	claims, err := db.ListPostedFees(ctx, tab.ID)
	if err != nil {
		t.Fatalf("list posted fees: %v", err)
	}
	if len(claims) != 1 || claims[0].Base != 25000 {
		t.Fatalf("claims %+v, want one with base $250", claims)
	}
}

// Re-assessing a claimed date returns the original and posts no second fee.
func TestPostFeeEntryIsIdempotent(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	user := mustUser(t, db, "a@example.com")
	tab := mustTab(t, db, user.ID)

	first, _, err := db.PostFeeEntry(ctx,
		feeClaim(tab.ID, "2026-01-01", 25000), feeEntry(tab.ID, user.ID, "2026-01-01", 2500))
	if err != nil {
		t.Fatalf("first: %v", err)
	}

	second := feeEntry(tab.ID, user.ID, "2026-01-01", 2500)
	second.IdempotencyKey = "different-key"
	got, replayed, err := db.PostFeeEntry(ctx, feeClaim(tab.ID, "2026-01-01", 25000), second)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if !replayed {
		t.Error("re-assessing a claimed date did not report replayed")
	}
	if got.Seq != first.Seq {
		t.Errorf("replay returned entry %d, want %d", got.Seq, first.Seq)
	}

	balance, _ := db.SumEntries(ctx, tab.ID)
	if balance != -2500 {
		t.Errorf("balance %s, want a single -25.00 fee", balance)
	}
}

// A fee entry must be a fee: the method rejects any other kind, so a period
// charge cannot be mislabeled through this path.
func TestPostFeeEntryRejectsNonFee(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	user := mustUser(t, db, "a@example.com")
	tab := mustTab(t, db, user.ID)

	e := feeEntry(tab.ID, user.ID, "2026-01-01", 2500)
	e.Kind = store.KindCharge
	if _, _, err := db.PostFeeEntry(ctx, feeClaim(tab.ID, "2026-01-01", 25000), e); err == nil {
		t.Error("PostFeeEntry accepted a non-fee entry")
	}
}

// Fee claims are append-only, like period claims and the ledger itself.
func TestPostedFeesAreAppendOnly(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	user := mustUser(t, db, "a@example.com")
	tab := mustTab(t, db, user.ID)

	if _, _, err := db.PostFeeEntry(ctx,
		feeClaim(tab.ID, "2026-01-01", 25000), feeEntry(tab.ID, user.ID, "2026-01-01", 2500)); err != nil {
		t.Fatalf("post fee: %v", err)
	}

	for _, sql := range []string{
		`UPDATE posted_fees SET entry_seq = 999 WHERE tab_id = ?`,
		`DELETE FROM posted_fees WHERE tab_id = ?`,
	} {
		_, err := db.db.ExecContext(ctx, sql, tab.ID)
		if err == nil {
			t.Errorf("%q succeeded; the append-only trigger is not active", sql)
			continue
		}
		if !strings.Contains(err.Error(), "append-only") {
			t.Errorf("%q blocked by an unexpected error: %v", sql, err)
		}
	}
}

// Disabling the append-only triggers must also drop the posted_fees ones, or a
// development database would still refuse fee repairs.
func TestFeeTriggersDropWhenDisabled(t *testing.T) {
	db, err := Open(Options{Path: t.TempDir() + "/notrig.db", AppendOnlyTriggers: false})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	var count int
	if err := db.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sqlite_master WHERE type='trigger' AND name LIKE 'posted_fees_%'`).Scan(&count); err != nil {
		t.Fatalf("count triggers: %v", err)
	}
	if count != 0 {
		t.Errorf("%d posted_fees triggers remain with enforcement disabled, want 0", count)
	}
}

func TestInterestRateRoundTripAndClaims(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	user := mustUser(t, db, "a@example.com")
	tab := mustTab(t, db, user.ID)

	if err := db.SetInterestRate(ctx, tab.ID, 650); err != nil {
		t.Fatalf("set interest: %v", err)
	}
	if got, _ := db.GetTab(ctx, tab.ID); got.InterestAPRBp != 650 {
		t.Errorf("interest rate is %d, want 650", got.InterestAPRBp)
	}
	if err := db.SetInterestRate(ctx, 9999, 100); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("set interest on a missing tab = %v, want ErrNotFound", err)
	}

	// PostInterestEntry claims a period and rejects a second claim / non-interest.
	in := store.PostedInterest{TabID: tab.ID, Key: "2026-01-01", AccruedFor: mustDate("2026-01-01"), Base: 500000}
	e := store.NewEntry{TabID: tab.ID, Kind: store.KindCharge, Category: store.CategoryInterest,
		Amount: -2500, ActorUserID: user.ID, IdempotencyKey: "interest:1", EffectiveAt: time.Now().UTC()}
	first, replayed, err := db.PostInterestEntry(ctx, in, e)
	if err != nil || replayed {
		t.Fatalf("post interest: %v replayed=%v", err, replayed)
	}
	e2 := e
	e2.IdempotencyKey = "different"
	got, replayed, err := db.PostInterestEntry(ctx, in, e2)
	if err != nil || !replayed || got.Seq != first.Seq {
		t.Errorf("re-claim = seq %d replayed %v err %v, want the original replayed", got.Seq, replayed, err)
	}
	// A non-interest entry is refused through this path.
	bad := e
	bad.Category = store.CategoryNone
	bad.IdempotencyKey = "non-interest"
	if _, _, err := db.PostInterestEntry(ctx, store.PostedInterest{TabID: tab.ID, Key: "2026-02-01", AccruedFor: mustDate("2026-02-01")}, bad); err == nil {
		t.Error("PostInterestEntry accepted a non-interest entry")
	}

	// Append-only.
	if _, err := db.db.ExecContext(ctx, `DELETE FROM posted_interest WHERE tab_id = ?`, tab.ID); err == nil {
		t.Error("DELETE on posted_interest succeeded")
	}
}
