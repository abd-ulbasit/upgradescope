package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"io/fs"
	"slices"
	"time"
)

// migrationDialect carries the only driver-specific piece of Migrate: the
// placeholder style of its two bookkeeping queries. The migration files
// themselves are placeholder-free DDL/DML, so everything else is shared.
type migrationDialect struct {
	checkStmt  string // SELECT COUNT(*) ... WHERE version = <ph1>
	insertStmt string // INSERT INTO schema_migrations ... VALUES (<ph1>, <ph2>)
}

var (
	sqliteMigrations = migrationDialect{
		checkStmt:  `SELECT COUNT(*) FROM schema_migrations WHERE version = ?`,
		insertStmt: `INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)`,
	}
	postgresMigrations = migrationDialect{
		checkStmt:  `SELECT COUNT(*) FROM schema_migrations WHERE version = $1`,
		insertStmt: `INSERT INTO schema_migrations (version, applied_at) VALUES ($1, $2)`,
	}
)

// Migrate applies every "*.sql" file in fsys that has not been applied yet,
// in lexicographic filename order. Each migration runs in its own
// transaction and its filename is recorded in schema_migrations inside that
// same transaction, so a failed migration leaves no trace. Running Migrate
// again is a no-op for already-recorded files. Returns the filenames
// applied on this run.
//
// Migrate speaks '?' placeholders (SQLite); the Postgres store calls
// migratePostgres, which runs migrate with postgresMigrations under an
// advisory lock. applied_at stays TEXT (RFC 3339) in both backends — it is
// bookkeeping written and read only by this file.
func Migrate(ctx context.Context, db *sql.DB, fsys fs.FS) ([]string, error) {
	return migrate(ctx, db, fsys, sqliteMigrations)
}

// migrationConn is what migrate needs from its connection: *sql.DB for
// SQLite, one pinned *sql.Conn for Postgres (the advisory lock is held by
// a session, so every migration must run on the session that holds it).
type migrationConn interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	BeginTx(ctx context.Context, opts *sql.TxOptions) (*sql.Tx, error)
}

// pgMigrationLockKey is the pg_advisory_lock key that serializes Postgres
// migration runs across server replicas ("upgradescope" as an int64 tag).
const pgMigrationLockKey int64 = 0x7570677363707365

// migratePostgres runs migrate under a session-level advisory lock held on
// one dedicated connection for the whole run. Replicas that start together
// otherwise race: both see 0001 unapplied, both run its CREATE TABLEs, and
// one fails. The waiting replica blocks on the lock, then finds every
// migration recorded and applies nothing. The lock is released explicitly,
// and Postgres also drops it if the session dies.
func migratePostgres(ctx context.Context, db *sql.DB, fsys fs.FS) ([]string, error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("migration connection: %w", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `SELECT pg_advisory_lock($1)`, pgMigrationLockKey); err != nil {
		return nil, fmt.Errorf("acquire migration lock: %w", err)
	}
	applied, err := migrate(ctx, conn, fsys, postgresMigrations)
	// Unlock on a fresh context: ctx may be done, and a session returned to
	// the pool still holding the lock would block every later migrator.
	if _, uerr := conn.ExecContext(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock($1)`, pgMigrationLockKey); uerr != nil {
		// Closing the session is the only other way to drop the lock.
		_ = conn.Raw(func(any) error { return driver.ErrBadConn })
		if err == nil {
			err = fmt.Errorf("release migration lock: %w", uerr)
		}
	}
	return applied, err
}

func migrate(ctx context.Context, db migrationConn, fsys fs.FS, d migrationDialect) ([]string, error) {
	if _, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version    TEXT PRIMARY KEY,
			applied_at TEXT NOT NULL
		)`); err != nil {
		return nil, fmt.Errorf("create schema_migrations: %w", err)
	}
	names, err := fs.Glob(fsys, "*.sql")
	if err != nil {
		return nil, fmt.Errorf("glob migrations: %w", err)
	}
	slices.Sort(names)
	var applied []string
	for _, name := range names {
		ok, err := applyMigration(ctx, db, fsys, name, d)
		if err != nil {
			return applied, err
		}
		if ok {
			applied = append(applied, name)
		}
	}
	return applied, nil
}

func applyMigration(ctx context.Context, db migrationConn, fsys fs.FS, name string, d migrationDialect) (bool, error) {
	var n int
	if err := db.QueryRowContext(ctx, d.checkStmt, name).Scan(&n); err != nil {
		return false, fmt.Errorf("check migration %s: %w", name, err)
	}
	if n > 0 {
		return false, nil
	}
	src, err := fs.ReadFile(fsys, name)
	if err != nil {
		return false, fmt.Errorf("read migration %s: %w", name, err)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin migration %s: %w", name, err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit
	if _, err := tx.ExecContext(ctx, string(src)); err != nil {
		return false, fmt.Errorf("apply migration %s: %w", name, err)
	}
	if _, err := tx.ExecContext(ctx, d.insertStmt,
		name, formatTime(time.Now())); err != nil {
		return false, fmt.Errorf("record migration %s: %w", name, err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit migration %s: %w", name, err)
	}
	return true, nil
}
