package sqldb

import (
	"context"
	"database/sql"
	"fmt"
	"io/fs"
	"strings"

	_ "github.com/go-sql-driver/mysql" // pure Go, so CGO stays off (DEPLOY-04)
)

// mariaDialect talks to MariaDB or MySQL (DEPLOY-03).
//
// It exists for a deployment that already runs one, or that wants the database
// somewhere other than the application's disk. SQLite remains the default and
// the recommendation: a household instance gains nothing from a second server
// process, and loses the property that a backup is one file.
type mariaDialect struct{}

func (mariaDialect) name() string       { return "mariadb" }
func (mariaDialect) driverName() string { return "mysql" }

func (mariaDialect) migrations() (fs.FS, error) {
	return fs.Sub(migrationFS, "migrations/mariadb")
}

func (mariaDialect) configure(ctx context.Context, db *sql.DB) error {
	// A real server handles concurrent writers, so unlike SQLite this is not
	// pinned to one connection. The ceiling is low all the same: this is a
	// household application, and an idle pool of dozens would be holding server
	// resources for load that never arrives.
	db.SetMaxOpenConns(10)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(0)

	// The session settings the ledger's guarantees depend on, applied to every
	// connection through the DSN rather than here -- see mariaDSN. This hook
	// stays for pool configuration only.
	return nil
}

func (mariaDialect) migrationsTable(ctx context.Context, db *sql.DB) error {
	// VARCHAR rather than TEXT: MySQL cannot index a TEXT column without a
	// prefix length, and a primary key is an index.
	_, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version    VARCHAR(191) NOT NULL PRIMARY KEY,
			applied_at VARCHAR(32)  NOT NULL
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin`)
	if err != nil {
		return fmt.Errorf("mariadb: create schema_migrations: %w", err)
	}
	return nil
}

// splitStatements divides a migration into individual statements.
//
// The MySQL protocol executes one statement per call unless multi-statement
// mode is enabled, and enabling that on a connection the application also uses
// for queries widens SQL injection from "impossible" to "one bug away", which
// is not a trade worth making for a startup path.
//
// Splitting on semicolons is wrong in general and wrong here specifically: a
// CREATE TRIGGER body contains them. So the migrations for this dialect wrap
// each trigger in an explicit delimiter block, and this understands that one
// construct rather than trying to parse SQL.
func (mariaDialect) splitStatements(body string) []string {
	var (
		out       []string
		current   strings.Builder
		delimiter = ";"
	)
	flush := func() {
		if s := strings.TrimSpace(current.String()); s != "" {
			out = append(out, s)
		}
		current.Reset()
	}

	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)

		// A DELIMITER directive is client-side syntax, never sent to the
		// server. It is how a trigger body's semicolons are escaped.
		if rest, ok := cutPrefixFold(trimmed, "DELIMITER "); ok {
			flush()
			if d := strings.TrimSpace(rest); d != "" {
				delimiter = d
			}
			continue
		}
		// A comment line carries no statement text and can hold a stray
		// semicolon in prose, which would split a statement in half.
		if strings.HasPrefix(trimmed, "--") {
			continue
		}

		current.WriteString(line)
		current.WriteString("\n")

		if strings.HasSuffix(trimmed, delimiter) {
			// Drop the terminator itself; the driver does not want it.
			s := strings.TrimSpace(current.String())
			s = strings.TrimSuffix(s, delimiter)
			current.Reset()
			if s = strings.TrimSpace(s); s != "" {
				out = append(out, s)
			}
		}
	}
	flush()
	return out
}

// cutPrefixFold is strings.CutPrefix, case-insensitively, for SQL keywords that
// a migration might write in either case.
func cutPrefixFold(s, prefix string) (string, bool) {
	if len(s) >= len(prefix) && strings.EqualFold(s[:len(prefix)], prefix) {
		return s[len(prefix):], true
	}
	return "", false
}

// MySQL server error numbers this package reacts to.
const (
	erDupEntry     = 1062 // duplicate key
	erDupEntryKey  = 1586 // duplicate entry for a named key
	erSignalRaised = 1644 // an unhandled user-defined SIGNAL: the append-only triggers
)

// lockRows takes a write lock on the matched rows for the rest of the
// transaction. It is what makes a guarded check-then-act -- the last-admin
// guard, above all -- correct on a server that runs transactions in parallel,
// where SQLite's single-writer serialization does not exist to lean on.
func (mariaDialect) lockRows() string { return " FOR UPDATE" }
