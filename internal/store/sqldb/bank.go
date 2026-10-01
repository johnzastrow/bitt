package sqldb

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/johnzastrow/bitt/internal/money"
	"github.com/johnzastrow/bitt/internal/store"
)

// Bank statement imports (SPEC-BANK-RECONCILE, RECON-02). Migration 0014.

const bankFormatColumns = `id, name, header_signature, header_text, date_col, date_layout,
	amount_col, debit_col, credit_col, incoming_negative, decimal_comma,
	description_col, account_col, note_col, reference_col, created_by, created_at`

func scanBankFormat(row interface{ Scan(...any) error }) (store.BankFormat, error) {
	var (
		f            store.BankFormat
		neg, decComm int
		created      string
	)
	m := &f.Mapping
	if err := row.Scan(&f.ID, &f.Name, &f.Signature, &f.HeaderText, &m.Date, &m.DateLayout,
		&m.Amount, &m.Debit, &m.Credit, &neg, &decComm,
		&m.Description, &m.Account, &m.Note, &m.Reference, &f.CreatedBy, &created); err != nil {
		return store.BankFormat{}, translate(err)
	}
	m.IncomingNegative, m.DecimalComma = neg != 0, decComm != 0
	var err error
	if f.CreatedAt, err = parseTime(created); err != nil {
		return store.BankFormat{}, fmt.Errorf("parse bank format created_at: %w", err)
	}
	return f, nil
}

// BankFormatBySignature finds the saved mapping for a header.
func (d *DB) BankFormatBySignature(ctx context.Context, signature string) (store.BankFormat, error) {
	return scanBankFormat(d.db.QueryRowContext(ctx,
		`SELECT `+bankFormatColumns+` FROM bank_formats WHERE header_signature = ?`, signature))
}

// GetBankFormat reads one saved mapping.
func (d *DB) GetBankFormat(ctx context.Context, id int64) (store.BankFormat, error) {
	return scanBankFormat(d.db.QueryRowContext(ctx,
		`SELECT `+bankFormatColumns+` FROM bank_formats WHERE id = ?`, id))
}

