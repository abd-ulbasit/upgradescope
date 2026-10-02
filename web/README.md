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

## Read token

With `serve --read-token`, set the token in the dashboard header. It is
sent as a bearer header and kept in `sessionStorage`, so it is gone when
the tab closes. Ticking **Remember on this device** keeps it in
`localStorage` instead; saving with it unticked, or clearing the token,
removes it from there. A token stored in `localStorage` by an older
dashboard is still read.

## Serving under a path prefix

Asset and API URLs are relative, so the dashboard also works under a path
prefix, for example `https://ops.example.com/upgradescope/`, behind a
reverse proxy that strips the prefix before forwarding to `serve`. Open it
with the trailing slash: without it the browser resolves the relative
asset URLs against the parent path and they fail to load.
