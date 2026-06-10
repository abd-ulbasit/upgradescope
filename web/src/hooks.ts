import { useCallback, useEffect, useState } from "react";

export interface Async<T> {
  data?: T;
  error?: Error;
  loading: boolean;
  reload: () => void;
}

// useAsync runs fn on mount and whenever deps change; stale responses from
// superseded loads are dropped. reload() re-runs with the same deps (retry
// buttons, after saving a token).
export function useAsync<T>(fn: () => Promise<T>, deps: unknown[]): Async<T> {
  const [state, setState] = useState<{ data?: T; error?: Error; loading: boolean }>({
    loading: true,
  });
  const [epoch, setEpoch] = useState(0);
  const reload = useCallback(() => setEpoch((e) => e + 1), []);

  useEffect(() => {
    let stale = false;
    setState((s) => ({ ...s, loading: true }));
    fn().then(
      (data) => {
        if (!stale) setState({ data, loading: false });
      },
      (error: Error) => {
        if (!stale) setState({ error, loading: false });
      },
    );
    return () => {
      stale = true;
    };
    // eslint-style exhaustive-deps doesn't apply: fn is intentionally keyed
    // by the caller-provided deps list.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [...deps, epoch]);

  return { ...state, reload };
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
