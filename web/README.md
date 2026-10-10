# upgradescope dashboard

The single-page app that `upgradescope serve` embeds at `/`: the fleet
score matrix, a per-team rollup, the per-cluster drill-down (any target as
a what-if, CSV/HTML auditor exports, score trend) and the add-on registry.
React only; charts are hand-rolled SVG.

## Develop

Needs Node 22.12 or newer (Vitest 5 does not support Node 20).

```sh
npm ci
npm test         # vitest: api unit tests + component tests in happy-dom
npm run dev      # Vite on :5173, proxying /api to `upgradescope serve` on :8080
npm run build    # tsc --noEmit, then vite build into dist/
```

After any change here run `make web` from the repository root and commit
`internal/server/webdist`: the binary embeds that copy, and CI fails when
it differs from a fresh build.

## Operating the dashboard

Screens and hash routes (including the Fleet and Cluster query parameters),
token entry and storage, the team-scoped banner, and serving under a path
prefix behind a proxy are documented for operators in
[docs/guides/dashboard.md](../docs/guides/dashboard.md). In short: open the
trailing-slash URL under a prefix, and the proxy must strip the prefix; asset
and API URLs are relative on purpose.

## Layout

- `src/views/`: one file per screen (`Fleet`, `Cluster`, `Teams`, `Registry`).
- `src/fleet.ts`: the pure functions behind the Fleet toolbar and summary
  (buckets, filters, sorts); unit-tested through the views.
- `src/hooks.ts`: `useAsync` (stale-while-revalidate: a refetch keeps the data
  on screen; a hidden tab refetches on return when its data is over 60 s old),
  and the hash-route helpers `useRouteQuery` / `setRouteQuery`, which views use
  to keep their filters in the URL.
