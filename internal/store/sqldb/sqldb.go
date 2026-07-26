// Package sqldb implements store.Store on SQL, against either SQLite or
// MariaDB (DEPLOY-02, DEPLOY-03).
//
// One implementation serves both. That is not a happy accident -- the schema
// was written for it from Phase 1: timestamps are ISO-8601 TEXT so they compare
// lexicographically on either backend, there are no dialect typing tricks, and
// no query uses ON CONFLICT or RETURNING. What genuinely differs is the DDL,
// the trigger syntax, and how each driver spells a constraint violation, and
// all of that is behind the small `dialect` interface in dialect.go. The query
// code in accounts.go, tabs.go, and entries.go does not know which backend it
// is talking to.
//
// Every statement here is parameterized. No query is assembled by
// concatenating caller input, anywhere in this package.
package sqldb

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

	"github.com/go-sql-driver/mysql"
	"github.com/johnzastrow/bitt/internal/store"
	_ "modernc.org/sqlite" // pure-Go driver: no cgo, so the binary stays static (DEPLOY-04)
)

//go:embed migrations
var migrationFS embed.FS

// timeFormat is ISO-8601 in UTC with milliseconds. Stored as text so values
// compare lexicographically and survive the move to MariaDB unchanged.
const timeFormat = "2006-01-02T15:04:05.000Z"

// DB is a SQL-backed store. The dialect decides which backend.
type DB struct {
	db      *sql.DB
	dialect dialect
	// path is the SQLite file, empty on MariaDB. It is only used to tighten
	// file permissions, which is a concern the file-backed dialect alone has.
	path string
	// triggers records whether append-only triggers were requested. When false,
	// the migration still creates them and startup drops them, so that the
	// disabled state is explicit and recoverable rather than a schema variant.
	triggers bool
}

// Options configures Open.
type Options struct {
	// Driver selects the backend. Empty means SQLite, so every existing caller
	// keeps working unchanged.
	Driver Driver
	// Path is the SQLite database file. ":memory:" is accepted for tests.
	// Ignored on MariaDB.
	Path string
	// DSN is the MariaDB connection string, e.g.
	// "bitt:secret@tcp(db:3306)/bitt". Ignored on SQLite. It carries a password,
	// so it arrives from the environment or a file and is never logged.
	DSN string
	// AppendOnlyTriggers enables the LEDGER-01 abort triggers. Default true;
	// disable only for development or manual repair.
	AppendOnlyTriggers bool
}

// Open connects to the database. It does not migrate; call Migrate explicitly
// so startup ordering stays visible.
func Open(opts Options) (*DB, error) {
	dl, err := dialectFor(opts.Driver)
	if err != nil {
		return nil, err
	}

	dsn, path, err := dataSource(dl, opts)
	if err != nil {
		return nil, err
	}

	sqlDB, err := sql.Open(dl.driverName(), dsn)
	if err != nil {
		// The DSN can carry a password, so it is never included in an error.
		return nil, fmt.Errorf("%s: open: %w", dl.name(), err)
	}
	if err := dl.configure(context.Background(), sqlDB); err != nil {
		_ = sqlDB.Close()
		return nil, err
	}

	ctx, cancel := context.WithTimeout(context.Background(), connectTimeout)
	defer cancel()
	if err := sqlDB.PingContext(ctx); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("%s: ping: %w", dl.name(), err)
	}

	db := &DB{db: sqlDB, dialect: dl, path: path, triggers: opts.AppendOnlyTriggers}
	if err := db.restrictPermissions(); err != nil {
		_ = sqlDB.Close()
		return nil, err
	}
	return db, nil
}

// connectTimeout bounds the initial ping. A database that is not answering
// should fail startup quickly and visibly rather than hanging a container.
const connectTimeout = 10 * time.Second

// dataSource builds the connection string for the chosen backend, and reports
// the file path when there is one.
func dataSource(dl dialect, opts Options) (dsn, path string, err error) {
	switch dl.name() {
	case "sqlite":
		if opts.Path == "" {
			return "", "", errors.New("sqldb: empty database path")
		}
		// WAL for concurrent readers alongside a writer; busy_timeout so a
		// concurrent write waits rather than failing immediately; foreign_keys
		// because SQLite leaves them off by default and the schema relies on
		// them.
		dsn = opts.Path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)"
		if strings.HasPrefix(opts.Path, ":memory:") || strings.Contains(opts.Path, "mode=memory") {
			dsn = opts.Path
		}
		return dsn, opts.Path, nil
	case "mariadb":
		if opts.DSN == "" {
			return "", "", errors.New("sqldb: mariadb selected but no DSN given (set BITT_DB_DSN)")
		}
		dsn, err = mariaDSN(opts.DSN)
		return dsn, "", err
	}
	return "", "", fmt.Errorf("sqldb: no data source for dialect %q", dl.name())
}

