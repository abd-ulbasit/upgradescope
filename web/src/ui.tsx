// Shared presentational pieces: loading / error / empty states (every view
// renders one of these before data), score + verdict + severity styling,
// the target picker and citation labels.

import { useId, useState } from "react";
import { ApiError, getScope } from "./api";
import type { Severity, Verdict } from "./types";

// SET_TOKEN_LABEL is the header button's text when no token is set; the
// 401 hint names the button by it.
export const SET_TOKEN_LABEL = "set token";

export function Loading({ label = "Loading…" }: { label?: string }) {
  return (
    <div className="state" role="status" aria-live="polite">
      <span className="spinner" aria-hidden="true" />
      {label}
    </div>
  );
}

export function ErrorState({
  error,
  onRetry,
}: {
  error: Error;
  onRetry?: () => void;
}) {
  const unauthorized = error instanceof ApiError && error.status === 401;
  // A scoped token's clusters of other teams answer 404, as unknown ones do.
  const hidden = error instanceof ApiError && error.status === 404 && getScope() !== null;
  return (
    <div className="state state-error" role="alert">
      <p className="state-title">
        {unauthorized ? "Unauthorized" : "Request failed"}
      </p>
      <p className="state-detail">{error.message}</p>
      {unauthorized && (
        <p className="state-hint">
          This server requires a read token — set it with the “{SET_TOKEN_LABEL}”
          button in the header.
        </p>
      )}
      {hidden && (
        <p className="state-hint">
          It does not exist, or it is outside the teams this read token reads.
        </p>
      )}
      {onRetry && (
        <button type="button" className="btn" onClick={onRetry}>
          Retry
        </button>
      )}
    </div>
  );
}

export function Empty({ title, hint }: { title: string; hint?: string }) {
  return (
    <div className="state">
      <p className="state-title">{title}</p>
      {hint && <p className="state-hint">{hint}</p>}
    </div>
  );
}

// scoreClass buckets a 0–100 readiness score for color coding.
export function scoreClass(score: number): string {
  if (score >= 90) return "score-good";
  if (score >= 70) return "score-warn";
  return "score-bad";
}

// verdictOf reads a verdict, falling back to the v0.1 ready flag for
// servers that predate it.
export function verdictOf(x: { verdict?: Verdict; ready: boolean }): Verdict {
  return x.verdict ?? (x.ready ? "ready" : "blocked");
}

const VERDICT_MARK: Record<Verdict, string> = {
  ready: "✓",
  blocked: "✗",
  unknown: "?",
};

// ScoreBadge: the score, coloured by bucket, plus the verdict as a mark
// and as text for screen readers — never colour alone. Without a verdict
// (team rollups) it shows the score only.
export function ScoreBadge({ score, verdict }: { score: number; verdict?: Verdict }) {
  return (
    <span
      className={`score-badge ${scoreClass(score)}${verdict === "unknown" ? " verdict-unknown" : ""}`}
      title={verdict}
    >
      {score}
      {verdict && (
        <>
          <span className="score-ready" aria-hidden="true">
            {VERDICT_MARK[verdict]}
          </span>
          <span className="visually-hidden">{verdict}</span>
        </>
      )}
    </span>
  );
}

const VERDICT_TEXT: Record<Verdict, string> = {
  ready: "Ready",
  blocked: "Blocked",
  unknown: "Unknown",
};

// VerdictPill spells the verdict out; unknown has its own style so it never
// reads as ready or blocked.
export function VerdictPill({ verdict }: { verdict: Verdict }) {
  return (
    <span className={`verdict verdict-${verdict}`}>
      <span aria-hidden="true">{VERDICT_MARK[verdict]} </span>
      {VERDICT_TEXT[verdict]}
    </span>
  );
}

export function SeverityPill({ severity }: { severity: Severity }) {
  return <span className={`sev sev-${severity}`}>{severity}</span>;
}

// StaleBadge flags a cluster whose agent has stopped pushing: its scores
// describe the cluster as it was at lastSeen.
export function StaleBadge({ lastSeen }: { lastSeen?: string }) {
  return (
    <span
      className="badge badge-stale"
      title={lastSeen ? `no data since ${formatTime(lastSeen)}` : undefined}
    >
      <span aria-hidden="true">⚠ </span>stale
    </span>
  );
}

