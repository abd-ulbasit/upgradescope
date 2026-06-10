package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // database/sql driver, registered as "pgx"
)

//go:embed pgmigrations/*.sql
var pgMigrationsFS embed.FS

// Postgres is the Store implementation for fleets: same contract as SQLite
// (pinned by storetest.RunStoreConformance), backed by jackc/pgx via
// database/sql. Times are TIMESTAMPTZ columns; every time.Time returned is
// normalized to UTC.
type Postgres struct {
	db *sql.DB
}

var _ Store = (*Postgres)(nil)

// OpenPostgres connects to dsn (any pgx-accepted form, e.g.
// postgres://user:pass@host:5432/db), verifies the connection with a ping,
// and applies the embedded pgmigrations. Fails fast on unreachable or
// unauthorized servers instead of deferring the error to the first query.
func OpenPostgres(dsn string) (*Postgres, error) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("open postgres: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping postgres: %w", err)
	}
	sub, err := fs.Sub(pgMigrationsFS, "pgmigrations")
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("embedded pgmigrations: %w", err)
	}
	if _, err := migrate(ctx, db, sub, postgresMigrations); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate postgres: %w", err)
	}
	return &Postgres{db: db}, nil
}

// Close closes the underlying connection pool.
func (p *Postgres) Close() error { return p.db.Close() }

// UpsertCluster inserts the cluster or, if a row with the same name exists,
// updates cluster_uid and last_seen (first_seen never moves). Zero
// FirstSeen/LastSeen default to time.Now().UTC().
func (p *Postgres) UpsertCluster(ctx context.Context, c Cluster) (int64, error) {
	now := time.Now().UTC()
	first, last := c.FirstSeen, c.LastSeen
	if first.IsZero() {
		first = now
	}
	if last.IsZero() {
		last = now
	}
	var id int64
	err := p.db.QueryRowContext(ctx, `
		INSERT INTO clusters (name, cluster_uid, first_seen, last_seen)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (name) DO UPDATE SET
			cluster_uid = excluded.cluster_uid,
			last_seen   = excluded.last_seen
		RETURNING id`,
		c.Name, c.ClusterUID, first, last).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("upsert cluster %q: %w", c.Name, err)
	}
	return id, nil
}

// scanClusterPg mirrors scanCluster for TIMESTAMPTZ columns: the driver
// hands back time.Time directly; normalize to UTC.
func scanClusterPg(rs rowScanner) (Cluster, error) {
	var c Cluster
	if err := rs.Scan(&c.ID, &c.Name, &c.ClusterUID, &c.FirstSeen, &c.LastSeen); err != nil {
		return Cluster{}, err
	}
	c.FirstSeen = c.FirstSeen.UTC()
	c.LastSeen = c.LastSeen.UTC()
	return c, nil
}

