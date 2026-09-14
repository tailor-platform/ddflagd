# OFREP specification fixture

`openapi.yaml` is a verbatim copy of `service/openapi.yaml` from
[open-feature/protocol](https://github.com/open-feature/protocol), taken at
commit `9183895d888b55ddfece43412e023ef62e0655b3` (OFREP specification version
0.3.0). Nothing in it has been changed.

The contract test in `internal/ofrep/contract_test.go` checks ddflagd's
responses against this document, so it is kept here rather than fetched at test
time. A pinned copy is what makes the check reproducible, and a fetch would make
the suite depend on the network and on whatever the upstream default branch
happens to hold that day.

The file is licensed under the Apache License 2.0, unlike the rest of this
repository, which is MIT. `LICENSE` in this directory is the upstream license
text, included as Apache-2.0 section 4(a) requires. Upstream ships no NOTICE
file, so there is none to reproduce here.

To update it, replace `openapi.yaml` with the upstream file and update the
commit and version above.
