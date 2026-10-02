// @vitest-environment happy-dom
import { render, screen, within } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { mockApi } from "../test-utils";
import type { FleetResponse } from "../types";
import { Fleet } from "./Fleet";

const fleet: FleetResponse = {
  targets: ["1.35", "1.36"],
  clusters: [
    {
      clusterId: 1,
      name: "prod-eu",
      serverVersion: "v1.34.2",
      lastSeen: "2026-07-01T00:00:00Z",
      stale: true,
      cells: {
        "1.35": { score: 95, ready: false, verdict: "unknown", blockers: 0 },
        "1.36": null,
      },
    },
    {
      clusterId: 2,
      name: "prod-us",
      serverVersion: "v1.35.1",
      lastSeen: "2026-10-01T12:00:00Z",
      stale: false,
      cells: {
        "1.35": null,
        "1.36": { score: 100, ready: true, verdict: "ready", blockers: 0 },
      },
      notApplicable: ["1.35"],
    },
  ],
};

async function renderFleet(data: FleetResponse = fleet) {
  mockApi({ "api/v1/fleet": data });
  render(<Fleet />);
  await screen.findByRole("heading", { level: 1, name: "Fleet" });
}

function row(name: string) {
  return screen.getByRole("row", { name: new RegExp(name) });
}

describe("Fleet view", () => {
  it("links a never-evaluated cell to a what-if of that target", async () => {
    await renderFleet();
    const link = within(row("prod-eu")).getByRole("link", { name: /1\.36.*what-if/ });
    expect(link.getAttribute("href")).toBe("#/cluster/1?target=1.36");
  });

  it("does not link a target the cluster already runs", async () => {
    await renderFleet();
    const cells = within(row("prod-us")).getAllByRole("cell");
    expect(cells[0]!.textContent).toMatch(/n\/a/);
    expect(within(cells[0]!).queryByRole("link")).toBeNull();
  });

  it("shows an unknown verdict distinctly from ready", async () => {
    await renderFleet();
    expect(within(row("prod-eu")).getByText("unknown")).toBeTruthy();
    expect(within(row("prod-us")).getByText("ready")).toBeTruthy();
  });

  it("flags stale clusters and shows when each was last seen", async () => {
    await renderFleet();
    expect(within(row("prod-eu")).getByText("stale")).toBeTruthy();
    expect(within(row("prod-us")).queryByText("stale")).toBeNull();
    expect(within(row("prod-us")).getByText(/seen/)).toBeTruthy();
  });

  it("says how many default targets the server left out", async () => {
    await renderFleet({ ...fleet, targetsOmitted: 3 });
    expect(screen.getByRole("note").textContent).toMatch(/3 more targets not\s+shown/);
  });

  it("says nothing of omitted targets when there are none", async () => {
    await renderFleet();
    expect(screen.queryByRole("note")).toBeNull();
  });
});
