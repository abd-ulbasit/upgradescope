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
  teams: {
    payments: { worstScore: 40, blockers: 3, clusters: ["prod-eu", "prod-us"] },
    platform: { worstScore: 90, blockers: 0, clusters: ["prod-eu"] },
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
});
