// Minimal API client: fetch + optional bearer token from localStorage.
// Every server error becomes an ApiError carrying the HTTP status and the
// server's {"error": "..."} message when present.

import type {
  ClusterDetail,
  FindingsResponse,
  FleetResponse,
  RegistryResponse,
  Report,
  ScorePoint,
} from "./types";

export const TOKEN_KEY = "upgradescope.readToken";

// localStorage access is guarded: unavailable in some private modes and in
// the node test environment.
export function getToken(): string {
  try {
    return globalThis.localStorage?.getItem(TOKEN_KEY) ?? "";
  } catch {
    return "";
  }
}

export function setToken(token: string): void {
  try {
    if (token) globalThis.localStorage?.setItem(TOKEN_KEY, token);
    else globalThis.localStorage?.removeItem(TOKEN_KEY);
  } catch {
    /* storage unavailable: token lives only for this page load */
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

export async function api<T>(path: string): Promise<T> {
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

export const getFleet = () => api<FleetResponse>("/api/v1/fleet");

export const getCluster = (id: number) =>
  api<ClusterDetail>(`/api/v1/clusters/${id}`);

export const getReport = (id: number, target?: string) =>
  api<Report>(`/api/v1/clusters/${id}/report${query({ target })}`);

export const getFindings = (id: number, target?: string) =>
  api<FindingsResponse>(`/api/v1/clusters/${id}/findings${query({ target })}`);

export const getHistory = (id: number, target?: string, limit = 60) =>
  api<ScorePoint[]>(
    `/api/v1/clusters/${id}/history${query({ target, limit: String(limit) })}`,
  );

export const getRegistry = () => api<RegistryResponse>("/api/v1/registry");
