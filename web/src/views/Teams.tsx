import { getFleet, getFleetTeams } from "../api";
import { useAsync } from "../hooks";
import type { FleetResponse, FleetTeamsResponse } from "../types";
import {
  Empty,
  ErrorState,
  Loading,
  ScoreBadge,
  TargetPicker,
  uniqueSortedVersions,
} from "../ui";

// Teams: the fleet-wide per-team rollup (/api/v1/fleet/teams) for one
// target — worst score, blockers and the clusters each team has findings
// in. The server answers any target: clusters without a stored evaluation
// for it are computed as what-ifs. Without a target in the route the
// fleet column that applies to the most clusters is used.
export function Teams({ target }: { target?: string }) {
  const fleet = useAsync(getFleet, []);
  const effective = target ?? (fleet.data && defaultTarget(fleet.data));
  const rollup = useAsync(
    () => (effective ? getFleetTeams(effective) : Promise.resolve(undefined)),
    [effective],
  );

  if (fleet.loading) return <Loading label="Loading fleet…" />;
  if (fleet.error) return <ErrorState error={fleet.error} onRetry={fleet.reload} />;

  const pick = (t: string | undefined) => {
    window.location.hash = t ? `#/teams?target=${t}` : "#/teams";
  };

  return (
    <section>
      <header className="page-head">
        <div className="head-row">
          <h1>Teams</h1>
          <TargetPicker
            value={effective}
            suggestions={uniqueSortedVersions(fleet.data!.targets)}
            onPick={pick}
            placeholder="1.38"
          />
        </div>
        <p className="muted">
          Each team's worst score and total blockers across the fleet
          {effective ? (
            <>
              {" "}
              for <strong>→ {effective}</strong>
            </>
          ) : null}
          . Teams come from namespace labels and the team map.
        </p>
      </header>
      {!effective ? (
        <Empty
          title="No target to roll up"
          hint="Enter a target above, or push a snapshot so the fleet has a default one."
        />
      ) : rollup.loading ? (
        <Loading label="Rolling up teams…" />
      ) : rollup.error ? (
        <ErrorState error={rollup.error} onRetry={rollup.reload} />
      ) : (
        rollup.data && <TeamsTable data={rollup.data} />
      )}
    </section>
  );
}

// defaultTarget picks the fleet column that is an upgrade for the most
// clusters (the lowest on a tie): the first column is often one most
// clusters already run.
function defaultTarget({ targets, clusters }: FleetResponse): string | undefined {
  let best: string | undefined;
  let bestCount = -1;
  for (const t of targets) {
    const n = clusters.filter((c) => !c.notApplicable?.includes(t)).length;
    if (n > bestCount) {
      best = t;
      bestCount = n;
    }
  }
  return best;
}

function TeamsTable({ data }: { data: FleetTeamsResponse }) {
  const ids = new Map(data.evaluated.map((e) => [e.name, e.clusterId]));
  const entries = Object.entries(data.teams).sort(
    ([an, a], [bn, b]) => a.worstScore - b.worstScore || an.localeCompare(bn),
  );
  const whatIfs = data.evaluated.filter((e) => e.source === "what-if").length;

  return (
    <>
      {entries.length === 0 ? (
        <Empty
          title={`No team has findings for → ${data.target}`}
          hint="Findings are attributed to teams through namespace labels or serve --team-map."
        />
      ) : (
        <div className="card table-wrap">
          <table>
            <thead>
              <tr>
                <th scope="col">Team</th>
                <th scope="col">Worst score</th>
                <th scope="col">Blockers</th>
                <th scope="col">Clusters</th>
              </tr>
            </thead>
            <tbody>
              {entries.map(([team, agg]) => (
                <tr key={team}>
                  <th scope="row">{team}</th>
                  <td>
                    <ScoreBadge score={agg.worstScore} />
                  </td>
                  <td className={agg.blockers > 0 ? "blockers" : undefined}>
                    {agg.blockers}
                  </td>
                  <td className="cluster-links">
                    {agg.clusters.map((name) => {
                      const id = ids.get(name);
                      return id === undefined ? (
                        <span key={name}>{name}</span>
                      ) : (
                        <a
                          key={name}
                          href={`#/cluster/${id}?target=${data.target}&team=${encodeURIComponent(team)}`}
                        >
                          {name}
                        </a>
                      );
                    })}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
      <ul className="muted rollup-notes">
        {whatIfs > 0 && (
          <li>
            {whatIfs} cluster{whatIfs > 1 ? "s" : ""} computed as a what-if (no
            stored evaluation for → {data.target}; not stored).
          </li>
        )}
        {data.missing.length > 0 && (
          <li>No snapshot yet: {data.missing.join(", ")}.</li>
        )}
        {data.notApplicable.length > 0 && (
          <li>
            Already at or past {data.target}: {data.notApplicable.join(", ")}.
          </li>
        )}
      </ul>
    </>
  );
}
