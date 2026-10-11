#!/usr/bin/env bash
# Every section of a doc that shows `cosign verify` of the image or the chart
# states the minimum cosign next to it (#377): the signatures are cosign 3
# bundles, and cosign 2.6.2 and older exit 10 with "no signatures found" for
# the same command. `cosign verify-blob` (checksums.txt) works on older
# versions and has no such need. A section is the text under one Markdown
# heading; the files are docs/operations/install.md and deploy/chart/README.md
# unless given as arguments. Part of `make claims-check`. Offline.
set -euo pipefail
cd "$(dirname "$0")/.."

want='cosign 3.x, or 2.6.3 or later'
files=("$@")
[ ${#files[@]} -gt 0 ] || files=(docs/operations/install.md deploy/chart/README.md)

bad=0
checked=0
for f in "${files[@]}"; do
  [ -f "$f" ] || { echo "docs-cosign-minimum: no $f" >&2; exit 1; }
  # One line per section: the heading line number, then whether it has a
  # `cosign verify ` (a space after: not verify-blob) and the minimum.
  while IFS=$'\t' read -r line verify minimum; do
    [ "$verify" = 1 ] || continue
    checked=$((checked + 1))
    if [ "$minimum" != 1 ]; then
      echo "docs-cosign-minimum: $f:$line: the section shows 'cosign verify' without '$want'" >&2
      bad=$((bad + 1))
    fi
  done < <(awk -v want="$want" '
    function flush() { if (h) printf "%d\t%d\t%d\n", h, v, m }
    /^```/ { fence = !fence }
    !fence && /^#+ / { flush(); h = NR; v = 0; m = 0; next }
    !h { h = 1 }   # text before the first heading is a section too
    /cosign verify[ \\]/ { v = 1 }
    index($0, want) { m = 1 }
    # a line break inside the phrase (prose wrap): join with the next line
    { tail = (prev " " $0); if (index(tail, want)) m = 1; prev = $0 }
    END { flush() }
  ' "$f")
done
if [ "$bad" -ne 0 ]; then exit 1; fi
[ "$checked" -gt 0 ] || { echo "docs-cosign-minimum: no 'cosign verify' section in ${files[*]}" >&2; exit 1; }
echo "docs-cosign-minimum: $checked section(s) with 'cosign verify' state the minimum"
