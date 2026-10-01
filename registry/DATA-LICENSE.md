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

Entries that declare an `endoflife_product` slug (10 of the current 18) do
not hand-maintain `support.status` and `support.eol_date`. Those two fields
are synced by [`tools/eol-sync`](../tools/eol-sync) from the
[endoflife.date](https://endoflife.date) API
(`https://endoflife.date/api/<slug>.json`).

endoflife.date is published under the
[MIT License](https://github.com/endoflife-date/endoflife.date/blob/master/LICENSE),
Copyright 2020 endoflife.date contributors. We confirmed this on 2026-10-02
from the `LICENSE` file of
[endoflife-date/endoflife.date](https://github.com/endoflife-date/endoflife.date).
The MIT License requires the copyright notice and permission notice to travel
with copies. Both are reproduced in full in the repository's
[`NOTICE`](../NOTICE) file.

When you redistribute the synced fields, keep this attribution:

> End-of-life data for synced entries from [endoflife.date](https://endoflife.date),
> Copyright 2020 endoflife.date contributors, MIT License.

The remaining entries are hand-curated from the upstream project pages they
cite, such as release and support-policy pages and compatibility matrices.

## Reusing the dataset

- The entries are plain YAML, and the schema is documented in
  [`CONTRIBUTING.md`](CONTRIBUTING.md#schema-schema_version-1).
- Go programs can import `github.com/abd-ulbasit/upgradescope/registry` and
  call `registry.Load()`, which returns the validated, embedded entries.
- No warranty: dates and statuses are as accurate as the cited sources and
  the last sync. Check the citations before you make an upgrade decision.
