import { getFleet } from "../api";
import { useAsync } from "../hooks";
import type { FleetRow } from "../types";
import { Empty, ErrorState, formatTime, Loading, ScoreBadge, StaleBadge, verdictOf } from "../ui";

// Fleet: clusters × targets score matrix from /api/v1/fleet. Cells link to
// the cluster drill-down pinned at that target; an empty cell links there
// too, where the server computes a what-if.
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
          evaluations only. An empty cell opens a what-if for that target.
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
                  {row.stale && (
                    <>
                      {" "}
                      <StaleBadge lastSeen={row.lastSeen} />
                    </>
                  )}
                  <RowMeta row={row} />
                </th>
                {targets.map((t) => (
                  <td key={t}>
                    <FleetCellView row={row} target={t} />
                  </td>
                ))}
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </section>
  );
}

function RowMeta({ row }: { row: FleetRow }) {
  const parts = [
    row.serverVersion,
    row.lastSeen && `seen ${formatTime(row.lastSeen)}`,
  ].filter(Boolean);
  if (parts.length === 0) return null;
  return <span className="row-meta muted">{parts.join(" · ")}</span>;
}

function FleetCellView({ row, target }: { row: FleetRow; target: string }) {
  const cell = row.cells[target];
  const href = `#/cluster/${row.clusterId}?target=${target}`;
  if (row.notApplicable?.includes(target)) {
    return (
      <span className="muted" title={`${row.name} already runs ${target} or newer`}>
        n/a
      </span>
    );
  }
  if (!cell) {
    return (
      <a
        className="cell-link cell-empty"
        href={href}
        title="no stored evaluation for this target — open a what-if"
        aria-label={`${row.name} → ${target}: not stored, open a what-if`}
      >
        —
      </a>
    );
  }
  const verdict = verdictOf(cell);
  return (
    <a
      className="cell-link"
      href={href}
      aria-label={`${row.name} → ${target}: score ${cell.score}, ${verdict}`}
    >
      <ScoreBadge score={cell.score} verdict={verdict} />
      {cell.blockers > 0 && (
        <span className="blockers">
          {cell.blockers} blocker{cell.blockers > 1 ? "s" : ""}
        </span>
      )}
    </a>
  );
}
