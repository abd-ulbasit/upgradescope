import { getFleet } from "../api";
import { useAsync } from "../hooks";
import { Empty, ErrorState, Loading, ScoreBadge } from "../ui";

// Fleet: clusters × targets score matrix from /api/v1/fleet. Cells link to
// the cluster drill-down pinned at that target.
export function Fleet() {
  const fleet = useAsync(getFleet, []);

  if (fleet.loading) return <Loading label="Loading fleet…" />;
  if (fleet.error) return <ErrorState error={fleet.error} onRetry={fleet.reload} />;
  const { targets, clusters } = fleet.data!;

  if (clusters.length === 0) {
    return (
      <Empty
        title="No clusters yet"
        hint="Deploy the agent (upgradescope agent) or push a snapshot to POST /api/v1/snapshots and the fleet matrix will appear here."
      />
    );
  }

  return (
    <section>
      <header className="page-head">
        <h1>Fleet</h1>
        <p className="muted">
          Readiness score per cluster and upgrade target — latest stored
          evaluations only.
        </p>
      </header>
      <div className="card table-wrap">
        <table className="matrix">
          <thead>
            <tr>
              <th scope="col">Cluster</th>
              {targets.map((t) => (
                <th scope="col" key={t}>
                  → {t}
                </th>
              ))}
            </tr>
          </thead>
          <tbody>
            {clusters.map((row) => (
              <tr key={row.clusterId}>
                <th scope="row">
                  <a href={`#/cluster/${row.clusterId}`}>{row.name}</a>
                </th>
                {targets.map((t) => {
                  const cell = row.cells[t];
                  return (
                    <td key={t}>
                      {cell ? (
                        <a
                          className="cell-link"
                          href={`#/cluster/${row.clusterId}?target=${t}`}
                          aria-label={`${row.name} → ${t}: score ${cell.score}`}
                        >
                          <ScoreBadge score={cell.score} ready={cell.ready} />
                          {cell.blockers > 0 && (
                            <span className="blockers">
                              {cell.blockers} blocker{cell.blockers > 1 ? "s" : ""}
                            </span>
                          )}
                        </a>
                      ) : (
                        <span className="muted" title="never evaluated for this target">
                          —
                        </span>
                      )}
                    </td>
                  );
                })}
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </section>
  );
}
