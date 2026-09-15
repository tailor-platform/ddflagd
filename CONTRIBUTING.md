# Contributing guide

ddflagd is maintained by the `maintainers-ddflagd` team at Tailor, and contributions are welcome from anyone. No Tailor account or affiliation is expected.

## Issues

Bugs and feature requests belong on [GitHub Issues](https://github.com/tailor-platform/ddflagd/issues).

A bug report is usually enough to act on with the version (`ddflagd --version`, or the image tag), how it is deployed (sidecar or its own Deployment), and the body of `GET /debug/status` on the operational listener. That endpoint redacts the shared secret, so it is safe to paste as it comes. Nothing else is, so keep Datadog API keys, application keys and the value of `DDFLAGD_API_KEY` out of the issue.

## Pull requests

Open an issue first when the change alters behaviour or adds configuration, so that the shape can be settled before the code is written. A bug fix, a missing test or a documentation correction can arrive as a pull request directly.

Before opening one:

```
make test   # unit and contract tests
make lint   # golangci-lint, govulncheck, gostyle
```

`make e2e` and `make rust-test` are the slower suites and need a Docker daemon and cargo respectively. CI runs both on every pull request, so leaving them to CI is fine when the toolchain is not at hand. The conformance fixtures live in a submodule, so clone with `--recurse-submodules`, or run `git submodule update --init` once. `README.md` describes what each suite covers and why the submodule is pinned where it is.

What review looks at:

- The commit subject follows [Conventional Commits](https://www.conventionalcommits.org/) (`feat:`, `fix:`, `docs:`, `chore:`, `test:`, `ci:`), and the body says why the change is being made. The diff already says what it does.
- A behaviour change carries a test. The e2e suite runs against Datadog's fake Agent and the conformance suite against Datadog's own fixtures, so a change to the Remote Configuration path or the OFREP mapping can be covered without a Datadog account.
- `CHANGELOG.md`, the version, and `CREDITS` are not edited by hand. [tagpr](https://github.com/Songmu/tagpr) maintains them, and `make prerelease_for_tagpr` regenerates `CREDITS` when dependencies change.

Reviews route to `maintainers-ddflagd` through `.github/CODEOWNERS`. `main` requires one approving review from a code owner, and pull requests land as a squash merge. Release notes are assembled from pull request labels (`.github/release.yml`), which maintainers apply.

## License

By contributing you agree that your contribution is licensed under the MIT License in [LICENSE](LICENSE). There is no CLA to sign.
