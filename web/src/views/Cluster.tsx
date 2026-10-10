import { useEffect, useMemo, useRef, useState } from "react";
import { fetchExport, getCluster, getHistory, getReport, saveBlob } from "../api";
import type { ExportFormat } from "../api";
import { setRouteQuery, useAsync, useRouteQuery } from "../hooks";
import { Sparkline } from "../Sparkline";
import { UnrecognizedImages } from "../UnrecognizedImages";
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
  Freshness,
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
// The server's name for the bucket of findings no team owns. It carries
// parentheses, which a label value cannot, so a team a namespace label calls
// "unattributed" stays a separate row and a separate filter choice (#243).
const UNATTRIBUTED = "(unattributed)";

// Cluster drill-down: verdict + score + trend + findings + per-team table
// for one (cluster, target). target === undefined lets the server pick the
// cluster's default next-minor target; a target with no stored evaluation
// comes back as a what-if. The findings filters live in the hash query next
// to the target (#/cluster/3?target=1.37&category=removed-api&severity=
// blocker&team=payments&q=ingress), so a reload or a pasted link restores
// them; finding=<key> scrolls to and highlights one finding. App keys this
// component by cluster and target: a new target starts without filters.
export function Cluster({ id, target }: { id: number; target?: string }) {
  const query = useRouteQuery();
  const category = query.get("category") ?? "";
  const team = query.get("team") ?? "";
  const q = query.get("q") ?? "";
  const finding = query.get("finding") ?? "";
  const severityParam = query.get("severity") ?? "";
  const severity = SEVERITIES.find((s) => s === severityParam) ?? "";

  const detail = useAsync(() => getCluster(id), [id]);
  const evaluation = useAsync(
    () => Promise.all([getReport(id, target), getHistory(id, target)]),
    [id, target],
  );

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
        ...(team ? [team] : []),
      ]),
    [findings, team],
  );
  const categoryChoices = useMemo(
    () => uniqueSorted([...categories, ...(category ? [category] : [])]),
    [categories, category],
  );

  // A linked finding is scrolled to once it is on the page; later refreshes
  // do not move the view again.
  const scrolled = useRef("");
  const ready = detail.data !== undefined && evaluation.data !== undefined;
  useEffect(() => {
    if (!finding || !ready || scrolled.current === finding) return;
    const el = Array.from(
      document.querySelectorAll<HTMLElement>("[data-finding-key]"),
    ).find((e) => e.dataset.findingKey === finding);
    if (!el) return;
    scrolled.current = finding;
    if (typeof el.scrollIntoView === "function") el.scrollIntoView({ block: "center" });
  }, [finding, ready, category, severity, team, q]);

  if (detail.loading) return <Loading label="Loading cluster…" />;
  if (detail.error && !detail.data) return <ErrorState error={detail.error} onRetry={detail.reload} />;
  const c = detail.data!;

  const suggestions = uniqueSortedVersions([
    ...c.evaluations.map((e) => e.target),
    ...nextMinors(c.serverVersion, 3),
  ]);
  const refresh = () => {
    detail.reload();
    evaluation.reload();
  };
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
          <Freshness
            updatedAt={evaluation.updatedAt ?? detail.updatedAt}
            refreshing={detail.refreshing || evaluation.refreshing}
            error={detail.error ?? (evaluation.data ? evaluation.error : undefined)}
            onRefresh={refresh}
          />
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
      {evaluation.error && !evaluation.data && (
        <ErrorState error={evaluation.error} onRetry={evaluation.reload} />
      )}
      {evaluation.data &&
        (() => {
          const [report, history] = evaluation.data;
          const visible = report.findings.filter(
            (f) =>
              (category === "" || f.category === category) &&
              (severity === "" || f.severity === severity) &&
              (team === "" || findingTeams(f).includes(team)) &&
              matchesQuery(f, q),
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
                    {report.addOnEvidenceAgeSeconds != null &&
                      report.addOnEvidenceAgeSeconds > 0 && (
                        <p className="muted" data-testid="pod-evidence-age">
                          Pod evidence {Math.max(1, Math.round(report.addOnEvidenceAgeSeconds / 60))}{" "}
                          min old: the agent lists every pod only every few ticks, so an add-on
                          installed or upgraded since may still show its old version.
                        </p>
                      )}
                    {report.notApplicable && (
                      <p className="muted">
                        This cluster already runs {report.serverVersion ?? "this version or newer"}:
                        → {report.target} is not an upgrade.
                      </p>
                    )}
                  </div>
                </div>
                {/* History holds stored evaluations only; under a what-if
                    label it would chart earlier snapshots as if they
                    were this report's trend. */}
                {!whatIf && history.length > 1 ? (
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
                      Search
                      <input
                        type="search"
                        placeholder="Title, detail, namespace…"
                        value={q}
                        onChange={(e) => setRouteQuery({ q: e.target.value })}
                      />
                    </label>
                    <label>
                      Category
                      <select
                        value={category}
                        onChange={(e) => setRouteQuery({ category: e.target.value })}
                      >
                        <option value="">all</option>
                        {categoryChoices.map((cat) => (
                          <option key={cat} value={cat}>
                            {cat}
                          </option>
                        ))}
                      </select>
                    </label>
                    <label>
                      Severity
                      <select
                        value={severity}
                        onChange={(e) => setRouteQuery({ severity: e.target.value })}
                      >
                        <option value="">all</option>
                        {SEVERITIES.map((sev) => (
                          <option key={sev} value={sev}>
                            {sev}
                          </option>
                        ))}
                      </select>
                    </label>
                    <label>
                      Team
                      <select
                        value={team}
                        onChange={(e) => setRouteQuery({ team: e.target.value })}
                      >
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
                            <FindingItem
                              key={f.key ?? f.title}
                              f={f}
                              href={`#/cluster/${id}?target=${encodeURIComponent(report.target)}&finding=${encodeURIComponent(findingKey(f))}`}
                              highlighted={finding !== "" && findingKey(f) === finding}
                            />
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

              <UnrecognizedImages
                images={report.unrecognizedImages}
                omitted={report.unrecognizedImagesOmitted}
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

function FindingItem({
  f,
  href,
  highlighted,
}: {
  f: Finding;
  href: string;
  highlighted: boolean;
}) {
  return (
    <li
      className={highlighted ? "finding finding-target" : "finding"}
      data-finding-key={findingKey(f)}
      aria-current={highlighted ? "location" : undefined}
    >
      <p className="finding-title">
        <span className="cat">{f.category}</span> {f.title}{" "}
        <a
          className="finding-link"
          href={href}
          aria-label={`Link to this finding: ${f.title}`}
          title="Link to this finding"
        >
          #
        </a>
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
          {f.namespacesOmitted ? (
            <span className="chip muted">and {f.namespacesOmitted} more</span>
          ) : null}
        </p>
      ) : null}
      {f.remediation && <p className="remediation">{f.remediation}</p>}
      {f.citations && f.citations.length > 0 && <Citations urls={f.citations} />}
    </li>
  );
}

// teamVerdict: the server's verdict for the team (blocked by its own or an
// unattributed blocker, unknown on a required gap). An older server sends
// only ready, which counted the team's own findings: a team with blockers
// is blocked; otherwise it is only as ready as the cluster's assessment.
function teamVerdict(ts: TeamScore, cluster: Verdict): Verdict {
  if (ts.verdict) return ts.verdict;
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

// findingKey identifies a finding in a link: the server's stable key, or
// the title for a server that sends none.
function findingKey(f: Finding): string {
  return f.key ?? f.title;
}

// matchesQuery: a case-insensitive substring of what the finding shows —
// title, detail, and the team and namespace chips.
function matchesQuery(f: Finding, q: string): boolean {
  const needle = q.trim().toLowerCase();
  if (needle === "") return true;
  return [f.title, f.detail, ...(f.teams ?? []), ...(f.namespaces ?? [])].some((t) =>
    t.toLowerCase().includes(needle),
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
