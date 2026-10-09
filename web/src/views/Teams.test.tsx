// @vitest-environment happy-dom
import { fireEvent, render, screen, within } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { fetchedUrls, mockApi } from "../test-utils";
import type { FleetResponse, FleetTeamsResponse } from "../types";
import { Teams } from "./Teams";

const fleet: FleetResponse = {
  targets: ["1.35", "1.36"],
  clusters: [],
};

const teams: FleetTeamsResponse = {
  target: "1.35",
  // Better team first, so the worst-first order below comes from the sort.
  teams: {
    platform: { worstScore: 90, blockers: 0, clusters: ["prod-eu"] },
    payments: { worstScore: 40, blockers: 3, clusters: ["prod-eu", "prod-us"] },
  },
  evaluated: [
    { name: "prod-eu", clusterId: 1, source: "stored", evaluatedAt: "2026-10-01T12:00:00Z", snapshotId: 4 },
    { name: "prod-us", clusterId: 2, source: "what-if", evaluatedAt: "2026-10-01T12:00:00Z", snapshotId: 9 },
  ],
  missing: ["staging"],
  notApplicable: [],
};

describe("Teams view", () => {
  it("lists the fleet team rollup with links to each filtered cluster view", async () => {
    const fetchMock = mockApi({
      "api/v1/fleet": fleet,
      "api/v1/fleet/teams?target=1.35": teams,
    });
    render(<Teams />);
    const payments = await screen.findByRole("row", { name: /payments/ });
    expect(within(payments).getByText("40")).toBeTruthy();
    expect(within(payments).getByText("3")).toBeTruthy();
    expect(
      within(payments).getByRole("link", { name: "prod-us" }).getAttribute("href"),
    ).toBe("#/cluster/2?target=1.35&team=payments");
    // worst first
    const rows = screen.getAllByRole("row").slice(1);
    expect(rows[0]!.textContent).toMatch(/payments/);
    expect(screen.getByText(/1 cluster computed as a what-if/)).toBeTruthy();
    expect(screen.getByText(/staging/)).toBeTruthy();
    expect(fetchedUrls(fetchMock)).toContain("api/v1/fleet/teams?target=1.35");
  });

  it("does not link a cluster name that more than one cluster shares", async () => {
    mockApi({
      "api/v1/fleet": fleet,
      "api/v1/fleet/teams?target=1.35": {
        ...teams,
        evaluated: [
          ...teams.evaluated,
          { name: "prod-eu", clusterId: 7, source: "stored", evaluatedAt: "2026-10-01T12:00:00Z", snapshotId: 11 },
        ],
      },
    });
    render(<Teams />);
    const payments = await screen.findByRole("row", { name: /payments/ });
    // Clusters are identified by UID; a shared name cannot pick one.
    expect(within(payments).queryByRole("link", { name: "prod-eu" })).toBeNull();
    expect(within(payments).getByText("prod-eu").getAttribute("title")).toMatch(
      /2 clusters are named prod-eu/,
    );
    expect(
      within(payments).getByRole("link", { name: "prod-us" }).getAttribute("href"),
    ).toBe("#/cluster/2?target=1.35&team=payments");
  });

  it("rolls up any target typed into the picker", async () => {
    mockApi({
      "api/v1/fleet": fleet,
      "api/v1/fleet/teams?target=1.35": teams,
      "api/v1/fleet/teams?target=1.40": { ...teams, target: "1.40" },
    });
    render(<Teams />);
    await screen.findByRole("row", { name: /payments/ });
    fireEvent.change(screen.getByLabelText("Target"), { target: { value: "1.40" } });
    fireEvent.click(screen.getByRole("button", { name: "Evaluate" }));
    expect(window.location.hash).toBe("#/teams?target=1.40");
  });

  it("defaults to the target that applies to the most clusters", async () => {
    const fetchMock = mockApi({
      "api/v1/fleet": {
        targets: ["1.30", "1.36", "1.37"],
        clusters: [
          { clusterId: 1, name: "a", cells: {}, notApplicable: ["1.30", "1.36"] },
          { clusterId: 2, name: "b", cells: {}, notApplicable: ["1.30"] },
        ],
      },
      "api/v1/fleet/teams": { ...teams, target: "1.37" },
    });
    render(<Teams />);
    await screen.findByRole("row", { name: /payments/ });
    expect(fetchedUrls(fetchMock)).toContain("api/v1/fleet/teams?target=1.37");
  });

  // #243, VS-14: a team's score counts only its own findings. One warning
  // scores 98, but a blocker no team owns makes the team blocked, and a
  // check that did not run makes it unknown; neither may read green.
  it("shows each team's verdict and never colours a blocked or unknown team green", async () => {
    mockApi({
      "api/v1/fleet": fleet,
      "api/v1/fleet/teams?target=1.35": {
        ...teams,
        teams: {
          payments: { worstScore: 98, blockers: 0, verdict: "blocked", clusters: ["prod-eu"] },
          platform: { worstScore: 98, blockers: 0, verdict: "unknown", clusters: ["prod-eu"] },
          shop: { worstScore: 98, blockers: 0, verdict: "ready", clusters: ["prod-eu"] },
        },
      } satisfies FleetTeamsResponse,
    });
    render(<Teams />);
    const row = async (name: string) => (await screen.findByRole("row", { name: new RegExp(name) })) as HTMLElement;

    const payments = await row("payments");
    expect(within(payments).getByText("Blocked")).toBeTruthy();
    expect(within(payments).getByText("98").className).not.toMatch(/score-good/);
    expect(within(payments).getByText("98").className).toMatch(/score-bad/);

    const platform = await row("platform");
    expect(within(platform).getByText("Unknown")).toBeTruthy();
    expect(within(platform).getByText("98").className).not.toMatch(/score-good/);
    expect(within(platform).getByText("98").className).toMatch(/verdict-unknown/);

    const shop = await row("shop");
    expect(within(shop).getByText("Ready")).toBeTruthy();
    expect(within(shop).getByText("98").className).toMatch(/score-good/);
  });

  it("labels each excluded cluster by its reason, not all as no snapshot yet", async () => {
    mockApi({
      "api/v1/fleet": fleet,
      "api/v1/fleet/teams?target=1.35": {
        ...teams,
        missing: ["fresh", "huge", "broken"],
        excluded: [
          { name: "fresh", clusterId: 5, reason: "no-snapshot" },
          { name: "huge", clusterId: 6, reason: "too-large" },
          { name: "broken", clusterId: 7, reason: "unreadable" },
        ],
      } satisfies FleetTeamsResponse,
    });
    render(<Teams />);
    await screen.findByRole("row", { name: /payments/ });
    const notes = [...document.querySelectorAll(".rollup-notes li")].map((li) => li.textContent);
    expect(notes).toContain("No snapshot yet: fresh.");
    expect(notes.find((n) => n?.includes("huge"))).toMatch(/report for → 1.35 would be over --max-snapshot-bytes/);
    expect(notes.find((n) => n?.includes("broken"))).toMatch(/unreadable/);
    // The cluster that has snapshots is never "no snapshot yet".
    expect(notes.find((n) => n?.startsWith("No snapshot yet"))).not.toMatch(/huge|broken/);
  });

  it("describes missing clusters neutrally when the server sends no reasons", async () => {
    mockApi({
      "api/v1/fleet": fleet,
      "api/v1/fleet/teams?target=1.35": teams, // an older server: `missing` only
    });
    render(<Teams />);
    await screen.findByRole("row", { name: /payments/ });
    const note = [...document.querySelectorAll(".rollup-notes li")].find((li) => li.textContent?.includes("staging"));
    expect(note?.textContent).toMatch(/Not included/);
    expect(note?.textContent).not.toMatch(/No snapshot yet/);
  });
});
