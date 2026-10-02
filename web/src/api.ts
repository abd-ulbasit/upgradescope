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
// The server names CSV downloads; for the HTML report (served inline) the
// name follows the same upgradescope-<cluster>-<target> pattern.
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
  const named = /filename="([^"]+)"/.exec(disposition)?.[1];
  return {
    blob: await res.blob(),
    filename:
      named ?? `upgradescope-${clusterName ?? `cluster-${id}`}-${target}.${format}`,
  };
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
