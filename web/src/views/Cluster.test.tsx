// @vitest-environment happy-dom
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { App } from "../App";
import { setToken } from "../api";
import { fetchedUrls, mockApi, navigate } from "../test-utils";
import type { ClusterDetail, Finding, Report } from "../types";

const detail: ClusterDetail = {
  id: 1,
  name: "prod-eu",
  clusterUid: "uid-1",
  firstSeen: "2026-09-01T00:00:00Z",
  lastSeen: "2026-10-01T12:00:00Z",
  serverVersion: "v1.34.2",
  evaluations: [
    { target: "1.35", score: 60, ready: false, verdict: "blocked", blockers: 1, warnings: 1, kbVersion: "kb1", evaluatedAt: "2026-10-01T12:00:00Z" },
    { target: "1.37", score: 40, ready: false, verdict: "blocked", blockers: 1, warnings: 1, kbVersion: "kb1", evaluatedAt: "2026-10-01T12:00:00Z" },
  ],
};

const ingressCitations = [
  "https://kubernetes.io/blog/2025/11/11/ingress-nginx-retirement/",
  "https://kubernetes.io/docs/reference/using-api/deprecation-guide/",
];

function finding(over: Partial<Finding>): Finding {
  return {
    category: "deprecated-api",
    severity: "warning",
    title: "t",
    detail: "d",
    ...over,
  };
}

function report(target: string, over: Partial<Report> = {}): Report {
  const findings =
    target === "1.35"
      ? [
          finding({ key: "a", category: "deprecated-api", severity: "blocker", title: "flowcontrol v1beta3 in use", teams: ["payments"] }),
          finding({ key: "b", category: "eol-addon", severity: "warning", title: "ingress-nginx is EOL", teams: ["platform"], citations: ingressCitations }),
        ]
      : [
          finding({ key: "a", category: "removed-api", severity: "blocker", title: "flowcontrol v1beta3 removed", teams: ["payments"] }),
          finding({ key: "b", category: "eol-addon", severity: "warning", title: "ingress-nginx is EOL", teams: ["platform"] }),
        ];
  return {
    clusterId: "uid-1",
    target,
    kbVersion: "kb1",
    score: 60,
    ready: false,
    verdict: "blocked",
    findings,
    source: "stored",
    evaluatedAt: "2026-10-01T12:00:00Z",
    snapshotId: 4,
    serverVersion: "v1.34.2",
    ...over,
  };
}

function routes(extra: Record<string, unknown> = {}) {
  return {
    "api/v1/clusters/1": detail,
    "api/v1/clusters/1/report": report("1.35"),
    "api/v1/clusters/1/report?target=1.35": report("1.35"),
    "api/v1/clusters/1/report?target=1.37": report("1.37"),
    "api/v1/clusters/1/history": [],
    ...extra,
  };
}

async function openCluster(hash: string, extra: Record<string, unknown> = {}) {
  const fetchMock = mockApi(routes(extra));
  navigate(hash);
  render(<App />);
  await screen.findByRole("heading", { level: 1, name: "prod-eu" });
  await screen.findByText(/readiness for/);
  return fetchMock;
}

function targetInput(): HTMLInputElement {
  return screen.getByLabelText("Target") as HTMLInputElement;
}

function pickTarget(value: string) {
  fireEvent.change(targetInput(), { target: { value } });
  fireEvent.click(screen.getByRole("button", { name: "Evaluate" }));
}

