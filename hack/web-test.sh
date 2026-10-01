#!/usr/bin/env bash
# The dashboard gate (make web-test; CI's web job, on every supported Node
# line):
#   - npm ci (the lockfile, exactly);
#   - unit tests (vitest);
#   - npm run build = typecheck (tsc --noEmit) + vite build into web/dist;
#   - no high or critical advisory in production dependencies (dev-only
#     tooling never reaches the embedded bundle);
#   - the committed go:embed copy (internal/server/webdist) is byte-identical
#     to the fresh build: `go install` users get that copy, so a stale one
#     ships an old dashboard. Fix with `make web` and commit the result.
# Never writes outside web/ (node_modules, dist), so it is safe on a dirty
# tree. Needs Node and npm; npm ci and npm audit need the registry.
set -euo pipefail
cd "$(dirname "$0")/.."

echo "== node $(node --version), npm $(npm --version)"
(
  cd web
  npm ci --no-fund --no-audit
  npm test
  npm run build
  npm audit --omit=dev --audit-level=high
)

echo "== internal/server/webdist matches the fresh build"
if ! diff -r -x .gitkeep web/dist internal/server/webdist; then
  echo "::error::internal/server/webdist is stale — run 'make web' and commit the result" >&2
  exit 1
fi
echo "web-test: OK"
