package sqldb

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"github.com/go-sql-driver/mysql"
)

// mariaCounter names each test's throwaway database uniquely within a run,
// without Date.now or randomness. A plain int is enough: tests within a package
// run in one process, and t.Name() disambiguates across it.
var mariaCounter int

// newMariaTestDB creates a fresh, empty database on the MariaDB server named by
// dsn, migrates it, and drops it when the test ends.
//
// The suite is destructive -- it truncates, it relies on auto-increment
// starting at 1 -- so every test gets its own database rather than sharing one
// and racing. The name is derived from the test so a failure points at the
// database that held its state, had it not been dropped.
func newMariaTestDB(t *testing.T, adminDSN string) *DB {
	t.Helper()

	cfg, err := mysql.ParseDSN(adminDSN)
	if err != nil {
		t.Fatalf("BITT_TEST_MARIADB_DSN: %v", err)
	}
	base := cfg.DBName

	mariaCounter++
	// A name unique to this test and bounded to MySQL's 64-char identifier
	// limit. Non-identifier characters in a test name become underscores.
	name := fmt.Sprintf("%s_t%d_%s", base, mariaCounter, sanitizeIdent(t.Name()))
	if len(name) > 60 {
		name = name[:60]
	}

	admin, err := sql.Open("mysql", adminDSN)
	if err != nil {
		t.Fatalf("open admin: %v", err)
	}
	defer func() { _ = admin.Close() }()

	ctx := context.Background()
	// Identifiers cannot be parameterized, so the name is built from a strict
	// charset (sanitizeIdent) rather than from arbitrary input -- there is no
	// user data here, only test names, and they are filtered regardless.
	if _, err := admin.ExecContext(ctx, "DROP DATABASE IF EXISTS `"+name+"`"); err != nil {
		t.Fatalf("drop stale test db: %v", err)
	}
	if _, err := admin.ExecContext(ctx,
		"CREATE DATABASE `"+name+"` CHARACTER SET utf8mb4 COLLATE utf8mb4_bin"); err != nil {
		t.Fatalf("create test db: %v", err)
	}

	testCfg := *cfg
	testCfg.DBName = name
	db, err := Open(Options{
		Driver:             MariaDB,
		DSN:                testCfg.FormatDSN(),
		AppendOnlyTriggers: true,
	})
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	t.Cleanup(func() {
		_ = db.Close()
		cleanup, err := sql.Open("mysql", adminDSN)
		if err != nil {
			return
		}
		defer func() { _ = cleanup.Close() }()
		_, _ = cleanup.ExecContext(context.Background(), "DROP DATABASE IF EXISTS `"+name+"`")
	})
	return db
}

// sanitizeIdent reduces a test name to the characters allowed in an unquoted
// MySQL identifier, lower-cased.
func sanitizeIdent(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}
