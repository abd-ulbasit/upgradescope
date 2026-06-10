import { useMemo, useState } from "react";
import { getCluster, getFindings, getHistory, getReport } from "../api";
import { useAsync } from "../hooks";
import { Sparkline } from "../Sparkline";
import type { Finding, Severity } from "../types";
import {
  Empty,
  ErrorState,
  formatTime,
  Loading,
  ScoreBadge,
  SeverityPill,
} from "../ui";

const SEVERITIES: Severity[] = ["blocker", "warning", "info"];
const UNATTRIBUTED = "unattributed";

// Cluster drill-down: score + trend + findings (category/team filters) +
// per-team table for one (cluster, target). target === undefined lets the
// server pick the cluster's default next-minor target.
export function Cluster({ id, target }: { id: number; target?: string }) {
  const detail = useAsync(() => getCluster(id), [id]);
  const evaluation = useAsync(
    () =>
      Promise.all([
        getReport(id, target),
        getFindings(id, target),
        getHistory(id, target),
      ]),
    [id, target],
  );

  const [category, setCategory] = useState("");
  const [team, setTeam] = useState("");

  const findings = evaluation.data?.[1].findings;
  const categories = useMemo(
    () => uniqueSorted((findings ?? []).map((f) => f.category)),
    [findings],
  );
  const teams = useMemo(
    () =>
      uniqueSorted(
        (findings ?? []).flatMap((f) =>
          f.teams && f.teams.length > 0 ? f.teams : [UNATTRIBUTED],
        ),
      ),
    [findings],
  );

  if (detail.loading) return <Loading label="Loading cluster…" />;
  if (detail.error) return <ErrorState error={detail.error} onRetry={detail.reload} />;
  const c = detail.data!;

  const targets = uniqueSorted(c.evaluations.map((e) => e.target));
  const setTarget = (t: string) => {
    window.location.hash = t ? `#/cluster/${id}?target=${t}` : `#/cluster/${id}`;
  };

  return (
    <section>
      <header className="page-head">
        <nav aria-label="Breadcrumb" className="crumbs">
          <a href="#/">Fleet</a> <span aria-hidden="true">/</span> {c.name}
        </nav>
        <div className="head-row">
          <h1>{c.name}</h1>
          <label className="target-pick">
            Target
            <select
              value={target ?? ""}
              onChange={(e) => setTarget(e.target.value)}
            >
              <option value="">default (next minor)</option>
              {targets.map((t) => (
                <option key={t} value={t}>
                  {t}
                </option>
              ))}
            </select>
          </label>
        </div>
        <p className="muted">
          last seen {formatTime(c.lastSeen)} · uid{" "}
          <code>{c.clusterUid || "unknown"}</code>
        </p>
      </header>

      {evaluation.loading && <Loading label="Evaluating…" />}
      {evaluation.error && (
        <ErrorState error={evaluation.error} onRetry={evaluation.reload} />
      )}
      {evaluation.data &&
        (() => {
          const [report, findingsRes, history] = evaluation.data;
          const visible = findingsRes.findings.filter(
            (f) =>
              (category === "" || f.category === category) &&
              (team === "" || findingTeams(f).includes(team)),
          );
          return (
            <>
              <div className="card score-card">
                <div className="score-big">
                  <ScoreBadge score={report.score} ready={report.ready} />
                  <div>
                    <p className="score-target">
                      readiness for <strong>→ {report.target}</strong>
                    </p>
                    <p className="muted">
                      {count(findingsRes.findings, "blocker")} blockers ·{" "}
                      {count(findingsRes.findings, "warning")} warnings ·{" "}
                      {count(findingsRes.findings, "info")} info · KB{" "}
                      {report.kbVersion}
                    </p>
                  </div>
                </div>
                {history.length > 1 ? (
                  <Sparkline points={history} />
                ) : (
                  <p className="muted">
                    Trend appears after the second stored evaluation.
                  </p>
                )}
              </div>

              {report.notAssessed && report.notAssessed.length > 0 && (
                <div className="card not-assessed">
                  <h2>Not assessed</h2>
                  <ul>
                    {report.notAssessed.map((g) => (
                      <li key={g.capability}>
                        <code>{g.capability}</code> — {g.reason}
                      </li>
                    ))}
                  </ul>
                </div>
              )}

              <div className="card">
                <div className="head-row">
                  <h2>Findings</h2>
                  <div className="filters">
                    <label>
                      Category
                      <select
                        value={category}
                        onChange={(e) => setCategory(e.target.value)}
                      >
                        <option value="">all</option>
                        {categories.map((cat) => (
                          <option key={cat} value={cat}>
                            {cat}
                          </option>
                        ))}
                      </select>
                    </label>
                    <label>
                      Team
                      <select value={team} onChange={(e) => setTeam(e.target.value)}>
                        <option value="">all</option>
                        {teams.map((t) => (
                          <option key={t} value={t}>
                            {t}
                          </option>
                        ))}
                      </select>
                    </label>
                  </div>
                </div>
                {findingsRes.findings.length === 0 ? (
                  <Empty
                    title="No findings"
                    hint="Nothing in this cluster blocks or degrades the target upgrade."
                  />
                ) : visible.length === 0 ? (
                  <Empty title="No findings match the current filters" />
                ) : (
                  SEVERITIES.map((sev) => {
                    const group = visible.filter((f) => f.severity === sev);
                    if (group.length === 0) return null;
                    return (
                      <div key={sev} className="sev-group">
                        <h3>
                          <SeverityPill severity={sev} />{" "}
                          <span className="muted">{group.length}</span>
                        </h3>
                        <ul className="findings">
                          {group.map((f) => (
                            <FindingItem key={f.key ?? f.title} f={f} />
                          ))}
                        </ul>
                      </div>
                    );
                  })
                )}
              </div>

              <TeamsTable teams={report.teams} />
            </>
          );
        })()}
    </section>
  );
}

