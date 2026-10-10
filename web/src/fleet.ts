// Fleet triage: the pure functions behind the Fleet page's toolbar and
// summary strip. Everything is computed client-side from /api/v1/fleet,
// which the server already bounds (500 clusters × 16 targets).

import type { FleetResponse, FleetRow } from "./types";
import { verdictOf } from "./ui";

// Bucket is where one cluster stands for one target. A cluster is in
// exactly one bucket per target, so the buckets sum to the cluster count.
// Precedence: n/a (the cluster already runs the target), stale (the agent
// stopped pushing, so an old verdict says nothing about now), missing (no
// stored evaluation), then the stored verdict. An unknown verdict is never
// counted as ready (#243).
export type Bucket = "ready" | "blocked" | "unknown" | "stale" | "na" | "missing";

export const BUCKETS: Bucket[] = ["ready", "blocked", "unknown", "stale", "na", "missing"];

export const BUCKET_LABEL: Record<Bucket, string> = {
  ready: "Ready",
  blocked: "Blocked",
  unknown: "Unknown",
  stale: "Stale",
  na: "n/a",
  missing: "No stored evaluation",
};

export function bucketOf(row: FleetRow, target: string): Bucket {
  if (row.notApplicable?.includes(target)) return "na";
  if (row.stale) return "stale";
  const cell = row.cells[target];
  if (!cell) return "missing";
  return verdictOf(cell);
}

// countBuckets counts every cluster once, in its bucket for target.
export function countBuckets(rows: FleetRow[], target: string): Record<Bucket, number> {
  const counts: Record<Bucket, number> = {
    ready: 0,
    blocked: 0,
    unknown: 0,
    stale: 0,
    na: 0,
    missing: 0,
  };
  for (const row of rows) counts[bucketOf(row, target)]++;
  return counts;
}

// Filter is the toolbar's quick filter: a bucket, or has-blockers (a cell
// that lists at least one blocker).
export type Filter = "" | Bucket | "has-blockers";

export const FILTERS: { value: Filter; label: string }[] = [
  { value: "", label: "All clusters" },
  { value: "blocked", label: "Blocked" },
  { value: "unknown", label: "Unknown" },
  { value: "stale", label: "Stale" },
  { value: "has-blockers", label: "Has blockers" },
  { value: "ready", label: "Ready" },
  { value: "na", label: "n/a" },
  { value: "missing", label: "No stored evaluation" },
];

export function parseFilter(v: string | null): Filter {
  return FILTERS.some((f) => f.value === v) ? (v as Filter) : "";
}

export type Sort = "score" | "name" | "seen";

export const SORTS: { value: Sort; label: string }[] = [
  { value: "score", label: "Worst score first" },
  { value: "name", label: "Name" },
  { value: "seen", label: "Last seen (oldest first)" },
];

export function parseSort(v: string | null): Sort {
  return SORTS.some((s) => s.value === v) ? (v as Sort) : "score";
}

// scopeOf lists the targets a filter or sort looks at: the pinned target,
// or every target the response shows. Targets the server left out
// (targetsOmitted) are not in the response, so they never count.
export function scopeOf(fleet: FleetResponse, pinned?: string): string[] {
  return pinned && fleet.targets.includes(pinned) ? [pinned] : fleet.targets;
}

export function matchesFilter(row: FleetRow, filter: Filter, scope: string[]): boolean {
  switch (filter) {
    case "":
      return true;
    case "has-blockers":
      return scope.some((t) => !row.notApplicable?.includes(t) && (row.cells[t]?.blockers ?? 0) > 0);
    default:
      return scope.some((t) => bucketOf(row, t) === filter);
  }
}

export function matchesName(row: FleetRow, q: string): boolean {
  const needle = q.trim().toLowerCase();
  return needle === "" || row.name.toLowerCase().includes(needle);
}

// worstScore is the lowest stored score over the scope; a cluster with none
// (n/a or never evaluated everywhere) has no score and sorts last.
function worstScore(row: FleetRow, scope: string[]): number {
  let worst = Infinity;
  for (const t of scope) {
    if (row.notApplicable?.includes(t)) continue;
    const cell = row.cells[t];
    if (cell && cell.score < worst) worst = cell.score;
  }
  return worst;
}

function seenAt(row: FleetRow): number {
  const t = row.lastSeen ? Date.parse(row.lastSeen) : NaN;
  return Number.isNaN(t) ? Infinity : t;
}

export function sortRows(rows: FleetRow[], sort: Sort, scope: string[]): FleetRow[] {
  const byName = (a: FleetRow, b: FleetRow) => a.name.localeCompare(b.name) || a.clusterId - b.clusterId;
  const key = (r: FleetRow) => (sort === "score" ? worstScore(r, scope) : sort === "seen" ? seenAt(r) : 0);
  return [...rows].sort((a, b) => {
    if (sort === "name") return byName(a, b);
    const ka = key(a);
    const kb = key(b);
    if (ka === kb) return byName(a, b);
    return ka < kb ? -1 : 1;
  });
}

// defaultTarget picks the fleet column that is an upgrade for the most
// clusters (the lowest on a tie): the first column is often one most
// clusters already run.
export function defaultTarget({ targets, clusters }: FleetResponse): string | undefined {
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

// MAX_ROWS is how many matrix rows render at once; "show more" adds this
// many. The fleet is bounded, so this is windowing the DOM, not paging.
export const MAX_ROWS = 100;