// mariaDSN normalises the operator's connection string and pins the session
// settings the ledger depends on.
//
// Three of them are not preferences:
//
//   - parseTime=false. Timestamps are stored and read as ISO-8601 TEXT on both
//     backends, and letting the driver hand back time.Time for some columns
//     would make the scan code dialect-dependent, which is exactly what this
//     package is arranged to avoid.
//   - sql_mode includes STRICT_ALL_TABLES. Without it MySQL silently truncates
//     an over-long value and coerces a bad one instead of refusing, which on a
//     ledger means a wrong number rather than an error.
//   - transaction isolation stays at the server default (REPEATABLE READ on
//     InnoDB), which is what the accrual claims rely on: a period claim is a
//     unique-key insert, and its conflict is detected by the key, not by the
//     isolation level.
func mariaDSN(raw string) (string, error) {
	cfg, err := mysql.ParseDSN(raw)
	if err != nil {
		// The DSN carries a password. Report that it did not parse, never what
		// it contained.
		return "", errors.New("sqldb: BITT_DB_DSN is not a valid MySQL DSN (want user:pass@tcp(host:port)/dbname)")
	}
	if cfg.DBName == "" {
		return "", errors.New("sqldb: BITT_DB_DSN names no database")
	}
	cfg.ParseTime = false
	// Report rows MATCHED by an UPDATE's WHERE, not rows CHANGED. MySQL/MariaDB
	// default to changed, so an UPDATE that writes a row's existing values back
	// (saving a form without altering a field) returns RowsAffected()==0 -- and
	// the store reads 0 as "no such row" and raises store.ErrNotFound, turning an
	// idempotent re-save into a 500. SQLite counts a matched row as affected
	// whether or not a value changed, so this bug is MariaDB-only and never
	// showed in the SQLite suite. clientFoundRows aligns the two: on both
	// backends RowsAffected()==0 now means the id matched nothing, which is what
	// every "== 0 -> ErrNotFound" check in this package intends. It is safe for
	// the one conditional update here (removed_at IS NULL): a matched row there
	// always changes, so matched and changed agree.
	cfg.ClientFoundRows = true
	if cfg.Params == nil {
		cfg.Params = map[string]string{}
	}
	if _, ok := cfg.Params["sql_mode"]; !ok {
		cfg.Params["sql_mode"] = "'STRICT_ALL_TABLES,NO_ENGINE_SUBSTITUTION'"
	}
	if cfg.Collation == "" {
		// Binary collation, so string comparison is case- and accent-sensitive.
		// MySQL's default is case-INSENSITIVE, under which two period keys or
		// two idempotency keys differing only in case would collide -- and the
		// claim tables that stop a double charge are built on exactly those
		// comparisons.
		cfg.Collation = "utf8mb4_bin"
	}
	return cfg.FormatDSN(), nil
}

// ---------------------------------------------------------------------------
// Migrations (DEPLOY-01)
// ---------------------------------------------------------------------------

