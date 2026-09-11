# Standalone releases

This guide applies after importing a component as its own repository. Keep the
component's contents at the repository root, including dotfiles and vendor
snapshots. Import contracts first. Do not copy the backend's Git history or
private files. Creating repositories and synchronizing source are operator tasks.

The release workflow runs on version tags and can be dispatched on the same tag.
Until RELEASE_PUBLISH_ENABLED=true, it only tests and prepares artifacts.
Manual dispatch defaults publish to false even after publication is enabled.
Each release reruns component CI and conformance twice; Tauri also runs its
three-platform native/example/C19 checks and verifies its shipping Cargo package.

## Configure before the first tag

Use release.py in the contracts repository. In an SDK repository use
vendor/release-tools/release.py:

~~~sh
JELTO_RELEASE_TOOL=release.py
# In an SDK repository instead:
# JELTO_RELEASE_TOOL=vendor/release-tools/release.py
python3 "$JELTO_RELEASE_TOOL" configure --repository ACTUAL_OWNER/ACTUAL_REPOSITORY
~~~

This writes release.json and any npm/Cargo/NuGet repository metadata. Review and
commit the changes before tagging. Repository/package ownership and public access
must use the real account names; the backend repository is not the SDK repository.
Existing package IDs are @jelto/analytics, @jelto/crawler, @jelto/electron,
@jelto/tauri, NuGet Jelto, and tauri-plugin-jelto. Resolve unavailable names before
release; these workflows do not rename packages. Preserve existing license notices.

Electron, Swift, .NET and Tauri also need a published contracts archive:

~~~sh
python3 "$JELTO_RELEASE_TOOL" configure \
  --repository ACTUAL_OWNER/ACTUAL_REPOSITORY \
  --contracts-url https://github.com/ACTUAL_OWNER/jelto-contracts/releases/download/v0.1.0/jelto-contracts-0.1.0.zip \
  --contracts-sha256 ACTUAL_64_CHARACTER_SHA256
~~~

For a later contracts version, also pass --contracts-version VERSION.
Use the checksum from the contracts release, not an unverified download. The
installer checks the archive and every file. Configure public HTTPS assets;
private asset authentication is not implemented. Missing pins fail preparation.
The verified archive supplies its Go version. Ordinary builds never refresh pins.

Keep each manifest and lockfile version synchronized. Electron/.NET reported
client-version constants must match their manifests. Swift's Wire.sdkClientVersion
is its version source. Tauri's Cargo and npm versions must match, including its
example manifest/lockfiles; its wire version comes from Cargo. Initial Tauri is
1.0.0; this does not change the example application's own version.

Run normal component CI on main before tagging. SwiftPM resolves a public version
tag immediately; a GitHub Release cannot gate tag visibility.

## First publication

1. Leave RELEASE_PUBLISH_ENABLED unset. Push an unused matching version tag, such
   as v1.0.0 or v1.0.0-rc.1, from main history. Build metadata and leading zeroes
   are rejected. All versions remain independently selected.
2. Wait for the release workflow's complete platform matrix to pass. Download
   release-Linux, or release-macOS for Swift, from that exact run. The directory
   contains release.json, CHECKSUMS and the verified distribution packages.
   Check the repository, tag and commit in release.json. Verify CHECKSUMS with
   sha256sum -c CHECKSUMS (or shasum -a 256 -c CHECKSUMS on macOS).
3. For npm, log in locally with npm login and publish that exact tarball:
   npm publish ./jelto-COMPONENT-VERSION.tgz --ignore-scripts --access public.
   Add --tag next for a prerelease. Never publish a placeholder or expend a
   stable version merely to test authentication.
4. For NuGet, upload the verified Jelto.VERSION.nupkg through NuGet.org's Upload
   package page using the intended owner account.
5. For Tauri, publish Rust first. In a clean standalone checkout of the same tag,
   run cargo publish --dry-run --locked and cargo package --locked. Compare
   target/package/*.crate with the downloaded release crate, then cargo login
   and cargo publish --locked. The explicit package command retains the archive
   that Cargo's publish dry-run otherwise cleans up.
   Wait until crates.io serves that version before publishing the npm tarball.
   Do not use --no-verify. Bootstrap requires the pinned Rust toolchain and native
   prerequisites from CI.
6. Configure trusted publishing below, enable RELEASE_PUBLISH_ENABLED, and
   dispatch the same tag with publish=true. It checks the existing registry
   bytes, runs registry consumer tests and finishes the GitHub Release.

Contracts and Swift need no registry login. Enable RELEASE_PUBLISH_ENABLED and
dispatch publish=true after preparation to create their first GitHub Release.
Contracts publishes its versioned ZIP and .zip.sha256, which SDK pins reference.
Swift publishes a source ZIP; customers use its SwiftPM version tag, with
Package.swift at the root (macOS 12+, Swift 6).

## Trusted publishing

Create a GitHub environment named release. Configure each registry's trusted
publisher with the actual owner, repository, workflow filename release.yml and
environment release. Permit direct publishing. No reviewer is required by these
workflows; applying one in environment settings makes releases wait.

- npm: configure each package's Trusted Publisher for this repository. The
  workflow uses Node 24, pinned npm 11.14.0, public access and provenance.
  Public repositories are required for provenance. Do not set NPM_TOKEN or
  NODE_AUTH_TOKEN. See https://docs.npmjs.com/trusted-publishers/.
- NuGet: configure the trusted policy and repository variable NUGET_USER with
  the NuGet profile name, not an email. NuGet/login@v1 exchanges OIDC for a
  temporary publishing key. See
  https://learn.microsoft.com/en-us/nuget/nuget-org/trusted-publishing.
- crates.io: configure the published crate's Trusted Publishing policy.
  rust-lang/crates-io-auth-action@v1 obtains and revokes a temporary token.
  Recurring releases do not use CRATES_IO_TOKEN. See
  https://crates.io/docs/trusted-publishing.
- GitHub Releases: GITHUB_TOKEN receives contents: write in the publish job.
  PRs and verification jobs receive no publishing permissions.

Set the repository Actions variable RELEASE_PUBLISH_ENABLED to the literal true.
Future tag pushes now verify and publish automatically. Stable npm releases use
latest; prereleases use next. Select monotonically increasing stable versions
to avoid moving latest backward.

## Reruns and recovery

Rerun or dispatch the same tag; never move it to fix a failed release. Publication
checks registry contents before skipping an existing version. Only a 404 counts
as absent; authorization and network errors fail. NuGet comparison excludes the
repository-added signature, while npm and Cargo comparisons require exact bytes.
Conflicting bytes require investigation and a new version, not overwriting.

Tauri registries are not atomic. If Rust succeeds and npm fails, fix authentication
or registry availability and rerun the same tag. The matching Rust version is
retained; npm finishes next. A GitHub Release is completed only after both
packages pass registry content and installation checks.

GitHub Releases are assembled as drafts. Matching assets can be reused on rerun;
different bytes are never replaced. CHECKSUMS and release.json record what was
verified. CI artifacts expire after 14 days; completed GitHub Release assets are
the durable downloads. Fix any source problem in a new tag, and retain published
versions for existing customers.

These files configure workflows, not accounts. Record local checks, actual
standalone Actions runs, first publication, and registry installation separately.
No publication or trusted policy is implied by committing these files.
