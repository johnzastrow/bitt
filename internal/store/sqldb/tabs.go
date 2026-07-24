package sqldb

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/johnzastrow/bitt/internal/fee"
	"github.com/johnzastrow/bitt/internal/money"
	"github.com/johnzastrow/bitt/internal/schedule"
	"github.com/johnzastrow/bitt/internal/store"
)

const (
	tabColumns = `id, name, kind, description, created_by, created_at, archived_at, ` +
		`schedule_kind, schedule_anchor, schedule_billing, schedule_interval, ` +
		`fee_kind, fee_fixed_cents, fee_percent_bp, fee_grace_days, fee_cap_cents, ` +
		`interest_apr_bp, loan_term_periods, loan_payment_cents`
	// same list qualified for joins; kept literal rather than derived, since a
	// helper that rewrites SQL strings is more machinery than two constants.
	tabColumnsT = `t.id, t.name, t.kind, t.description, t.created_by, t.created_at, t.archived_at, ` +
		`t.schedule_kind, t.schedule_anchor, t.schedule_billing, t.schedule_interval, ` +
		`t.fee_kind, t.fee_fixed_cents, t.fee_percent_bp, t.fee_grace_days, t.fee_cap_cents, ` +
		`t.interest_apr_bp, t.loan_term_periods, t.loan_payment_cents`
)

func scanTab(row interface{ Scan(...any) error }) (store.Tab, error) {
	var (
		t          store.Tab
		kind       string
		created    string
		archived   sql.NullString
		schedKind  string
		anchor     string
		billing    string
		feeKind    string
		feeFixed   int64
		feePercent int64
		feeGrace   int
		feeCap     int64
		interestBP int64
		schedEvery int
		term       int
		payment    int64
	)
	if err := row.Scan(&t.ID, &t.Name, &kind, &t.Description, &t.CreatedBy, &created, &archived,
		&schedKind, &anchor, &billing, &schedEvery,
		&feeKind, &feeFixed, &feePercent, &feeGrace, &feeCap,
		&interestBP, &term, &payment); err != nil {
		return store.Tab{}, translate(err)
	}
	t.Kind = store.TabKind(kind)

	var err error
	if t.CreatedAt, err = parseTime(created); err != nil {
		return store.Tab{}, fmt.Errorf("sqlite: parse tab created_at: %w", err)
	}
	if t.ArchivedAt, err = fromNullText(archived); err != nil {
		return store.Tab{}, fmt.Errorf("sqlite: parse tab archived_at: %w", err)
	}
	if t.Schedule, err = scheduleFrom(schedKind, anchor, billing, schedEvery); err != nil {
		return store.Tab{}, fmt.Errorf("sqlite: parse tab schedule: %w", err)
	}
	t.Fee = fee.Policy{
		Kind:      fee.Kind(feeKind),
		Fixed:     money.Cents(feeFixed),
		PercentBP: feePercent,
		GraceDays: feeGrace,
		Cap:       money.Cents(feeCap),
	}
	t.InterestAPRBp = interestBP
	t.LoanTermPeriods = term
	t.LoanPayment = money.Cents(payment)
	return t, nil
}

// feePolicyCols flattens a fee policy into its stored columns. An unset policy
// writes an empty kind and zeros rather than nulls (DEPLOY-02).
func feePolicyCols(p fee.Policy) (kind string, fixed, percent int64, grace int, cap int64) {
	if !p.Set() {
		return "", 0, 0, 0, 0
	}
	return string(p.Kind), int64(p.Fixed), p.PercentBP, p.GraceDays, int64(p.Cap)
}

// scheduleFrom rebuilds a schedule from its three stored columns. All three are
// empty for a tab that is billed by hand, which is not an error.
func scheduleFrom(kind, anchor, billing string, interval int) (schedule.Schedule, error) {
	if kind == "" && anchor == "" && billing == "" {
		return schedule.Schedule{}, nil
	}
	s := schedule.Schedule{
		Kind:     schedule.Kind(kind),
		Billing:  schedule.Billing(billing),
		Interval: interval,
	}
	if anchor != "" {
		d, err := schedule.ParseDate(anchor)
		if err != nil {
			return schedule.Schedule{}, err
		}
		s.Anchor = d
	}
	// Normalize on the way out so a row written before intervals existed --
	// interval 0, or the retired 'biweekly' kind that migration 0006 should
	// have rewritten -- behaves as the equivalent modern schedule. The rewrite
	// is date-identical, so nothing re-bills.
	return s.Normalize(), nil
}

