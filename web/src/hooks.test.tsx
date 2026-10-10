// @vitest-environment happy-dom
import { act, renderHook, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { STALE_AFTER_MS, useAsync } from "./hooks";

// Only Date is faked: promises and React's scheduler keep running.
let visibility: DocumentVisibilityState = "visible";

function setVisibility(v: DocumentVisibilityState) {
  visibility = v;
  document.dispatchEvent(new Event("visibilitychange"));
}

function deferred<T>() {
  let resolve!: (v: T) => void;
  let reject!: (e: Error) => void;
  const promise = new Promise<T>((res, rej) => {
    resolve = res;
    reject = rej;
  });
  return { promise, resolve, reject };
}

beforeEach(() => {
  visibility = "visible";
  Object.defineProperty(document, "visibilityState", {
    configurable: true,
    get: () => visibility,
  });
  vi.useFakeTimers({ toFake: ["Date"] });
  vi.setSystemTime(new Date("2026-10-11T12:00:00Z"));
});

afterEach(() => {
  vi.useRealTimers();
  delete (document as unknown as Record<string, unknown>).visibilityState;
});

describe("useAsync", () => {
  it("keeps the data on screen during a refetch and only then swaps it", async () => {
    const pending: ReturnType<typeof deferred<number>>[] = [];
    const fn = vi.fn(() => {
      const d = deferred<number>();
      pending.push(d);
      return d.promise;
    });
    const { result } = renderHook(() => useAsync(fn, []));
    expect(result.current.loading).toBe(true);
    await act(async () => pending[0]!.resolve(1));
    expect(result.current.data).toBe(1);
    expect(result.current.updatedAt).toBe(Date.now());

    vi.setSystemTime(Date.now() + 5_000);
    act(() => result.current.reload());
    expect(result.current.data).toBe(1);
    expect(result.current.loading).toBe(false);
    expect(result.current.refreshing).toBe(true);

    await act(async () => pending[1]!.resolve(2));
    expect(result.current.data).toBe(2);
    expect(result.current.refreshing).toBe(false);
    expect(result.current.updatedAt).toBe(Date.now());
  });

  it("keeps the data when a refetch fails and reports the error", async () => {
    let n = 0;
    const fn = vi.fn(() => (n++ === 0 ? Promise.resolve("a") : Promise.reject(new Error("boom"))));
    const { result } = renderHook(() => useAsync(fn, []));
    await waitFor(() => expect(result.current.data).toBe("a"));
    act(() => result.current.reload());
    await waitFor(() => expect(result.current.error?.message).toBe("boom"));
    expect(result.current.data).toBe("a");
    expect(result.current.loading).toBe(false);
  });

  it("drops the data when its deps change: it answers another question", async () => {
    const pending: ReturnType<typeof deferred<string>>[] = [];
    const fn = vi.fn((_: string) => {
      const d = deferred<string>();
      pending.push(d);
      return d.promise;
    });
    const { result, rerender } = renderHook(({ k }) => useAsync(() => fn(k), [k]), {
      initialProps: { k: "a" },
    });
    await act(async () => pending[0]!.resolve("A"));
    rerender({ k: "b" });
    expect(result.current.data).toBeUndefined();
    expect(result.current.loading).toBe(true);
  });

  it("refetches on becoming visible only when the data is older than 60s", async () => {
    const fn = vi.fn(() => Promise.resolve(fn.mock.calls.length));
    const { result } = renderHook(() => useAsync(fn, []));
    await waitFor(() => expect(result.current.data).toBe(1));

    vi.setSystemTime(Date.now() + STALE_AFTER_MS - 1_000);
    act(() => setVisibility("visible"));
    expect(fn).toHaveBeenCalledTimes(1);

    vi.setSystemTime(Date.now() + 2_000);
    act(() => setVisibility("hidden"));
    expect(fn).toHaveBeenCalledTimes(1);

    act(() => setVisibility("visible"));
    expect(fn).toHaveBeenCalledTimes(2);
    // The old data is still shown while the refetch runs.
    expect(result.current.data).toBe(1);
    expect(result.current.refreshing).toBe(true);
    await waitFor(() => expect(result.current.data).toBe(2));
  });

  it("does not poll: time passing alone refetches nothing", async () => {
    const fn = vi.fn(() => Promise.resolve(1));
    const { result } = renderHook(() => useAsync(fn, []));
    await waitFor(() => expect(result.current.data).toBe(1));
    vi.setSystemTime(Date.now() + 60 * STALE_AFTER_MS);
    await act(async () => {});
    expect(fn).toHaveBeenCalledTimes(1);
  });
});
