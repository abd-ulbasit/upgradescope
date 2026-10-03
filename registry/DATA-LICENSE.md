# Registry data license and attribution

The add-on registry (`registry/data/*.yaml`) is a public dataset of
Kubernetes add-on end-of-life status and Kubernetes compatibility, with
citations. This file states the terms on which you may reuse it.

## License

The registry dataset is licensed under the
**[Apache License, Version 2.0](../LICENSE)**, the same license as the rest
of the repository. You may copy, modify and redistribute the entries,
including in commercial products, provided you keep the license and the
attributions below.

Copyright 2026 Abdul Basit and the upgradescope contributors.

## Sources and attribution

Every entry cites its sources in `support.citations` and
`compat[].citations`. The registry stores facts (statuses, dates, version
ranges) and links to the pages that state them. It does not copy those pages.

### endoflife.date

Entries that declare an `endoflife_product` slug (11 of the 20 on
2026-10-03; `grep -l '^endoflife_product:' registry/data/*.yaml` counts them)
do not hand-maintain their `cycles`. The `cycles:` block, each release line
with its end-of-life date and the Kubernetes range it supports, is synced by
[`tools/eol-sync`](../tools/eol-sync) from the
[endoflife.date](https://endoflife.date) API
(`https://endoflife.date/api/<slug>.json`), one cycle at a time. eol-sync
never writes `support.status` or `support.eol_date`: a person records those.

The managed-provider calendars in `registry/data/providers` follow the same
rule. The `versions` of `eks.yaml` (`amazon-eks`) and `aks.yaml`
(`azure-kubernetes-service`) are synced from endoflife.date, and are the
same kind of dated fact. `gke.yaml` is read by hand from Google's release
schedule, and the prices are read by hand from the providers' pricing pages;
each cites the page and, for a price, the day it was read. A price is a list
price, not a quote.

endoflife.date is published under the
[MIT License](https://github.com/endoflife-date/endoflife.date/blob/master/LICENSE),
Copyright 2020 endoflife.date contributors. We confirmed this on 2026-10-02
from the `LICENSE` file of
[endoflife-date/endoflife.date](https://github.com/endoflife-date/endoflife.date).
The MIT License requires the copyright notice and permission notice to travel
with copies. Both are reproduced in full in the repository's
[`NOTICE`](../NOTICE) file.

When you redistribute the synced cycles, keep this attribution:

> End-of-life data for synced cycles from [endoflife.date](https://endoflife.date),
> Copyright 2020 endoflife.date contributors, MIT License.

The remaining entries are hand-curated from the upstream project pages they
cite, such as release and support-policy pages and compatibility matrices.

## Reusing the dataset

- The entries are plain YAML, and the schema is documented in
  [`CONTRIBUTING.md`](CONTRIBUTING.md#schema-schema_version-2).
- Go programs can import `github.com/abd-ulbasit/upgradescope/registry` and
  call `registry.Load()`, which returns the validated, embedded entries.
- No warranty: dates and statuses are as accurate as the cited sources and
  the last sync. Check the citations before you make an upgrade decision.
