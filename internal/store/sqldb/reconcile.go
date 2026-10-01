package sqldb

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/johnzastrow/bitt/internal/money"
	"github.com/johnzastrow/bitt/internal/reconcile"
	"github.com/johnzastrow/bitt/internal/store"
)

// Reconciliation setup, ignore rules, and the reads that feed suggestions
// (SPEC-BANK-RECONCILE, RECON-03). Migration 0015.

// GetReconcileSettings reads the single settings row.
func (d *DB) GetReconcileSettings(ctx context.Context) (store.ReconcileSettings, error) {
	var (
		out                  store.ReconcileSettings
		capC, floorC, minC   int64
		useNames             int
		updatedBy            sql.NullInt64
		updatedByName, updAt sql.NullString
	)
	err := d.db.QueryRowContext(ctx,
		`SELECT s.date_window_days, s.tolerance_percent, s.tolerance_cap_cents,
		        s.tolerance_floor_cents, s.min_line_cents, s.use_names, s.date_flag_days,
		        s.updated_by, u.display_name, s.updated_at
		   FROM reconcile_settings s
		   LEFT JOIN users u ON u.id = s.updated_by
		  WHERE s.id = 1`).
		Scan(&out.DateWindowDays, &out.TolerancePercent, &capC, &floorC, &minC, &useNames,
			&out.DateFlagDays, &updatedBy, &updatedByName, &updAt)
	if err != nil {
		return store.ReconcileSettings{}, translate(err)
	}
	out.ToleranceCap, out.ToleranceFloor, out.MinLineAmount = money.Cents(capC), money.Cents(floorC), money.Cents(minC)
	out.UseNames = useNames != 0
	out.UpdatedBy, out.UpdatedByName = updatedBy.Int64, updatedByName.String
	if updAt.String != "" {
		if out.UpdatedAt, err = parseTime(updAt.String); err != nil {
			return store.ReconcileSettings{}, fmt.Errorf("parse reconcile settings updated_at: %w", err)
		}
	}
	return out, nil
}

// SetReconcileSettings replaces the controls and records who changed them.
func (d *DB) SetReconcileSettings(ctx context.Context, s reconcile.Settings, by int64) error {
	res, err := d.db.ExecContext(ctx,
		`UPDATE reconcile_settings
		    SET date_window_days = ?, tolerance_percent = ?, tolerance_cap_cents = ?,
		        tolerance_floor_cents = ?, min_line_cents = ?, use_names = ?, date_flag_days = ?,
		        updated_by = ?, updated_at = ?
		  WHERE id = 1`,
		s.DateWindowDays, s.TolerancePercent, int64(s.ToleranceCap), int64(s.ToleranceFloor),
		int64(s.MinLineAmount), boolToInt(s.UseNames), s.DateFlagDays, by, nowText())
	if err != nil {
		return translate(err)
	}
	// updated_at always changes, so this matches a row even on MariaDB.
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return store.ErrNotFound
	}
	return nil
}

// ListIgnoreRules returns every rule, by layout then text.
func (d *DB) ListIgnoreRules(ctx context.Context) ([]store.IgnoreRule, error) {
	rows, err := d.db.QueryContext(ctx,
		`SELECT r.id, r.format_id, f.name, r.contains, r.created_by, r.created_at
		   FROM bank_ignore_rules r
		   JOIN bank_formats f ON f.id = r.format_id
		  ORDER BY f.name, r.format_id, r.contains`)
	if err != nil {
		return nil, translate(err)
	}
	defer func() { _ = rows.Close() }()
	var out []store.IgnoreRule
	for rows.Next() {
		var (
			r       store.IgnoreRule
			created string
		)
		if err := rows.Scan(&r.ID, &r.FormatID, &r.FormatName, &r.Contains, &r.CreatedBy, &created); err != nil {
			return nil, translate(err)
		}
		if r.CreatedAt, err = parseTime(created); err != nil {
			return nil, fmt.Errorf("parse ignore rule created_at: %w", err)
		}
		out = append(out, r)
	}
	return out, translate(rows.Err())
}

