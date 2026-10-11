package store

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

// The Postgres startup window (#364). OpenPostgres once ran its ping, the
// wait on the migration lock and every migration under one shared 10 s
// context, so any start that took longer died with "context deadline
// exceeded" and the kubelet restarted it into the same failure. Only the
// ping is bounded now; the startupProbe bounds the rest, as for SQLite.
// hack/pg-test.sh runs these (TestPostgres*).

// pgAdmin opens a pool on the test server itself (not a test schema),
// closed on cleanup.
func pgAdmin(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("pgx", os.Getenv("UPGRADESCOPE_PG_TEST_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// A replica that starts while another holds the migration lock for 15 s
// (another replica migrating) queues on it and then opens, instead of
// failing at 10 s. Mutation check: restoring the shared 10 s context
// fails this test with "acquire migration lock: ... deadline exceeded".
func TestPostgresOpenWaitsForMigrationLock(t *testing.T) {
	dsn := pgTestSchema(t, "lockwait")
	ctx := context.Background()
	holder, err := pgAdmin(t).Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	if _, err := holder.ExecContext(ctx, `SELECT pg_advisory_lock($1)`, pgMigrationLockKey); err != nil {
		t.Fatal(err)
	}
	const hold = 15 * time.Second
	start := time.Now()
	released := time.AfterFunc(hold, func() {
		_, _ = holder.ExecContext(ctx, `SELECT pg_advisory_unlock($1)`, pgMigrationLockKey)
	})
	defer released.Stop()

	p, err := OpenPostgres(ctx, dsn)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("OpenPostgres while the migration lock was held: %v (after %s)", err, elapsed.Round(time.Millisecond))
	}
	defer p.Close()
	if elapsed < hold {
		t.Errorf("OpenPostgres returned after %s, before the lock was released at %s: it did not wait on the lock", elapsed, hold)
	}
	var n int
	if err := p.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations`).Scan(&n); err != nil || n == 0 {
		t.Errorf("schema_migrations rows = (%d, %v), want the embedded migrations applied", n, err)
	}
}

// A migration that runs for 12 s applies and is recorded: the startup
// window is the startupProbe's, not a fixed 10 s.
func TestPostgresOpenSlowMigrationApplies(t *testing.T) {
	dsn := pgTestSchema(t, "slowmig")
	fsys := fstest.MapFS{
		"0001_slow.sql": {Data: []byte(`SELECT pg_sleep(12)`)},
	}
	start := time.Now()
	p, err := openPostgres(context.Background(), dsn, fsys)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("openPostgres with a 12 s migration: %v (after %s)", err, elapsed.Round(time.Millisecond))
	}
	defer p.Close()
	if elapsed < 12*time.Second {
		t.Errorf("openPostgres returned after %s, want the 12 s migration to have run", elapsed)
	}
	var n int
	if err := p.db.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM schema_migrations WHERE version = '0001_slow.sql'`).Scan(&n); err != nil || n != 1 {
		t.Errorf("0001_slow.sql recorded (%d, %v), want once", n, err)
	}
}

// Cancelling the open (serve's SIGTERM) returns promptly with
// context.Canceled; the cancelled migration is not recorded, so the next
// start re-runs it; the statement is gone server-side and the migration
// lock is free within 10 s; and the migration session carries
// client_connection_check_interval. In this process the server-side stop
// comes from the CancelRequest pgx sends while closing the abandoned
// connection; a process that exits at once (serve after SIGTERM, a pod
// killed) may never send it, which TestPostgresKilledMigratorStopsServerSide
// covers.
func TestPostgresOpenCancelledMigrationStopsServerSide(t *testing.T) {
	dsn := pgTestSchema(t, "cancelmig")
	admin := pgAdmin(t)
	bg := context.Background()
	var version int
	if err := admin.QueryRowContext(bg, `SELECT current_setting('server_version_num')::int`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version < 140000 {
		t.Skipf("server_version_num %d: client_connection_check_interval needs PostgreSQL 14+", version)
	}
	const tag = "us364_cancelled_migration"
	fsys := fstest.MapFS{
		// Records the migration session's check interval (0001 commits in
		// its own transaction before 0002 starts).
		"0001_seen.sql": {Data: []byte(`CREATE TABLE seen AS SELECT current_setting('client_connection_check_interval') AS v`)},
		"0002_hang.sql": {Data: []byte(`SELECT pg_sleep(60) /* ` + tag + ` */`)},
	}

	ctx, cancel := context.WithCancel(bg)
	defer cancel()
	const cancelAfter = 2 * time.Second
	start := time.Now()
	cancelAt := time.AfterFunc(cancelAfter, cancel)
	defer cancelAt.Stop()
	p, err := openPostgres(ctx, dsn, fsys)
	returned := time.Now()
	if err == nil {
		_ = p.Close()
		t.Fatal("openPostgres succeeded although its context was cancelled mid-migration")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want it to wrap context.Canceled", err)
	}
	if !strings.Contains(err.Error(), "interrupted") {
		t.Errorf("err = %q, want it to say the migrations were interrupted", err)
	}
	// The cancel fires 2 s in; the open must be back within 5 s of it.
	if d := returned.Sub(start); d > cancelAfter+5*time.Second {
		t.Errorf("openPostgres returned %s after start, want within 5 s of the cancel at %s", d, cancelAfter)
	}

	deadline := returned.Add(10 * time.Second)
	for {
		var running int
		if err := admin.QueryRowContext(bg, `
			SELECT COUNT(*) FROM pg_stat_activity
			WHERE pid <> pg_backend_pid() AND query LIKE '%' || $1 || '%'`, tag).Scan(&running); err != nil {
			t.Fatal(err)
		}
		if running == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d backend(s) still running the cancelled migration 10 s after the open returned", running)
		}
		time.Sleep(200 * time.Millisecond)
	}
	// The aborted session took the migration lock with it: the next start
	// does not fail on the lock.
	var locked bool
	if err := admin.QueryRowContext(bg, `SELECT pg_try_advisory_lock($1)`, pgMigrationLockKey).Scan(&locked); err != nil {
		t.Fatal(err)
	}
	if !locked {
		t.Error("migration lock still held after the cancelled migration's backend went away")
	} else {
		_, _ = admin.ExecContext(bg, `SELECT pg_advisory_unlock($1)`, pgMigrationLockKey)
	}

	check, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer check.Close()
	var seen string
	if err := check.QueryRowContext(bg, `SELECT v FROM seen`).Scan(&seen); err != nil || seen != "2s" {
		t.Errorf("migration session's client_connection_check_interval = (%q, %v), want 2s", seen, err)
	}
	var hung int
	if err := check.QueryRowContext(bg,
		`SELECT COUNT(*) FROM schema_migrations WHERE version = '0002_hang.sql'`).Scan(&hung); err != nil || hung != 0 {
		t.Errorf("cancelled 0002_hang.sql recorded (%d, %v), want absent: the next start re-runs it", hung, err)
	}
}

// The ping keeps its own bound now that the migrations have none: a DSN
// whose host never answers (TEST-NET-1, no connect_timeout) fails within
// pgPingTimeout instead of hanging on the OS connect timeout.
// TestPostgresOpenBadDSN covers the refused-connection case.
func TestPostgresOpenUnreachableHostFailsFast(t *testing.T) {
	pgTestSchema(t, "unreach") // gate: only where a pg environment exists
	start := time.Now()
	p, err := OpenPostgres(context.Background(), "postgres://nobody:wrong@192.0.2.1:5432/none?sslmode=disable")
	elapsed := time.Since(start)
	if err == nil {
		_ = p.Close()
		t.Fatal("OpenPostgres(unreachable host) succeeded, want error")
	}
	if elapsed > pgPingTimeout+5*time.Second {
		t.Errorf("OpenPostgres(unreachable host) took %s, want it bounded by the %s ping timeout", elapsed, pgPingTimeout)
	}
}

// killedMigratorEnv names the DSN a TestPostgresKilledMigratorHelper child
// migrates; the variable's presence is what makes the helper run.
const killedMigratorEnv = "UPGRADESCOPE_PG_KILLED_MIGRATOR_DSN"

const killedMigratorTag = "us364_killed_migrator"

// TestPostgresKilledMigratorHelper is the child process of
// TestPostgresKilledMigratorStopsServerSide: it starts a 60 s migration and
// is SIGKILLed during it. It skips in every other run.
func TestPostgresKilledMigratorHelper(t *testing.T) {
	dsn := os.Getenv(killedMigratorEnv)
	if dsn == "" {
		t.Skip("child process of TestPostgresKilledMigratorStopsServerSide")
	}
	fsys := fstest.MapFS{"0001_hang.sql": {Data: []byte(`SELECT pg_sleep(60) /* ` + killedMigratorTag + ` */`)}}
	if p, err := openPostgres(context.Background(), dsn, fsys); err == nil {
		_ = p.Close()
	}
}

// A migrator killed mid-statement (SIGKILL: no CancelRequest, no
// Terminate, only the kernel closing its socket) does not leave the
// statement running server-side with its locks and the migration lock, as
// 0010's CREATE INDEX once did, failing the next start on the lock: the
// migration session's client_connection_check_interval makes the backend
// notice the closed socket and abort within seconds. Mutation check:
// dropping the SET leaves pg_sleep(60) running past the 10 s wait.
func TestPostgresKilledMigratorStopsServerSide(t *testing.T) {
	dsn := pgTestSchema(t, "killedmig")
	admin := pgAdmin(t)
	bg := context.Background()
	var version int
	if err := admin.QueryRowContext(bg, `SELECT current_setting('server_version_num')::int`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version < 140000 {
		t.Skipf("server_version_num %d: client_connection_check_interval needs PostgreSQL 14+", version)
	}
	running := func() int {
		t.Helper()
		var n int
		if err := admin.QueryRowContext(bg, `
			SELECT COUNT(*) FROM pg_stat_activity
			WHERE pid <> pg_backend_pid() AND state = 'active' AND query LIKE '%' || $1 || '%'`,
			killedMigratorTag).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := running(); n != 0 {
		t.Fatalf("%d backend(s) already running the helper's statement", n)
	}
	// A failed run must not leave the 60 s statement holding the
	// migration lock for the tests after it.
	t.Cleanup(func() {
		_, _ = admin.ExecContext(bg, `
			SELECT pg_terminate_backend(pid) FROM pg_stat_activity
			WHERE pid <> pg_backend_pid() AND query LIKE '%' || $1 || '%'`, killedMigratorTag)
	})

	child := exec.Command(os.Args[0], "-test.run=^TestPostgresKilledMigratorHelper$", "-test.count=1")
	child.Env = append(os.Environ(), killedMigratorEnv+"="+dsn)
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- child.Wait() }()
	defer func() { _ = child.Process.Kill() }()

	for deadline := time.Now().Add(30 * time.Second); running() == 0; time.Sleep(100 * time.Millisecond) {
		select {
		case err := <-exited:
			t.Fatalf("helper exited before its migration started: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("helper's migration did not start within 30 s")
		}
	}
	if err := child.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	<-exited
	killed := time.Now()

	for running() != 0 {
		if time.Since(killed) > 10*time.Second {
			t.Fatal("the killed migrator's statement still runs server-side 10 s after the kill")
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Logf("server-side statement gone %s after the kill", time.Since(killed).Round(10*time.Millisecond))
	var locked bool
	if err := admin.QueryRowContext(bg, `SELECT pg_try_advisory_lock($1)`, pgMigrationLockKey).Scan(&locked); err != nil {
		t.Fatal(err)
	}
	if !locked {
		t.Error("migration lock still held after the killed migrator's backend went away")
	} else {
		_, _ = admin.ExecContext(bg, `SELECT pg_advisory_unlock($1)`, pgMigrationLockKey)
	}
}
