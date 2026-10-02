# Vendored GitLab Code Quality schema

`gitlab-codeclimate.schema.json` is GitLab's own schema for one Code Quality
report entry ("degradation"), copied verbatim from the GitLab source:

- Source: <https://gitlab.com/gitlab-org/gitlab/-/blob/master/app/validators/json_schemas/codeclimate.json>
  (mirror: <https://github.com/gitlabhq/gitlabhq/blob/master/app/validators/json_schemas/codeclimate.json>)
- Revision: last changed in gitlabhq/gitlabhq commit
  `f986ce9ffa56e25d0a3010c78d9481664742d766` (2021-03-23); fetched 2026-10-02.
- How GitLab uses it: `Gitlab::Ci::Reports::CodequalityReports#valid_degradation?`
  validates every entry against it, and the parser
  (`Gitlab::Ci::Parsers::Codequality::CodeClimate`) stops reading the report
  at the first entry that fails. Entries are keyed by `fingerprint`, so a
  repeated fingerprint replaces the earlier entry.

The schema is looser than GitLab's documentation
(<https://docs.gitlab.com/ci/testing/code_quality/#code-quality-report-format>),
which also requires `check_name`, a repository-relative `location.path`
without a `./` prefix, a line, and a severity of `info`, `minor`, `major`,
`critical` or `blocker`; `codequalitytest.AssertGitLabAcceptable` checks
those too.

License: the file lies outside GitLab's `doc/`, `ee/` and `jh/` directories,
so it is under the MIT Expat license of the GitLab repository:

```
Copyright (c) 2011-present GitLab Inc.

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.
```

It is test data only: nothing in the shipped binary reads it.
