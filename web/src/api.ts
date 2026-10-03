// Minimal API client: fetch + optional bearer token from Web Storage.
// Every server error becomes an ApiError carrying the HTTP status and the
// server's {"error": "..."} message when present.
//
// Paths are relative ("api/v1/…", no leading slash): the browser resolves
// them against the page URL, so a dashboard served under a path prefix
// (https://host/upgradescope/) calls https://host/upgradescope/api/v1/….

import type {
  ClusterDetail,
  FleetResponse,
  FleetTeamsResponse,
  RegistryResponse,
  Report,
  ScorePoint,
} from "./types";

export const TOKEN_KEY = "upgradescope.readToken";

// The read token lives in sessionStorage by default: it survives reloads
// but is gone when the tab closes. "Remember on this device" opts into
// localStorage instead. Either way only one store holds it.
//
// Storage access is guarded: unavailable in some private modes and in the
// node test environment.
function store(kind: "localStorage" | "sessionStorage"): Storage | undefined {
  try {
    return globalThis[kind] ?? undefined;
  } catch {
    return undefined;
  }
}

export function getToken(): string {
  try {
    return (
      store("sessionStorage")?.getItem(TOKEN_KEY) ??
      store("localStorage")?.getItem(TOKEN_KEY) ??
      ""
    );
  } catch {
    return "";
  }
}

// isTokenRemembered reports whether the token is kept in localStorage.
export function isTokenRemembered(): boolean {
  try {
    return !!store("localStorage")?.getItem(TOKEN_KEY);
  } catch {
    return false;
  }
}

export function setToken(token: string, opts: { remember?: boolean } = {}): void {
  const [keep, drop] = opts.remember
    ? [store("localStorage"), store("sessionStorage")]
    : [store("sessionStorage"), store("localStorage")];
  try {
    drop?.removeItem(TOKEN_KEY);
    if (token) keep?.setItem(TOKEN_KEY, token);
    else keep?.removeItem(TOKEN_KEY);
  } catch {
    /* storage unavailable: the token lives only for this page load */
  }
}

// The read scope: the teams a team-scoped read token (or an authenticating
// proxy's team header) reads, as the server names them in SCOPE_HEADER on
// every scoped answer; null for a fleet-wide view, which carries none. The
// server has already filtered what it answers; this only lets the
// dashboard say so.
export const SCOPE_HEADER = "X-Upgradescope-Teams";

let scope: string[] | null = null;
const scopeListeners = new Set<() => void>();

export function getScope(): string[] | null {
  return scope;
}

// subscribeScope calls fn whenever the scope changes; it returns the
// unsubscribe function (useSyncExternalStore's contract).
export function subscribeScope(fn: () => void): () => void {
  scopeListeners.add(fn);
  return () => scopeListeners.delete(fn);
}

function setScope(next: string[] | null): void {
  const same =
    next === scope || (next !== null && scope !== null && JSON.stringify(next) === JSON.stringify(scope));
  if (same) return;
  scope = next;
  for (const fn of scopeListeners) fn();
}

// clearScope forgets the scope until the next answer names one: after the
// token changes, the old token's scope says nothing about the new one.
export function clearScope(): void {
  setScope(null);
}

// decodeTeams reads SCOPE_HEADER's team list encoding: comma-separated
// team names, each percent-encoded (UTF-8), since a team name is free
// text ("Platform Team", "Équipe, Paris"). An entry that does not decode
// is shown as sent.
export function decodeTeams(h: string): string[] {
  return h
    .split(",")
    .map((t) => t.trim())
    .filter((t) => t !== "")
    .map((t) => {
      try {
        return decodeURIComponent(t);
      } catch {
        return t;
      }
    });
}

// noteScope records the scope an answer was given for.
function noteScope(res: Response): void {
  const h = res.headers.get(SCOPE_HEADER);
  setScope(h === null ? null : decodeTeams(h));
}

export class ApiError extends Error {
  constructor(
    readonly status: number,
    message: string,
  ) {
    super(message);
    this.name = "ApiError";
  }
}