describe("Cluster view", () => {
  it("resets the category and team filters when the target changes", async () => {
    await openCluster("#/cluster/1?target=1.35");
    fireEvent.change(screen.getByLabelText("Category"), {
      target: { value: "deprecated-api" },
    });
    expect(screen.queryByText("ingress-nginx is EOL")).toBeNull();

    pickTarget("1.37");
    expect(window.location.hash).toBe("#/cluster/1?target=1.37");
    await screen.findByText("flowcontrol v1beta3 removed");
    expect((screen.getByLabelText("Category") as HTMLSelectElement).value).toBe("");
    expect((screen.getByLabelText("Team") as HTMLSelectElement).value).toBe("");
    expect(screen.getByText("ingress-nginx is EOL")).toBeTruthy();
    expect(screen.queryByText("No findings match the current filters")).toBeNull();
  });

  it("evaluates any typed minor as a what-if, labelled as not stored", async () => {
    const whatIf = report("1.40", { source: "what-if" });
    await openCluster("#/cluster/1", { "api/v1/clusters/1/report?target=1.40": whatIf });
    pickTarget("1.40");
    expect(window.location.hash).toBe("#/cluster/1?target=1.40");
    expect(await screen.findByText("what-if (not stored, no history)")).toBeTruthy();
  });

  it("draws no trend under a what-if, even when older snapshots had one", async () => {
    // The latest snapshot has no stored evaluation for 1.35, so the report
    // is a what-if, while earlier snapshots still have history points.
    await openCluster("#/cluster/1?target=1.35", {
      "api/v1/clusters/1/report?target=1.35": report("1.35", { source: "what-if" }),
      "api/v1/clusters/1/history": [
        { at: "2026-09-01T00:00:00Z", score: 50, ready: false },
        { at: "2026-09-02T00:00:00Z", score: 55, ready: false },
      ],
    });
    expect(screen.getByText("what-if (not stored, no history)")).toBeTruthy();
    expect(screen.queryByRole("img", { name: /score trend/i })).toBeNull();
    expect(screen.getByText(/what-ifs are computed on request/i)).toBeTruthy();
  });

  it("draws the trend of stored evaluations", async () => {
    await openCluster("#/cluster/1?target=1.35", {
      "api/v1/clusters/1/history": [
        { at: "2026-09-01T00:00:00Z", score: 50, ready: false },
        { at: "2026-09-02T00:00:00Z", score: 60, ready: false },
      ],
    });
    expect(screen.getByRole("img", { name: /score trend/i })).toBeTruthy();
  });

  it("rejects a target that is not a minor version", async () => {
    const fetchMock = await openCluster("#/cluster/1");
    const before = fetchMock.mock.calls.length;
    pickTarget("1.x");
    expect(screen.getByRole("alert").textContent).toMatch(/minor version/);
    expect(targetInput().getAttribute("aria-invalid")).toBe("true");
    expect(window.location.hash).toBe("#/cluster/1");
    expect(fetchMock.mock.calls.length).toBe(before);
  });

  it("shows a target missing from the stored list as the picked value", async () => {
    await openCluster("#/cluster/1?target=1.40", {
      "api/v1/clusters/1/report?target=1.40": report("1.40", { source: "what-if" }),
    });
    expect(targetInput().value).toBe("1.40");
  });

  it("fetches the report once, without a separate findings call", async () => {
    const fetchMock = await openCluster("#/cluster/1?target=1.35");
    expect(fetchedUrls(fetchMock).some((u) => u.includes("/findings"))).toBe(false);
  });

  it("downloads exports with the bearer token", async () => {
    setToken("read-tok");
    const createObjectURL = vi.spyOn(URL, "createObjectURL").mockReturnValue("blob:x");
    vi.spyOn(URL, "revokeObjectURL").mockImplementation(() => {});
    // Record the save instead of letting happy-dom navigate to the blob.
    const saved: string[] = [];
    vi.spyOn(HTMLAnchorElement.prototype, "click").mockImplementation(function (
      this: HTMLAnchorElement,
    ) {
      saved.push(this.download);
    });
    const fetchMock = await openCluster("#/cluster/1?target=1.35", {
      "api/v1/clusters/1/export": () =>
        new Response("a,b\n", {
          headers: { "Content-Disposition": 'attachment; filename="prod-eu-1.35.csv"' },
        }),
    });

    fireEvent.click(screen.getByRole("button", { name: "Download CSV" }));
    await waitFor(() => expect(createObjectURL).toHaveBeenCalled());
    const call = fetchMock.mock.calls.find((c) => String(c[0]).includes("/export"))!;
    expect(call[0]).toBe("api/v1/clusters/1/export?target=1.35&format=csv");
    expect((call[1] as RequestInit).headers).toEqual({ Authorization: "Bearer read-tok" });
    await waitFor(() => expect(saved).toEqual(["prod-eu-1.35.csv"]));

    const html = screen.getByRole("button", { name: "Download HTML report" });
    await waitFor(() => expect((html as HTMLButtonElement).disabled).toBe(false));
    fireEvent.click(html);
    await waitFor(() => expect(saved).toHaveLength(2));
    expect(fetchedUrls(fetchMock)).toContain(
      "api/v1/clusters/1/export?target=1.35&format=html",
    );
  });

  it("disables exports for a what-if, which is not stored", async () => {
    await openCluster("#/cluster/1?target=1.40", {
      "api/v1/clusters/1/report?target=1.40": report("1.40", { source: "what-if" }),
    });
    const csv = screen.getByRole("button", { name: "Download CSV" }) as HTMLButtonElement;
    expect(csv.disabled).toBe(true);
    expect(screen.getByText(/exports cover stored evaluations/i)).toBeTruthy();
  });

  it("shows an unknown verdict apart from ready and blocked, with its reasons", async () => {
    await openCluster("#/cluster/1", {
      "api/v1/clusters/1/report": report("1.35", {
        verdict: "unknown",
        ready: false,
        findings: [],
        notAssessed: [
          { capability: "api-usage", reason: "metrics endpoint forbidden", required: true },
          {
            capability: "addons",
            reason: "partial: list ingresses forbidden",
            required: true,
            partial: true,
            skipped: ["networking.k8s.io/v1 ingresses"],
          },
          { capability: "helm", reason: "no releases readable" },
        ],
      }),
    });
    expect(screen.getByText("Unknown")).toBeTruthy();
    expect(screen.queryByText("Ready")).toBeNull();
    const unknown = screen.getByRole("region", { name: "Verdict unknown" });
    expect(within(unknown).getByText(/metrics endpoint forbidden/)).toBeTruthy();
    expect(within(unknown).getByText(/partial: list ingresses forbidden/)).toBeTruthy();
    expect(within(unknown).queryByText(/no releases readable/)).toBeNull();
    const gaps = screen.getByRole("region", { name: "Not assessed" });
    expect(within(gaps).getByText("partial")).toBeTruthy();
    expect(within(gaps).getByText("networking.k8s.io/v1 ingresses")).toBeTruthy();
  });

  it("shows each team's own verdict from the server", async () => {
    // A blocked cluster (payments' blocker) with a required gap: platform,
    // with only a warning, is unknown — not ready, and not blocked.
    await openCluster("#/cluster/1", {
      "api/v1/clusters/1/report": report("1.35", {
        notAssessed: [{ capability: "api-usage", reason: "metrics endpoint forbidden", required: true }],
        teams: {
          payments: { score: 75, ready: false, verdict: "blocked", blockers: 1, warnings: 0 },
          platform: { score: 95, ready: false, verdict: "unknown", blockers: 0, warnings: 1 },
        },
      }),
    });
    const teams = screen.getByRole("heading", { name: "Teams" }).closest(".card") as HTMLElement;
    const row = (name: string) => within(teams).getByRole("link", { name }).closest("tr") as HTMLElement;
    expect(within(row("payments")).getByText("blocked")).toBeTruthy();
    expect(within(row("platform")).getByText("unknown")).toBeTruthy();
  });

  it("derives a team's verdict from an older server's ready and the cluster's verdict", async () => {
    await openCluster("#/cluster/1", {
      "api/v1/clusters/1/report": report("1.35", {
        verdict: "unknown",
        teams: { platform: { score: 95, ready: true, blockers: 0, warnings: 1 } },
      }),
    });
    const teams = screen.getByRole("heading", { name: "Teams" }).closest(".card") as HTMLElement;
    expect(within(teams).getByText("unknown")).toBeTruthy();
  });

  it("counts suppressed findings when present", async () => {
    await openCluster("#/cluster/1", {
      "api/v1/clusters/1/report": report("1.35", {
        suppressed: [
          { ...finding({ title: "x" }), reason: "accepted", source: ".upgradescope.yaml" },
          { ...finding({ title: "y" }), reason: "accepted", source: "annotation" },
        ],
      }),
    });
    expect(screen.getByText(/2 suppressed/)).toBeTruthy();
  });

  it("shows the images no add-on registry entry recognizes, only when there are any", async () => {
    await openCluster("#/cluster/1", {
      "api/v1/clusters/1/report": report("1.35", {
        unrecognizedImages: ["corp.example/edge/nginx-controller"],
        unrecognizedImagesOmitted: 1,
      }),
    });
    const section = screen.getByRole("region", { name: "Unrecognized images (2)" });
    expect(within(section).getByText("corp.example/edge/nginx-controller")).toBeTruthy();
    cleanup();

    await openCluster("#/cluster/1?target=1.37");
    expect(screen.queryByRole("region", { name: /Unrecognized images/ })).toBeNull();
  });

  it("says how old the pod evidence is only when the agent reused a pod pass", async () => {
    await openCluster("#/cluster/1", {
      "api/v1/clusters/1/report": report("1.35", { addOnEvidenceAgeSeconds: 1230 }),
    });
    expect(screen.getByTestId("pod-evidence-age").textContent).toMatch(/^Pod evidence 21 min old/);
    cleanup();

    await openCluster("#/cluster/1", {
      "api/v1/clusters/1/report": report("1.35", { addOnEvidenceAgeSeconds: 7 }),
    });
    expect(screen.getByTestId("pod-evidence-age").textContent).toMatch(/^Pod evidence 1 min old/);
    cleanup();

    await openCluster("#/cluster/1", {
      "api/v1/clusters/1/report": report("1.35", { addOnEvidenceAgeSeconds: 0 }),
    });
    expect(screen.queryByTestId("pod-evidence-age")).toBeNull();
    cleanup();

    await openCluster("#/cluster/1");
    expect(screen.queryByTestId("pod-evidence-age")).toBeNull();
  });

  it("flags a stale cluster and shows when it was last seen", async () => {
    mockApi(routes({ "api/v1/clusters/1": { ...detail, stale: true } }));
    navigate("#/cluster/1");
    render(<App />);
    expect(await screen.findByText("stale")).toBeTruthy();
    expect(screen.getByText(/last seen/)).toBeTruthy();
  });

  it("gives two citations on one host distinct link names", async () => {
    await openCluster("#/cluster/1?target=1.35");
    const names = ingressCitations.map(
      (url) => screen.getByRole("link", { name: (_, el) => el.getAttribute("href") === url }).textContent,
    );
    expect(names[0]).not.toBe(names[1]);
    expect(names[0]).toMatch(/^kubernetes\.io/);
  });

  it("counts the namespaces a finding does not list", async () => {
    const many = report("1.35", {
      findings: [finding({ key: "w", title: "wide", namespaces: ["ns-a", "ns-b"], namespacesOmitted: 98 })],
    });
    await openCluster("#/cluster/1?target=1.35", { "api/v1/clusters/1/report?target=1.35": many });
    expect(screen.getByText("ns-a")).toBeTruthy();
    expect(screen.getByText("and 98 more")).toBeTruthy();
  });

  it("applies a team filter from the route", async () => {
    await openCluster("#/cluster/1?target=1.35&team=platform");
    expect((screen.getByLabelText("Team") as HTMLSelectElement).value).toBe("platform");
    expect(screen.getByText("ingress-nginx is EOL")).toBeTruthy();
    expect(screen.queryByText("flowcontrol v1beta3 in use")).toBeNull();
  });

  // #243: a team a namespace label calls "unattributed" and the findings no
  // team owns are two rows and two filter choices.
  it("keeps a team named unattributed apart from the findings no team owns", async () => {
    const findings = [
      finding({ key: "a", severity: "warning", title: "owned by the real team", teams: ["unattributed"] }),
      finding({ key: "b", severity: "blocker", title: "owned by nobody" }),
    ];
    await openCluster("#/cluster/1?target=1.35", {
      "api/v1/clusters/1/report?target=1.35": report("1.35", {
        findings,
        teams: {
          unattributed: { score: 95, ready: false, verdict: "blocked", blockers: 0, warnings: 1 },
          "(unattributed)": { score: 75, ready: false, verdict: "blocked", blockers: 1, warnings: 0 },
        },
      }),
    });
    const teams = screen.getByRole("heading", { name: "Teams" }).closest(".card") as HTMLElement;
    const links = within(teams).getAllByRole("link").map((a) => [a.textContent, a.getAttribute("href")]);
    expect(links).toEqual([
      ["(unattributed)", "#/cluster/1?target=1.35&team=(unattributed)"],
      ["unattributed", "#/cluster/1?target=1.35&team=unattributed"],
    ]);

    const select = screen.getByLabelText("Team") as HTMLSelectElement;
    fireEvent.change(select, { target: { value: "unattributed" } });
    expect(screen.getByText("owned by the real team")).toBeTruthy();
    expect(screen.queryByText("owned by nobody")).toBeNull();
    fireEvent.change(select, { target: { value: "(unattributed)" } });
    expect(screen.getByText("owned by nobody")).toBeTruthy();
    expect(screen.queryByText("owned by the real team")).toBeNull();
  });

  it("opens the unowned findings from the route", async () => {
    await openCluster("#/cluster/1?target=1.35&team=%28unattributed%29", {
      "api/v1/clusters/1/report?target=1.35": report("1.35", {
        findings: [
          finding({ key: "a", title: "owned by the real team", teams: ["unattributed"] }),
          finding({ key: "b", title: "owned by nobody" }),
        ],
      }),
    });
    expect((screen.getByLabelText("Team") as HTMLSelectElement).value).toBe("(unattributed)");
    expect(screen.getByText("owned by nobody")).toBeTruthy();
    expect(screen.queryByText("owned by the real team")).toBeNull();
  });
});
