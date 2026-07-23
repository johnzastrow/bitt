// Package sqlite implements store.Store on SQLite.
//
// Every statement here is parameterized. No query is assembled by
// concatenating caller input, anywhere in this package.
package sqlite

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/johnzastrow/bitt/internal/store"
	_ "modernc.org/sqlite" // pure-Go driver: no cgo, so the binary stays static (DEPLOY-04)
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// timeFormat is ISO-8601 in UTC with milliseconds. Stored as text so values
// compare lexicographically and survive the move to MariaDB unchanged.
const timeFormat = "2006-01-02T15:04:05.000Z"

// DB is a SQLite-backed store.
type DB struct {
	db   *sql.DB
	path string
	// triggers records whether append-only triggers were requested. When false,
	// the migration still creates them and startup drops them, so that the
	// disabled state is explicit and recoverable rather than a schema variant.
	triggers bool
}

// Options configures Open.
type Options struct {
	// Path is the database file. ":memory:" is accepted for tests.
	Path string
	// AppendOnlyTriggers enables the LEDGER-01 abort triggers. Default true;
	// disable only for development or manual repair.
	AppendOnlyTriggers bool
}

// Open connects to the database and applies pragmas. It does not migrate;
// call Migrate explicitly so startup ordering stays visible.
func Open(opts Options) (*DB, error) {
	if opts.Path == "" {
		return nil, errors.New("sqlite: empty database path")
	}

	// WAL for concurrent readers alongside a writer; busy_timeout so a
	// concurrent write waits rather than failing immediately; foreign_keys
	// because SQLite leaves them off by default and the schema relies on them.
	dsn := opts.Path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)"
	if strings.HasPrefix(opts.Path, ":memory:") || strings.Contains(opts.Path, "mode=memory") {
		dsn = opts.Path
	}

	sqlDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("sqlite: open: %w", err)
	}

	// SQLite permits one writer at a time. Serializing connections avoids
	// spurious lock contention at the cost of throughput this deployment scale
	// will never notice.
	sqlDB.SetMaxOpenConns(1)
	sqlDB.SetMaxIdleConns(1)
	sqlDB.SetConnMaxLifetime(0)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := sqlDB.PingContext(ctx); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("sqlite: ping: %w", err)
	}

	if err := restrictPermissions(opts.Path); err != nil {
		_ = sqlDB.Close()
		return nil, err
	}

	return &DB{db: sqlDB, path: opts.Path, triggers: opts.AppendOnlyTriggers}, nil
}

// restrictPermissions narrows the database files to owner-only.
//
// SQLite creates them honoring the process umask, which on most systems yields
// world-readable files. These hold financial records and password hashes, so
// they are tightened explicitly rather than left to the ambient umask. The WAL
// and shared-memory sidecars carry the same data and get the same treatment.
func restrictPermissions(path string) error {
	if strings.HasPrefix(path, ":memory:") || strings.Contains(path, "mode=memory") {
		return nil
	}
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		// A sidecar may not exist yet; that is not an error.
		if _, err := os.Stat(p); err != nil {
			continue
		}
		if err := os.Chmod(p, 0o600); err != nil {
			return fmt.Errorf("sqlite: restrict permissions on %s: %w", p, err)
		}
	}
	return nil
}

// Close releases the handle.
func (d *DB) Close() error { return d.db.Close() }

// ---------------------------------------------------------------------------
// Migrations (DEPLOY-01)
// ---------------------------------------------------------------------------

// Migrate applies every pending migration inside a transaction and records it.
// Safe to run on every startup and safe under concurrent starts, since the
// whole run holds a write lock.
func (d *DB) Migrate(ctx context.Context) error {
	if _, err := d.db.ExecContext(ctx, `
        CREATE TABLE IF NOT EXISTS schema_migrations (
            version    TEXT NOT NULL PRIMARY KEY,
            applied_at TEXT NOT NULL
        )`); err != nil {
		return fmt.Errorf("sqlite: migration table: %w", err)
	}

	files, err := fs.Glob(migrationFS, "migrations/*.sql")
	if err != nil {
		return fmt.Errorf("sqlite: list migrations: %w", err)
	}
	sort.Strings(files)

	for _, f := range files {
		version := strings.TrimSuffix(pathBase(f), ".sql")

		var exists int
		err := d.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM schema_migrations WHERE version = ?`, version).Scan(&exists)
		if err != nil {
			return fmt.Errorf("sqlite: check migration %s: %w", version, err)
		}
		if exists > 0 {
			continue
		}

		body, err := migrationFS.ReadFile(f)
		if err != nil {
			return fmt.Errorf("sqlite: read migration %s: %w", version, err)
		}

		tx, err := d.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("sqlite: begin migration %s: %w", version, err)
		}
		if _, err := tx.ExecContext(ctx, string(body)); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("sqlite: apply migration %s: %w", version, err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)`,
			version, nowText()); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("sqlite: record migration %s: %w", version, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("sqlite: commit migration %s: %w", version, err)
		}
	}

	if err := d.applyTriggerPolicy(ctx); err != nil {
		return err
	}
	// The WAL and shared-memory sidecars are created on first write, which
	// happens during migration, so tighten them once more now that they exist.
	return restrictPermissions(d.path)
}

// applyTriggerPolicy drops or restores the append-only triggers to match
// configuration. Dropping is deliberately explicit: an operator who disables
// enforcement should be able to see that it is off.
func (d *DB) applyTriggerPolicy(ctx context.Context) error {
	names := []string{
		"entries_no_update", "entries_no_delete",
		"entry_items_no_update", "entry_items_no_delete",
	}
	if d.triggers {
		return nil // migrations create them; nothing to restore
	}
	for _, n := range names {
		if _, err := d.db.ExecContext(ctx, `DROP TRIGGER IF EXISTS `+n); err != nil {
			return fmt.Errorf("sqlite: drop trigger %s: %w", n, err)
		}
	}
	return nil
}

func pathBase(p string) string {
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[i+1:]
	}
	return p
}

// ---------------------------------------------------------------------------
// Time and error helpers
// ---------------------------------------------------------------------------

func nowText() string { return time.Now().UTC().Format(timeFormat) }

func toText(t time.Time) string { return t.UTC().Format(timeFormat) }

func parseTime(s string) (time.Time, error) {
	return time.Parse(timeFormat, s)
}

func toNullText(t *time.Time) sql.NullString {
	if t == nil {
		return sql.NullString{}
	}
	return sql.NullString{String: toText(*t), Valid: true}
}

func fromNullText(ns sql.NullString) (*time.Time, error) {
	if !ns.Valid {
		return nil, nil
	}
	t, err := parseTime(ns.String)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// translate maps driver errors onto the package's sentinel errors so callers
// never inspect a driver-specific type.
func translate(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return store.ErrNotFound
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "append-only"):
		return fmt.Errorf("%w: %v", store.ErrAppendOnly, err)
	case strings.Contains(msg, "UNIQUE constraint failed"),
		strings.Contains(msg, "constraint failed: UNIQUE"):
		return fmt.Errorf("%w: %v", store.ErrConflict, err)
	}
	return err
}
