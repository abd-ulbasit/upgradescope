import { useEffect, useMemo, useState } from "react";
import { getFleet } from "../api";
import {
  BUCKET_LABEL,
  BUCKETS,
  countBuckets,
  defaultTarget,
  FILTERS,
  matchesFilter,
  matchesName,
  MAX_ROWS,
  parseFilter,
  parseSort,
  scopeOf,
  SORTS,
  sortRows,
} from "../fleet";
import type { Bucket } from "../fleet";
import { setRouteQuery, useAsync, useRouteQuery } from "../hooks";
import type { FleetResponse, FleetRow } from "../types";
import {
  Empty,
  ErrorState,
  formatTime,
  Freshness,
  Loading,
  ScoreBadge,
  StaleBadge,
  verdictOf,
} from "../ui";

// Fleet: clusters × targets score matrix from /api/v1/fleet. Cells link to
// the cluster drill-down pinned at that target; an empty cell links there
// too, where the server computes a what-if. The toolbar (name search, quick
// filter, sort) and the summary strip's target live in the hash query
// (#/?q=prod&filter=blocked&sort=name&target=1.37), so a reload or a pasted
// link restores them. Everything is client-side: /fleet is already bounded.
export function Fleet() {
  const fleet = useAsync(getFleet, []);
  const query = useRouteQuery();

  if (fleet.loading) return <Loading label="Loading fleet…" />;
  if (fleet.error && !fleet.data) return <ErrorState error={fleet.error} onRetry={fleet.reload} />;
  const data = fleet.data!;

  return (
    <section>
      <header className="page-head">
        <div className="head-row">
          <h1>Fleet</h1>
          <Freshness
            updatedAt={fleet.updatedAt}
            refreshing={fleet.refreshing}
            error={fleet.error}
            onRefresh={fleet.reload}
          />
        </div>
        {data.clusters.length > 0 && (
          <p className="muted">
            Readiness score per cluster and upgrade target — latest stored
            evaluations only. An empty cell opens a what-if for that target.
          </p>
        )}
        {data.targetsOmitted ? (
          <p className="muted" role="note">
            {data.targetsOmitted} more target{data.targetsOmitted > 1 ? "s" : ""} not
            shown: the fleet runs more minors than the 16 columns the server
            opens, so these are the ones with the most clusters.
          </p>
        ) : null}
      </header>
      {data.clusters.length === 0 ? (
        <Empty
          title="No clusters yet"
          hint="Deploy the agent (upgradescope agent) or push a snapshot to POST /api/v1/snapshots, then press Refresh: the fleet matrix will appear here."
        />
      ) : (
        <FleetBody
          fleet={data}
          q={query.get("q") ?? ""}
          filter={parseFilter(query.get("filter"))}
          sort={parseSort(query.get("sort"))}
          target={query.get("target") ?? undefined}
        />
      )}
    </section>
  );
}