// GetCluster returns the cluster by id, or ErrNotFound.
func (p *Postgres) GetCluster(ctx context.Context, id int64) (Cluster, error) {
	c, err := scanClusterPg(p.db.QueryRowContext(ctx,
		`SELECT id, name, cluster_uid, first_seen, last_seen FROM clusters WHERE id = $1`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Cluster{}, fmt.Errorf("cluster %d: %w", id, ErrNotFound)
	}
	if err != nil {
		return Cluster{}, fmt.Errorf("get cluster %d: %w", id, err)
	}
	return c, nil
}

// ListClusters returns all clusters, ascending by name.
func (p *Postgres) ListClusters(ctx context.Context) ([]Cluster, error) {
	rows, err := p.db.QueryContext(ctx,
		`SELECT id, name, cluster_uid, first_seen, last_seen FROM clusters ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("list clusters: %w", err)
	}
	defer rows.Close()
	var out []Cluster
	for rows.Next() {
		c, err := scanClusterPg(rows)
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
//
// Concurrency: the read-latest → insert pair must be serialized per cluster
// or two racing pushes of the same hash both miss the duplicate and insert
// twice (READ COMMITTED gives no protection — both SELECTs see the same
// "latest"). Locking the parent clusters row FOR UPDATE serializes writers
// of one cluster without blocking other clusters.
func (p *Postgres) InsertSnapshot(ctx context.Context, snap Snapshot) (int64, bool, error) {
	received := snap.ReceivedAt
	if received.IsZero() {
		received = time.Now().UTC()
	}
	inv := snap.Inventory
	if inv == nil {
		inv = []byte{}
	}
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, false, fmt.Errorf("insert snapshot: begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit

	var lockID int64
	err = tx.QueryRowContext(ctx,
		`SELECT id FROM clusters WHERE id = $1 FOR UPDATE`, snap.ClusterID).Scan(&lockID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		// Unknown cluster: fall through — the INSERT's FK reports it properly.
		return 0, false, fmt.Errorf("insert snapshot: lock cluster: %w", err)
	}

	var latestID int64
	var latestHash string
	err = tx.QueryRowContext(ctx,
		`SELECT id, hash FROM snapshots WHERE cluster_id = $1 ORDER BY id DESC LIMIT 1`,
		snap.ClusterID).Scan(&latestID, &latestHash)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// first snapshot for this cluster — fall through to insert
	case err != nil:
		return 0, false, fmt.Errorf("insert snapshot: query latest: %w", err)
	case latestHash == snap.Hash:
		return latestID, true, nil
	}

	var id int64
	if err := tx.QueryRowContext(ctx, `
		INSERT INTO snapshots (cluster_id, hash, kb_version, agent_version, received_at, inventory)
		VALUES ($1, $2, $3, $4, $5, $6) RETURNING id`,
		snap.ClusterID, snap.Hash, snap.KBVersion, snap.AgentVersion, received, inv).Scan(&id); err != nil {
		return 0, false, fmt.Errorf("insert snapshot: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, false, fmt.Errorf("insert snapshot: commit: %w", err)
	}
	return id, false, nil
}

// LatestSnapshot returns the most recently inserted snapshot for the
// cluster (highest id), or ErrNotFound.
func (p *Postgres) LatestSnapshot(ctx context.Context, clusterID int64) (Snapshot, error) {
	var snap Snapshot
	err := p.db.QueryRowContext(ctx, `
		SELECT id, cluster_id, hash, kb_version, agent_version, received_at, inventory
		FROM snapshots WHERE cluster_id = $1 ORDER BY id DESC LIMIT 1`, clusterID).
		Scan(&snap.ID, &snap.ClusterID, &snap.Hash, &snap.KBVersion, &snap.AgentVersion, &snap.ReceivedAt, &snap.Inventory)
	if errors.Is(err, sql.ErrNoRows) {
		return Snapshot{}, fmt.Errorf("latest snapshot for cluster %d: %w", clusterID, ErrNotFound)
	}
	if err != nil {
		return Snapshot{}, fmt.Errorf("latest snapshot for cluster %d: %w", clusterID, err)
	}
	snap.ReceivedAt = snap.ReceivedAt.UTC()
	return snap, nil
}

// InsertEvaluation stores e. Zero CreatedAt defaults to now (UTC).
func (p *Postgres) InsertEvaluation(ctx context.Context, e Evaluation) (int64, error) {
	created := e.CreatedAt
	if created.IsZero() {
		created = time.Now().UTC()
	}
	var id int64
	err := p.db.QueryRowContext(ctx, `
		INSERT INTO evaluations (cluster_id, snapshot_id, target, kb_version, score, ready, blockers, warnings, report, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10) RETURNING id`,
		e.ClusterID, e.SnapshotID, e.Target, e.KBVersion, e.Score, e.Ready, e.Blockers, e.Warnings, e.Report, created).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("insert evaluation: %w", err)
	}
	return id, nil
}

// LatestEvaluation returns the newest evaluation for (cluster, target) by
// created_at (ties broken by id), or ErrNotFound.
func (p *Postgres) LatestEvaluation(ctx context.Context, clusterID int64, target string) (Evaluation, error) {
	var e Evaluation
	err := p.db.QueryRowContext(ctx, `
		SELECT id, cluster_id, snapshot_id, target, kb_version, score, ready, blockers, warnings, report, created_at
		FROM evaluations WHERE cluster_id = $1 AND target = $2
		ORDER BY created_at DESC, id DESC LIMIT 1`, clusterID, target).
		Scan(&e.ID, &e.ClusterID, &e.SnapshotID, &e.Target, &e.KBVersion,
			&e.Score, &e.Ready, &e.Blockers, &e.Warnings, &e.Report, &e.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Evaluation{}, fmt.Errorf("latest evaluation for cluster %d target %s: %w", clusterID, target, ErrNotFound)
	}
	if err != nil {
		return Evaluation{}, fmt.Errorf("latest evaluation for cluster %d target %s: %w", clusterID, target, err)
	}
	e.CreatedAt = e.CreatedAt.UTC()
	return e, nil
}

// ScoreHistory returns score points for (cluster, target), oldest-first
// ascending by created_at. limit > 0 selects the most recent N rows (still
// returned oldest-first); limit <= 0 returns all. An unknown cluster or
// target yields an empty slice and nil error.
func (p *Postgres) ScoreHistory(ctx context.Context, clusterID int64, target string, limit int) ([]ScorePoint, error) {
	var lim any // LIMIT NULL == no limit in Postgres
	if limit > 0 {
		lim = limit
	}
	rows, err := p.db.QueryContext(ctx, `
		SELECT created_at, score, ready FROM (
			SELECT id, created_at, score, ready FROM evaluations
			WHERE cluster_id = $1 AND target = $2
			ORDER BY created_at DESC, id DESC LIMIT $3
		) recent ORDER BY created_at ASC, id ASC`, clusterID, target, lim)
	if err != nil {
		return nil, fmt.Errorf("score history cluster %d target %s: %w", clusterID, target, err)
	}
	defer rows.Close()
	var out []ScorePoint
	for rows.Next() {
		var pt ScorePoint
		if err := rows.Scan(&pt.At, &pt.Score, &pt.Ready); err != nil {
			return nil, fmt.Errorf("score history cluster %d target %s: %w", clusterID, target, err)
		}
		pt.At = pt.At.UTC()
		out = append(out, pt)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("score history cluster %d target %s: %w", clusterID, target, err)
	}
	return out, nil
}
