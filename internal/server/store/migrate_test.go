package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"slices"
	"testing"
	"testing/fstest"

	_ "modernc.org/sqlite"
)

// openRawDB opens a plain (un-migrated, no pragmas) SQLite database in a
// temp dir. A file database is used deliberately: an in-memory DSN gives
// every pooled connection its own empty database.
func openRawDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "migrate-test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// migFS: 0002 depends on 0001's table, proving lexicographic apply order.
// The README must be ignored (only *.sql counts).
func migFS() fstest.MapFS {
	return fstest.MapFS{
		"0001_users.sql": {Data: []byte(`CREATE TABLE users (id INTEGER PRIMARY KEY, name TEXT NOT NULL);`)},
		"0002_seed.sql": {Data: []byte(`INSERT INTO users (name) VALUES ('alpha');
CREATE TABLE widgets (id INTEGER PRIMARY KEY);`)},
		"README.md": {Data: []byte("not a migration")},
	}
}

func TestMigrateAppliesAllInOrder(t *testing.T) {
	db := openRawDB(t)
	applied, err := Migrate(context.Background(), db, migFS())
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	want := []string{"0001_users.sql", "0002_seed.sql"}
	if !slices.Equal(applied, want) {
		t.Errorf("applied = %v, want %v", applied, want)
	}
	var users, recorded int
	if err := db.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&users); err != nil {
		t.Fatalf("count users: %v", err)
	}
	if users != 1 {
		t.Errorf("users rows = %d, want 1 (seed must run after create)", users)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&recorded); err != nil {
		t.Fatalf("count schema_migrations: %v", err)
	}
	if recorded != 2 {
		t.Errorf("schema_migrations rows = %d, want 2", recorded)
	}
}

func TestMigrateIdempotent(t *testing.T) {
	db := openRawDB(t)
	fsys := migFS()
	if _, err := Migrate(context.Background(), db, fsys); err != nil {
		t.Fatalf("first run: %v", err)
	}
	applied, err := Migrate(context.Background(), db, fsys)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if len(applied) != 0 {
		t.Errorf("second run applied %v, want nothing", applied)
	}
	var users int
	if err := db.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&users); err != nil {
		t.Fatalf("count users: %v", err)
	}
	if users != 1 {
		t.Errorf("users rows = %d, want 1 (seed must not run twice)", users)
	}
}

func TestMigrateAppliesOnlyNewFiles(t *testing.T) {
	db := openRawDB(t)
	first := fstest.MapFS{"0001_users.sql": migFS()["0001_users.sql"]}
	if _, err := Migrate(context.Background(), db, first); err != nil {
		t.Fatalf("first run: %v", err)
	}
	applied, err := Migrate(context.Background(), db, migFS())
	if err != nil {
		t.Fatalf("second run with extra file: %v", err)
	}
	if !slices.Equal(applied, []string{"0002_seed.sql"}) {
		t.Errorf("applied = %v, want [0002_seed.sql]", applied)
	}
}

func TestMigrateRollsBackFailedMigration(t *testing.T) {
	db := openRawDB(t)
	bad := fstest.MapFS{
		"0001_bad.sql": {Data: []byte(`CREATE TABLE good (id INTEGER PRIMARY KEY);
INSERT INTO does_not_exist VALUES (1);`)},
	}
	if _, err := Migrate(context.Background(), db, bad); err == nil {
		t.Fatal("Migrate succeeded on a failing migration")
	}
	// The CREATE from the same file must have been rolled back.
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM good`).Scan(&n); err == nil {
		t.Error("table 'good' exists after failed migration — not transactional")
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&n); err != nil {
		t.Fatalf("count schema_migrations: %v", err)
	}
	if n != 0 {
		t.Errorf("failed migration was recorded (%d rows), want 0", n)
	}
}
