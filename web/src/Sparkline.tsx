import type { ScorePoint } from "./types";
import { formatTime, scoreClass } from "./ui";

// Sparkline: hand-rolled SVG score trend (0–100). x is time: evaluations
// arrive irregularly (ingest skips unchanged inventories), so points are
// placed by their timestamp between the first one and now. The line is a
// step — a score holds until the next evaluation replaces it — and runs on
// to now, so a cluster that stopped reporting shows a long flat tail.
export function Sparkline({
  points,
  now = Date.now(),
}: {
  points: ScorePoint[];
  now?: number;
}) {
  if (points.length === 0) return null;

  const w = 560;
  const h = 96;
  const pad = 8;
  const times = points.map((p) => Date.parse(p.at));
  const start = times[0]!;
  const end = Math.max(times[times.length - 1]!, now);
  const span = end - start;
  const x = (t: number) =>
    span > 0 ? pad + ((t - start) * (w - 2 * pad)) / span : w / 2;
  const y = (score: number) => pad + ((100 - score) * (h - 2 * pad)) / 100;

  const path =
    points
      .map((p, i) => {
        const px = x(times[i]!).toFixed(1);
        const py = y(p.score).toFixed(1);
        return i === 0 ? `M${px},${py}` : `H${px} V${py}`;
      })
      .join(" ") + ` H${x(end).toFixed(1)}`;
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
          <circle
            key={i}
            cx={x(times[i]!)}
            cy={y(p.score)}
            r={i === points.length - 1 ? 4 : 2.5}
            className={`spark-dot ${scoreClass(p.score)}`}
          >
            <title>{`${formatTime(p.at)} — score ${p.score}${p.ready ? ", ready" : ""}`}</title>
          </circle>
        ))}
      </svg>
      <figcaption className="muted">
        {formatTime(first.at)} → now · {points.length} evaluations · latest{" "}
        <strong>{last.score}</strong> ({formatTime(last.at)})
      </figcaption>
    </figure>
  );
}