// AddIgnoreRule saves a rule and ignores the layout's open lines it matches,
// in one transaction. The match is done here in Go, the same function the
// import uses, so "matches" means one thing whichever path a line takes.
func (d *DB) AddIgnoreRule(ctx context.Context, r store.IgnoreRule) (store.IgnoreRule, int, error) {
	if r.CreatedAt.IsZero() {
		r.CreatedAt = time.Now().UTC()
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return store.IgnoreRule{}, 0, fmt.Errorf("begin ignore rule: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx,
		`INSERT INTO bank_ignore_rules (format_id, contains, created_by, created_at) VALUES (?, ?, ?, ?)`,
		r.FormatID, r.Contains, r.CreatedBy, toText(r.CreatedAt))
	if err != nil {
		return store.IgnoreRule{}, 0, translate(err)
	}
	if r.ID, err = res.LastInsertId(); err != nil {
		return store.IgnoreRule{}, 0, fmt.Errorf("ignore rule id: %w", err)
	}

	rows, err := tx.QueryContext(ctx,
		`SELECT l.id, l.description
		   FROM bank_lines l
		   JOIN bank_imports i ON i.id = l.import_id
		  WHERE i.format_id = ? AND l.state = 'open'`+d.dialect.lockRows(), r.FormatID)
	if err != nil {
		return store.IgnoreRule{}, 0, translate(err)
	}
	var ids []int64
	for rows.Next() {
		var (
			id   int64
			desc string
		)
		if err := rows.Scan(&id, &desc); err != nil {
			_ = rows.Close()
			return store.IgnoreRule{}, 0, translate(err)
		}
		if r.Matches(desc) {
			ids = append(ids, id)
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return store.IgnoreRule{}, 0, translate(err)
	}
	_ = rows.Close() // before the writes: SQLite has one connection

	for _, id := range ids {
		if _, err := tx.ExecContext(ctx,
			`UPDATE bank_lines SET state = 'ignored', ignore_reason = ? WHERE id = ? AND state = 'open'`,
			r.Reason(), id); err != nil {
			return store.IgnoreRule{}, 0, translate(err)
		}
	}
	if err := tx.Commit(); err != nil {
		return store.IgnoreRule{}, 0, fmt.Errorf("commit ignore rule: %w", err)
	}
	return r, len(ids), nil
}

// DeleteIgnoreRule removes a rule. Lines it ignored keep their state.
func (d *DB) DeleteIgnoreRule(ctx context.Context, id int64) error {
	res, err := d.db.ExecContext(ctx, `DELETE FROM bank_ignore_rules WHERE id = ?`, id)
	if err != nil {
		return translate(err)
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return store.ErrNotFound
	}
	return nil
}

// ListOpenBankLines returns every open line, oldest first.
func (d *DB) ListOpenBankLines(ctx context.Context) ([]store.OpenBankLine, error) {
	rows, err := d.db.QueryContext(ctx,
		`SELECT l.id, l.import_id, l.file_row, l.original_text, l.account, l.posted_on,
		        l.amount_cents, l.description, l.note, l.reference, l.fingerprint, l.state,
		        l.ignore_reason, i.format_id, i.file_name
		   FROM bank_lines l
		   JOIN bank_imports i ON i.id = l.import_id
		  WHERE l.state = 'open'
		  ORDER BY l.posted_on, l.id`)
	if err != nil {
		return nil, translate(err)
	}
	defer func() { _ = rows.Close() }()
	var out []store.OpenBankLine
	for rows.Next() {
		var (
			l     store.OpenBankLine
			cents int64
			state string
		)
		if err := rows.Scan(&l.ID, &l.ImportID, &l.Row, &l.Raw, &l.Account, &l.PostedOn, &cents,
			&l.Description, &l.Note, &l.Reference, &l.Fingerprint, &state, &l.IgnoreReason,
			&l.FormatID, &l.FileName); err != nil {
			return nil, translate(err)
		}
		l.Amount, l.State = money.Cents(cents), store.BankLineState(state)
		out = append(out, l)
	}
	return out, translate(rows.Err())
}

// ListPaymentCandidates returns unreversed payments effective in [from, to).
//
// "Unreversed" is a NOT EXISTS against the reversal that points at the
// payment; reversals are themselves kind 'reversal', so they never appear.
// Matched payments are excluded once matches exist (RECON-04).
func (d *DB) ListPaymentCandidates(ctx context.Context, from, to time.Time) ([]store.PaymentCandidate, error) {
	rows, err := d.db.QueryContext(ctx,
		`SELECT e.seq, e.tab_id, t.name, e.amount_cents, e.method, e.effective_at, e.memo,
		        e.actor_user_id, u.display_name
		   FROM entries e
		   JOIN tabs t ON t.id = e.tab_id
		   JOIN users u ON u.id = e.actor_user_id
		  WHERE e.kind = 'payment'
		    AND e.effective_at >= ? AND e.effective_at < ?
		    AND NOT EXISTS (SELECT 1 FROM entries r WHERE r.reverses_seq = e.seq)
		  ORDER BY e.effective_at, e.seq`, toText(from), toText(to))
	if err != nil {
		return nil, translate(err)
	}
	var out []store.PaymentCandidate
	tabs := map[int64]bool{}
	for rows.Next() {
		var (
			p         store.PaymentCandidate
			cents     int64
			method    string
			effective string
		)
		if err := rows.Scan(&p.Seq, &p.TabID, &p.TabName, &cents, &method, &effective, &p.Memo,
			&p.ActorUserID, &p.ActorName); err != nil {
			_ = rows.Close()
			return nil, translate(err)
		}
		p.Amount, p.Method = money.Cents(cents), store.PaymentMethod(method)
		if p.EffectiveAt, err = parseTime(effective); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("parse payment effective_at: %w", err)
		}
		tabs[p.TabID] = true
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, translate(err)
	}
	_ = rows.Close() // before the next query: SQLite has one connection

	names := map[int64][]string{}
	for tabID := range tabs {
		ps, err := d.ListParticipants(ctx, tabID)
		if err != nil {
			return nil, err
		}
		for _, p := range ps {
			names[tabID] = append(names[tabID], p.DisplayName)
		}
	}
	for i := range out {
		out[i].Participants = names[out[i].TabID]
	}
	return out, nil
}
