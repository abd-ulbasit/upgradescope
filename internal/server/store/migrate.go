package store

import (
	"context"
	"database/sql"
	"fmt"
	"io/fs"
	"slices"
	"time"
)

// Migrate applies every "*.sql" file in fsys that has not been applied yet,
// in lexicographic filename order. Each migration runs in its own
// transaction and its filename is recorded in schema_migrations inside that
// same transaction, so a failed migration leaves no trace. Running Migrate
// again is a no-op for already-recorded files. Returns the filenames
// applied on this run.
//
// Driver-agnostic on purpose (plain *sql.DB): P3's Postgres store reuses it.
func Migrate(ctx context.Context, db *sql.DB, fsys fs.FS) ([]string, error) {
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
		ok, err := applyMigration(ctx, db, fsys, name)
		if err != nil {
			return applied, err
		}
		if ok {
			applied = append(applied, name)
		}
	}
	return applied, nil
}

func applyMigration(ctx context.Context, db *sql.DB, fsys fs.FS, name string) (bool, error) {
	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM schema_migrations WHERE version = ?`, name).Scan(&n); err != nil {
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
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)`,
		name, formatTime(time.Now())); err != nil {
		return false, fmt.Errorf("record migration %s: %w", name, err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit migration %s: %w", name, err)
	}
	return true, nil
}