// request fetches path with the bearer token and turns any failure into an
// ApiError.
async function request(path: string): Promise<Response> {
  const headers: Record<string, string> = {};
  const token = getToken();
  if (token) headers["Authorization"] = `Bearer ${token}`;
  let res: Response;
  try {
    res = await fetch(path, { headers });
  } catch (err) {
    throw new ApiError(0, `network error: ${(err as Error).message}`);
  }
  // A scoped answer names its teams, a 404 for another team's cluster
  // included; a 401 reads nothing.
  if (res.status === 401) setScope(null);
  else if (res.ok || res.status === 404) noteScope(res);
  if (!res.ok) {
    let msg = `HTTP ${res.status}`;
    try {
      const body: unknown = await res.json();
      if (
        typeof body === "object" &&
        body !== null &&
        typeof (body as { error?: unknown }).error === "string"
      ) {
        msg = (body as { error: string }).error;
      }
    } catch {
      /* non-JSON error body: keep the status fallback */
    }
    throw new ApiError(res.status, msg);
  }
  return res;
}

export async function api<T>(path: string): Promise<T> {
  const res = await request(path);
  return res.json() as Promise<T>;
}

// query renders "?k=v&..." from defined params only ("" → no query string).
function query(params: Record<string, string | undefined>): string {
  const s = new URLSearchParams();
  for (const [k, v] of Object.entries(params)) {
    if (v !== undefined && v !== "") s.set(k, v);
  }
  const out = s.toString();
  return out ? `?${out}` : "";
}

export const getFleet = () => api<FleetResponse>("api/v1/fleet");

export const getFleetTeams = (target: string) =>
  api<FleetTeamsResponse>(`api/v1/fleet/teams${query({ target })}`);

export const getCluster = (id: number) =>
  api<ClusterDetail>(`api/v1/clusters/${id}`);

// getReport also carries the findings, so the cluster view needs no
// separate /findings call.
export const getReport = (id: number, target?: string) =>
  api<Report>(`api/v1/clusters/${id}/report${query({ target })}`);

export const getHistory = (id: number, target?: string, limit = 60) =>
  api<ScorePoint[]>(
    `api/v1/clusters/${id}/history${query({ target, limit: String(limit) })}`,
  );

export const getRegistry = () => api<RegistryResponse>("api/v1/registry");

export type ExportFormat = "csv" | "html";

// fetchExport downloads the auditor export of a stored evaluation. It goes
// through fetch rather than a plain link so the bearer token is sent: with
// serve --read-token a bare <a href> would get 401.
// The server names CSV downloads (quoted with Go's %q); for the HTML report
// (served inline) the name follows the same upgradescope-<cluster>-<target>
// pattern. Cluster names are free text, so either name is sanitized.
export async function fetchExport(
  id: number,
  target: string,
  format: ExportFormat,
  clusterName?: string,
): Promise<{ blob: Blob; filename: string }> {
  const res = await request(
    `api/v1/clusters/${id}/export${query({ target, format })}`,
  );
  const disposition = res.headers.get("Content-Disposition") ?? "";
  const quoted = /filename="((?:[^"\\]|\\.)+)"/.exec(disposition)?.[1];
  const named = quoted?.replace(/\\(.)/g, "$1");
  return {
    blob: await res.blob(),
    filename: safeFilename(
      named ?? `upgradescope-${clusterName ?? `cluster-${id}`}-${target}.${format}`,
    ),
  };
}

// safeFilename replaces each run of anything but letters, digits, dot,
// dash and underscore, so a cluster name cannot put path separators,
// spaces or quotes into a download name.
function safeFilename(name: string): string {
  return name.replace(/[^A-Za-z0-9._-]+/g, "_");
}

// saveBlob hands a fetched file to the browser's download flow.
export function saveBlob(blob: Blob, filename: string): void {
  const url = URL.createObjectURL(blob);
  const a = document.createElement("a");
  a.href = url;
  a.download = filename;
  document.body.appendChild(a);
  a.click();
  a.remove();
  // Revoke after the click has been dispatched; the download keeps its copy.
  setTimeout(() => URL.revokeObjectURL(url), 0);
}
