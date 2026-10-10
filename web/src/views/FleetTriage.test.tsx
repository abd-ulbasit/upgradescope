// @vitest-environment happy-dom
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { App } from "../App";
import { mockApi, navigate } from "../test-utils";
import type { FleetCell, FleetResponse, FleetRow } from "../types";

function cell(score: number, verdict: "ready" | "blocked" | "unknown", blockers = 0): FleetCell {
  return { score, ready: verdict === "ready", verdict, blockers };
}

const NOW = "2026-10-11T00:00:00Z";

// 60 clusters on two shown targets: c-000.. in a repeating pattern of
// blocked / unknown / ready, every 10th stale, every 7th n/a for 1.36.
function sixty(): FleetResponse {
  const clusters: FleetRow[] = Array.from({ length: 60 }, (_, i) => {
    const name = `${i % 2 ? "prod" : "dev"}-${String(i).padStart(3, "0")}`;
    const kind = i % 3 === 0 ? "blocked" : i % 3 === 1 ? "unknown" : "ready";
    const score = kind === "blocked" ? 20 + (i % 7) : kind === "unknown" ? 80 + (i % 5) : 100 - (i % 3);
    const row: FleetRow = {
      clusterId: i + 1,
      name,
      serverVersion: "v1.35.1",
      lastSeen: `2026-10-${String(1 + (i % 9)).padStart(2, "0")}T00:00:00Z`,
      stale: i % 10 === 5,
      cells: {
        "1.36": cell(score, kind, kind === "blocked" ? 2 : 0),
        "1.37": cell(100, "ready"),
      },
    };
    if (i % 7 === 6) row.notApplicable = ["1.36"];
    return row;
  });
  return { targets: ["1.36", "1.37"], clusters };
}

async function open(data: FleetResponse, hash = "#/") {
  const fetchMock = mockApi({ "api/v1/fleet": data });
  navigate(hash);
  render(<App />);
  await screen.findByRole("heading", { level: 1, name: "Fleet" });
  return fetchMock;
}

function bodyRows(): HTMLElement[] {
  return Array.from(document.querySelectorAll<HTMLElement>("table.matrix tbody tr"));
}

function names(): string[] {
  return bodyRows().map((r) => r.querySelector("th a")!.textContent!);
}

const search = () => screen.getByLabelText("Search clusters by name") as HTMLInputElement;
const filterSel = () => screen.getByLabelText("Filter") as HTMLSelectElement;
const sortSel = () => screen.getByLabelText("Sort") as HTMLSelectElement;

