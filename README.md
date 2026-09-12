# Jelto contracts

Shared analytics protocols, executable schemas, signature catalogs and SDK
conformance tools for Jelto. This standalone module owns the runner, mock server,
reference host and wire validator. It builds, tests and packages without the
backend or any SDK checkout. Version 0.1.0 is prepared for its first public release.

## Layout

- `contracts.go`: embedded protocol documents for Go consumers.
- `spec/wire-v1.md` and `spec/wire-v1.schema.json`: normative wire contract.
- `spec/sdk-conformance.md` and `spec/conformance/`: SDK behavior and its harness.
- `spec/wirecheck/`: schema validation and captured SDK traffic checks.
- `spec/signatures/`: versioned bot, source and referrer-spam catalogs.
- `spec/contracts/manifest.json`: package identity, version and export allowlist.
- `package.py`, `install.py`, `tests/`: deterministic packaging and verified installation.

Backend domain specs, OpenAPI definitions, database migrations, billing/query
logic and domain fixture expectations stay with the backend. References to
`spec/...` inside these documents are stable contract identifiers. SDK source
examples refer to the corresponding SDK repository.

## Local commands

Requires Go 1.26.8 or later (see `go.mod`), Python 3.11 or later for the complete
release tooling, and Make. Run from this directory:

```sh
make test                 # uncached Go tests and package/installer checks
make build                # runner, mockd, refhost and dogfood tools in bin/
make conformance-twice    # full reference behavior checks, twice
make package              # dist/jelto-contracts-0.1.0.zip and .zip.sha256
```

Conformance reports C11 runtime budgets and C19 SDK reproducibility as separate
SDK-owned gates; a reference run does not certify those properties for an SDK.
`make signatures` explicitly refreshes the generated spam read list. Review its
changes and run the consuming backend's fixture gates before adopting new data.
Ordinary builds and tests never fetch or refresh signature data.

The local `.github/workflows/ci.yml` runs tests, builds, packaging and reference
conformance twice when this directory becomes a repository root. It uploads the
versioned archive and checksum; it does not publish a repository or release.

## Consumers

The Go module is `jelto.io/jelto/contracts`, initially version **0.1.0**. Backend
code imports its `spec/signatures` package; verification reads `WireSpec()` and
`WireSchema()` from the root package. These inputs are embedded in the module,
so consumers do not need filesystem paths into this repository.

The monorepo pins this module and uses `replace jelto.io/jelto/contracts =>
./contracts` for local development. After extraction, configure the module's
publication/import location and replace that override with the pinned release.

SDK and snippet tools use an absolute `JELTO_CONTRACTS_DIR` pointing at this root
or an extracted release. The archive preserves `spec/conformance/runner`,
`spec/wirecheck/dogfood`, the schema and version-manifest paths expected by those
tools. Install into a new directory with the checksum from the matching release:

```sh
python3 install.py /absolute/path/jelto-contracts-0.1.0.zip SHA256 /absolute/path/contracts
```

The installer validates the archive checksum, version, paths and every file's
hash before extracting. Run an SDK's `make conformance` twice with that directory.
Frontend/crawler consumers use small versioned data snapshots from the same
sources. In the monorepo, `make inputs` refreshes their pins explicitly and
`make inputs-check` detects drift.

Published versions are immutable. Change the manifest version and coordinate
consumer version/checksum updates before publishing changed contracts. The root
monorepo `make package-contracts` remains a convenience command and writes its
archive under the monorepo's `dist/contracts/` directory.

## Releases

The component-owned release workflow publishes verified ZIP/checksum assets
after tag validation and standalone CI. Follow [RELEASING.md](https://github.com/usejelto/contracts/blob/main/RELEASING.md)
to configure the repository and enable publishing; no account setup is implied.

## Community and license

Questions, bug reports and documentation improvements are welcome. See
[Support](https://github.com/usejelto/contracts/blob/main/SUPPORT.md),
[Contributing](https://github.com/usejelto/contracts/blob/main/CONTRIBUTING.md),
[Code of Conduct](https://github.com/usejelto/contracts/blob/main/CODE_OF_CONDUCT.md), and
[Security policy](https://github.com/usejelto/contracts/blob/main/SECURITY.md).
Until the public repository is available, these files are also included in the
source root; contact [taha@jelto.io](mailto:taha@jelto.io) for help.

Jelto-owned software and associated documentation use the [MIT license](LICENSE).
Third-party materials retain their own terms, including the Contributor Covenant
attribution. Jelto names, logos, mascots and original brand artwork are excluded
from the software license; no trademark rights are granted.
