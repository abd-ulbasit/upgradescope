import type { ScorePoint } from "./types";
import { formatTime, scoreClass } from "./ui";

// Sparkline: hand-rolled SVG score trend (0–100). Points are spaced evenly
// by index — snapshots arrive on a fixed agent cadence, so index spacing
// reads the same as time spacing without axis machinery.
export function Sparkline({ points }: { points: ScorePoint[] }) {
  if (points.length === 0) return null;

  const w = 560;
  const h = 96;
  const pad = 8;
  const x = (i: number) =>
    points.length === 1
      ? w / 2
      : pad + (i * (w - 2 * pad)) / (points.length - 1);
  const y = (score: number) => pad + ((100 - score) * (h - 2 * pad)) / 100;

  const path = points
    .map((p, i) => `${i === 0 ? "M" : "L"}${x(i).toFixed(1)},${y(p.score).toFixed(1)}`)
    .join(" ");
  const last = points[points.length - 1]!;
  const first = points[0]!;

  return (
    <figure className="sparkline">
      <svg
        viewBox={`0 0 ${w} ${h}`}
        role="img"
        aria-label={`Score trend over ${points.length} evaluations, latest ${last.score}`}
        preserveAspectRatio="none"
      >
        {/* reference lines at 100 / 50 / 0 */}
        {[100, 50, 0].map((v) => (
          <line
            key={v}
            x1={pad}
            x2={w - pad}
            y1={y(v)}
            y2={y(v)}
            className="spark-grid"
          />
        ))}
        <path d={path} className="spark-line" fill="none" />
        {points.map((p, i) => (
          <circle key={i} cx={x(i)} cy={y(p.score)} r={i === points.length - 1 ? 4 : 2.5} className={`spark-dot ${scoreClass(p.score)}`}>
            <title>{`${formatTime(p.at)} — score ${p.score}${p.ready ? ", ready" : ""}`}</title>
          </circle>
        ))}
      </svg>
      <figcaption className="muted">
        {formatTime(first.at)} → {formatTime(last.at)} · {points.length}{" "}
        evaluations · latest <strong>{last.score}</strong>
      </figcaption>
    </figure>
  );
}
