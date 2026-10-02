import { useMemo, useState } from "react";
import { fetchExport, getCluster, getHistory, getReport, saveBlob } from "../api";
import type { ExportFormat } from "../api";
import { useAsync } from "../hooks";
import { Sparkline } from "../Sparkline";
import type {
  CapabilityGap,
  Finding,
  Report,
  Severity,
  TeamScore,
  Verdict,
} from "../types";
import {
  Citations,
  Empty,
  ErrorState,
  formatTime,
  Loading,
  nextMinors,
  ScoreBadge,
  SeverityPill,
  StaleBadge,
  TargetPicker,
  uniqueSortedVersions,
  VerdictPill,
  verdictOf,
} from "../ui";

const SEVERITIES: Severity[] = ["blocker", "warning", "info"];
const UNATTRIBUTED = "unattributed";

// Cluster drill-down: verdict + score + trend + findings (category/team
// filters) + per-team table for one (cluster, target). target === undefined
// lets the server pick the cluster's default next-minor target; a target
// with no stored evaluation comes back as a what-if. App keys this
// component by route, so filters start fresh for every target.
export function Cluster({
  id,
  target,
  team: initialTeam,
}: {
  id: number;
  target?: string;
  team?: string;
}) {
  const detail = useAsync(() => getCluster(id), [id]);
  const evaluation = useAsync(
    () => Promise.all([getReport(id, target), getHistory(id, target)]),
    [id, target],
  );

  const [category, setCategory] = useState("");
  const [team, setTeam] = useState(initialTeam ?? "");

  const findings = evaluation.data?.[0].findings;
  const categories = useMemo(
    () => uniqueSorted((findings ?? []).map((f) => f.category)),
    [findings],
  );
  // A team from the route stays selectable even when it has no findings
  // at this target, so the select never silently shows "all".
  const teams = useMemo(
    () =>
      uniqueSorted([
        ...(findings ?? []).flatMap(findingTeams),
        ...(initialTeam ? [initialTeam] : []),
      ]),
    [findings, initialTeam],
  );

  if (detail.loading) return <Loading label="Loading cluster…" />;
  if (detail.error) return <ErrorState error={detail.error} onRetry={detail.reload} />;
  const c = detail.data!;

  const suggestions = uniqueSortedVersions([
    ...c.evaluations.map((e) => e.target),
    ...nextMinors(c.serverVersion, 3),
  ]);
  const setTarget = (t: string | undefined) => {
    window.location.hash = t ? `#/cluster/${id}?target=${t}` : `#/cluster/${id}`;
  };

  return (
    <section>
      <header className="page-head">
        <nav aria-label="Breadcrumb" className="crumbs">
          <a href="#/">Fleet</a> <span aria-hidden="true">/</span> {c.name}
        </nav>
        <div className="head-row">
          <h1>
            {c.name}
            {c.stale && (
              <>
                {" "}
                <StaleBadge lastSeen={c.lastSeen} />
              </>
            )}
          </h1>
          <TargetPicker
            value={target}
            suggestions={suggestions}
            onPick={setTarget}
            placeholder="default (next minor)"
            allowEmpty
          />
        </div>
        <p className="muted">
          last seen {formatTime(c.lastSeen)}
          {c.serverVersion && <> · runs {c.serverVersion}</>} · uid{" "}
          <code>{c.clusterUid || "unknown"}</code>
        </p>
      </header>

      {evaluation.loading && <Loading label="Evaluating…" />}
      {evaluation.error && (
        <ErrorState error={evaluation.error} onRetry={evaluation.reload} />
      )}
      {evaluation.data &&
        (() => {
          const [report, history] = evaluation.data;
          const visible = report.findings.filter(
            (f) =>
              (category === "" || f.category === category) &&
              (team === "" || findingTeams(f).includes(team)),
          );
          const verdict = verdictOf(report);
          const whatIf = report.source === "what-if";
          return (
            <>
              <div className="card score-card">
                <div className="score-big">
                  <ScoreBadge score={report.score} verdict={verdict} />
                  <div>
                    <p className="score-target">
                      readiness for <strong>→ {report.target}</strong>{" "}
                      <VerdictPill verdict={verdict} />
                      {whatIf && (
                        <>
                          {" "}
                          <span className="badge badge-whatif">
                            what-if (not stored, no history)
                          </span>
                        </>
                      )}
                    </p>
                    <p className="muted">
                      {count(report.findings, "blocker")} blockers ·{" "}
                      {count(report.findings, "warning")} warnings ·{" "}
                      {count(report.findings, "info")} info
                      {report.suppressed && report.suppressed.length > 0 && (
                        <> · {report.suppressed.length} suppressed</>
                      )}{" "}
                      · KB {report.kbVersion}
                      {report.evaluatedAt && (
                        <>
                          {" "}
                          · {whatIf ? "computed" : "evaluated"}{" "}
                          {formatTime(report.evaluatedAt)}
                        </>
                      )}
                    </p>
                    {report.notApplicable && (
                      <p className="muted">
                        This cluster already runs {report.serverVersion ?? "this version or newer"}:
                        → {report.target} is not an upgrade.
                      </p>
                    )}
                  </div>
                </div>
                {history.length > 1 ? (
                  <Sparkline points={history} />
                ) : (
                  <p className="muted">
                    {whatIf
                      ? "What-ifs are computed on request and not stored, so they have no trend."
                      : "Trend appears after the second stored evaluation."}
                  </p>
                )}
                <ExportButtons
                  id={id}
                  clusterName={c.name}
                  report={report}
                  disabledReason={
                    whatIf
                      ? "Exports cover stored evaluations only; a what-if is not stored."
                      : report.notApplicable
                        ? "Nothing to export: the cluster already runs this target."
                        : undefined
                  }
                />
              </div>

              {verdict === "unknown" && <UnknownVerdict gaps={report.notAssessed ?? []} />}
              {report.notAssessed && report.notAssessed.length > 0 && (
                <NotAssessed gaps={report.notAssessed} />
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
                {report.findings.length === 0 ? (
                  <Empty
                    title="No findings"
                    hint={
                      verdict === "unknown"
                        ? "None found — but some required checks did not run fully (see above)."
                        : "Nothing in this cluster blocks or degrades the target upgrade."
                    }
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

              <TeamsTable
                teams={report.teams}
                clusterId={id}
                target={report.target}
                verdict={verdict}
              />
            </>
          );
        })()}
    </section>
  );
}

// ExportButtons download the auditor export (CSV or HTML) of the stored
// evaluation shown. They fetch with the read token and save the response,
// so they work with serve --read-token where a plain link would get 401.
function ExportButtons({
  id,
  clusterName,
  report,
  disabledReason,
}: {
  id: number;
  clusterName: string;
  report: Report;
  disabledReason?: string;
}) {
  const [busy, setBusy] = useState<ExportFormat | "">("");
  const [error, setError] = useState("");

  const download = async (format: ExportFormat) => {
    setBusy(format);
    setError("");
    try {
      const { blob, filename } = await fetchExport(id, report.target, format, clusterName);
      saveBlob(blob, filename);
    } catch (err) {
      setError(`Export failed: ${(err as Error).message}`);
    } finally {
      setBusy("");
    }
  };

  const noteId = `export-note-${id}`;
  return (
    <div className="exports">
      <button
        type="button"
        className="btn btn-ghost"
        disabled={!!disabledReason || busy !== ""}
        aria-describedby={disabledReason ? noteId : undefined}
        onClick={() => void download("csv")}
      >
        Download CSV
      </button>
      <button
        type="button"
        className="btn btn-ghost"
        disabled={!!disabledReason || busy !== ""}
        aria-describedby={disabledReason ? noteId : undefined}
        onClick={() => void download("html")}
      >
        Download HTML report
      </button>
      {disabledReason && (
        <span id={noteId} className="muted">
          {disabledReason}
        </span>
      )}
      {error && (
        <span className="field-error" role="alert">
          {error}
        </span>
      )}
    </div>
  );
}

// UnknownVerdict explains an unknown verdict: nothing blocked, but these
// required checks did not run (fully), so blockers may have been missed.
function UnknownVerdict({ gaps }: { gaps: CapabilityGap[] }) {
  const required = gaps.filter((g) => g.required);
  return (
    <section className="card verdict-unknown-card" aria-labelledby="verdict-unknown">
      <h2 id="verdict-unknown">Verdict unknown</h2>
      <p>
        No blockers were found, but{" "}
        {required.length > 0
          ? "these required checks did not run fully, so blockers may have been missed:"
          : "the evaluation could not assess everything it needs."}
      </p>
      {required.length > 0 && (
        <ul>
          {required.map((g) => (
            <li key={g.capability}>
              <code>{g.capability}</code>
              {g.partial ? " (partial)" : ""} — {g.reason}
            </li>
          ))}
        </ul>
      )}
    </section>
  );
}

function NotAssessed({ gaps }: { gaps: CapabilityGap[] }) {
  return (
    <section className="card not-assessed" aria-labelledby="not-assessed">
      <h2 id="not-assessed">Not assessed</h2>
      <ul>
        {gaps.map((g) => (
          <li key={g.capability}>
            <code>{g.capability}</code>{" "}
            {g.required && <span className="tag tag-required">required</span>}{" "}
            {g.partial && <span className="tag">partial</span>} — {g.reason}
            {g.skipped && g.skipped.length > 0 && (
              <p className="chips">
                <span className="visually-hidden">Skipped: </span>
                {g.skipped.map((s) => (
                  <span key={s} className="chip" title="skipped">
                    {s}
                  </span>
                ))}
              </p>
            )}
          </li>
        ))}
      </ul>
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
      {f.citations && f.citations.length > 0 && <Citations urls={f.citations} />}
    </li>
  );
}

// teamVerdict: a team with blockers is blocked; otherwise it is only as
// ready as the cluster's assessment — an unknown cluster verdict means the
// team's blockers may have been missed too.
function teamVerdict(ts: TeamScore, cluster: Verdict): Verdict {
  if (!ts.ready) return "blocked";
  return cluster === "unknown" ? "unknown" : "ready";
}

function TeamsTable({
  teams,
  clusterId,
  target,
  verdict,
}: {
  teams?: Record<string, TeamScore>;
  clusterId: number;
  target: string;
  verdict: Verdict;
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
                <th scope="row">
                  <a
                    href={`#/cluster/${clusterId}?target=${target}&team=${encodeURIComponent(name)}`}
                  >
                    {name}
                  </a>
                </th>
                <td>
                  <ScoreBadge score={ts.score} verdict={teamVerdict(ts, verdict)} />
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
