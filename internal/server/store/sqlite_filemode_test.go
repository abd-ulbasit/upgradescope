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
