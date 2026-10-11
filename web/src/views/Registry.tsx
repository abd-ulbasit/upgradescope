import { useMemo, useState } from "react";
import { getRegistry } from "../api";
import { useAsync } from "../hooks";
import type { AddOn } from "../types";
import { Citations, Empty, ErrorState, Freshness, Loading } from "../ui";

// Registry: browse the add-on EOL/compat dataset embedded in this server
// binary (GET /api/v1/registry) — exactly what evaluations run against.
// The status shown is derived from the entry's dates and today's UTC date,
// not read from the hand-maintained support.status (see effectiveStatus);
// `now` is the injectable clock, for deterministic tests.
export function Registry({ now }: { now?: Date } = {}) {
  const today = utcDate(now ?? new Date());
  const reg = useAsync(getRegistry, []);
  const [q, setQ] = useState("");

  const addons = reg.data?.addons;
  const visible = useMemo(() => {
    if (!addons) return [];
    const needle = q.trim().toLowerCase();
    if (!needle) return addons;
    return addons.filter(
      (a) =>
        a.id.toLowerCase().includes(needle) ||
        a.display_name.toLowerCase().includes(needle),
    );
  }, [addons, q]);

  if (reg.loading) return <Loading label="Loading registry…" />;
  if (reg.error && !reg.data) return <ErrorState error={reg.error} onRetry={reg.reload} />;

  return (
    <section>
      <header className="page-head">
        <div className="head-row">
          <h1>Add-on registry</h1>
          <Freshness
            updatedAt={reg.updatedAt}
            refreshing={reg.refreshing}
            error={reg.error}
            onRefresh={reg.reload}
          />
          <input
            type="search"
            placeholder="Filter add-ons…"
            aria-label="Filter add-ons"
            value={q}
            onChange={(e) => setQ(e.target.value)}
          />
        </div>
        <p className="muted">
          {addons!.length} curated entries embedded in this binary — every
          status carries an upstream citation.
        </p>
      </header>
      {visible.length === 0 ? (
        <Empty title={`No add-ons match “${q}”`} />
      ) : (
        <div className="addon-grid">
          {visible.map((a) => (
            <AddOnCard key={a.id} a={a} today={today} />
          ))}
        </div>
      )}
    </section>
  );
}

// utcDate is d's calendar date in UTC, YYYY-MM-DD: the form registry dates
// are written in, so string comparison orders them.
function utcDate(d: Date): string {
  return d.toISOString().slice(0, 10);
}

type Effective = {
  // CSS suffix and badge text.
  tone: "supported" | "eol" | "unknown" | "extended";
  label: string;
  // The eol_date and extended_eol_date are on or before today. A date that
  // is today has passed, as in the engine (a date is not "after" now).
  eolPassed: boolean;
  extPassed: boolean;
};

// effectiveStatus re-derives what the badge says from the dates: a stored
// status is written when an entry is added and is stale once its date
// passes. The engine's findings already use the dates; this keeps the
// dashboard's display in step. The stored status stays as data.
//   eol_date passed, extended_eol_date ahead -> extended support only
//   eol_date passed otherwise                 -> eol
//   anything else                             -> the stored status
export function effectiveStatus(a: AddOn, today: string): Effective {
  const { status, eol_date: eol, extended_eol_date: ext } = a.support;
  const eolPassed = !!eol && eol <= today;
  const extPassed = !!ext && ext <= today;
  // Like the engine, an entry stored as eol ignores its extended date.
  if (eolPassed && ext && !extPassed && status !== "eol") {
    return { tone: "extended", label: `extended support only · until ${ext}`, eolPassed, extPassed };
  }
  if (eolPassed) {
    return { tone: "eol", label: `eol · ${eol}`, eolPassed, extPassed };
  }
  return { tone: status as Effective["tone"], label: `${status}${eol ? ` · ${eol}` : ""}`, eolPassed, extPassed };
}

function AddOnCard({ a, today }: { a: AddOn; today: string }) {
  const eff = effectiveStatus(a, today);
  return (
    <article className="card addon">
      <header className="head-row">
        <h2>{a.display_name}</h2>
        <span className={`status status-${eff.tone}`}>{eff.label}</span>
      </header>
      <p className="muted">
        <code>{a.id}</code>
      </p>
      {a.support.extended_eol_date && (
        <p className="support-split">
          {a.support.status === "eol" || eff.extPassed
            ? "Extended support ended "
            : "Supported until "}
          {a.support.extended_eol_date}
          {a.support.extended_support_condition
            ? ` only if ${a.support.extended_support_condition}`
            : ""}
          {a.support.eol_date
            ? `; for everyone else support ${eff.eolPassed ? "ended" : "ends"} ${a.support.eol_date}`
            : ""}
          .
        </p>
      )}
      {(a.matchers.images?.length ||
        a.matchers.components?.length ||
        a.matchers.charts?.length) ? (
        <p className="chips">
          {a.matchers.images?.map((m) => (
            <span key={`i-${m}`} className="chip" title="image prefix matcher">
              {m}
            </span>
          ))}
          {a.matchers.components?.map((c) => (
            <span
              key={`k-${c.image}`}
              className="chip"
              title="component image, versioned on its own"
            >
              {c.image}
            </span>
          ))}
          {a.matchers.charts?.map((m) => (
            <span key={`c-${m}`} className="chip chip-team" title="chart matcher">
              {m}
            </span>
          ))}
        </p>
      ) : null}
      {a.compat && a.compat.length > 0 && (
        <div className="table-wrap">
          <table className="compat">
            <thead>
              <tr>
                <th scope="col">Version range</th>
                <th scope="col">Kubernetes</th>
              </tr>
            </thead>
            <tbody>
              {a.compat.map((c) => (
                <tr key={c.range}>
                  <td>
                    <code>{c.range}</code>
                  </td>
                  <td>
                    {c.k8s_min} – {c.k8s_max}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
      {a.recommendation && <p className="remediation">{a.recommendation}</p>}
      <Citations urls={a.support.citations} />
    </article>
  );
}
