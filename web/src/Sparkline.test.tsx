// @vitest-environment happy-dom
import { render } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { Sparkline } from "./Sparkline";
import "./test-utils";

const t0 = Date.parse("2026-06-01T00:00:00Z");
const minute = 60_000;
const day = 24 * 60 * minute;

function dots(container: HTMLElement): number[] {
  return [...container.querySelectorAll("circle")].map((c) =>
    Number(c.getAttribute("cx")),
  );
}

describe("Sparkline", () => {
  it("spaces points by evaluation time, not by index", () => {
    const at = [t0, t0 + minute, t0 + 90 * day];
    const { container } = render(
      <Sparkline
        points={at.map((t, i) => ({ at: new Date(t).toISOString(), score: 50 + i * 20, ready: false }))}
        now={at[2]}
      />,
    );
    const [a, b, c] = dots(container) as [number, number, number];
    const span = c - a;
    expect(span).toBeGreaterThan(0);
    // One minute out of ninety days sits on top of the first point; index
    // spacing would have put it halfway across.
    expect((b - a) / span).toBeCloseTo(minute / (90 * day), 4);
  });

  it("holds the last score until now", () => {
    const at = [t0, t0 + day];
    const { container } = render(
      <Sparkline
        points={at.map((t) => ({ at: new Date(t).toISOString(), score: 80, ready: false }))}
        now={t0 + 4 * day}
      />,
    );
    const [a, b] = dots(container) as [number, number];
    const path = container.querySelector("path.spark-line")!.getAttribute("d")!;
    const end = Number(/H([\d.]+)\s*$/.exec(path)?.[1]);
    // The second point sits a quarter of the way to now; the step line
    // runs on to the right edge.
    expect((b - a) / (end - a)).toBeCloseTo(0.25, 3);
  });

  // #243: an evaluation whose required check did not run can score 100. The
  // trend draws verdicts, not scores: unknown is not the colour of ready.
  it("colours each dot by its verdict, not its score", () => {
    const at = (i: number) => new Date(t0 + i * day).toISOString();
    const { container } = render(
      <Sparkline
        points={[
          { at: at(0), score: 100, ready: true, verdict: "ready" },
          { at: at(1), score: 100, ready: false, verdict: "unknown" },
          { at: at(2), score: 75, ready: false, verdict: "blocked" },
          { at: at(3), score: 82, ready: false }, // an older server: no verdict
        ]}
        now={t0 + 3 * day}
      />,
    );
    const dots = [...container.querySelectorAll("circle")];
    expect(dots[0]!.getAttribute("class")).toMatch(/spark-ready/);
    expect(dots[1]!.getAttribute("class")).toMatch(/spark-unknown/);
    expect(dots[1]!.getAttribute("class")).not.toMatch(/spark-ready|score-good/);
    expect(dots[2]!.getAttribute("class")).toMatch(/spark-blocked/);
    // Without a verdict a dot is neutral: no guess from the score.
    expect(dots[3]!.getAttribute("class")).not.toMatch(/spark-(ready|unknown|blocked)|score-/);
    expect(dots[1]!.querySelector("title")!.textContent).toMatch(/score 100, unknown/);
    expect(dots[3]!.querySelector("title")!.textContent).not.toMatch(/ready|unknown|blocked/);
  });

  it("treats ready=true as ready even without a verdict", () => {
    const { container } = render(
      <Sparkline points={[{ at: new Date(t0).toISOString(), score: 100, ready: true }]} now={t0} />,
    );
    expect(container.querySelector("circle")!.getAttribute("class")).toMatch(/spark-ready/);
  });
});