function FleetBody({
  fleet,
  q,
  filter,
  sort,
  target,
}: {
  fleet: FleetResponse;
  q: string;
  filter: ReturnType<typeof parseFilter>;
  sort: ReturnType<typeof parseSort>;
  target?: string;
}) {
  const { targets, clusters } = fleet;
  const pinned = target && targets.includes(target) ? target : undefined;
  const summaryTarget = pinned ?? defaultTarget(fleet);
  const scope = scopeOf(fleet, pinned);

  const rows = useMemo(
    () =>
      sortRows(
        clusters.filter((r) => matchesName(r, q) && matchesFilter(r, filter, scope)),
        sort,
        scope,
      ),
    // scope is derived from fleet and pinned
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [clusters, q, filter, sort, pinned],
  );

  // "Show more" is per toolbar state: any change to it starts at the first
  // page again.
  const sig = `${q}\n${filter}\n${sort}\n${pinned ?? ""}`;
  const [windowed, setWindowed] = useState({ sig, limit: MAX_ROWS });
  const limit = windowed.sig === sig ? windowed.limit : MAX_ROWS;
  useEffect(() => {
    if (windowed.sig !== sig) setWindowed({ sig, limit: MAX_ROWS });
  }, [sig, windowed.sig]);

  const shown = rows.slice(0, limit);
  const filtered = q.trim() !== "" || filter !== "";
  const clear = () => setRouteQuery({ q: undefined, filter: undefined });

  return (
    <>
      {summaryTarget && (
        <Summary
          fleet={fleet}
          target={summaryTarget}
          active={filter}
          onTarget={(t) => setRouteQuery({ target: t })}
          onBucket={(b) =>
            filter === b
              ? setRouteQuery({ filter: undefined })
              : setRouteQuery({ filter: b, target: summaryTarget })
          }
        />
      )}
      <div className="toolbar">
        <label>
          Search
          <input
            type="search"
            placeholder="Cluster name…"
            aria-label="Search clusters by name"
            value={q}
            onChange={(e) => setRouteQuery({ q: e.target.value })}
          />
        </label>
        <label>
          Filter
          <select
            value={filter}
            onChange={(e) => setRouteQuery({ filter: e.target.value })}
          >
            {FILTERS.map((f) => (
              <option key={f.value} value={f.value}>
                {f.label}
              </option>
            ))}
          </select>
        </label>
        <label>
          Sort
          <select
            value={sort}
            onChange={(e) => setRouteQuery({ sort: e.target.value === "score" ? undefined : e.target.value })}
          >
            {SORTS.map((s) => (
              <option key={s.value} value={s.value}>
                {s.label}
              </option>
            ))}
          </select>
        </label>
        <p className="muted toolbar-count" role="status" aria-live="polite">
          {rows.length} of {clusters.length} clusters
          {filter !== "" && (
            <>
              {" "}
              ·{" "}
              {pinned ? (
                <>
                  for <strong>→ {pinned}</strong>
                </>
              ) : (
                "over the shown targets"
              )}
            </>
          )}
        </p>
      </div>
      {rows.length === 0 ? (
        <div className="state">
          <p className="state-title">No clusters match</p>
          {filtered && (
            <button type="button" className="btn" onClick={clear}>
              Clear filters
            </button>
          )}
        </div>
      ) : (
        <>
          <div className="card table-wrap matrix-wrap">
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
                {shown.map((row) => (
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
          {rows.length > shown.length && (
            <p className="show-more">
              <button
                type="button"
                className="btn btn-ghost"
                onClick={() => setWindowed({ sig, limit: limit + MAX_ROWS })}
              >
                Show {Math.min(MAX_ROWS, rows.length - shown.length)} more
              </button>{" "}
              <span className="muted">
                {shown.length} of {rows.length} rows shown
              </span>
            </p>
          )}
        </>
      )}
    </>
  );
}

// Summary: how many clusters are ready, blocked, unknown, stale, n/a or
// without a stored evaluation for one target. Each cluster is in exactly
// one bucket, so they sum to the cluster count. A bucket applies the
// matching filter (and pins its target).
function Summary({
  fleet,
  target,
  active,
  onTarget,
  onBucket,
}: {
  fleet: FleetResponse;
  target: string;
  active: string;
  onTarget: (t: string) => void;
  onBucket: (b: Bucket) => void;
}) {
  const counts = useMemo(() => countBuckets(fleet.clusters, target), [fleet.clusters, target]);
  return (
    <div className="card summary" role="group" aria-label={`Fleet summary for → ${target}`}>
      <div className="head-row">
        <h2>
          {fleet.clusters.length} cluster{fleet.clusters.length === 1 ? "" : "s"} for →{" "}
          {target}
        </h2>
        <label>
          Target
          <select value={target} onChange={(e) => onTarget(e.target.value)}>
            {fleet.targets.map((t) => (
              <option key={t} value={t}>
                {t}
              </option>
            ))}
          </select>
        </label>
      </div>
      <ul className="buckets">
        {BUCKETS.map((b) => (
          <li key={b}>
            <button
              type="button"
              className={`bucket bucket-${b}`}
              aria-pressed={active === b}
              onClick={() => onBucket(b)}
            >
              <span className="bucket-count">{counts[b]}</span>
              <span className="bucket-label">{BUCKET_LABEL[b]}</span>
            </button>
          </li>
        ))}
      </ul>
    </div>
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
