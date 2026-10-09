package store

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// The chart runs the server with a read-only root filesystem and, until
// #242, only /data writable. A large delete (a retention prune of a backlog,
// `clusters delete` of a big cluster) makes SQLite spill a temp file, and
// with no writable temp directory it fails with SQLITE_IOERR_GETTEMPPATH,
// extended code 6410 ("disk I/O error"). The chart now mounts an emptyDir at
// /tmp and sets SQLITE_TMPDIR to it. These tests run the operation in a
// child process, because modernc's libc reads the environment once, at the
// process's first SQLite use, so a t.Setenv in the parent has no effect.

const (
	tempDirChildEnv  = "UPGRADESCOPE_STORE_TEMPDIR_CHILD"
	tempDirChildDB   = "UPGRADESCOPE_STORE_TEMPDIR_DB"
	backlogSnapshots = 150 // x 400 KB = 60 MB, the size the issue failed at
	backlogSnapBytes = 400 << 10
)

// seedBacklog writes a database whose "big" cluster has backlogSnapshots old
// snapshots of backlogSnapBytes each, then one recent snapshot (its latest,
// which retention spares).
func seedBacklog(t *testing.T, path string) {
	t.Helper()
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	ctx := context.Background()
	now := time.Now().UTC()
	id, err := s.UpsertCluster(ctx, Cluster{Name: "big", ClusterUID: "uid-big", FirstSeen: now, LastSeen: now})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i <= backlogSnapshots; i++ {
		at := now.Add(-400 * 24 * time.Hour).Add(time.Duration(i) * time.Minute)
		if i == backlogSnapshots {
			at = now
		}
		pad := strings.Repeat(fmt.Sprintf("%08d", i), backlogSnapBytes/8)
		if _, _, err := s.InsertSnapshot(ctx, Snapshot{
			ClusterID: id, Hash: fmt.Sprintf("h%d", i), KBVersion: "kb-1", AgentVersion: "v0.2.0",
			ReceivedAt: at, Inventory: []byte(`{"pad":"` + pad + `"}`),
		}); err != nil {
			t.Fatalf("InsertSnapshot %d: %v", i, err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestTempDirChild is the child process of the tests below, not a test of its
// own: it opens the database named by tempDirChildDB and runs the operation
// named by tempDirChildEnv, printing "ok <detail>" or the error.
func TestTempDirChild(t *testing.T) {
	op := os.Getenv(tempDirChildEnv)
	if op == "" {
		t.Skip("child of the temp-dir tests")
	}
	s, err := Open(os.Getenv(tempDirChildDB))
	if err != nil {
		fmt.Printf("error open: %v\n", err)
		os.Exit(1)
	}
	defer s.Close()
	switch op {
	case "prune":
		res, err := s.Prune(context.Background(), time.Now().UTC().Add(-90*24*time.Hour))
		if err != nil {
			fmt.Printf("error %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("ok pruned %d snapshots\n", res.Snapshots)
	case "delete":
		if err := s.DeleteCluster(context.Background(), "big"); err != nil {
			fmt.Printf("error %v\n", err)
			os.Exit(1)
		}
		fmt.Println("ok deleted")
	default:
		fmt.Printf("error unknown op %q\n", op)
		os.Exit(1)
	}
}

// runChild runs the operation in a child of the test binary. sandboxDeny,
// on macOS, runs it under sandbox-exec with every write denied except under
// allow, which is how a read-only root filesystem looks to SQLite.
func runChild(t *testing.T, op, dbPath string, env []string, allow []string) (string, error) {
	t.Helper()
	args := []string{"-test.run=^TestTempDirChild$", "-test.count=1"}
	var cmd *exec.Cmd
	if allow != nil {
		var b strings.Builder
		b.WriteString("(version 1)(allow default)(deny file-write*)")
		for _, d := range allow {
			// /private/var vs /var: the sandbox matches the resolved path.
			if r, err := filepath.EvalSymlinks(d); err == nil {
				d = r
			}
			fmt.Fprintf(&b, `(allow file-write* (subpath %q))`, d)
		}
		b.WriteString(`(allow file-write* (literal "/dev/null"))`)
		cmd = exec.Command("sandbox-exec", append([]string{"-p", b.String(), os.Args[0]}, args...)...)
	} else {
		cmd = exec.Command(os.Args[0], args...)
	}
	// A clean environment: no TMPDIR from the developer's shell.
	cmd.Env = append([]string{
		"PATH=" + os.Getenv("PATH"),
		tempDirChildEnv + "=" + op,
		tempDirChildDB + "=" + dbPath,
	}, env...)
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func sandboxExecAvailable() bool {
	if runtime.GOOS != "darwin" {
		return false
	}
	_, err := exec.LookPath("sandbox-exec")
	return err == nil
}

func countSnapshots(t *testing.T, path string) int {
	t.Helper()
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM snapshots`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// With SQLITE_TMPDIR on a writable directory (what the chart sets), a 60 MB
// prune and a 60 MB cluster delete succeed.
func TestLargeDeleteWithSQLiteTmpDir(t *testing.T) {
	if testing.Short() {
		t.Skip("seeds 60 MB")
	}
	for _, op := range []string{"prune", "delete"} {
		t.Run(op, func(t *testing.T) {
			db := filepath.Join(t.TempDir(), "u.sqlite")
			seedBacklog(t, db)
			tmp := t.TempDir()
			out, err := runChild(t, op, db, []string{"SQLITE_TMPDIR=" + tmp}, nil)
			if err != nil || !strings.HasPrefix(out, "ok") {
				t.Fatalf("%s with SQLITE_TMPDIR=%s: %v\n%s", op, tmp, err, out)
			}
			want := 1 // the latest snapshot
			if op == "delete" {
				want = 0
			}
			if got := countSnapshots(t, db); got != want {
				t.Errorf("snapshots left after %s = %d, want %d", op, got, want)
			}
		})
	}
}

// The read-only root filesystem, emulated with sandbox-exec (macOS; the
// sandbox denies every write except under the database directory and the
// directory SQLITE_TMPDIR names): without a temp directory the delete fails
// with error 6410, the failure #242 reports, and with SQLITE_TMPDIR on the
// one writable directory it succeeds. The writable database directory is
// not the temp directory: SQLite looks in SQLITE_TMPDIR, TMPDIR, /var/tmp,
// /usr/tmp, /tmp and the working directory, never next to the database.
func TestLargeDeleteOnReadOnlyRootFilesystem(t *testing.T) {
	if !sandboxExecAvailable() {
		t.Skip("needs macOS sandbox-exec to emulate a read-only root filesystem")
	}
	if testing.Short() {
		t.Skip("seeds 60 MB")
	}
	for _, op := range []string{"prune", "delete"} {
		t.Run(op, func(t *testing.T) {
			dir := t.TempDir()
			db := filepath.Join(dir, "u.sqlite")
			seedBacklog(t, db)
			tmp := filepath.Join(dir, "tmp")
			if err := os.Mkdir(tmp, 0o700); err != nil {
				t.Fatal(err)
			}
			out, err := runChild(t, op, db, nil, []string{dir})
			if err == nil || !strings.Contains(out, "6410") {
				t.Fatalf("%s with no writable temp directory: want error 6410 (the bug), got err=%v\n%s", op, err, out)
			}
			out, err = runChild(t, op, db, []string{"SQLITE_TMPDIR=" + tmp}, []string{dir})
			if err != nil || !strings.HasPrefix(out, "ok") {
				t.Fatalf("%s with SQLITE_TMPDIR=%s: %v\n%s", op, tmp, err, out)
			}
		})
	}
}
