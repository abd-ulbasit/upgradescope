package store

import (
	"cmp"
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"time"

	_ "modernc.org/sqlite" // database/sql driver, registered as "sqlite"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// SQLite is the Store implementation backed by a single SQLite database
// file (modernc.org/sqlite — pure Go, CGO-free).
type SQLite struct {
	db *sql.DB
}

var _ Store = (*SQLite)(nil)

// Open opens (creating if needed) the database at path and applies all
// embedded migrations. Every pooled connection gets WAL journaling, a 5s
// busy timeout and foreign-key enforcement via DSN pragmas.
//
// path must not contain '?' or '#' — it is interpolated into a SQLite URI,
// where either character corrupts the path.
func Open(path string) (*SQLite, error) {
	// _txlock=immediate makes every transaction start as BEGIN IMMEDIATE,
	// taking the write lock up front. Without it, a deferred transaction
	// that reads before writing (InsertSnapshot: SELECT latest, then INSERT)
	// fails with SQLITE_BUSY under concurrent ingest — busy_timeout is never
	// consulted for the read→write lock upgrade. This is safe because every
	// BeginTx call site in this package (snapshot and evaluation writes,
	// DeleteCluster, applyMigration) is a write path; a read-only
	// transaction here would needlessly take the write lock, so keep it
	// that way.
	dsn := "file:" + path + "?_txlock=immediate&_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(ON)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite %s: %w", path, err)
	}
	sub, err := fs.Sub(migrationsFS, "migrations")
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("embedded migrations: %w", err)
	}
	if _, err := Migrate(context.Background(), db, sub); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate %s: %w", path, err)
	}
	return &SQLite{db: db}, nil
}

// Close closes the underlying database.
func (s *SQLite) Close() error { return s.db.Close() }

// Ping checks the database answers (the server's /readyz).
func (s *SQLite) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

// UpsertCluster inserts the cluster or, if a row with the same name exists,
// bumps last_seen (first_seen never moves) and adopts c.ClusterUID when the
// stored one is empty. Once bound, any other UID is refused, an empty one
// included (it is no wildcard: a UID-less push may be another cluster): the guarded
// DO UPDATE matches no row, so RETURNING yields nothing and the stored UID
// is read back for the *ClusterUIDConflictError. Zero FirstSeen/LastSeen
// default to time.Now().UTC().
func (s *SQLite) UpsertCluster(ctx context.Context, c Cluster) (int64, error) {
	now := time.Now().UTC()
	first, last := c.FirstSeen, c.LastSeen
	if first.IsZero() {
		first = now
	}
	if last.IsZero() {
		last = now
	}
	var id int64
	err := s.db.QueryRowContext(ctx, `
		INSERT INTO clusters (name, cluster_uid, first_seen, last_seen)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET
			cluster_uid = CASE WHEN excluded.cluster_uid = '' THEN clusters.cluster_uid ELSE excluded.cluster_uid END,
			last_seen   = excluded.last_seen
		WHERE clusters.cluster_uid = '' OR clusters.cluster_uid = excluded.cluster_uid
		RETURNING id`,
		c.Name, c.ClusterUID, formatTime(first), formatTime(last)).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		var stored string
		if err := s.db.QueryRowContext(ctx, `SELECT cluster_uid FROM clusters WHERE name = ?`, c.Name).Scan(&stored); err != nil {
			return 0, fmt.Errorf("upsert cluster %q: read stored uid: %w", c.Name, err)
		}
		return 0, &ClusterUIDConflictError{Name: c.Name, StoredUID: stored, PushedUID: c.ClusterUID}
	}
	if err != nil {
		return 0, fmt.Errorf("upsert cluster %q: %w", c.Name, err)
	}
	return id, nil
}

