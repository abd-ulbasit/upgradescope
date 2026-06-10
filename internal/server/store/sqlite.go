package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
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
// path must not contain '?' — it is interpolated into a SQLite URI.
func Open(path string) (*SQLite, error) {
	dsn := "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(ON)"
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

// UpsertCluster inserts the cluster or, if a row with the same name exists,
// updates cluster_uid and last_seen (first_seen never moves). Zero
// FirstSeen/LastSeen default to time.Now().UTC().
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
			cluster_uid = excluded.cluster_uid,
			last_seen   = excluded.last_seen
		RETURNING id`,
		c.Name, c.ClusterUID, formatTime(first), formatTime(last)).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("upsert cluster %q: %w", c.Name, err)
	}
	return id, nil
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
func (s *SQLite) InsertSnapshot(ctx context.Context, snap Snapshot) (int64, bool, error) {
	received := snap.ReceivedAt
	if received.IsZero() {
		received = time.Now().UTC()
	}
	inv := snap.Inventory
	if inv == nil {
		inv = []byte{}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, false, fmt.Errorf("insert snapshot: begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit

	var latestID int64
	var latestHash string
	err = tx.QueryRowContext(ctx,
		`SELECT id, hash FROM snapshots WHERE cluster_id = ? ORDER BY id DESC LIMIT 1`,
		snap.ClusterID).Scan(&latestID, &latestHash)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// first snapshot for this cluster — fall through to insert
	case err != nil:
		return 0, false, fmt.Errorf("insert snapshot: query latest: %w", err)
	case latestHash == snap.Hash:
		return latestID, true, nil
	}

	res, err := tx.ExecContext(ctx, `
		INSERT INTO snapshots (cluster_id, hash, kb_version, agent_version, received_at, inventory)
		VALUES (?, ?, ?, ?, ?, ?)`,
		snap.ClusterID, snap.Hash, snap.KBVersion, snap.AgentVersion, formatTime(received), inv)
	if err != nil {
		return 0, false, fmt.Errorf("insert snapshot: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, false, fmt.Errorf("insert snapshot: id: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, false, fmt.Errorf("insert snapshot: commit: %w", err)
	}
	return id, false, nil
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

// InsertEvaluation stores e. Zero CreatedAt defaults to now (UTC).
func (s *SQLite) InsertEvaluation(ctx context.Context, e Evaluation) (int64, error) {
	created := e.CreatedAt
	if created.IsZero() {
		created = time.Now().UTC()
	}
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO evaluations (cluster_id, snapshot_id, target, kb_version, score, ready, blockers, warnings, report, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.ClusterID, e.SnapshotID, e.Target, e.KBVersion, e.Score, e.Ready, e.Blockers, e.Warnings, e.Report, formatTime(created))
	if err != nil {
		return 0, fmt.Errorf("insert evaluation: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("insert evaluation: id: %w", err)
	}
	return id, nil
}

// LatestEvaluation returns the newest evaluation for (cluster, target) by
// created_at (ties broken by id), or ErrNotFound.
func (s *SQLite) LatestEvaluation(ctx context.Context, clusterID int64, target string) (Evaluation, error) {
	var e Evaluation
	var created string
	err := s.db.QueryRowContext(ctx, `
		SELECT id, cluster_id, snapshot_id, target, kb_version, score, ready, blockers, warnings, report, created_at
		FROM evaluations WHERE cluster_id = ? AND target = ?
		ORDER BY created_at DESC, id DESC LIMIT 1`, clusterID, target).
		Scan(&e.ID, &e.ClusterID, &e.SnapshotID, &e.Target, &e.KBVersion,
			&e.Score, &e.Ready, &e.Blockers, &e.Warnings, &e.Report, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return Evaluation{}, fmt.Errorf("latest evaluation for cluster %d target %s: %w", clusterID, target, ErrNotFound)
	}
	if err != nil {
		return Evaluation{}, fmt.Errorf("latest evaluation for cluster %d target %s: %w", clusterID, target, err)
	}
	if e.CreatedAt, err = parseStoredTime(created); err != nil {
		return Evaluation{}, fmt.Errorf("latest evaluation for cluster %d target %s: %w", clusterID, target, err)
	}
	return e, nil
}

// ScoreHistory returns score points for (cluster, target), oldest-first
// ascending by created_at. limit > 0 selects the most recent N rows (still
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
			ORDER BY created_at DESC, id DESC LIMIT ?
		) ORDER BY created_at ASC, id ASC`, clusterID, target, lim)
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
