package sqldb

import (
	"context"
	"database/sql"
	"fmt"
	"io/fs"
)

// Driver names the database this store is talking to (DEPLOY-03).
type Driver string

const (
	// SQLite is the default and the one a household instance should use: a
	// single file, no server, no operational surface at all.
	SQLite Driver = "sqlite"
	// MariaDB is for a deployment that already runs one, or that wants the
	// database somewhere other than the application's disk.
	MariaDB Driver = "mariadb"
)

// Drivers lists what is selectable, for error messages and documentation.
func Drivers() []Driver { return []Driver{SQLite, MariaDB} }

// Valid reports whether d names a supported backend.
func (d Driver) Valid() bool { return d == SQLite || d == MariaDB }

// dialect is everything the two backends do not share.
//
// The deliberate shape of this interface is how small it is. The queries in
// accounts.go, tabs.go, and entries.go are plain parameterized SQL and are
// identical on both -- which is not luck: the schema was written for it, with
// timestamps stored as ISO-8601 TEXT so they compare lexicographically
// everywhere, no dialect typing tricks, and no ON CONFLICT or RETURNING. What
// differs is the DDL, the trigger syntax, and how each driver spells a
// constraint violation, and that is what lives here.
//
// If this interface starts growing methods that return fragments of SELECTs,
// the abstraction has failed and a second implementation would be the honest
// answer instead.
type dialect interface {
	// name identifies the dialect in errors.
	name() string
	// driverName is what database/sql was registered with.
	driverName() string
	// migrations is the DDL set for this backend, rooted at the directory
	// holding the numbered files.
	migrations() (fs.FS, error)
	// configure applies connection settings that only make sense per backend --
	// pool limits, session variables.
	configure(ctx context.Context, db *sql.DB) error
	// migrationsTable creates the bookkeeping table if it does not exist.
	migrationsTable(ctx context.Context, db *sql.DB) error
	// splitStatements divides one migration file into statements to execute.
	//
	// SQLite's driver accepts a whole file in one Exec; MySQL's protocol does
	// not, and a CREATE TRIGGER body contains the semicolons that make naive
	// splitting wrong. Each dialect handles its own.
	splitStatements(body string) []string
	// lockRows is the clause that takes a write lock on the rows a SELECT
	// matches, so a check-then-act inside a transaction serializes against a
	// concurrent one. " FOR UPDATE" on MariaDB; empty on SQLite, whose single
	// writer already serializes every transaction and which has no such syntax.
	lockRows() string
}

// dialectFor returns the implementation for a driver.
func dialectFor(d Driver) (dialect, error) {
	switch d {
	case SQLite, "":
		return sqliteDialect{}, nil
	case MariaDB:
		return mariaDialect{}, nil
	}
	return nil, fmt.Errorf("sqldb: unknown driver %q (want one of %v)", d, Drivers())
}