// DeleteCluster removes the named cluster, its evaluations and snapshots in
// one transaction, or returns ErrNotFound. Tokens are keyed by name and
// are left alone.
func (s *SQLite) DeleteCluster(ctx context.Context, name string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("delete cluster %q: begin: %w", name, err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit
	var id int64
	err = tx.QueryRowContext(ctx, `SELECT id FROM clusters WHERE name = ?`, name).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("cluster %q: %w", name, ErrNotFound)
	}
	if err != nil {
		return fmt.Errorf("delete cluster %q: %w", name, err)
	}
	for _, stmt := range []string{
		`DELETE FROM evaluations WHERE cluster_id = ?`,
		`DELETE FROM snapshots WHERE cluster_id = ?`,
		`DELETE FROM clusters WHERE id = ?`,
	} {
		if _, err := tx.ExecContext(ctx, stmt, id); err != nil {
			return fmt.Errorf("delete cluster %q: %w", name, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("delete cluster %q: commit: %w", name, err)
	}
	return nil
}

// rowScanner abstracts *sql.Row and *sql.Rows for shared scan helpers.
type rowScanner interface{ Scan(dest ...any) error }

func scanCluster(rs rowScanner) (Cluster, error) {
	var c Cluster
	var first, last string
	if err := rs.Scan(&c.ID, &c.Name, &c.ClusterUID, &first, &last); err != nil {
		return Cluster{}, err
	}
	var err error
	if c.FirstSeen, err = parseStoredTime(first); err != nil {
		return Cluster{}, err
	}
	if c.LastSeen, err = parseStoredTime(last); err != nil {
		return Cluster{}, err
	}
	return c, nil
}

// GetCluster returns the cluster by id, or ErrNotFound.
func (s *SQLite) GetCluster(ctx context.Context, id int64) (Cluster, error) {
	c, err := scanCluster(s.db.QueryRowContext(ctx,
		`SELECT id, name, cluster_uid, first_seen, last_seen FROM clusters WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Cluster{}, fmt.Errorf("cluster %d: %w", id, ErrNotFound)
	}
	if err != nil {
		return Cluster{}, fmt.Errorf("get cluster %d: %w", id, err)
	}
	return c, nil
}

// ListClusters returns all clusters, ascending by name.
func (s *SQLite) ListClusters(ctx context.Context) ([]Cluster, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, name, cluster_uid, first_seen, last_seen FROM clusters ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("list clusters: %w", err)
	}
	defer rows.Close()
	var out []Cluster
	for rows.Next() {
		c, err := scanCluster(rows)
		if err != nil {
			return nil, fmt.Errorf("list clusters: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list clusters: %w", err)
	}
	return out, nil
}

// InsertSnapshot stores snap unless its hash equals the hash of the
// cluster's LATEST snapshot, in which case it returns (latestID, true, nil)
// without writing. An older same-hash snapshot superseded by a different
// one does NOT count as a duplicate. Zero ReceivedAt defaults to now (UTC).
// It is CommitEvaluations with nothing but the snapshot.
func (s *SQLite) InsertSnapshot(ctx context.Context, snap Snapshot) (int64, bool, error) {
	id, dup, err := s.CommitEvaluations(ctx, EvaluationBatch{ClusterID: snap.ClusterID, Snapshot: &snap})
	if err != nil {
		return 0, false, fmt.Errorf("insert snapshot: %w", err)
	}
	return id, dup, nil
}

// LatestSnapshot returns the most recently inserted snapshot for the
// cluster (highest id), or ErrNotFound.
func (s *SQLite) LatestSnapshot(ctx context.Context, clusterID int64) (Snapshot, error) {
	var snap Snapshot
	var received string
	err := s.db.QueryRowContext(ctx, `
		SELECT id, cluster_id, hash, kb_version, agent_version, received_at, inventory
		FROM snapshots WHERE cluster_id = ? ORDER BY id DESC LIMIT 1`, clusterID).
		Scan(&snap.ID, &snap.ClusterID, &snap.Hash, &snap.KBVersion, &snap.AgentVersion, &received, &snap.Inventory)
	if errors.Is(err, sql.ErrNoRows) {
		return Snapshot{}, fmt.Errorf("latest snapshot for cluster %d: %w", clusterID, ErrNotFound)
	}
	if err != nil {
		return Snapshot{}, fmt.Errorf("latest snapshot for cluster %d: %w", clusterID, err)
	}
	if snap.ReceivedAt, err = parseStoredTime(received); err != nil {
		return Snapshot{}, fmt.Errorf("latest snapshot for cluster %d: %w", clusterID, err)
	}
	return snap, nil
}

// sqlExecer is the subset of *sql.DB and *sql.Tx the write helpers use.
type sqlExecer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// evaluationColumns is the SELECT list scanEvaluation reads.
const evaluationColumns = `id, cluster_id, snapshot_id, target, kb_version, score, ready, blockers, warnings, report, created_at, evaluated_at, team_map_hash`

func scanEvaluation(rs rowScanner) (Evaluation, error) {
	var e Evaluation
	var created, evaluated string
	if err := rs.Scan(&e.ID, &e.ClusterID, &e.SnapshotID, &e.Target, &e.KBVersion,
		&e.Score, &e.Ready, &e.Blockers, &e.Warnings, &e.Report, &created, &evaluated, &e.TeamMapHash); err != nil {
		return Evaluation{}, err
	}
	var err error
	if e.CreatedAt, err = parseStoredTime(created); err != nil {
		return Evaluation{}, err
	}
	// '' is the 0004 column default: a binary that predates it (a rollback
	// after the migration ran) inserts without evaluated_at. Such a row was
	// last evaluated when created; it reads as stale-but-valid and the next
	// pass refreshes it, instead of failing every read of the cluster.
	if evaluated == "" {
		e.EvaluatedAt = e.CreatedAt
	} else if e.EvaluatedAt, err = parseStoredTime(evaluated); err != nil {
		return Evaluation{}, err
	}
	return e, nil
}

// InsertEvaluation stores e. Zero CreatedAt defaults to now (UTC); zero
// EvaluatedAt to CreatedAt.
func (s *SQLite) InsertEvaluation(ctx context.Context, e Evaluation) (int64, error) {
	return insertEvaluationSQLite(ctx, s.db, e)
}

func insertEvaluationSQLite(ctx context.Context, x sqlExecer, e Evaluation) (int64, error) {
	created := e.CreatedAt
	if created.IsZero() {
		created = time.Now().UTC()
	}
	evaluated := e.EvaluatedAt
	if evaluated.IsZero() {
		evaluated = created
	}
	res, err := x.ExecContext(ctx, `
		INSERT INTO evaluations (cluster_id, snapshot_id, target, kb_version, score, ready, blockers, warnings, report, created_at, evaluated_at, team_map_hash)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.ClusterID, e.SnapshotID, e.Target, e.KBVersion, e.Score, e.Ready, e.Blockers, e.Warnings, e.Report,
		formatTime(created), formatTime(evaluated), e.TeamMapHash)
	if err != nil {
		return 0, fmt.Errorf("insert evaluation: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("insert evaluation: id: %w", err)
	}
	return id, nil
}

// queryEvaluation runs a single-row evaluation query, mapping no row to
// ErrNotFound (wrapped with what).
func (s *SQLite) queryEvaluation(ctx context.Context, what, query string, args ...any) (Evaluation, error) {
	e, err := scanEvaluation(s.db.QueryRowContext(ctx, query, args...))
	if errors.Is(err, sql.ErrNoRows) {
		return Evaluation{}, fmt.Errorf("%s: %w", what, ErrNotFound)
	}
	if err != nil {
		return Evaluation{}, fmt.Errorf("%s: %w", what, err)
	}
	return e, nil
}

// LatestEvaluation returns the newest evaluation for (cluster, target) by
// insertion order (highest id; never created_at, which a clock step can
// reorder), or ErrNotFound.
func (s *SQLite) LatestEvaluation(ctx context.Context, clusterID int64, target string) (Evaluation, error) {
	return s.queryEvaluation(ctx, fmt.Sprintf("latest evaluation for cluster %d target %s", clusterID, target), `
		SELECT `+evaluationColumns+` FROM evaluations WHERE cluster_id = ? AND target = ?
		ORDER BY id DESC LIMIT 1`, clusterID, target)
}

// CurrentEvaluation returns the newest evaluation for target of the
// cluster's latest snapshot (highest id), or ErrNotFound.
func (s *SQLite) CurrentEvaluation(ctx context.Context, clusterID int64, target string) (Evaluation, error) {
	return s.queryEvaluation(ctx, fmt.Sprintf("current evaluation for cluster %d target %s", clusterID, target), `
		SELECT `+evaluationColumns+` FROM evaluations
		WHERE snapshot_id = (SELECT MAX(id) FROM snapshots WHERE cluster_id = ?) AND target = ?
		ORDER BY id DESC LIMIT 1`, clusterID, target)
}

// LatestKnownEvaluation returns the newest evaluation for (cluster, target)
// that is ready or has a blocker, or ErrNotFound.
func (s *SQLite) LatestKnownEvaluation(ctx context.Context, clusterID int64, target string) (Evaluation, error) {
	return s.queryEvaluation(ctx, fmt.Sprintf("latest known evaluation for cluster %d target %s", clusterID, target), `
		SELECT `+evaluationColumns+` FROM evaluations
		WHERE cluster_id = ? AND target = ? AND (ready = 1 OR blockers > 0)
		ORDER BY id DESC LIMIT 1`, clusterID, target)
}

// CommitEvaluations writes b in one BEGIN IMMEDIATE transaction (the DSN's
// _txlock), which also serializes it against every other writer: the
// duplicate check and the Current/SnapshotID checks read state no one can
// change before the commit.
func (s *SQLite) CommitEvaluations(ctx context.Context, b EvaluationBatch) (int64, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, false, fmt.Errorf("commit evaluations: begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit

	var latestID int64
	var latestHash string
	err = tx.QueryRowContext(ctx,
		`SELECT id, hash FROM snapshots WHERE cluster_id = ? ORDER BY id DESC LIMIT 1`,
		b.ClusterID).Scan(&latestID, &latestHash)
	noSnapshot := errors.Is(err, sql.ErrNoRows)
	if err != nil && !noSnapshot {
		return 0, false, fmt.Errorf("commit evaluations: query latest snapshot: %w", err)
	}

	snapID := b.SnapshotID
	if b.Snapshot != nil {
		if !noSnapshot && latestHash == b.Snapshot.Hash {
			return latestID, true, nil
		}
		received := b.Snapshot.ReceivedAt
		if received.IsZero() {
			received = time.Now().UTC()
		}
		inv := b.Snapshot.Inventory
		if inv == nil {
			inv = []byte{}
		}
		res, err := tx.ExecContext(ctx, `
			INSERT INTO snapshots (cluster_id, hash, kb_version, agent_version, received_at, inventory)
			VALUES (?, ?, ?, ?, ?, ?)`,
			b.ClusterID, b.Snapshot.Hash, b.Snapshot.KBVersion, b.Snapshot.AgentVersion, formatTime(received), inv)
		if err != nil {
			return 0, false, fmt.Errorf("commit evaluations: insert snapshot: %w", err)
		}
		if snapID, err = res.LastInsertId(); err != nil {
			return 0, false, fmt.Errorf("commit evaluations: snapshot id: %w", err)
		}
	} else {
		if noSnapshot || latestID != snapID {
			return 0, false, fmt.Errorf("commit evaluations: snapshot %d is no longer cluster %d's latest: %w", snapID, b.ClusterID, ErrConflict)
		}
		for target, want := range b.Current {
			var got int64
			err := tx.QueryRowContext(ctx, `
				SELECT id FROM evaluations WHERE snapshot_id = ? AND target = ?
				ORDER BY id DESC LIMIT 1`, snapID, target).Scan(&got)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return 0, false, fmt.Errorf("commit evaluations: current %s: %w", target, err)
			}
			if got != want {
				return 0, false, fmt.Errorf("commit evaluations: target %s changed (evaluation %d, expected %d): %w", target, got, want, ErrConflict)
			}
		}
	}

	for _, e := range b.Insert {
		e.SnapshotID = snapID
		if _, err := insertEvaluationSQLite(ctx, tx, e); err != nil {
			return 0, false, fmt.Errorf("commit evaluations: %w", err)
		}
	}
	for _, e := range b.Refresh {
		evaluated := e.EvaluatedAt
		if evaluated.IsZero() {
			evaluated = time.Now().UTC()
		}
		if err := execOne(ctx, tx, fmt.Sprintf("commit evaluations: refresh evaluation %d", e.ID), `
			UPDATE evaluations SET report = ?, kb_version = ?, team_map_hash = ?, blockers = ?, warnings = ?, evaluated_at = ?
			WHERE id = ? AND cluster_id = ?`,
			e.Report, e.KBVersion, e.TeamMapHash, e.Blockers, e.Warnings, formatTime(evaluated), e.ID, b.ClusterID); err != nil {
			return 0, false, err
		}
	}
	for _, m := range b.Outbox {
		created := m.CreatedAt
		if created.IsZero() {
			created = time.Now().UTC()
		}
		next := m.NextAttemptAt
		if next.IsZero() {
			next = created
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO outbox (sink, payload, attempts, created_at, next_attempt_at) VALUES (?, ?, 0, ?, ?)`,
			m.Sink, m.Payload, formatTime(created), formatTime(next)); err != nil {
			return 0, false, fmt.Errorf("commit evaluations: outbox: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, false, fmt.Errorf("commit evaluations: commit: %w", err)
	}
	return snapID, false, nil
}

// ClaimOutbox leases up to limit due messages (next_attempt_at <= now),
// oldest first, in one statement.
func (s *SQLite) ClaimOutbox(ctx context.Context, now time.Time, lease time.Duration, limit int) ([]OutboxMessage, error) {
	rows, err := s.db.QueryContext(ctx, `
		UPDATE outbox SET attempts = attempts + 1, next_attempt_at = ?
		WHERE id IN (SELECT id FROM outbox WHERE next_attempt_at <= ? ORDER BY id LIMIT ?)
		RETURNING id, sink, payload, attempts, created_at, next_attempt_at`,
		formatTime(now.Add(lease)), formatTime(now), limit)
	if err != nil {
		return nil, fmt.Errorf("claim outbox: %w", err)
	}
	defer rows.Close()
	var out []OutboxMessage
	for rows.Next() {
		var m OutboxMessage
		var created, next string
		if err := rows.Scan(&m.ID, &m.Sink, &m.Payload, &m.Attempts, &created, &next); err != nil {
			return nil, fmt.Errorf("claim outbox: %w", err)
		}
		if m.CreatedAt, err = parseStoredTime(created); err != nil {
			return nil, fmt.Errorf("claim outbox: %w", err)
		}
		if m.NextAttemptAt, err = parseStoredTime(next); err != nil {
			return nil, fmt.Errorf("claim outbox: %w", err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("claim outbox: %w", err)
	}
	slices.SortFunc(out, func(a, b OutboxMessage) int { return cmp.Compare(a.ID, b.ID) }) // RETURNING order is unspecified
	return out, nil
}

// DeleteOutbox removes message id, or returns ErrNotFound.
func (s *SQLite) DeleteOutbox(ctx context.Context, id int64) error {
	return execOne(ctx, s.db, fmt.Sprintf("delete outbox message %d", id), `DELETE FROM outbox WHERE id = ?`, id)
}

// RescheduleOutbox records a failed attempt and when to try again, or
// returns ErrNotFound.
func (s *SQLite) RescheduleOutbox(ctx context.Context, id int64, next time.Time, lastErr string) error {
	return execOne(ctx, s.db, fmt.Sprintf("reschedule outbox message %d", id),
		`UPDATE outbox SET next_attempt_at = ?, last_error = ? WHERE id = ?`, formatTime(next), lastErr, id)
}

// execOne runs a statement that must affect a row; none is ErrNotFound.
// Shared by both backends (the query carries the dialect's placeholders).
func execOne(ctx context.Context, x sqlExecer, what, query string, args ...any) error {
	res, err := x.ExecContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	if n == 0 {
		return fmt.Errorf("%s: %w", what, ErrNotFound)
	}
	return nil
}

// CreateToken stores a new active ingest token for clusterName and returns
// its id; only the sha256 of token (plus TokenPrefix) is persisted. Fails
// if the token is already issued.
func (s *SQLite) CreateToken(ctx context.Context, clusterName, token string) (int64, error) {
	if clusterName == "" || token == "" {
		return 0, errors.New("create token: cluster name and token must be non-empty")
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO tokens (cluster_name, token_hash, token_prefix, created_at) VALUES (?, ?, ?, ?)`,
		clusterName, HashToken(token), TokenPrefix(token), formatTime(time.Now()))
	if err != nil {
		return 0, fmt.Errorf("create token for %q: %w", clusterName, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("create token for %q: %w", clusterName, err)
	}
	return id, nil
}

// ListTokens returns token metadata, ascending by id, for clusterName or
// for every cluster when clusterName is "".
func (s *SQLite) ListTokens(ctx context.Context, clusterName string) ([]Token, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, cluster_name, token_prefix, created_at, revoked_at FROM tokens
		WHERE ? = '' OR cluster_name = ? ORDER BY id`, clusterName, clusterName)
	if err != nil {
		return nil, fmt.Errorf("list tokens: %w", err)
	}
	defer rows.Close()
	var out []Token
	for rows.Next() {
		var tk Token
		var created string
		var revoked sql.NullString
		if err := rows.Scan(&tk.ID, &tk.ClusterName, &tk.Prefix, &created, &revoked); err != nil {
			return nil, fmt.Errorf("list tokens: %w", err)
		}
		if tk.CreatedAt, err = parseStoredTime(created); err != nil {
			return nil, fmt.Errorf("list tokens: %w", err)
		}
		if revoked.Valid {
			at, err := parseStoredTime(revoked.String)
			if err != nil {
				return nil, fmt.Errorf("list tokens: %w", err)
			}
			tk.RevokedAt = &at
		}
		out = append(out, tk)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list tokens: %w", err)
	}
	return out, nil
}

// RevokeTokenID revokes the active token id of clusterName, or ErrNotFound
// when there is none (unknown id, another cluster's token, or already
// revoked). The cluster check guards against a mistyped id.
func (s *SQLite) RevokeTokenID(ctx context.Context, clusterName string, id int64) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE tokens SET revoked_at = ? WHERE id = ? AND cluster_name = ? AND revoked_at IS NULL`,
		formatTime(time.Now()), id, clusterName)
	if err != nil {
		return fmt.Errorf("revoke token %d of %q: %w", id, clusterName, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("revoke token %d of %q: %w", id, clusterName, err)
	}
	if n == 0 {
		return fmt.Errorf("no active token %d for cluster %q: %w", id, clusterName, ErrNotFound)
	}
	return nil
}

// ValidToken resolves a presented plaintext token to its cluster name.
// Unknown or revoked tokens are ("", false, nil) — not an error.
func (s *SQLite) ValidToken(ctx context.Context, token string) (string, bool, error) {
	var name string
	err := s.db.QueryRowContext(ctx,
		`SELECT cluster_name FROM tokens WHERE token_hash = ? AND revoked_at IS NULL`,
		HashToken(token)).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("validate token: %w", err)
	}
	return name, true, nil
}

// RevokeToken revokes every active token of clusterName, or ErrNotFound
// when the cluster has none.
func (s *SQLite) RevokeToken(ctx context.Context, clusterName string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE tokens SET revoked_at = ? WHERE cluster_name = ? AND revoked_at IS NULL`,
		formatTime(time.Now()), clusterName)
	if err != nil {
		return fmt.Errorf("revoke tokens for %q: %w", clusterName, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("revoke tokens for %q: %w", clusterName, err)
	}
	if n == 0 {
		return fmt.Errorf("no active tokens for cluster %q: %w", clusterName, ErrNotFound)
	}
	return nil
}

// ScoreHistory returns score points for (cluster, target), oldest-first
// in insertion order (id). limit > 0 selects the most recent N rows (still
// returned oldest-first); limit <= 0 returns all. An unknown cluster or
// target yields an empty slice and nil error.
func (s *SQLite) ScoreHistory(ctx context.Context, clusterID int64, target string, limit int) ([]ScorePoint, error) {
	lim := int64(limit)
	if limit <= 0 {
		lim = -1 // SQLite: LIMIT -1 == no limit
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT created_at, score, ready FROM (
			SELECT id, created_at, score, ready FROM evaluations
			WHERE cluster_id = ? AND target = ?
			ORDER BY id DESC LIMIT ?
		) ORDER BY id ASC`, clusterID, target, lim)
	if err != nil {
		return nil, fmt.Errorf("score history cluster %d target %s: %w", clusterID, target, err)
	}
	defer rows.Close()
	var out []ScorePoint
	for rows.Next() {
		var p ScorePoint
		var created string
		if err := rows.Scan(&created, &p.Score, &p.Ready); err != nil {
			return nil, fmt.Errorf("score history cluster %d target %s: %w", clusterID, target, err)
		}
		if p.At, err = parseStoredTime(created); err != nil {
			return nil, fmt.Errorf("score history cluster %d target %s: %w", clusterID, target, err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("score history cluster %d target %s: %w", clusterID, target, err)
	}
	return out, nil
}
