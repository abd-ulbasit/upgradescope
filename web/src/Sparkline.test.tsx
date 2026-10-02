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
});
