# Contributing to Jelto contracts

Bug reports, documentation corrections, examples and focused code changes are
welcome. Participants follow our [Code of Conduct](CODE_OF_CONDUCT.md).

## Start a contribution

Search [existing issues](https://github.com/usejelto/contracts/issues) before opening one. Use a bug report for
reproducible failures, a feature request to explain a use case, or a blank issue
for questions and documentation feedback. Discuss substantial features and
breaking changes before implementing them. Small fixes can go straight to a PR.

Send security reports privately as described in [SECURITY.md](SECURITY.md).
For installation and account help, see [SUPPORT.md](SUPPORT.md).

## Local development

The Go release `go.mod` names (`make` selects it through `GOTOOLCHAIN`, so Go
downloads it once if a different one is installed), Python 3.11 or later for
release tooling, and Make.

Fork and clone this repository, create a branch from `main`, and run commands
from this component's root. The standalone checkout contains its build inputs;
you do not need the private Jelto backend.

```sh
make hooks   # once per clone
make check   # the whole gate; CI runs the same command
```

`make check` runs `lint`, `test`, `build`, `package`, `security` and
`conformance-twice`.
Run the smallest relevant target while iterating, then `make check`.
Documentation-only changes need link and example review; SDK behavior or wire
changes require the appropriate tests and two consecutive conformance passes
before certification. Package changes must also pass packaging checks.

Wire changes belong in `spec/wire-v1.md` and its schema; conformance behavior
belongs in `spec/sdk-conformance.md` and `spec/conformance/`. Include the governing
section in the PR. Derive expected results from the normative contract rather
than copying observed output. Coordinate version and checksum changes with
consumers. Published archives are immutable; `make signatures` is an explicit
catalog refresh and is not part of ordinary tests. Backend metrics, billing,
database changes and backend-only fixtures belong to the service repository.

## Checks and exceptions

The security targets run separate programs, not project dependencies.
`tools/quality/tools.json` pins each one's exact version and SHA-256; the first
run downloads and verifies it into `~/.cache/jelto-tools` (`JELTO_TOOLS_DIR`
overrides the location) and `make tools` installs them all ahead of time. The
targets themselves live in `tools/quality/quality.mk`, which every other Jelto
repository consumes as a checksummed snapshot, so change them here.

| Target | Tool | What it checks |
|---|---|---|
| `make lint` | golangci-lint | `.golangci.yml`: the standard linters and gofmt, against `.golangci-baseline.json` |
| `make secrets` | gitleaks | the whole Git history for credentials |
| `make workflow-audit` | actionlint, zizmor | workflow syntax, script injection, permissions and pinning |
| `make osv` | OSV-Scanner | `go.sum` against the OSV database, downloaded and matched locally |
| `make govulncheck` | govulncheck | Go code paths that reach a known vulnerability |

`make hooks` enables two Git hooks: `commit-msg` rejects a subject that is not a
Conventional Commit, and `pre-commit` runs gitleaks over the staged changes. Do
not bypass them with `--no-verify`.

When a check reports something, fix a real problem in the same change. A
deliberate exception, or a finding the tool got wrong, is marked at the one
place it applies, with the reason beside it:

- gitleaks: a `.gitleaks.toml` allowlist entry naming the one literal and why;
- zizmor: `# zizmor: ignore[<audit>]` on the line, with the reason above it;
- OSV-Scanner: an `[[IgnoredVulns]]` entry in `osv-scanner.toml` with a
  `reason` and an `ignoreUntil` date.

- golangci-lint: `//nolint:<linter> // <reason>` on the line. The baseline
  holds findings that predate the linter; a fix removes its entry (`make
  lint-baseline`, then review the diff), and nothing is added to it to make a
  check pass.

Never disable a rule, a file or a directory wholesale, and never relax a
threshold to make a check pass.

## Review and acceptance

Keep changes focused and match the surrounding style.
Format Go changes with `gofmt`, use standard Go names and wrap errors with context.
Add regression coverage for behavior changes. Do not update expected values just to match a
failing implementation, and do not edit generated or vendored inputs by hand.

Use a scoped Conventional Commit title, such as `fix(contracts): clarify retry handling` or `docs(contracts): explain local installation`. Describe the problem,
the resulting behavior, relevant issue/specification, compatibility impact and
exact verification commands with results. Include screenshots for visible UI
changes. State when a check was not run and why. A separate specification change
should precede its implementation; fixture expectation changes need a separate
commit explaining their derivation.

Use synthetic data in examples and reports. Never include credentials, customer
payloads or IP addresses in public logs. Contributions must preserve Jelto's
privacy boundaries: no IP storage or logging, and no row-level identifier that
joins a website visitor to an app install.

Taha Bozdemir reviews scope, correctness, tests and compatibility, and decides
whether to merge. Review is best effort with no guaranteed turnaround. Maintainers
may request changes or decline work outside the component's purpose; explain
tradeoffs in the issue or PR so future contributors can follow the decision.

## Licensing and releases

Submit only work you have the right to contribute, under this repository's
[MIT license](LICENSE). Preserve third-party notices and the Contributor Covenant
attribution. Jelto names, logos, mascots and original brand artwork are excluded
from the software license; no trademark rights are granted. No separate CLA or
DCO sign-off is required.

Maintainers publish releases using [RELEASING.md](RELEASING.md). Local packaging
does not publish, and a successful test run is not a release announcement.