// CreateBankFormat saves a mapping; the signature is UNIQUE, so a second save
// of the same layout is ErrConflict rather than a second mapping.
func (d *DB) CreateBankFormat(ctx context.Context, f store.BankFormat) (store.BankFormat, error) {
	if f.CreatedAt.IsZero() {
		f.CreatedAt = time.Now().UTC()
	}
	m := f.Mapping
	res, err := d.db.ExecContext(ctx,
		`INSERT INTO bank_formats (name, header_signature, header_text, date_col, date_layout,
		        amount_col, debit_col, credit_col, incoming_negative, decimal_comma,
		        description_col, account_col, note_col, reference_col, created_by, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		f.Name, f.Signature, f.HeaderText, m.Date, m.DateLayout,
		m.Amount, m.Debit, m.Credit, boolToInt(m.IncomingNegative), boolToInt(m.DecimalComma),
		m.Description, m.Account, m.Note, m.Reference, f.CreatedBy, toText(f.CreatedAt))
	if err != nil {
		return store.BankFormat{}, translate(err)
	}
	if f.ID, err = res.LastInsertId(); err != nil {
		return store.BankFormat{}, fmt.Errorf("bank format id: %w", err)
	}
	return f, nil
}

// ListBankFormats returns every saved mapping, by name.
func (d *DB) ListBankFormats(ctx context.Context) ([]store.BankFormat, error) {
	rows, err := d.db.QueryContext(ctx,
		`SELECT `+bankFormatColumns+` FROM bank_formats ORDER BY name, id`)
	if err != nil {
		return nil, translate(err)
	}
	defer func() { _ = rows.Close() }()
	var out []store.BankFormat
	for rows.Next() {
		f, err := scanBankFormat(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, translate(rows.Err())
}

// SaveBankImport stores an import and its lines in one transaction.
//
// Duplicates are decided by the UNIQUE fingerprint, not by the pre-check
// alone: the SELECT is only so the common case does not raise and catch an
// error per line. When two uploads of overlapping files race, the second
// INSERT of a fingerprint waits on the first transaction's key and then fails
// as a duplicate, and is counted as one. A failed INSERT rolls back only that
// statement on both backends, so the transaction carries on.
//
// Imports of one layout are serialized by a lock on the layout row (below), so
// the race above is a second line of defence. A deadlock InnoDB still reports
// rolls the transaction back whole, and it is safe to run again from the top.
func (d *DB) SaveBankImport(ctx context.Context, imp store.BankImport, lines []store.BankLine) (store.BankImport, error) {
	var (
		out store.BankImport
		err error
	)
	for attempt := 0; attempt < 4; attempt++ {
		out, err = d.saveBankImport(ctx, imp, lines)
		if !isDeadlock(err) {
			return out, err
		}
	}
	return out, err
}

func (d *DB) saveBankImport(ctx context.Context, imp store.BankImport, lines []store.BankLine) (store.BankImport, error) {
	if imp.UploadedAt.IsZero() {
		imp.UploadedAt = time.Now().UTC()
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return store.BankImport{}, fmt.Errorf("begin bank import: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Serialize imports of one layout. Only they can share fingerprints (the
	// layout is part of the key), and two of them inserting the same keys in
	// different orders deadlock on MariaDB, repeatedly enough that retrying
	// alone does not converge. Locking the layout row first makes them queue
	// instead; imports of other layouts are unaffected. On SQLite the single
	// writer already serializes, and lockRows() is empty.
	var formatID int64
	if err := tx.QueryRowContext(ctx,
		`SELECT id FROM bank_formats WHERE id = ?`+d.dialect.lockRows(), imp.FormatID).
		Scan(&formatID); err != nil {
		return store.BankImport{}, translate(err)
	}

	res, err := tx.ExecContext(ctx,
		`INSERT INTO bank_imports (format_id, file_name, file_sha256, uploaded_by, uploaded_at,
		        kept, duplicates, outgoing, ignored, refused, first_date, last_date, refused_detail)
		 VALUES (?, ?, ?, ?, ?, 0, 0, ?, 0, ?, ?, ?, ?)`,
		imp.FormatID, imp.FileName, imp.FileSHA256, imp.UploadedBy, toText(imp.UploadedAt),
		imp.Outgoing, imp.Refused, imp.FirstDate, imp.LastDate, imp.RefusedDetail)
	if err != nil {
		return store.BankImport{}, translate(err)
	}
	if imp.ID, err = res.LastInsertId(); err != nil {
		return store.BankImport{}, fmt.Errorf("bank import id: %w", err)
	}

	imp.Kept, imp.Duplicates, imp.Ignored = 0, 0, 0
	for _, l := range lines {
		var one int
		err := tx.QueryRowContext(ctx,
			`SELECT 1 FROM bank_lines WHERE fingerprint = ?`, l.Fingerprint).Scan(&one)
		switch {
		case err == nil:
			imp.Duplicates++
			continue
		case !errors.Is(err, sql.ErrNoRows):
			return store.BankImport{}, translate(err)
		}

		state := l.State
		if state == "" {
			state = store.BankLineOpen
		}
		_, err = tx.ExecContext(ctx,
			`INSERT INTO bank_lines (import_id, file_row, original_text, account, posted_on,
			        amount_cents, description, note, reference, fingerprint, state, ignore_reason)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			imp.ID, l.Row, l.Raw, l.Account, l.PostedOn, int64(l.Amount),
			l.Description, l.Note, l.Reference, l.Fingerprint, string(state), l.IgnoreReason)
		switch err = translate(err); {
		case errors.Is(err, store.ErrConflict):
			imp.Duplicates++
		case err != nil:
			return store.BankImport{}, err
		case state == store.BankLineIgnored:
			imp.Ignored++
		default:
			imp.Kept++
		}
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE bank_imports SET kept = ?, duplicates = ?, ignored = ? WHERE id = ?`,
		imp.Kept, imp.Duplicates, imp.Ignored, imp.ID); err != nil {
		return store.BankImport{}, translate(err)
	}
	if err := tx.Commit(); err != nil {
		return store.BankImport{}, fmt.Errorf("commit bank import: %w", err)
	}
	return imp, nil
}

const bankImportSelect = `SELECT i.id, i.format_id, f.name, i.file_name, i.file_sha256,
	        i.uploaded_by, u.display_name, i.uploaded_at, i.kept, i.duplicates, i.outgoing,
	        i.ignored, i.refused, i.first_date, i.last_date, i.refused_detail
	   FROM bank_imports i
	   JOIN bank_formats f ON f.id = i.format_id
	   JOIN users u ON u.id = i.uploaded_by`

func scanBankImport(row interface{ Scan(...any) error }) (store.BankImport, error) {
	var (
		i        store.BankImport
		uploaded string
	)
	if err := row.Scan(&i.ID, &i.FormatID, &i.FormatName, &i.FileName, &i.FileSHA256,
		&i.UploadedBy, &i.UploaderName, &uploaded, &i.Kept, &i.Duplicates, &i.Outgoing,
		&i.Ignored, &i.Refused, &i.FirstDate, &i.LastDate, &i.RefusedDetail); err != nil {
		return store.BankImport{}, translate(err)
	}
	var err error
	if i.UploadedAt, err = parseTime(uploaded); err != nil {
		return store.BankImport{}, fmt.Errorf("parse bank import uploaded_at: %w", err)
	}
	return i, nil
}

// GetBankImport reads one import.
func (d *DB) GetBankImport(ctx context.Context, id int64) (store.BankImport, error) {
	return scanBankImport(d.db.QueryRowContext(ctx, bankImportSelect+` WHERE i.id = ?`, id))
}

// ListBankImports returns the most recent imports first.
func (d *DB) ListBankImports(ctx context.Context, limit int) ([]store.BankImport, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := d.db.QueryContext(ctx,
		bankImportSelect+` ORDER BY i.uploaded_at DESC, i.id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, translate(err)
	}
	defer func() { _ = rows.Close() }()
	var out []store.BankImport
	for rows.Next() {
		i, err := scanBankImport(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, i)
	}
	return out, translate(rows.Err())
}

// ListBankLines returns an import's stored lines in file order.
func (d *DB) ListBankLines(ctx context.Context, importID int64) ([]store.BankLine, error) {
	rows, err := d.db.QueryContext(ctx,
		`SELECT id, import_id, file_row, original_text, account, posted_on, amount_cents,
		        description, note, reference, fingerprint, state, ignore_reason
		   FROM bank_lines WHERE import_id = ? ORDER BY file_row`, importID)
	if err != nil {
		return nil, translate(err)
	}
	defer func() { _ = rows.Close() }()
	var out []store.BankLine
	for rows.Next() {
		var (
			l     store.BankLine
			cents int64
			state string
		)
		if err := rows.Scan(&l.ID, &l.ImportID, &l.Row, &l.Raw, &l.Account, &l.PostedOn, &cents,
			&l.Description, &l.Note, &l.Reference, &l.Fingerprint, &state, &l.IgnoreReason); err != nil {
			return nil, translate(err)
		}
		l.Amount, l.State = money.Cents(cents), store.BankLineState(state)
		out = append(out, l)
	}
	return out, translate(rows.Err())
}
