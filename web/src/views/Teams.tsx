import { getFleet, getFleetTeams } from "../api";
import { defaultTarget } from "../fleet";
import { useAsync } from "../hooks";
import type { ExcludedReason, FleetTeamsResponse } from "../types";
import {
  Empty,
  ErrorState,
  Freshness,
  Loading,
  ScoreBadge,
  TargetPicker,
  uniqueSortedVersions,
  VerdictPill,
} from "../ui";

// Teams: the fleet-wide per-team rollup (/api/v1/fleet/teams) for one
// target — worst score, verdict, blockers and the clusters each team has
// findings in. The verdict is what says whether a team can upgrade: its
// score counts only its own findings, so a team with one warning scores 98
// even when a blocker no team owns, or a check that did not run, makes it
// blocked or unknown. The server answers any target: clusters without a stored evaluation
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
  if (fleet.error && !fleet.data) return <ErrorState error={fleet.error} onRetry={fleet.reload} />;

  const refresh = () => {
    fleet.reload();
    rollup.reload();
  };
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
          <Freshness
            updatedAt={rollup.updatedAt ?? fleet.updatedAt}
            refreshing={fleet.refreshing || rollup.refreshing}
            error={fleet.error ?? rollup.error}
            onRefresh={refresh}
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
      ) : rollup.error && !rollup.data ? (
        <ErrorState error={rollup.error} onRetry={rollup.reload} />
      ) : (
        rollup.data && <TeamsTable data={rollup.data} />
      )}
    </section>
  );
}

function TeamsTable({ data }: { data: FleetTeamsResponse }) {
  // The rollup names clusters, but clusters are identified by UID and two
  // can share a name: such a name gets no link rather than a wrong one.
  const ids = new Map<string, number[]>();
  for (const e of data.evaluated) ids.set(e.name, [...(ids.get(e.name) ?? []), e.clusterId]);
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
                <th scope="col">Verdict</th>
                <th scope="col">Blockers</th>
                <th scope="col">Clusters</th>
              </tr>
            </thead>
            <tbody>
              {entries.map(([team, agg]) => (
                <tr key={team}>
                  <th scope="row">{team}</th>
                  <td>
                    <ScoreBadge score={agg.worstScore} verdict={agg.verdict} />
                  </td>
                  <td>{agg.verdict && <VerdictPill verdict={agg.verdict} />}</td>
                  <td className={agg.blockers > 0 ? "blockers" : undefined}>
                    {agg.blockers}
                  </td>
                  <td className="cluster-links">
                    {agg.clusters.map((name) => {
                      const matches = ids.get(name) ?? [];
                      return matches.length === 1 ? (
                        <a
                          key={name}
                          href={`#/cluster/${matches[0]}?target=${data.target}&team=${encodeURIComponent(team)}`}
                        >
                          {name}
                        </a>
                      ) : (
                        <span
                          key={name}
                          title={
                            matches.length > 1
                              ? `${matches.length} clusters are named ${name}; open them from the fleet view.`
                              : undefined
                          }
                        >
                          {name}
                        </span>
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
        {excludedNotes(data).map((n) => (
          <li key={n.key}>
            {n.label}: {n.names.join(", ")}.
          </li>
        ))}
        {data.notApplicable.length > 0 && (
          <li>
            Already at or past {data.target}: {data.notApplicable.join(", ")}.
          </li>
        )}
      </ul>
    </>
  );
}

// excludedNotes groups the clusters left out of the rollup by the reason the
// server gives, each with a label that says what is wrong: a cluster whose
// agent is pushing is not "no snapshot yet". A server that sends no reasons
// (`excluded` absent) only names the clusters, so the label stays neutral.
function excludedNotes(
  data: FleetTeamsResponse,
): { key: string; label: string; names: string[] }[] {
  if (!data.excluded) {
    return data.missing.length > 0
      ? [{ key: "missing", label: "Not included (no snapshot, or its report was too large or unreadable)", names: data.missing }]
      : [];
  }
  const labels: Record<ExcludedReason, string> = {
    "no-snapshot": "No snapshot yet",
    "too-large": "Not included, its report for → TARGET would be over --max-snapshot-bytes",
    unreadable: "Not included, its stored inventory could not be read (see the server log)",
  };
  const order: ExcludedReason[] = ["no-snapshot", "too-large", "unreadable"];
  const notes = order.flatMap((reason) => {
    const names = data.excluded!.filter((e) => e.reason === reason).map((e) => e.name);
    return names.length > 0
      ? [{ key: reason, label: labels[reason].replace("TARGET", data.target), names }]
      : [];
  });
  // A cluster whose reason this dashboard does not know (a newer server), or
  // one `missing` names that `excluded` does not, is still said to be left
  // out: never dropped from the page.
  const said = new Set(notes.flatMap((n) => n.names));
  const rest = [...new Set([...data.excluded.map((e) => e.name), ...data.missing])].filter((n) => !said.has(n));
  return rest.length > 0 ? [...notes, { key: "other", label: "Not included", names: rest }] : notes;
}
