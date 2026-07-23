package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/johnzastrow/bitt/internal/money"
	"github.com/johnzastrow/bitt/internal/store"
)

const (
	tabColumns = `id, name, kind, description, created_by, created_at, archived_at`
	// same list qualified for joins; kept literal rather than derived, since a
	// helper that rewrites SQL strings is more machinery than two constants.
	tabColumnsT = `t.id, t.name, t.kind, t.description, t.created_by, t.created_at, t.archived_at`
)

func scanTab(row interface{ Scan(...any) error }) (store.Tab, error) {
	var (
		t        store.Tab
		kind     string
		created  string
		archived sql.NullString
	)
	if err := row.Scan(&t.ID, &t.Name, &kind, &t.Description, &t.CreatedBy, &created, &archived); err != nil {
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
	return t, nil
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

	res, err := tx.ExecContext(ctx,
		`INSERT INTO tabs (name, kind, description, created_by, created_at, archived_at)
         VALUES (?, ?, ?, ?, ?, NULL)`,
		t.Name, string(t.Kind), t.Description, t.CreatedBy, toText(t.CreatedAt))
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

// ListItems returns a tab's active items in display order (TAB-04).
func (d *DB) ListItems(ctx context.Context, tabID int64) ([]store.TabItem, error) {
	rows, err := d.db.QueryContext(ctx,
		`SELECT id, tab_id, name, amount_cents, position, created_at, removed_at
         FROM tab_items
         WHERE tab_id = ? AND removed_at IS NULL
         ORDER BY position, id`, tabID)
	if err != nil {
		return nil, translate(err)
	}
	defer func() { _ = rows.Close() }()

	var out []store.TabItem
	for rows.Next() {
		var (
			it      store.TabItem
			amount  int64
			created string
			removed sql.NullString
		)
		if err := rows.Scan(&it.ID, &it.TabID, &it.Name, &amount, &it.Position, &created, &removed); err != nil {
			return nil, translate(err)
		}
		it.Amount = money.Cents(amount)
		if it.CreatedAt, err = parseTime(created); err != nil {
			return nil, fmt.Errorf("sqlite: parse item created_at: %w", err)
		}
		if it.RemovedAt, err = fromNullText(removed); err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, translate(rows.Err())
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
		`SELECT p.tab_id, p.user_id, p.role, p.added_at, u.display_name, u.email
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
		if err := rows.Scan(&p.TabID, &p.UserID, &role, &added, &p.DisplayName, &p.Email); err != nil {
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