// scheduleText flattens a schedule into its stored columns. An unset schedule
// writes three empty strings rather than nulls (DEPLOY-02).
func scheduleText(s schedule.Schedule) (kind, anchor, billing string, interval int) {
	if !s.Set() {
		// An unset schedule still stores interval 1 rather than 0, so the
		// column's NOT NULL DEFAULT and its stored value agree.
		return "", "", "", 1
	}
	s = s.Normalize()
	if !s.Anchor.IsZero() {
		anchor = s.Anchor.String()
	}
	return string(s.Kind), anchor, string(s.Billing), s.Every()
}

// CreateTab inserts a tab, its items, and its creating Provider atomically.
// A tab that reached the database without a Provider would be unreachable by
// any authorization check, so the three writes share one transaction.
func (d *DB) CreateTab(ctx context.Context, t store.Tab, items []store.TabItem) (store.Tab, error) {
	if !t.Kind.Valid() {
		return store.Tab{}, fmt.Errorf("sqlite: invalid tab kind %q", t.Kind)
	}
	if t.CreatedAt.IsZero() {
		t.CreatedAt = time.Now().UTC()
	}

	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return store.Tab{}, fmt.Errorf("sqlite: begin create tab: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	schedKind, anchor, billing, interval := scheduleText(t.Schedule)
	feeKind, feeFixed, feePercent, feeGrace, feeCap := feePolicyCols(t.Fee)
	res, err := tx.ExecContext(ctx,
		`INSERT INTO tabs (name, kind, description, created_by, created_at, archived_at,
                           schedule_kind, schedule_anchor, schedule_billing, schedule_interval,
                           fee_kind, fee_fixed_cents, fee_percent_bp, fee_grace_days, fee_cap_cents,
                           interest_apr_bp, loan_term_periods, loan_payment_cents)
         VALUES (?, ?, ?, ?, ?, NULL, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		t.Name, string(t.Kind), t.Description, t.CreatedBy, toText(t.CreatedAt),
		schedKind, anchor, billing, interval,
		feeKind, feeFixed, feePercent, feeGrace, feeCap,
		t.InterestAPRBp, t.LoanTermPeriods, int64(t.LoanPayment))
	if err != nil {
		return store.Tab{}, translate(err)
	}
	if t.ID, err = res.LastInsertId(); err != nil {
		return store.Tab{}, fmt.Errorf("sqlite: tab id: %w", err)
	}

	for i, item := range items {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO tab_items (tab_id, name, amount_cents, position, created_at, removed_at)
             VALUES (?, ?, ?, ?, ?, NULL)`,
			t.ID, item.Name, int64(item.Amount), i, toText(t.CreatedAt)); err != nil {
			return store.Tab{}, translate(err)
		}
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO tab_participants (tab_id, user_id, role, added_at) VALUES (?, ?, ?, ?)`,
		t.ID, t.CreatedBy, string(store.RoleProvider), toText(t.CreatedAt)); err != nil {
		return store.Tab{}, translate(err)
	}

	if err := tx.Commit(); err != nil {
		return store.Tab{}, fmt.Errorf("sqlite: commit create tab: %w", err)
	}
	return t, nil
}

// GetTab looks up a tab by id. It performs no authorization; callers must pair
// it with ParticipantRole (AUTH-05).
func (d *DB) GetTab(ctx context.Context, id int64) (store.Tab, error) {
	return scanTab(d.db.QueryRowContext(ctx,
		`SELECT `+tabColumns+` FROM tabs WHERE id = ?`, id))
}

// ListTabsForUser returns only tabs the user participates in. The join is the
// authorization: a non-participant's tab is never in the result set to begin
// with, rather than being filtered out afterward (AUTH-05).
func (d *DB) ListTabsForUser(ctx context.Context, userID int64) ([]store.Tab, error) {
	rows, err := d.db.QueryContext(ctx,
		`SELECT `+tabColumnsT+`
         FROM tabs t
         JOIN tab_participants p ON p.tab_id = t.id
         WHERE p.user_id = ?
         ORDER BY t.archived_at IS NOT NULL, t.created_at DESC, t.id DESC`,
		userID)
	if err != nil {
		return nil, translate(err)
	}
	defer func() { _ = rows.Close() }()

	var out []store.Tab
	for rows.Next() {
		t, err := scanTab(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, translate(rows.Err())
}

// ListAllTabs returns every tab, for the notification scan. Server-internal use
// only -- there is no per-user filter, so no handler may reach it directly.
func (d *DB) ListAllTabs(ctx context.Context) ([]store.Tab, error) {
	rows, err := d.db.QueryContext(ctx,
		`SELECT `+tabColumns+` FROM tabs ORDER BY id`)
	if err != nil {
		return nil, translate(err)
	}
	defer func() { _ = rows.Close() }()

	var out []store.Tab
	for rows.Next() {
		t, err := scanTab(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, translate(rows.Err())
}

const itemColumns = `id, tab_id, name, amount_cents, position, created_at, removed_at`

func scanItem(row interface{ Scan(...any) error }) (store.TabItem, error) {
	var (
		it      store.TabItem
		amount  int64
		created string
		removed sql.NullString
	)
	if err := row.Scan(&it.ID, &it.TabID, &it.Name, &amount, &it.Position, &created, &removed); err != nil {
		return store.TabItem{}, translate(err)
	}
	it.Amount = money.Cents(amount)

	var err error
	if it.CreatedAt, err = parseTime(created); err != nil {
		return store.TabItem{}, fmt.Errorf("sqlite: parse item created_at: %w", err)
	}
	if it.RemovedAt, err = fromNullText(removed); err != nil {
		return store.TabItem{}, fmt.Errorf("sqlite: parse item removed_at: %w", err)
	}
	return it, nil
}

// ListItems returns a tab's active items in display order (TAB-04). Superseded
// and retired items are excluded: they belong to the periods that already
// captured them, not to the next one to post.
func (d *DB) ListItems(ctx context.Context, tabID int64) ([]store.TabItem, error) {
	rows, err := d.db.QueryContext(ctx,
		`SELECT `+itemColumns+`
         FROM tab_items
         WHERE tab_id = ? AND removed_at IS NULL
         ORDER BY position, id`, tabID)
	if err != nil {
		return nil, translate(err)
	}
	defer func() { _ = rows.Close() }()

	var out []store.TabItem
	for rows.Next() {
		it, err := scanItem(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, translate(rows.Err())
}

// ListItemHistory returns every item the tab has ever carried, including
// superseded and retired rows, oldest first within each position (CHG-02).
func (d *DB) ListItemHistory(ctx context.Context, tabID int64) ([]store.TabItem, error) {
	rows, err := d.db.QueryContext(ctx,
		`SELECT `+itemColumns+`
         FROM tab_items
         WHERE tab_id = ?
         ORDER BY position, created_at, id`, tabID)
	if err != nil {
		return nil, translate(err)
	}
	defer func() { _ = rows.Close() }()

	var out []store.TabItem
	for rows.Next() {
		it, err := scanItem(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, translate(rows.Err())
}

// UpdateTabDetails changes a tab's name, description, and kind.
//
// None of the three appears on an entry, so this cannot move a balance. A tab
// switching between Services and Payoff changes how it is presented and
// measured, never what it has already charged.
func (d *DB) UpdateTabDetails(ctx context.Context, tabID int64, name, description string, kind store.TabKind) error {
	if !kind.Valid() {
		return fmt.Errorf("sqlite: invalid tab kind %q", kind)
	}
	res, err := d.db.ExecContext(ctx,
		`UPDATE tabs SET name = ?, description = ?, kind = ? WHERE id = ?`,
		name, description, string(kind), tabID)
	if err != nil {
		return translate(err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("sqlite: update tab: %w", err)
	}
	if n == 0 {
		return store.ErrNotFound
	}
	return nil
}

// SetTabArchived retires a tab from the active dashboard, or brings it back.
//
// Archiving is not deletion and deliberately touches nothing else: the entries,
// the balance, the posted periods, and the history all stay exactly as they
// were. What changes is that the tab sorts below the active ones and stops
// accruing scheduled charges.
func (d *DB) SetTabArchived(ctx context.Context, tabID int64, archived bool) error {
	var at any
	if archived {
		at = nowText()
	}
	res, err := d.db.ExecContext(ctx,
		`UPDATE tabs SET archived_at = ? WHERE id = ?`, at, tabID)
	if err != nil {
		return translate(err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("sqlite: set tab archived: %w", err)
	}
	if n == 0 {
		return store.ErrNotFound
	}
	return nil
}

// SetSchedule replaces a tab's recurrence (SCHED-01).
//
// It writes only the three schedule columns. Nothing here touches
// posted_periods, so changing or clearing a schedule can never re-bill or
// un-bill a cycle that has already posted.
func (d *DB) SetSchedule(ctx context.Context, tabID int64, s schedule.Schedule) error {
	if s.Set() {
		if err := s.Validate(); err != nil {
			return fmt.Errorf("sqlite: %w", err)
		}
	}
	kind, anchor, billing, interval := scheduleText(s)

	res, err := d.db.ExecContext(ctx,
		`UPDATE tabs SET schedule_kind = ?, schedule_anchor = ?, schedule_billing = ?,
                        schedule_interval = ? WHERE id = ?`,
		kind, anchor, billing, interval, tabID)
	if err != nil {
		return translate(err)
	}
	// A tab id that matched nothing is a missing tab, not a silent success.
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("sqlite: set schedule: %w", err)
	}
	if n == 0 {
		return store.ErrNotFound
	}
	return nil
}

// SetFeePolicy replaces a tab's late-fee policy (FEE-01, FEE-02, FEE-06).
//
// It writes only the five fee columns. Nothing here touches posted_fees, so
// changing or clearing the policy can never re-assess or unwind a fee already
// charged -- a fee, once posted, is an immutable ledger entry like any other.
func (d *DB) SetFeePolicy(ctx context.Context, tabID int64, p fee.Policy) error {
	if err := p.Validate(); err != nil {
		return fmt.Errorf("sqlite: %w", err)
	}
	kind, fixed, percent, grace, cap := feePolicyCols(p)

	res, err := d.db.ExecContext(ctx,
		`UPDATE tabs SET fee_kind = ?, fee_fixed_cents = ?, fee_percent_bp = ?,
                        fee_grace_days = ?, fee_cap_cents = ? WHERE id = ?`,
		kind, fixed, percent, grace, cap, tabID)
	if err != nil {
		return translate(err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("sqlite: set fee policy: %w", err)
	}
	if n == 0 {
		return store.ErrNotFound
	}
	return nil
}

// SetLoanTerms replaces a Payoff loan's term and scheduled payment.
//
// Nothing here touches posted_interest, posted_fees, or posted_periods, so a
// Provider truing the payment up to the bank's figure reaches future periods
// only. That is the point of the true-up being a setting rather than a
// recomputation: the ledger is append-only, and interest already charged was
// charged on the balance that stood at the time.
func (d *DB) SetLoanTerms(ctx context.Context, tabID int64, termPeriods int, payment money.Cents) error {
	if termPeriods < 0 {
		return fmt.Errorf("sqlite: loan term cannot be negative")
	}
	if payment < 0 {
		return fmt.Errorf("sqlite: loan payment cannot be negative")
	}
	res, err := d.db.ExecContext(ctx,
		`UPDATE tabs SET loan_term_periods = ?, loan_payment_cents = ? WHERE id = ?`,
		termPeriods, int64(payment), tabID)
	if err != nil {
		return translate(err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("sqlite: set loan terms: %w", err)
	}
	if n == 0 {
		return store.ErrNotFound
	}
	return nil
}

// SetInterestRate replaces a Payoff loan's annual interest rate, in basis
// points. Zero clears it. Nothing here touches posted_interest, so a rate
// change reaches future periods only -- interest already charged stands.
func (d *DB) SetInterestRate(ctx context.Context, tabID int64, annualBasisPoints int64) error {
	if annualBasisPoints < 0 {
		return fmt.Errorf("sqlite: interest rate cannot be negative")
	}
	res, err := d.db.ExecContext(ctx,
		`UPDATE tabs SET interest_apr_bp = ? WHERE id = ?`, annualBasisPoints, tabID)
	if err != nil {
		return translate(err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("sqlite: set interest rate: %w", err)
	}
	if n == 0 {
		return store.ErrNotFound
	}
	return nil
}

// ListTabReminders returns a tab's own payment reminders, longest lead first --
// the order they fire in, which is the order the interface lists them.
func (d *DB) ListTabReminders(ctx context.Context, tabID int64) ([]store.TabReminder, error) {
	rows, err := d.db.QueryContext(ctx,
		`SELECT days, title, body FROM tab_reminders WHERE tab_id = ? ORDER BY days DESC`, tabID)
	if err != nil {
		return nil, translate(err)
	}
	defer func() { _ = rows.Close() }()

	var out []store.TabReminder
	for rows.Next() {
		var r store.TabReminder
		if err := rows.Scan(&r.Days, &r.Title, &r.Body); err != nil {
			return nil, fmt.Errorf("sqlite: scan tab reminder: %w", err)
		}
		out = append(out, r)
	}
	return out, translate(rows.Err())
}

// SetTabReminders replaces a tab's reminders in one transaction.
//
// Delete-then-insert rather than a merge, because the set is the unit the
// Provider edits: dropping a lead time from the form must drop it from the tab,
// and clearing the form entirely must return the tab to the instance defaults.
// Doing both inside one transaction means a tab is never briefly reminderless
// to a concurrent tick scan.
func (d *DB) SetTabReminders(ctx context.Context, tabID int64, rs []store.TabReminder) error {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqlite: begin set tab reminders: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// The tab must exist, or a delete of nothing followed by an insert that the
	// foreign key rejects would report a confusing error.
	var exists int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM tabs WHERE id = ?`, tabID).
		Scan(&exists); err != nil {
		return translate(err)
	}
	if exists == 0 {
		return store.ErrNotFound
	}

	if _, err := tx.ExecContext(ctx, `DELETE FROM tab_reminders WHERE tab_id = ?`, tabID); err != nil {
		return translate(err)
	}
	for _, r := range rs {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO tab_reminders (tab_id, days, title, body) VALUES (?, ?, ?, ?)`,
			tabID, r.Days, r.Title, r.Body); err != nil {
			return translate(err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sqlite: commit set tab reminders: %w", err)
	}
	return nil
}

// GetItem loads one line item, so a caller can authorize against its tab
// before changing it.
func (d *DB) GetItem(ctx context.Context, itemID int64) (store.TabItem, error) {
	return scanItem(d.db.QueryRowContext(ctx,
		`SELECT `+itemColumns+` FROM tab_items WHERE id = ?`, itemID))
}

// UpdateItem changes an item's name or amount by superseding it (CHG-02).
//
// The existing row is marked removed and a replacement takes its position, in
// one transaction. Two reasons for supersede rather than an in-place UPDATE:
// the tab keeps a record of what it used to charge, and the change is visibly
// dated rather than appearing to have always been true.
//
// Posted entries are untouched. They carry their own item snapshot, so a change
// here reaches the next period to post and nothing already billed (CHG-01).
func (d *DB) UpdateItem(ctx context.Context, itemID int64, name string, amount money.Cents) (store.TabItem, error) {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return store.TabItem{}, fmt.Errorf("sqlite: begin update item: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	old, err := scanItem(tx.QueryRowContext(ctx,
		`SELECT `+itemColumns+` FROM tab_items WHERE id = ? AND removed_at IS NULL`, itemID))
	if err != nil {
		return store.TabItem{}, err
	}

	now := time.Now().UTC()
	if _, err := tx.ExecContext(ctx,
		`UPDATE tab_items SET removed_at = ? WHERE id = ?`, toText(now), itemID); err != nil {
		return store.TabItem{}, translate(err)
	}

	replacement := store.TabItem{
		TabID:     old.TabID,
		Name:      name,
		Amount:    amount,
		Position:  old.Position,
		CreatedAt: now,
	}
	res, err := tx.ExecContext(ctx,
		`INSERT INTO tab_items (tab_id, name, amount_cents, position, created_at, removed_at)
         VALUES (?, ?, ?, ?, ?, NULL)`,
		replacement.TabID, replacement.Name, int64(replacement.Amount),
		replacement.Position, toText(now))
	if err != nil {
		return store.TabItem{}, translate(err)
	}
	if replacement.ID, err = res.LastInsertId(); err != nil {
		return store.TabItem{}, fmt.Errorf("sqlite: replacement item id: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return store.TabItem{}, fmt.Errorf("sqlite: commit update item: %w", err)
	}
	return replacement, nil
}

// RemoveItem retires an item from future periods (CHG-02). Periods already
// posted keep it, because they hold their own snapshot.
func (d *DB) RemoveItem(ctx context.Context, itemID int64) error {
	res, err := d.db.ExecContext(ctx,
		`UPDATE tab_items SET removed_at = ? WHERE id = ? AND removed_at IS NULL`,
		nowText(), itemID)
	if err != nil {
		return translate(err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("sqlite: remove item: %w", err)
	}
	if n == 0 {
		return store.ErrNotFound
	}
	return nil
}

// AddItem appends an item to a tab.
func (d *DB) AddItem(ctx context.Context, item store.TabItem) (store.TabItem, error) {
	if item.CreatedAt.IsZero() {
		item.CreatedAt = time.Now().UTC()
	}
	if item.Position == 0 {
		var next sql.NullInt64
		if err := d.db.QueryRowContext(ctx,
			`SELECT MAX(position) FROM tab_items WHERE tab_id = ?`, item.TabID).Scan(&next); err != nil {
			return store.TabItem{}, translate(err)
		}
		if next.Valid {
			item.Position = int(next.Int64) + 1
		}
	}
	res, err := d.db.ExecContext(ctx,
		`INSERT INTO tab_items (tab_id, name, amount_cents, position, created_at, removed_at)
         VALUES (?, ?, ?, ?, ?, NULL)`,
		item.TabID, item.Name, int64(item.Amount), item.Position, toText(item.CreatedAt))
	if err != nil {
		return store.TabItem{}, translate(err)
	}
	if item.ID, err = res.LastInsertId(); err != nil {
		return store.TabItem{}, fmt.Errorf("sqlite: item id: %w", err)
	}
	return item, nil
}

// AddParticipant attaches a user to a tab in a role (TAB-03, Phase 2).
func (d *DB) AddParticipant(ctx context.Context, p store.Participant) error {
	if p.AddedAt.IsZero() {
		p.AddedAt = time.Now().UTC()
	}
	_, err := d.db.ExecContext(ctx,
		`INSERT INTO tab_participants (tab_id, user_id, role, added_at) VALUES (?, ?, ?, ?)`,
		p.TabID, p.UserID, string(p.Role), toText(p.AddedAt))
	return translate(err)
}

// ListParticipants returns a tab's participants with display details.
func (d *DB) ListParticipants(ctx context.Context, tabID int64) ([]store.Participant, error) {
	rows, err := d.db.QueryContext(ctx,
		`SELECT p.tab_id, p.user_id, p.role, p.added_at, u.display_name, u.email, u.avatar_updated_at
         FROM tab_participants p
         JOIN users u ON u.id = p.user_id
         WHERE p.tab_id = ?
         ORDER BY p.role, u.display_name`, tabID)
	if err != nil {
		return nil, translate(err)
	}
	defer func() { _ = rows.Close() }()

	var out []store.Participant
	for rows.Next() {
		var (
			p     store.Participant
			role  string
			added string
		)
		if err := rows.Scan(&p.TabID, &p.UserID, &role, &added, &p.DisplayName, &p.Email, &p.AvatarUpdatedAt); err != nil {
			return nil, translate(err)
		}
		p.Role = store.Role(role)
		if p.AddedAt, err = parseTime(added); err != nil {
			return nil, fmt.Errorf("sqlite: parse participant added_at: %w", err)
		}
		out = append(out, p)
	}
	return out, translate(rows.Err())
}

// ParticipantRole is the authorization primitive (AUTH-05). ErrNotFound means
// the user does not participate in the tab and must be denied.
func (d *DB) ParticipantRole(ctx context.Context, tabID, userID int64) (store.Role, error) {
	var role string
	err := d.db.QueryRowContext(ctx,
		`SELECT role FROM tab_participants WHERE tab_id = ? AND user_id = ?`,
		tabID, userID).Scan(&role)
	if err != nil {
		return "", translate(err)
	}
	return store.Role(role), nil
}

var _ store.TabStore = (*DB)(nil)

// RemoveParticipant detaches a user from a tab.
//
// It refuses to remove the last Provider: a tab with no Provider could never be
// billed again and would be unreachable by any provider-role check.
func (d *DB) RemoveParticipant(ctx context.Context, tabID, userID int64) error {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqlite: begin remove participant: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var role string
	if err := tx.QueryRowContext(ctx,
		`SELECT role FROM tab_participants WHERE tab_id = ? AND user_id = ?`,
		tabID, userID).Scan(&role); err != nil {
		return translate(err)
	}

	if store.Role(role) == store.RoleProvider {
		var others int
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM tab_participants
             WHERE tab_id = ? AND role = ? AND user_id <> ?`,
			tabID, string(store.RoleProvider), userID).Scan(&others); err != nil {
			return translate(err)
		}
		if others == 0 {
			return fmt.Errorf("%w: a tab must keep at least one provider", store.ErrConflict)
		}
	}

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM tab_participants WHERE tab_id = ? AND user_id = ?`, tabID, userID); err != nil {
		return translate(err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sqlite: commit remove participant: %w", err)
	}
	return nil
}
