package sqldb

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/johnzastrow/bitt/internal/money"
	"github.com/johnzastrow/bitt/internal/store"
)

// Payments with no bank transaction (migration 0018).

// lockPayment locks a payment's entry row and checks it can be decided on:
// a standing payment, not posted by reconciliation, with no standing match.
// Confirming a match takes the same lock, so a payment is never both matched
// and set aside. The lock reads the row; it never writes it, so the
// append-only triggers are not involved.
func (d *DB) lockPayment(ctx context.Context, tx *sql.Tx, seq int64) (int64, error) {
	var (
		kind  string
		tabID int64
		key   string
	)
	if err := tx.QueryRowContext(ctx,
		`SELECT kind, tab_id, idempotency_key FROM entries WHERE seq = ?`+d.dialect.lockRows(), seq).
		Scan(&kind, &tabID, &key); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, store.ErrNotAPayment
		}
		return 0, translate(err)
	}
	if kind != string(store.KindPayment) || strings.HasPrefix(key, store.ReconKeyPrefix) {
		return 0, store.ErrNotAPayment
	}
	var n int
	if err := tx.QueryRowContext(ctx,
		`SELECT (SELECT COUNT(*) FROM entries r WHERE r.reverses_seq = ?) +
		        (SELECT COUNT(*) FROM bank_matches m WHERE m.active_entry_seq = ?) +
		        (SELECT COUNT(*) FROM payment_reviews pr WHERE pr.active_entry_seq = ?)`, seq, seq, seq).Scan(&n); err != nil {
		return 0, translate(err)
	}
	if n > 0 {
		return 0, store.ErrPaymentMatched
	}
	return tabID, nil
}

// SetPaymentNotInBank sets a payment aside, with who, when and a note.
func (d *DB) SetPaymentNotInBank(ctx context.Context, seq, by int64, note string) (store.PaymentReview, error) {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return store.PaymentReview{}, fmt.Errorf("begin payment review: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	tabID, err := d.lockPayment(ctx, tx, seq)
	if err != nil {
		return store.PaymentReview{}, err
	}
	res, err := tx.ExecContext(ctx,
		`INSERT INTO payment_reviews (entry_seq, tab_id, note, by_user_id, at, active_entry_seq)
		 VALUES (?, ?, ?, ?, ?, ?)`, seq, tabID, note, by, nowText(), seq)
	if err != nil {
		if err = translate(err); errors.Is(err, store.ErrConflict) {
			return store.PaymentReview{}, store.ErrPaymentMatched
		}
		return store.PaymentReview{}, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return store.PaymentReview{}, fmt.Errorf("payment review id: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return store.PaymentReview{}, fmt.Errorf("commit payment review: %w", err)
	}
	rs, err := d.listPaymentReviews(ctx, `WHERE pr.id = ?`, []any{id}, 1)
	if err != nil || len(rs) == 0 {
		return store.PaymentReview{}, err
	}
	return rs[0], nil
}

// UndoPaymentReview undoes a standing review.
func (d *DB) UndoPaymentReview(ctx context.Context, reviewID, by int64) error {
	res, err := d.db.ExecContext(ctx,
		`UPDATE payment_reviews SET undone_at = ?, undone_by = ?, active_entry_seq = NULL
		  WHERE id = ? AND undone_at = ''`, nowText(), by, reviewID)
	if err != nil {
		return translate(err)
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		var exists int
		if err := d.db.QueryRowContext(ctx, `SELECT 1 FROM payment_reviews WHERE id = ?`, reviewID).Scan(&exists); err != nil {
			return translate(err)
		}
		return store.ErrMatchUndone
	}
	return nil
}

// ListPaymentReviews returns reviews newest first.
func (d *DB) ListPaymentReviews(ctx context.Context, tabID int64, limit int) ([]store.PaymentReview, error) {
	if tabID != 0 {
		return d.listPaymentReviews(ctx, `WHERE pr.tab_id = ?`, []any{tabID}, limit)
	}
	return d.listPaymentReviews(ctx, ``, nil, limit)
}

func (d *DB) listPaymentReviews(ctx context.Context, where string, args []any, limit int) ([]store.PaymentReview, error) {
	if limit <= 0 || limit > 500 {
		limit = 500
	}
	rows, err := d.db.QueryContext(ctx,
		`SELECT pr.id, pr.entry_seq, pr.tab_id, t.name, e.amount_cents, e.effective_at, pr.note,
		        pr.by_user_id, u.display_name, pr.at, pr.undone_at, uu.display_name
		   FROM payment_reviews pr
		   JOIN entries e ON e.seq = pr.entry_seq
		   JOIN tabs t ON t.id = pr.tab_id
		   JOIN users u ON u.id = pr.by_user_id
		   LEFT JOIN users uu ON uu.id = pr.undone_by `+where+`
		  ORDER BY pr.id DESC LIMIT ?`, append(args, limit)...)
	if err != nil {
		return nil, translate(err)
	}
	defer func() { _ = rows.Close() }()
	var out []store.PaymentReview
	for rows.Next() {
		var (
			r               store.PaymentReview
			cents           int64
			eff, at, undone string
			undoneByName    sql.NullString
		)
		if err := rows.Scan(&r.ID, &r.EntrySeq, &r.TabID, &r.TabName, &cents, &eff, &r.Note,
			&r.By, &r.ByName, &at, &undone, &undoneByName); err != nil {
			return nil, translate(err)
		}
		r.Amount, r.UndoneByName = money.Cents(cents), undoneByName.String
		if r.EffectiveAt, err = parseTime(eff); err != nil {
			return nil, err
		}
		if r.At, err = parseTime(at); err != nil {
			return nil, err
		}
		if undone != "" {
			t, err := parseTime(undone)
			if err != nil {
				return nil, err
			}
			r.UndoneAt = &t
		}
		out = append(out, r)
	}
	return out, translate(rows.Err())
}

// BankCoverage is the date range of all imported lines.
func (d *DB) BankCoverage(ctx context.Context) (string, string, error) {
	var first, last sql.NullString
	err := d.db.QueryRowContext(ctx, `SELECT MIN(posted_on), MAX(posted_on) FROM bank_lines`).Scan(&first, &last)
	return first.String, last.String, translate(err)
}

// ListUnaddressedPayments returns standing payments in [from, to) with no
// standing match, review, or recording from a bank line.
func (d *DB) ListUnaddressedPayments(ctx context.Context, from, to time.Time) ([]store.PaymentCandidate, error) {
	all, err := d.ListPaymentCandidates(ctx, from, to)
	if err != nil {
		return nil, err
	}
	reviewed := map[int64]bool{}
	rows, err := d.db.QueryContext(ctx, `SELECT active_entry_seq FROM payment_reviews WHERE active_entry_seq IS NOT NULL`)
	if err != nil {
		return nil, translate(err)
	}
	for rows.Next() {
		var seq int64
		if err := rows.Scan(&seq); err != nil {
			_ = rows.Close()
			return nil, translate(err)
		}
		reviewed[seq] = true
	}
	_ = rows.Close()
	out := all[:0]
	for _, p := range all {
		if !reviewed[p.Seq] {
			out = append(out, p)
		}
	}
	return out, nil
}
