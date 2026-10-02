# Vendored Jenkins JUnit schema

`junit-10.xsd` is the JUnit schema of the Jenkins xUnit plugin, copied
verbatim:

- Source: <https://github.com/jenkinsci/xunit-plugin/blob/master/src/main/resources/org/jenkinsci/plugins/xunit/types/model/xsd/junit-10.xsd>
- Revision: last changed in jenkinsci/xunit-plugin commit
  `ae25da5089d4f94ac6c4669bf736e4d416cc4665` (2018-12-15); fetched 2026-10-02
  (SHA-256 `a1a816f58d1bf95ebabf371994df0b9246dee66ea9572fbec4f9296f1b2c0ff6`).
- How it is used: the xUnit plugin validates JUnit input against it. Jenkins'
  own junit step, GitLab and Azure Pipelines' PublishTestResults read the
  same dialect without validating, so a report this schema accepts is one
  they all read. `junittest.Validate` checks every element, attribute,
  required attribute, time value and text content against the declarations
  in this file; it does not check child order or occurrence counts, which
  `junittest.Read` checks for the parts that matter (one outcome per test
  case, counts that match).

Cross-check by hand with libxml2:

```
xmllint --noout --schema internal/junit/junittest/testdata/junit-10.xsd internal/junit/testdata/*.xml
```

License: MIT, as stated in the file's own header comment (Copyright (c) 2014,
Gregory Boissinot), which the copy keeps.

It is test data only: nothing in the shipped binary reads it.
