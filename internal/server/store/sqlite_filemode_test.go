//go:build unix

package store

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// The database holds every inventory, evaluation and queued notification,
// and under the usual umask 022 it, its -wal and its -shm were created
// world-readable (#126 DB-09). Open creates the database 0600, which
// SQLite copies to -wal and -shm, whatever the umask, and tightens an
// existing world-readable one.
func TestOpenStoreFileMode(t *testing.T) {
	defer syscall.Umask(syscall.Umask(0)) // the most permissive umask: nothing masks a loose mode

	check := func(path string) {
		t.Helper()
		for _, p := range []string{path, path + "-wal", path + "-shm"} {
			fi, err := os.Stat(p)
			if err != nil {
				t.Fatal(err)
			}
			if mode := fi.Mode().Perm(); mode != 0o600 {
				t.Errorf("%s mode = %o, want 600", filepath.Base(p), mode)
			}
		}
	}

	fresh := filepath.Join(t.TempDir(), "fresh.db")
	s, err := Open(fresh)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	check(fresh)

	// A database an older version created 0644, with its -wal and -shm.
	old := filepath.Join(t.TempDir(), "old.db")
	for _, p := range []string{old, old + "-wal", old + "-shm"} {
		if err := os.WriteFile(p, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	s2, err := Open(old)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s2.Close() })
	check(old)
}

// The database is opened through a "file:" URI, where '%', '?' and '#' mean
// something, and SQLite decodes %XX in the path: `a%3Fb.db` created a real
// `a?b.db` 0644 next to an empty 0600 decoy `a%3Fb.db` (#195). Whatever
// the name, Open creates exactly the file named, with its -wal and -shm,
// all 0600, and nothing else.
func TestOpenUsesExactPath(t *testing.T) {
	defer syscall.Umask(syscall.Umask(0))

	for _, name := range []string{
		"a%3Fb.db", "c%23d.db", "x%20y.db", "g%00h.db", "e%41f.db", "100%.db", "%.db",
		"x y.db", "q?b.db", "a#b.db", "k.db?mode=ro", "a%b?c#d%25.db", "-j.db",
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			s, err := Open(filepath.Join(dir, name))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = s.Close() })

			want := map[string]bool{name: true, name + "-wal": true, name + "-shm": true}
			ents, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			for _, e := range ents {
				if !want[e.Name()] {
					t.Errorf("unexpected file %q beside the database", e.Name())
				}
			}
			for p := range want {
				fi, err := os.Stat(filepath.Join(dir, p))
				if err != nil {
					t.Errorf("%s: %v", p, err)
					continue
				}
				if mode := fi.Mode().Perm(); mode != 0o600 {
					t.Errorf("%s mode = %o, want 600", p, mode)
				}
			}
			// The named file holds the schema: it is the database SQLite opened.
			var n int
			if err := s.db.QueryRow(`SELECT count(*) FROM schema_migrations`).Scan(&n); err != nil || n == 0 {
				t.Errorf("schema_migrations: n=%d err=%v", n, err)
			}
		})
	}

	t.Run("directory with escapes", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "p%20q?r#s")
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		s, err := Open(filepath.Join(dir, "db.sqlite"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = s.Close() })
		if fi, err := os.Stat(filepath.Join(dir, "db.sqlite")); err != nil || fi.Mode().Perm() != 0o600 || fi.Size() == 0 {
			t.Errorf("db.sqlite: %v, %v", fi, err)
		}
	})

	// "file://" + "//x" must not read the first path element as a host, and
	// a relative "a:b.db" must not read "a" as a scheme.
	t.Run("absolute path starting with //", func(t *testing.T) {
		dir := t.TempDir()
		s, err := Open("/" + filepath.Join(dir, "db.sqlite"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = s.Close() })
		if fi, err := os.Stat(filepath.Join(dir, "db.sqlite")); err != nil || fi.Mode().Perm() != 0o600 || fi.Size() == 0 {
			t.Errorf("db.sqlite: %v, %v", fi, err)
		}
	})

	t.Run("relative with a colon", func(t *testing.T) {
		t.Chdir(t.TempDir())
		s, err := Open("a:b.db")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = s.Close() })
		if fi, err := os.Stat("a:b.db"); err != nil || fi.Size() == 0 || fi.Mode().Perm() != 0o600 {
			t.Errorf("a:b.db: %v, %v", fi, err)
		}
	})

	t.Run("relative", func(t *testing.T) {
		t.Chdir(t.TempDir())
		s, err := Open("r%3Fs.db")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = s.Close() })
		if fi, err := os.Stat("r%3Fs.db"); err != nil || fi.Size() == 0 || fi.Mode().Perm() != 0o600 {
			t.Errorf("r%%3Fs.db: %v, %v", fi, err)
		}
		if _, err := os.Stat("r?s.db"); err == nil {
			t.Error("decoded sibling r?s.db exists")
		}
	})
}
