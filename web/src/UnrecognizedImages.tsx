// UnrecognizedImages lists the image repositories no add-on registry entry
// matches (report.unrecognizedImages): a gap in add-on detection, not a
// finding, shown so a missed add-on is visible. The report carries at most
// 200, sorted; omitted counts the rest. The list starts folded.
export function UnrecognizedImages({
  images,
  omitted,
}: {
  images?: string[];
  omitted?: number;
}) {
  const listed = images ?? [];
  const total = listed.length + (omitted ?? 0);
  if (total === 0) return null;
  return (
    <section className="card unrecognized-images" aria-labelledby="unrecognized-images">
      <h2 id="unrecognized-images">Unrecognized images ({total})</h2>
      <p className="muted">
        No add-on registry entry matches these image repositories, so their lifecycle
        was not checked.
      </p>
      <details>
        <summary>Show repositories</summary>
        <ul>
          {listed.map((repo) => (
            <li key={repo}>
              <code>{repo}</code>
            </li>
          ))}
        </ul>
        {omitted ? <p className="muted">…and {omitted} more, not listed.</p> : null}
      </details>
    </section>
  );
}
