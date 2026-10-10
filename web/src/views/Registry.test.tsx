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

function card(name: string): HTMLElement {
  return screen.getByRole("heading", { name }).closest("article") as HTMLElement;
}

describe("Registry view", () => {
  it("shows split support as the scan report does, not just the first end date", async () => {
    mockApi({ "api/v1/registry": { addons: [rke2, flux, plain] } satisfies RegistryResponse });
    render(<Registry />);
    await screen.findByRole("heading", { name: "RKE2 Ingress NGINX" });

    const c = card("RKE2 Ingress NGINX");
    expect(within(c).getByText(/Supported until 2027-11-30 only if the cluster has a SUSE Rancher Prime LTS subscription/)).toBeTruthy();
    expect(within(c).getByText(/support ends 2026-03-31/)).toBeTruthy();
    // The badge keeps the status and the date everyone's support ends.
    expect(within(c).getByText("supported · 2026-03-31")).toBeTruthy();
  });

  it("says nothing of extended support for an entry without any", async () => {
    mockApi({ "api/v1/registry": { addons: [plain, flux] } });
    render(<Registry />);
    await screen.findByRole("heading", { name: "Ingress NGINX" });
    expect(within(card("Ingress NGINX")).queryByText(/only if/)).toBeNull();
    expect(within(card("Flux")).queryByText(/Supported until/)).toBeNull();
    expect(within(card("Ingress NGINX")).getByText("eol · 2026-03-24")).toBeTruthy();
  });

  it("lists the component images that identify a product beside its images", async () => {
    mockApi({ "api/v1/registry": { addons: [flux, plain] } });
    render(<Registry />);
    await screen.findByRole("heading", { name: "Flux" });
    const c = card("Flux");
    for (const m of ["fluxcd/flux", "weaveworks/flux", "fluxcd/source-controller", "fluxcd/helm-controller", "flux2"]) {
      expect(within(c).getByText(m)).toBeTruthy();
    }
    expect(within(c).getByText("fluxcd/source-controller").getAttribute("title")).toMatch(/component/);
  });

  it("still filters by id and name", async () => {
    mockApi({ "api/v1/registry": { addons: [rke2, flux, plain] } });
    render(<Registry />);
    await screen.findByRole("heading", { name: "Flux" });
    fireEvent.change(screen.getByLabelText("Filter add-ons"), { target: { value: "rke2" } });
    expect(screen.queryByRole("heading", { name: "Flux" })).toBeNull();
    expect(screen.getByRole("heading", { name: "RKE2 Ingress NGINX" })).toBeTruthy();
  });
});
