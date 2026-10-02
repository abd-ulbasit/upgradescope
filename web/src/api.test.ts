import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import {
  api,
  ApiError,
  fetchExport,
  getFleetTeams,
  getHistory,
  getReport,
  getToken,
  isTokenRemembered,
  setToken,
  TOKEN_KEY,
} from "./api";

// In-memory Web Storage: the test environment is plain node.
function fakeStorage() {
  const store = new Map<string, string>();
  return {
    getItem: (k: string) => store.get(k) ?? null,
    setItem: (k: string, v: string) => void store.set(k, v),
    removeItem: (k: string) => void store.delete(k),
  };
}

function jsonResponse(status: number, body: unknown): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

let fetchMock: ReturnType<typeof vi.fn>;

beforeEach(() => {
  vi.stubGlobal("localStorage", fakeStorage());
  vi.stubGlobal("sessionStorage", fakeStorage());
  fetchMock = vi.fn();
  vi.stubGlobal("fetch", fetchMock);
});

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("token storage", () => {
  it("keeps the token in sessionStorage by default", () => {
    expect(getToken()).toBe("");
    setToken("s3cret");
    expect(getToken()).toBe("s3cret");
    expect(sessionStorage.getItem(TOKEN_KEY)).toBe("s3cret");
    expect(localStorage.getItem(TOKEN_KEY)).toBeNull();
    expect(isTokenRemembered()).toBe(false);
    setToken("");
    expect(getToken()).toBe("");
  });

  it("uses localStorage only when asked to remember", () => {
    setToken("tab-only");
    setToken("kept", { remember: true });
    expect(localStorage.getItem(TOKEN_KEY)).toBe("kept");
    expect(sessionStorage.getItem(TOKEN_KEY)).toBeNull();
    expect(getToken()).toBe("kept");
    expect(isTokenRemembered()).toBe(true);

    // Un-remembering moves it back and leaves nothing on disk.
    setToken("kept");
    expect(localStorage.getItem(TOKEN_KEY)).toBeNull();
    expect(getToken()).toBe("kept");
    setToken("");
    expect(sessionStorage.getItem(TOKEN_KEY)).toBeNull();
  });
});

describe("api", () => {
  it("returns parsed JSON and sends no auth header without a token", async () => {
    fetchMock.mockResolvedValue(jsonResponse(200, { ok: true }));
    const out = await api<{ ok: boolean }>("api/v1/fleet");
    expect(out).toEqual({ ok: true });
    const [path, init] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(path).toBe("api/v1/fleet");
    expect(
      (init.headers as Record<string, string>)["Authorization"],
    ).toBeUndefined();
  });

  it("sends the stored token as a bearer header", async () => {
    setToken("read-tok");
    fetchMock.mockResolvedValue(jsonResponse(200, []));
    await api("api/v1/clusters");
    const [, init] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect((init.headers as Record<string, string>)["Authorization"]).toBe(
      "Bearer read-tok",
    );
  });

  it("throws ApiError with the server's error message", async () => {
    fetchMock.mockResolvedValue(
      jsonResponse(401, { error: "invalid or missing bearer token" }),
    );
    const err = await api("api/v1/fleet").catch((e: unknown) => e);
    expect(err).toBeInstanceOf(ApiError);
    expect((err as ApiError).status).toBe(401);
    expect((err as ApiError).message).toBe("invalid or missing bearer token");
  });

  it("falls back to the HTTP status for non-JSON error bodies", async () => {
    fetchMock.mockResolvedValue(new Response("nope", { status: 502 }));
    const err = await api("api/v1/fleet").catch((e: unknown) => e);
    expect((err as ApiError).status).toBe(502);
    expect((err as ApiError).message).toBe("HTTP 502");
  });

  it("wraps network failures as status-0 ApiError", async () => {
    fetchMock.mockRejectedValue(new TypeError("Failed to fetch"));
    const err = await api("api/v1/fleet").catch((e: unknown) => e);
    expect(err).toBeInstanceOf(ApiError);
    expect((err as ApiError).status).toBe(0);
  });
});

describe("endpoint URLs", () => {
  it("builds target query strings only when present", async () => {
    // Fresh Response per call: a body can only be consumed once.
    fetchMock.mockImplementation(() => Promise.resolve(jsonResponse(200, {})));
    await getReport(3);
    await getReport(3, "1.38");
    await getFleetTeams("1.37");
    expect(fetchMock.mock.calls.map((c) => c[0])).toEqual([
      "api/v1/clusters/3/report",
      "api/v1/clusters/3/report?target=1.38",
      "api/v1/fleet/teams?target=1.37",
    ]);
  });

  it("always sends a history limit", async () => {
    fetchMock.mockResolvedValue(jsonResponse(200, []));
    await getHistory(3, "1.38");
    expect(fetchMock.mock.calls[0]?.[0]).toBe(
      "api/v1/clusters/3/history?target=1.38&limit=60",
    );
  });
});

describe("fetchExport", () => {
  it("fetches with the bearer token and names the file from the server", async () => {
    setToken("read-tok");
    fetchMock.mockResolvedValue(
      new Response("cluster,target\n", {
        status: 200,
        headers: {
          "Content-Type": "text/csv; charset=utf-8",
          "Content-Disposition": 'attachment; filename="prod-1.38.csv"',
        },
      }),
    );
    const out = await fetchExport(3, "1.38", "csv");
    const [path, init] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(path).toBe("api/v1/clusters/3/export?target=1.38&format=csv");
    expect((init.headers as Record<string, string>)["Authorization"]).toBe(
      "Bearer read-tok",
    );
    expect(out.filename).toBe("prod-1.38.csv");
    expect(await out.blob.text()).toBe("cluster,target\n");
  });

  it("falls back to a generated file name and surfaces server errors", async () => {
    fetchMock.mockResolvedValueOnce(new Response("<html>", { status: 200 }));
    expect((await fetchExport(3, "1.38", "html", "prod")).filename).toBe(
      "upgradescope-prod-1.38.html",
    );

    fetchMock.mockResolvedValueOnce(
      jsonResponse(404, { error: "no stored evaluation" }),
    );
    const err = await fetchExport(3, "1.40", "csv").catch((e: unknown) => e);
    expect(err).toBeInstanceOf(ApiError);
    expect((err as ApiError).message).toBe("no stored evaluation");
  });
});