function FindingItem({ f }: { f: Finding }) {
  return (
    <li className="finding">
      <p className="finding-title">
        <span className="cat">{f.category}</span> {f.title}
      </p>
      <p className="finding-detail">{f.detail}</p>
      {(f.namespaces?.length || f.teams?.length) ? (
        <p className="chips">
          {f.teams?.map((t) => (
            <span key={`t-${t}`} className="chip chip-team">
              {t}
            </span>
          ))}
          {f.namespaces?.map((ns) => (
            <span key={`n-${ns}`} className="chip">
              {ns}
            </span>
          ))}
        </p>
      ) : null}
      {f.remediation && <p className="remediation">{f.remediation}</p>}
      {f.citations && f.citations.length > 0 && (
        <p className="citations">
          {f.citations.map((url) => (
            <a key={url} href={url} target="_blank" rel="noreferrer">
              {citationLabel(url)}
            </a>
          ))}
        </p>
      )}
    </li>
  );
}

function TeamsTable({
  teams,
}: {
  teams?: Record<string, import("../types").TeamScore>;
}) {
  const entries = Object.entries(teams ?? {}).sort(
    ([, a], [, b]) => a.score - b.score,
  );
  if (entries.length === 0) return null;
  return (
    <div className="card">
      <h2>Teams</h2>
      <div className="table-wrap">
        <table>
          <thead>
            <tr>
              <th scope="col">Team</th>
              <th scope="col">Score</th>
              <th scope="col">Blockers</th>
              <th scope="col">Warnings</th>
            </tr>
          </thead>
          <tbody>
            {entries.map(([name, ts]) => (
              <tr key={name}>
                <th scope="row">{name}</th>
                <td>
                  <ScoreBadge score={ts.score} ready={ts.ready} />
                </td>
                <td>{ts.blockers}</td>
                <td>{ts.warnings}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </div>
  );
}

function findingTeams(f: Finding): string[] {
  return f.teams && f.teams.length > 0 ? f.teams : [UNATTRIBUTED];
}

function count(fs: Finding[], sev: Severity): number {
  return fs.filter((f) => f.severity === sev).length;
}

function uniqueSorted(values: string[]): string[] {
  return [...new Set(values)].sort();
}

// citationLabel shortens a citation URL to its host for compact display.
function citationLabel(url: string): string {
  try {
    return new URL(url).host;
  } catch {
    return url;
  }
}
