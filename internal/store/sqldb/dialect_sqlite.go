package sqldb

import (
	"context"
	"database/sql"
	"fmt"
	"io/fs"
	"strings"

	_ "modernc.org/sqlite" // pure-Go driver: no cgo, so the binary stays static (DEPLOY-04)
)

// sqliteDialect is the default backend: one file, no server, nothing to
// operate. It is what a household instance should run, and what every other
// phase of this project was built and tested on.
type sqliteDialect struct{}

func (sqliteDialect) name() string       { return "sqlite" }
func (sqliteDialect) driverName() string { return "sqlite" }

func (sqliteDialect) migrations() (fs.FS, error) {
	return fs.Sub(migrationFS, "migrations/sqlite")
}

func (sqliteDialect) configure(ctx context.Context, db *sql.DB) error {
	// SQLite permits one writer at a time. Serializing connections avoids
	// spurious lock contention at the cost of throughput this deployment scale
	// will never notice.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)
	return nil
}

func (sqliteDialect) migrationsTable(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version    TEXT PRIMARY KEY,
			applied_at TEXT NOT NULL
		)`)
	if err != nil {
		return fmt.Errorf("sqlite: create schema_migrations: %w", err)
	}
	return nil
}

// splitStatements returns the file whole. The SQLite driver executes a
// multi-statement string in one call, which keeps a migration atomic in the
// enclosing transaction with no parsing on our side.
func (sqliteDialect) splitStatements(body string) []string {
	if strings.TrimSpace(body) == "" {
		return nil
	}
	return []string{body}
}

// lockRows is empty: SQLite serializes writers through the single connection
// this package pins it to, so a check-then-act transaction is already atomic
// against any other, and SQLite has no FOR UPDATE syntax to add.
func (sqliteDialect) lockRows() string { return "" }
