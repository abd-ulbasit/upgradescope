import { useCallback, useEffect, useRef, useState, useSyncExternalStore } from "react";
import { getScope, subscribeScope } from "./api";

export interface Async<T> {
  data?: T;
  // The last failure. After a failed refetch the previous data is kept, so
  // views show an error only when there is no data to show.
  error?: Error;
  // True only while there is nothing to show yet: the blocking state.
  loading: boolean;
  // True while a refetch runs and the previous data is still on screen.
  refreshing: boolean;
  // When the data on screen arrived (ms since the epoch).
  updatedAt?: number;
  reload: () => void;
}

// STALE_AFTER_MS is how old the data may be when the tab becomes visible
// again before it is fetched anew. There is deliberately no interval
// polling: server load stays predictable.
export const STALE_AFTER_MS = 60_000;

interface AsyncState<T> {
  data?: T;
  error?: Error;
  fetching: boolean;
  updatedAt?: number;
}

function sameDeps(a: unknown[], b: unknown[]): boolean {
  return a.length === b.length && a.every((v, i) => Object.is(v, b[i]));
}

// useAsync runs fn on mount and whenever deps change; stale responses from
// superseded loads are dropped. reload() re-runs with the same deps (retry
// and refresh buttons, after saving a token). It is stale-while-revalidate:
// a rerun with the same deps keeps the data on screen (refreshing is true,
// loading stays false); only changed deps drop it, because the data then
// answers a different question. A tab that becomes visible again refetches
// when its data is older than STALE_AFTER_MS.
export function useAsync<T>(fn: () => Promise<T>, deps: unknown[]): Async<T> {
  const [state, setState] = useState<AsyncState<T>>({ fetching: true });
  const [epoch, setEpoch] = useState(0);
  const reload = useCallback(() => setEpoch((e) => e + 1), []);
  const lastDeps = useRef(deps);
  const latest = useRef(state);
  latest.current = state;

  useEffect(() => {
    let stale = false;
    const changed = !sameDeps(lastDeps.current, deps);
    lastDeps.current = deps;
    setState((s) =>
      changed
        ? { fetching: true }
        : { data: s.data, updatedAt: s.updatedAt, fetching: true },
    );
    fn().then(
      (data) => {
        if (!stale) setState({ data, updatedAt: Date.now(), fetching: false });
      },
      (error: Error) => {
        if (!stale) setState((s) => ({ ...s, error, fetching: false }));
      },
    );
    return () => {
      stale = true;
    };
    // eslint-style exhaustive-deps doesn't apply: fn is intentionally keyed
    // by the caller-provided deps list.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [...deps, epoch]);

  useEffect(() => {
    const onVisible = () => {
      if (document.visibilityState !== "visible") return;
      const { fetching, updatedAt } = latest.current;
      if (fetching || updatedAt === undefined) return;
      if (Date.now() - updatedAt > STALE_AFTER_MS) reload();
    };
    document.addEventListener("visibilitychange", onVisible);
    return () => document.removeEventListener("visibilitychange", onVisible);
  }, [reload]);

  const has = state.data !== undefined;
  return {
    data: state.data,
    error: state.error,
    loading: state.fetching && !has,
    refreshing: state.fetching && has,
    updatedAt: state.updatedAt,
    reload,
  };
}

// useHashRoute returns the current location.hash without "#" ("/" when empty)
// and re-renders on hashchange. Hash routing keeps the embedded SPA free of
// server-side route rewrites beyond the index.html fallback.
export function useHashRoute(): string {
  const [hash, setHash] = useState(() => window.location.hash);
  useEffect(() => {
    const onChange = () => setHash(window.location.hash);
    window.addEventListener("hashchange", onChange);
    return () => window.removeEventListener("hashchange", onChange);
  }, []);
  const route = hash.replace(/^#/, "");
  return route === "" ? "/" : route;
}

// useRouteQuery is the query string of the current hash route
// (#/cluster/3?target=1.37&q=ingress), re-read on hashchange.
export function useRouteQuery(): URLSearchParams {
  const route = useHashRoute();
  const i = route.indexOf("?");
  return new URLSearchParams(i < 0 ? "" : route.slice(i + 1));
}

// setRouteQuery merges changes into the current hash route's query (an
// undefined or empty value removes the key) and tells the router. It
// replaces the history entry rather than pushing one, so typing in a search
// box does not fill the back button.
export function setRouteQuery(changes: Record<string, string | undefined>): void {
  const route = window.location.hash.replace(/^#/, "") || "/";
  const i = route.indexOf("?");
  const path = i < 0 ? route : route.slice(0, i);
  const params = new URLSearchParams(i < 0 ? "" : route.slice(i + 1));
  for (const [k, v] of Object.entries(changes)) {
    if (v === undefined || v === "") params.delete(k);
    else params.set(k, v);
  }
  const query = params.toString();
  const url = `${window.location.pathname}${window.location.search}#${path}${query ? `?${query}` : ""}`;
  window.history.replaceState(null, "", url);
  window.dispatchEvent(new HashChangeEvent("hashchange"));
}

// useScope is the read scope of the latest answer (api.getScope): the
// teams a scoped token reads, or null for the whole fleet.
export function useScope(): string[] | null {
  return useSyncExternalStore(subscribeScope, getScope, getScope);
}
