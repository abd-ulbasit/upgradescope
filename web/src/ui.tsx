// Shared presentational pieces: loading / error / empty states (every view
// renders one of these before data), score + severity styling.

import { ApiError } from "./api";
import type { Severity } from "./types";

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
  return (
    <div className="state state-error" role="alert">
      <p className="state-title">
        {unauthorized ? "Unauthorized" : "Request failed"}
      </p>
      <p className="state-detail">{error.message}</p>
      {unauthorized && (
        <p className="state-hint">
          This server requires a read token — set it via the key button in the
          header.
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

export function ScoreBadge({ score, ready }: { score: number; ready: boolean }) {
  return (
    <span
      className={`score-badge ${scoreClass(score)}`}
      title={ready ? "ready" : "not ready"}
    >
      {score}
      <span className="score-ready" aria-hidden="true">
        {ready ? "✓" : "✗"}
      </span>
      <span className="visually-hidden">{ready ? "ready" : "not ready"}</span>
    </span>
  );
}

export function SeverityPill({ severity }: { severity: Severity }) {
  return <span className={`sev sev-${severity}`}>{severity}</span>;
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