// Migrate applies every pending migration inside a transaction and records it.
// Safe to run on every startup and safe under concurrent starts, since the
// whole run holds a write lock.
func (d *DB) Migrate(ctx context.Context) error {
	if err := d.dialect.migrationsTable(ctx, d.db); err != nil {
		return err
	}

	root, err := d.dialect.migrations()
	if err != nil {
		return err
	}
	files, err := fs.Glob(root, "*.sql")
	if err != nil {
		return fmt.Errorf("%s: list migrations: %w", d.dialect.name(), err)
	}
	sort.Strings(files)

	for _, f := range files {
		version := strings.TrimSuffix(pathBase(f), ".sql")

		var exists int
		if err := d.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM schema_migrations WHERE version = ?`, version).Scan(&exists); err != nil {
			return fmt.Errorf("%s: check migration %s: %w", d.dialect.name(), version, err)
		}
		if exists > 0 {
			continue
		}

		body, err := fs.ReadFile(root, f)
		if err != nil {
			return fmt.Errorf("%s: read migration %s: %w", d.dialect.name(), version, err)
		}

		tx, err := d.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("%s: begin migration %s: %w", d.dialect.name(), version, err)
		}
		// One file may hold several statements; the dialect decides how to
		// divide them, since MariaDB executes one per call and SQLite takes the
		// whole file at once.
		for _, stmt := range d.dialect.splitStatements(string(body)) {
			if _, err := tx.ExecContext(ctx, stmt); err != nil {
				_ = tx.Rollback()
				return fmt.Errorf("%s: apply migration %s: %w", d.dialect.name(), version, err)
			}
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)`,
			version, nowText()); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("%s: record migration %s: %w", d.dialect.name(), version, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("%s: commit migration %s: %w", d.dialect.name(), version, err)
		}
	}

	if err := d.applyTriggerPolicy(ctx); err != nil {
		return err
	}
	// On SQLite the WAL and shared-memory sidecars are created on first write,
	// which happens during migration, so tighten them once more now they exist.
	// A no-op on MariaDB, which has no files to tighten.
	return d.restrictPermissions()
}

// applyTriggerPolicy drops or restores the append-only triggers to match
// configuration. Dropping is deliberately explicit: an operator who disables
// enforcement should be able to see that it is off.
func (d *DB) applyTriggerPolicy(ctx context.Context) error {
	names := []string{
		"entries_no_update", "entries_no_delete",
		"entry_items_no_update", "entry_items_no_delete",
		"posted_periods_no_update", "posted_periods_no_delete",
		"posted_fees_no_update", "posted_fees_no_delete",
		"posted_interest_no_update", "posted_interest_no_delete",
	}
	if d.triggers {
		return nil // migrations create them; nothing to restore
	}
	for _, n := range names {
		if _, err := d.db.ExecContext(ctx, `DROP TRIGGER IF EXISTS `+n); err != nil {
			return fmt.Errorf("%s: drop trigger %s: %w", d.dialect.name(), n, err)
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
//
// It handles both backends in one function rather than dispatching through the
// dialect, for two reasons. The error spaces cannot collide -- a *mysql.MySQLError
// is a distinct type and SQLite's constraint text does not appear in it -- and
// a free function cannot be called with the wrong dialect by a caller that
// happens not to have the DB in scope.
func translate(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return store.ErrNotFound
	}

	// Both dialects raise this text from their triggers: RAISE(ABORT, ...) on
	// SQLite, SIGNAL ... SET MESSAGE_TEXT on MariaDB. The ledger's append-only
	// guarantee therefore reports identically whichever is underneath.
	msg := err.Error()
	if strings.Contains(msg, "append-only") {
		return fmt.Errorf("%w: %v", store.ErrAppendOnly, err)
	}

	// MariaDB reports a constraint violation as a numbered server error.
	var me *mysql.MySQLError
	if errors.As(err, &me) {
		switch me.Number {
		case erDupEntry, erDupEntryKey, erSignalRaised:
			return fmt.Errorf("%w: %v", store.ErrConflict, err)
		}
		return err
	}

	// SQLite reports it in the message.
	if strings.Contains(msg, "UNIQUE constraint failed") ||
		strings.Contains(msg, "constraint failed: UNIQUE") {
		return fmt.Errorf("%w: %v", store.ErrConflict, err)
	}
	return err
}

// Close releases the handle.
func (d *DB) Close() error { return d.db.Close() }

// restrictPermissions narrows the SQLite database files to owner-only.
//
// SQLite creates them honoring the process umask, which on most systems yields
// world-readable files. These hold financial records and password hashes, so
// they are tightened explicitly rather than left to the ambient umask. The WAL
// and shared-memory sidecars carry the same data and get the same treatment.
//
// A no-op on MariaDB, which keeps its data inside a server whose file layout is
// the database administrator's concern, not this application's.
func (d *DB) restrictPermissions() error {
	if d.path == "" {
		return nil
	}
	if strings.HasPrefix(d.path, ":memory:") || strings.Contains(d.path, "mode=memory") {
		return nil
	}
	for _, p := range []string{d.path, d.path + "-wal", d.path + "-shm"} {
		// A sidecar may not exist yet; that is not an error.
		if _, err := os.Stat(p); err != nil {
			continue
		}
		if err := os.Chmod(p, 0o600); err != nil {
			return fmt.Errorf("sqldb: restrict permissions on %s: %w", p, err)
		}
	}
	return nil
}
