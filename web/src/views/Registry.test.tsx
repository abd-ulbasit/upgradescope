// @vitest-environment happy-dom
import { fireEvent, render, screen, within } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { mockApi } from "../test-utils";
import type { AddOn, RegistryResponse } from "../types";
import { Registry } from "./Registry";

const cite = ["https://example.com/lifecycle"];

const rke2: AddOn = {
  schema_version: 2,
  id: "rke2-ingress-nginx",
  display_name: "RKE2 Ingress NGINX",
  matchers: {
    images: ["rancher/nginx-ingress-controller:*-hardened*"],
    charts: ["rke2-ingress-nginx"],
  },
  support: {
    status: "supported",
    eol_date: "2026-03-31",
    extended_eol_date: "2027-11-30",
    extended_support_condition: "the cluster has a SUSE Rancher Prime LTS subscription",
    citations: cite,
  },
};

const flux: AddOn = {
  schema_version: 2,
  id: "flux",
  display_name: "Flux",
  matchers: {
    images: ["fluxcd/flux", "weaveworks/flux"],
    charts: ["flux2"],
    components: [{ image: "fluxcd/source-controller" }, { image: "fluxcd/helm-controller" }],
  },
  support: { status: "supported", citations: cite },
};

const plain: AddOn = {
  schema_version: 2,
  id: "ingress-nginx",
  display_name: "Ingress NGINX",
  matchers: { images: ["ingress-nginx/controller"] },
  support: { status: "eol", eol_date: "2026-03-24", citations: cite },
};

const staleSupported: AddOn = {
  schema_version: 2,
  id: "stale",
  display_name: "Stale Supported",
  matchers: { images: ["example/stale"] },
  support: { status: "supported", eol_date: "2026-03-31", citations: cite },
};

// Fixed clocks: the view derives its status from the dates and today (UTC).
const BEFORE_EOL = new Date("2026-01-15T12:00:00Z");
const AFTER_EOL = new Date("2026-10-11T12:00:00Z");

function card(name: string): HTMLElement {
  return screen.getByRole("heading", { name }).closest("article") as HTMLElement;
}

describe("Registry view", () => {
  it("reads a past eol_date with an extended date still ahead as extended support only", async () => {
    mockApi({ "api/v1/registry": { addons: [rke2, flux, plain] } satisfies RegistryResponse });
    render(<Registry now={AFTER_EOL} />);
    await screen.findByRole("heading", { name: "RKE2 Ingress NGINX" });

    const c = card("RKE2 Ingress NGINX");
    expect(within(c).getByText("extended support only · until 2027-11-30")).toBeTruthy();
    expect(within(c).queryByText(/supported · 2026-03-31/)).toBeNull();
    expect(within(c).getByText(/Supported until 2027-11-30 only if the cluster has a SUSE Rancher Prime LTS subscription/)).toBeTruthy();
    // Past tense: the date has passed.
    expect(within(c).getByText(/for everyone else support ended 2026-03-31/)).toBeTruthy();
    expect(within(c).queryByText(/support ends 2026-03-31/)).toBeNull();
  });

  it("reads a past eol_date with no extended date as eol, whatever the stored status", async () => {
    mockApi({ "api/v1/registry": { addons: [staleSupported] } });
    render(<Registry now={AFTER_EOL} />);
    await screen.findByRole("heading", { name: "Stale Supported" });
    const c = card("Stale Supported");
    expect(within(c).getByText("eol · 2026-03-31")).toBeTruthy();
    expect(within(c).queryByText(/supported ·/)).toBeNull();
  });

  it("reads a past eol_date with a past extended date as eol", async () => {
    mockApi({ "api/v1/registry": { addons: [rke2] } });
    render(<Registry now={new Date("2027-12-01T00:00:00Z")} />);
    await screen.findByRole("heading", { name: "RKE2 Ingress NGINX" });
    const c = card("RKE2 Ingress NGINX");
    expect(within(c).getByText("eol · 2026-03-31")).toBeTruthy();
    expect(within(c).getByText(/Extended support ended 2027-11-30 .*only if the cluster has/)).toBeTruthy();
  });

  it("keeps supported and the future tense while every date is ahead", async () => {
    mockApi({ "api/v1/registry": { addons: [rke2] } satisfies RegistryResponse });
    render(<Registry now={BEFORE_EOL} />);
    await screen.findByRole("heading", { name: "RKE2 Ingress NGINX" });
    const c = card("RKE2 Ingress NGINX");
    expect(within(c).getByText("supported · 2026-03-31")).toBeTruthy();
    expect(within(c).getByText(/for everyone else support ends 2026-03-31/)).toBeTruthy();
  });

  it("treats the eol_date itself as passed, as the engine does, by the UTC date", async () => {
    mockApi({ "api/v1/registry": { addons: [staleSupported] } });
    // 23:30 on 2026-03-30 in UTC is still ahead, even though a UTC+2 viewer is already on 03-31.
    const { unmount } = render(<Registry now={new Date("2026-03-30T23:30:00Z")} />);
    await screen.findByRole("heading", { name: "Stale Supported" });
    expect(within(card("Stale Supported")).getByText("supported · 2026-03-31")).toBeTruthy();
    unmount();
    render(<Registry now={new Date("2026-03-31T00:00:00Z")} />);
    await screen.findByRole("heading", { name: "Stale Supported" });
    expect(within(card("Stale Supported")).getByText("eol · 2026-03-31")).toBeTruthy();
  });

  it("says nothing of extended support for an entry without any", async () => {
    mockApi({ "api/v1/registry": { addons: [plain, flux] } });
    render(<Registry now={AFTER_EOL} />);
    await screen.findByRole("heading", { name: "Ingress NGINX" });
    expect(within(card("Ingress NGINX")).queryByText(/only if/)).toBeNull();
    expect(within(card("Flux")).queryByText(/Supported until/)).toBeNull();
    expect(within(card("Ingress NGINX")).getByText("eol · 2026-03-24")).toBeTruthy();
  });

  it("lists the component images that identify a product beside its images", async () => {
    mockApi({ "api/v1/registry": { addons: [flux, plain] } });
    render(<Registry now={AFTER_EOL} />);
    await screen.findByRole("heading", { name: "Flux" });
    const c = card("Flux");
    for (const m of ["fluxcd/flux", "weaveworks/flux", "fluxcd/source-controller", "fluxcd/helm-controller", "flux2"]) {
      expect(within(c).getByText(m)).toBeTruthy();
    }
    expect(within(c).getByText("fluxcd/source-controller").getAttribute("title")).toMatch(/component/);
  });

  it("still filters by id and name", async () => {
    mockApi({ "api/v1/registry": { addons: [rke2, flux, plain] } });
    render(<Registry now={AFTER_EOL} />);
    await screen.findByRole("heading", { name: "Flux" });
    fireEvent.change(screen.getByLabelText("Filter add-ons"), { target: { value: "rke2" } });
    expect(screen.queryByRole("heading", { name: "Flux" })).toBeNull();
    expect(screen.getByRole("heading", { name: "RKE2 Ingress NGINX" })).toBeTruthy();
  });
});
