// Component-test helpers (happy-dom): a routed fetch mock and per-test
// cleanup. Import from component tests only — it needs a DOM.

import { cleanup } from "@testing-library/react";
import { afterEach, vi } from "vitest";
import type { Mock } from "vitest";

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
  window.location.hash = "";
  sessionStorage.clear();
  localStorage.clear();
});

export function jsonResponse(status: number, body: unknown): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

type Route = unknown | ((url: string) => Response);

// mockApi stubs fetch with a route table keyed by request path. A key with
// a query string must match exactly; a key without one matches any query.
// Unknown paths answer 404 like the server does.
export function mockApi(routes: Record<string, Route>): Mock {
  const fetchMock = vi.fn((input: string) => {
    const url = String(input);
    const path = url.split("?", 1)[0]!;
    const route = url in routes ? routes[url] : routes[path];
    if (route === undefined) {
      return Promise.resolve(jsonResponse(404, { error: `no route ${url}` }));
    }
    if (typeof route === "function") {
      return Promise.resolve((route as (u: string) => Response)(url));
    }
    return Promise.resolve(jsonResponse(200, route));
  });
  vi.stubGlobal("fetch", fetchMock);
  return fetchMock;
}

// fetchedUrls lists the URLs a fetch mock was called with.
export function fetchedUrls(fetchMock: Mock): string[] {
  return fetchMock.mock.calls.map((c) => String(c[0]));
}

// navigate sets the hash route and fires hashchange (happy-dom does not
// always dispatch it synchronously).
export function navigate(hash: string): void {
  window.location.hash = hash;
  window.dispatchEvent(new HashChangeEvent("hashchange"));
}