describe("Fleet toolbar", () => {
  it("narrows the rows by a case-insensitive name search", async () => {
    await open(sixty());
    expect(bodyRows()).toHaveLength(60);
    fireEvent.change(search(), { target: { value: "PROD-00" } });
    expect(names().every((n) => n.startsWith("prod-00"))).toBe(true);
    expect(names()).toHaveLength(5);
    expect(screen.getByText("5 of 60 clusters")).toBeTruthy();
  });

  it("filters blocked over the shown targets only", async () => {
    const data = sixty();
    // Blocked only at 1.99, a target the response does not show.
    data.clusters[1]!.cells = { "1.36": cell(100, "ready"), "1.37": cell(100, "ready"), "1.99": cell(10, "blocked", 5) };
    await open(data);
    fireEvent.change(filterSel(), { target: { value: "blocked" } });
    const got = names();
    expect(got).not.toContain(data.clusters[1]!.name);
    // Every blocked row has a blocked, non-stale cell on a shown target.
    for (const row of data.clusters.filter((r) => got.includes(r.name))) {
      expect(["1.36", "1.37"].some((t) => row.cells[t]?.verdict === "blocked")).toBe(true);
    }
    const expected = data.clusters.filter(
      (r) => !r.stale && !r.notApplicable?.includes("1.36") && r.cells["1.36"]?.verdict === "blocked",
    );
    expect(got.sort()).toEqual(expected.map((r) => r.name).sort());
  });

  it("filters unknown, stale and has-blockers", async () => {
    const data = sixty();
    await open(data);
    fireEvent.change(filterSel(), { target: { value: "stale" } });
    expect(names().sort()).toEqual(data.clusters.filter((r) => r.stale).map((r) => r.name).sort());
    fireEvent.change(filterSel(), { target: { value: "has-blockers" } });
    const withBlockers = data.clusters.filter(
      (r) => !r.notApplicable?.includes("1.36") && (r.cells["1.36"]?.blockers ?? 0) > 0,
    );
    expect(names().sort()).toEqual(withBlockers.map((r) => r.name).sort());
    fireEvent.change(filterSel(), { target: { value: "unknown" } });
    expect(names().length).toBeGreaterThan(0);
    for (const n of names()) {
      const r = data.clusters.find((c) => c.name === n)!;
      expect(r.cells["1.36"]?.verdict).toBe("unknown");
    }
  });

  it("sorts the worst score first by default, then by name or last seen", async () => {
    const data = sixty();
    await open(data);
    const scoreOf = (n: string) => {
      const r = data.clusters.find((c) => c.name === n)!;
      return Math.min(
        ...["1.36", "1.37"].filter((t) => !r.notApplicable?.includes(t)).map((t) => r.cells[t]!.score),
      );
    };
    const scores = names().map(scoreOf);
    expect(scores).toEqual([...scores].sort((a, b) => a - b));
    expect(scores[0]).toBeLessThan(scores[scores.length - 1]!);

    fireEvent.change(sortSel(), { target: { value: "name" } });
    expect(names()).toEqual([...names()].sort((a, b) => a.localeCompare(b)));

    fireEvent.change(sortSel(), { target: { value: "seen" } });
    const seen = names().map((n) => Date.parse(data.clusters.find((c) => c.name === n)!.lastSeen!));
    expect(seen).toEqual([...seen].sort((a, b) => a - b));
  });

  it("keeps the toolbar in the hash and restores it from a pasted URL", async () => {
    const data = sixty();
    await open(data);
    fireEvent.change(search(), { target: { value: "prod" } });
    fireEvent.change(filterSel(), { target: { value: "blocked" } });
    fireEvent.change(sortSel(), { target: { value: "name" } });
    const hash = window.location.hash;
    const params = new URLSearchParams(hash.slice(hash.indexOf("?") + 1));
    expect(Object.fromEntries(params)).toEqual({ q: "prod", filter: "blocked", sort: "name" });
    const before = names();
    expect(before.length).toBeGreaterThan(0);

    // A reload: a fresh page with the same URL.
    document.body.innerHTML = "";
    const { unmount } = render(<App />);
    await screen.findByRole("heading", { level: 1, name: "Fleet" });
    expect(search().value).toBe("prod");
    expect(filterSel().value).toBe("blocked");
    expect(sortSel().value).toBe("name");
    expect(names()).toEqual(before);
    unmount();
  });

  it("restores the target a link pins and filters over it alone", async () => {
    const data = sixty();
    data.clusters[0]!.cells["1.37"] = cell(30, "blocked", 1);
    await open(data, "#/?filter=blocked&target=1.37");
    expect(names()).toEqual([data.clusters[0]!.name]);
    expect(screen.getByText(/1 of 60 clusters/)).toBeTruthy();
  });

  it("ignores unknown filter and sort values", async () => {
    await open(sixty(), "#/?filter=bogus&sort=nope&target=9.99");
    expect(filterSel().value).toBe("");
    expect(sortSel().value).toBe("score");
    expect(bodyRows()).toHaveLength(60);
  });

  it("offers a clear-filters button when nothing matches", async () => {
    await open(sixty(), "#/?q=zzz-no-such-cluster");
    expect(screen.getByText("No clusters match")).toBeTruthy();
    expect(screen.getByText("0 of 60 clusters")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Clear filters" }));
    expect(bodyRows()).toHaveLength(60);
    expect(window.location.hash).not.toMatch(/q=/);
  });

  it("renders at most 100 rows of 500 and shows more on request", async () => {
    const clusters: FleetRow[] = Array.from({ length: 500 }, (_, i) => ({
      clusterId: i + 1,
      name: `c-${String(i).padStart(3, "0")}`,
      cells: { "1.36": cell(i % 100, "blocked", 1) },
    }));
    await open({ targets: ["1.36"], clusters });
    expect(bodyRows()).toHaveLength(100);
    expect(screen.getByText("500 of 500 clusters")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Show 100 more" }));
    expect(bodyRows()).toHaveLength(200);
    // A new toolbar state starts at the first page again.
    fireEvent.change(sortSel(), { target: { value: "name" } });
    expect(bodyRows()).toHaveLength(100);
  });
});

describe("Fleet summary", () => {
  function bucketCounts(): Record<string, number> {
    const group = screen.getByRole("group", { name: /Fleet summary/ });
    const out: Record<string, number> = {};
    for (const b of within(group).getAllByRole("button")) {
      out[b.querySelector(".bucket-label")!.textContent!] = Number(
        b.querySelector(".bucket-count")!.textContent,
      );
    }
    return out;
  }

  function mixed(): FleetResponse {
    const row = (id: number, name: string, extra: Partial<FleetRow>): FleetRow => ({
      clusterId: id,
      name,
      lastSeen: NOW,
      cells: { "1.36": null, "1.37": null },
      ...extra,
    });
    return {
      targets: ["1.36", "1.37"],
      clusters: [
        row(1, "ready-1", { cells: { "1.36": cell(100, "ready"), "1.37": null } }),
        row(2, "ready-2", { cells: { "1.36": cell(99, "ready"), "1.37": null } }),
        row(3, "blocked-1", { cells: { "1.36": cell(30, "blocked", 3), "1.37": null } }),
        row(4, "unknown-1", { cells: { "1.36": cell(100, "unknown"), "1.37": null } }),
        // An older server sends only the ready flag.
        row(5, "legacy-notready", { cells: { "1.36": { score: 90, ready: false, blockers: 1 }, "1.37": null } }),
        row(6, "stale-1", { stale: true, cells: { "1.36": cell(100, "ready"), "1.37": null } }),
        row(7, "na-1", { notApplicable: ["1.36"], cells: { "1.36": null, "1.37": null } }),
        row(8, "missing-1", {}),
      ],
    };
  }

  it("counts every cluster once, and unknown is never ready", async () => {
    await open(mixed(), "#/?target=1.36");
    const counts = bucketCounts();
    expect(counts).toEqual({
      Ready: 2,
      Blocked: 2,
      Unknown: 1,
      Stale: 1,
      "n/a": 1,
      "No stored evaluation": 1,
    });
    expect(Object.values(counts).reduce((a, b) => a + b, 0)).toBe(8);
  });

  it("sums to the cluster count for a generated fleet, for every shown target", async () => {
    const data = sixty();
    await open(data);
    const total = () => Object.values(bucketCounts()).reduce((a, b) => a + b, 0);
    expect(total()).toBe(60);
    fireEvent.change(within(screen.getByRole("group", { name: /Fleet summary/ })).getByLabelText("Target"), {
      target: { value: "1.37" },
    });
    expect(total()).toBe(60);
    expect(window.location.hash).toMatch(/target=1\.37/);
  });

  it("defaults to the target most clusters can upgrade to, from the shown targets", async () => {
    const data = mixed();
    for (const r of data.clusters) r.notApplicable = [...(r.notApplicable ?? []), "1.36"];
    data.clusters[7]!.notApplicable = undefined;
    await open(data);
    const target = within(screen.getByRole("group", { name: /Fleet summary/ })).getByLabelText(
      "Target",
    ) as HTMLSelectElement;
    expect(target.value).toBe("1.37");
    expect(Array.from(target.options).map((o) => o.value)).toEqual(["1.36", "1.37"]);
  });

  it("applies the matching filter, and pins the target, when a bucket is clicked", async () => {
    await open(mixed(), "#/?target=1.36");
    const group = screen.getByRole("group", { name: /Fleet summary/ });
    fireEvent.click(within(group).getByRole("button", { name: /Blocked/ }));
    expect(filterSel().value).toBe("blocked");
    expect(names().sort()).toEqual(["blocked-1", "legacy-notready"]);
    expect(window.location.hash).toMatch(/filter=blocked/);
    expect(window.location.hash).toMatch(/target=1\.36/);
    expect(within(group).getByRole("button", { name: /Blocked/ }).getAttribute("aria-pressed")).toBe("true");

    // Clicking it again clears the filter.
    fireEvent.click(within(group).getByRole("button", { name: /Blocked/ }));
    await waitFor(() => expect(filterSel().value).toBe(""));
    expect(names()).toHaveLength(8);

    fireEvent.click(within(group).getByRole("button", { name: /Unknown/ }));
    expect(names()).toEqual(["unknown-1"]);
    fireEvent.click(within(group).getByRole("button", { name: /Stale/ }));
    expect(names()).toEqual(["stale-1"]);
  });

  it("refreshes in place: the matrix never gives way to the loading state", async () => {
    let calls = 0;
    const first = mixed();
    const second = { ...mixed(), clusters: [...mixed().clusters, { clusterId: 9, name: "pushed-later", cells: {} } as FleetRow] };
    let release!: () => void;
    const gate = new Promise<void>((r) => (release = r));
    mockApi({
      "api/v1/fleet": () => new Response(JSON.stringify(first), { status: 200 }),
    });
    const fetchSpy = globalThis.fetch;
    // Second answer is held back so the refetch is observable.
    vi.stubGlobal("fetch", async (...args: Parameters<typeof fetch>) => {
      calls++;
      if (calls === 1) return fetchSpy(...args);
      await gate;
      return new Response(JSON.stringify(second), { status: 200, headers: { "Content-Type": "application/json" } });
    });
    navigate("#/");
    render(<App />);
    await screen.findByRole("heading", { level: 1, name: "Fleet" });
    expect(screen.getByText(/^updated \d\d:\d\d:\d\d$/)).toBeTruthy();

    fireEvent.click(screen.getByRole("button", { name: "Refresh" }));
    await screen.findByRole("status", { name: "Refreshing" });
    expect(screen.queryByText("Loading fleet…")).toBeNull();
    expect(bodyRows()).toHaveLength(8);

    release();
    await waitFor(() => expect(bodyRows()).toHaveLength(9));
    expect(screen.queryByRole("status", { name: "Refreshing" })).toBeNull();
  });
});

describe("Fleet empty state", () => {
  it("keeps Refresh and the stamp, and picks up a snapshot pushed after load", async () => {
    let current: FleetResponse = { targets: [], clusters: [] };
    mockApi({
      "api/v1/fleet": () =>
        new Response(JSON.stringify(current), {
          status: 200,
          headers: { "Content-Type": "application/json" },
        }),
    });
    navigate("#/");
    render(<App />);
    await screen.findByText("No clusters yet");
    expect(screen.getByRole("heading", { level: 1, name: "Fleet" })).toBeTruthy();
    expect(screen.getByText(/^updated \d\d:\d\d:\d\d$/)).toBeTruthy();
    expect(bodyRows()).toHaveLength(0);

    current = {
      targets: ["1.37"],
      clusters: [{ clusterId: 1, name: "first-pushed", stale: false, cells: { "1.37": cell(100, "ready") } } as FleetRow],
    };
    fireEvent.click(screen.getByRole("button", { name: "Refresh" }));
    await waitFor(() => expect(names()).toEqual(["first-pushed"]));
    expect(screen.queryByText("No clusters yet")).toBeNull();
  });
});