// formatTime renders an RFC3339 timestamp compactly in local time.
export function formatTime(iso: string): string {
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return iso;
  return d.toLocaleString(undefined, {
    year: "numeric",
    month: "short",
    day: "numeric",
    hour: "2-digit",
    minute: "2-digit",
  });
}

// TARGET_PATTERN is a Kubernetes minor version, e.g. 1.38.
export const TARGET_PATTERN = /^\d+\.\d+$/;

// nextMinors lists the n minors after a server version ("v1.35.2" →
// 1.36, 1.37, …) as what-if suggestions; [] when it does not parse.
export function nextMinors(serverVersion: string | undefined, n: number): string[] {
  const m = /^v?(\d+)\.(\d+)/.exec(serverVersion ?? "");
  if (!m) return [];
  const major = Number(m[1]);
  const minor = Number(m[2]);
  return Array.from({ length: n }, (_, i) => `${major}.${minor + i + 1}`);
}

// TargetPicker: a free-text minor version with suggestions. Any valid
// minor can be asked for — the server computes a what-if when nothing is
// stored. Invalid input is rejected here, before a request. An empty
// value means "default" when allowEmpty is set.
export function TargetPicker({
  value,
  suggestions,
  onPick,
  placeholder,
  allowEmpty = false,
}: {
  value?: string;
  suggestions: string[];
  onPick: (target: string | undefined) => void;
  placeholder?: string;
  allowEmpty?: boolean;
}) {
  const id = useId();
  const [draft, setDraft] = useState(value ?? "");
  const [error, setError] = useState("");

  const submit = () => {
    const t = draft.trim();
    if (t === "" && allowEmpty) {
      setError("");
      onPick(undefined);
      return;
    }
    if (!TARGET_PATTERN.test(t)) {
      setError(`Enter a minor version such as 1.38${t ? ` (got “${t}”)` : ""}.`);
      return;
    }
    setError("");
    onPick(t);
  };

  return (
    <form
      className="target-pick"
      onSubmit={(e) => {
        e.preventDefault();
        submit();
      }}
    >
      <label htmlFor={`${id}-target`}>Target</label>
      <input
        id={`${id}-target`}
        list={`${id}-options`}
        inputMode="decimal"
        autoComplete="off"
        spellCheck={false}
        size={8}
        placeholder={placeholder}
        value={draft}
        aria-invalid={error ? true : undefined}
        aria-describedby={error ? `${id}-error` : undefined}
        onChange={(e) => {
          setDraft(e.target.value);
          if (error) setError("");
        }}
      />
      <datalist id={`${id}-options`}>
        {suggestions.map((s) => (
          <option key={s} value={s} />
        ))}
      </datalist>
      <button type="submit" className="btn btn-ghost">
        Evaluate
      </button>
      {error && (
        <p id={`${id}-error`} className="field-error" role="alert">
          {error}
        </p>
      )}
    </form>
  );
}

// uniqueSortedVersions dedupes minor versions and sorts them numerically.
export function uniqueSortedVersions(values: string[]): string[] {
  const num = (v: string) => v.split(".").map(Number);
  return [...new Set(values)].sort((a, b) => {
    const [am = 0, an = 0] = num(a);
    const [bm = 0, bn = 0] = num(b);
    return am - bm || an - bn;
  });
}

// citationLabels names each citation link by host plus the tail of its
// path, so two links to one host read differently (WCAG 2.4.4). Labels
// that still collide get a counter.
export function citationLabels(urls: string[]): string[] {
  const base = urls.map((url) => {
    try {
      const u = new URL(url);
      const tail = u.pathname.split("/").filter(Boolean).pop();
      const frag = u.hash ? u.hash : "";
      return tail || frag ? `${u.host} › ${tail ?? ""}${frag}` : u.host;
    } catch {
      return url;
    }
  });
  const seen = new Map<string, number>();
  return base.map((label) => {
    const n = (seen.get(label) ?? 0) + 1;
    seen.set(label, n);
    return n === 1 ? label : `${label} (${n})`;
  });
}

// Citations renders a citation list as external links. Keyed by position:
// a list may cite one URL twice.
export function Citations({ urls }: { urls: string[] }) {
  const labels = citationLabels(urls);
  return (
    <p className="citations">
      {urls.map((url, i) => (
        <a key={i} href={url} target="_blank" rel="noreferrer" title={url}>
          {labels[i]}
        </a>
      ))}
    </p>
  );
}
