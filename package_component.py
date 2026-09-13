#!/usr/bin/env python3
"""Build and publish a component package: the archive and manifest the backend installs.

A component package is `jelto-<component>-<version>.tar.gz` beside
`<component>.json`. The manifest records the component, version, producer
commit and repository, the archive SHA-256 and a per-file SHA-256, so a
consumer verifies the package without reaching the producer. The producer's
checked-in `component-package.json` names the component and the paths the
archive holds; the version comes from its `package.json`.

  build     write the pair into an output directory
  publish   build, then push the pair to the organization registry as one OCI
            artifact tagged <version> and sha-<commit>; an existing version is
            reused only when it is byte-identical, a collision fails

The archive is deterministic: identical inputs give identical bytes on any
machine, so a consumer's pin and a rebuild can be compared. This file is
consumed by each producer as a versioned component-local input and by the
backend's workspace packaging, so the two cannot drift.
"""
import argparse
import gzip
import hashlib
import json
import os
import re
import shutil
import subprocess
import sys
import tarfile
import tempfile
from pathlib import Path

CONFIG = 'component-package.json'
ARTIFACT_TYPE = 'application/vnd.jelto.component-package.v1+json'
IMAGE_SPECS = ['v1.1', 'v1.0']
COMPONENT = re.compile(r'[a-z][a-z0-9-]*')
SEMVER = re.compile(r'(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)'
                    r'(?:-([0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*))?')
COMMIT = re.compile(r'[0-9a-f]{40}')
DIGEST = re.compile(r'sha256:[0-9a-f]{64}')


class PackageError(Exception):
    """The package cannot be built or published as declared; nothing was pushed."""


def sha256(path):
    h = hashlib.sha256()
    with open(path, 'rb') as handle:
        for block in iter(lambda: handle.read(1 << 20), b''):
            h.update(block)
    return h.hexdigest()


def load_config(root):
    path = root / CONFIG
    if not path.is_file():
        raise PackageError(f'{CONFIG} is missing in {root}')
    try:
        config = json.loads(path.read_text())
    except ValueError as error:
        raise PackageError(f'{CONFIG}: not valid JSON ({error})')
    if not isinstance(config, dict):
        raise PackageError(f'{CONFIG}: expected an object')
    unknown = sorted(set(config) - {'component', 'include'})
    if unknown:
        raise PackageError(f'{CONFIG}: unknown key(s) {", ".join(unknown)}')
    component, include = config.get('component'), config.get('include')
    if not isinstance(component, str) or not COMPONENT.fullmatch(component):
        raise PackageError(f'{CONFIG}: component must be a lowercase name, not {component!r}')
    if not isinstance(include, list) or not include \
            or not all(isinstance(entry, str) and entry for entry in include):
        raise PackageError(f'{CONFIG}: include must be a non-empty list of paths')
    return component, include


def read_version(root):
    path = root / 'package.json'
    if not path.is_file():
        raise PackageError(f'package.json is missing in {root}; the version comes from it')
    version = json.loads(path.read_text()).get('version')
    match = SEMVER.fullmatch(version) if isinstance(version, str) else None
    if not match or (match[4] and any(re.fullmatch(r'0[0-9]+', part) for part in match[4].split('.'))):
        raise PackageError(f'package.json: version must be SemVer, not {version!r}')
    return version


def inside(root, path, shown):
    """Reject anything that resolves outside the root or passes through a symlink."""
    if not path.resolve().is_relative_to(root.resolve()):
        raise PackageError(f'{CONFIG}: {shown} leaves the component root')
    for parent in (path, *path.parents):
        if parent == root:
            break
        if parent.is_symlink():
            raise PackageError(f'{CONFIG}: {shown} passes through a symlink')


def collect(root, include):
    """Map every archive path to its file; each include entry must name something."""
    files = {}
    for entry in include:
        parts = Path(entry).parts
        if Path(entry).is_absolute() or '..' in parts or not parts:
            raise PackageError(f'{CONFIG}: include entry {entry!r} must be a relative path inside the root')
        matches = sorted(root.glob(entry)) if any(ch in entry for ch in '*?[') else [root / entry]
        found = False
        for match in matches:
            if not match.exists():
                continue
            inside(root, match, entry)
            candidates = sorted(p for p in match.rglob('*')) if match.is_dir() else [match]
            for candidate in candidates:
                if candidate.is_symlink():
                    raise PackageError(f'{CONFIG}: {candidate.relative_to(root).as_posix()} is a symlink')
                if candidate.is_file():
                    files[candidate.relative_to(root).as_posix()] = candidate
                    found = True
        if not found:
            raise PackageError(f'{CONFIG}: {entry!r} matches no file')
    return files


def write_archive(files, archive):
    # gzip stores an mtime and tar stores owner, mode and mtime, so identical
    # inputs would otherwise hash differently on every run and every machine.
    def normalise(info):
        info.mtime, info.uid, info.gid, info.mode = 0, 0, 0, 0o644
        info.uname = info.gname = ''
        return info
    with open(archive, 'wb') as raw, \
            gzip.GzipFile(filename='', mode='wb', fileobj=raw, mtime=0) as stream, \
            tarfile.open(fileobj=stream, mode='w', format=tarfile.GNU_FORMAT) as tar:
        for name in sorted(files):
            tar.add(files[name], arcname=name, recursive=False, filter=normalise)


def git_head(root):
    result = subprocess.run(['git', 'rev-parse', 'HEAD'], cwd=root, text=True, capture_output=True)
    return result.stdout.strip() if result.returncode == 0 else None


def build(root, out, repository=None, commit=None, published=False):
    component, include = load_config(root)
    version = read_version(root)
    commit = commit or git_head(root)
    if commit is not None and not COMMIT.fullmatch(commit):
        raise PackageError(f'commit must be the full 40-hex producer commit, not {commit!r}')
    files = collect(root, include)
    out.mkdir(parents=True, exist_ok=True)
    archive = out / f'jelto-{component}-{version}.tar.gz'
    write_archive(files, archive)
    manifest = {
        'component': component,
        'version': version,
        'source_commit': commit,
        'repository': repository or os.environ.get('GITHUB_REPOSITORY'),
        'archive': archive.name,
        'archive_sha256': sha256(archive),
        'files': {name: sha256(path) for name, path in sorted(files.items())},
        'published': published,
    }
    (out / f'{component}.json').write_text(json.dumps(manifest, indent=2) + '\n')
    return manifest, archive


# ---------------------------------------------------------------- registry

def oras(run, *args, cwd=None):
    if shutil.which('oras') is None:
        raise PackageError('oras is not installed')
    result = run(['oras', *args], cwd=cwd, capture_output=True, text=True)
    if result.returncode:
        detail = ((result.stderr or '') + (result.stdout or '')).strip().splitlines()
        raise PackageError(f'oras {args[0]} failed: {detail[-1] if detail else result.returncode}')
    return (result.stdout or '').strip()


def resolve(name, tag, run):
    """The digest a tag names, or None when the registry holds no such tag."""
    if shutil.which('oras') is None:
        raise PackageError('oras is not installed')
    result = run(['oras', 'resolve', f'{name}:{tag}'], cwd=None, capture_output=True, text=True)
    if result.returncode:
        text = ((result.stderr or '') + (result.stdout or '')).lower()
        if 'not found' in text or 'manifest unknown' in text:
            return None
        # Unauthorized or unreachable must never read as "absent".
        detail = text.strip().splitlines()
        raise PackageError(f'oras resolve {name}:{tag} failed: {detail[-1] if detail else result.returncode}')
    found = (result.stdout or '').strip().splitlines()
    if not found or not DIGEST.fullmatch(found[-1]):
        raise PackageError(f'oras resolve {name}:{tag} returned no digest')
    return found[-1]


def same_content(name, found, manifest, archive, run):
    """The published manifest when the artifact the registry holds is this package's content.

    The archive and every recorded field except the producer commit must match:
    a later commit that builds byte-identical content publishes nothing new,
    while changed content under the same version is a collision.
    """
    with tempfile.TemporaryDirectory(prefix='jelto-package-existing-') as temp:
        oras(run, 'pull', f'{name}@{found}', '--output', temp)
        pulled_archive = Path(temp) / archive.name
        pulled_manifest = Path(temp) / f'{manifest["component"]}.json'
        if not pulled_archive.is_file() or not pulled_manifest.is_file():
            return None
        if sha256(pulled_archive) != manifest['archive_sha256']:
            return None
        try:
            published = json.loads(pulled_manifest.read_text())
        except ValueError:
            return None
        without_commit = {k: v for k, v in published.items() if k != 'source_commit'}
        mine = {k: v for k, v in manifest.items() if k != 'source_commit'}
        return published if without_commit == mine else None


def publish(root, out, registry, repository=None, commit=None, image_spec='v1.1', run=subprocess.run):
    manifest, archive = build(root, out, repository, commit, published=True)
    component, version = manifest['component'], manifest['version']
    if not manifest['source_commit']:
        raise PackageError('publish needs the producer commit: run inside the checkout or pass --commit')
    if not manifest['repository']:
        raise PackageError('publish needs the producer repository: pass --repository or set GITHUB_REPOSITORY')
    name = f'{registry}/{component}-package'
    tags = [version, f'sha-{manifest["source_commit"]}']
    existing = {tag: resolve(name, tag, run) for tag in tags}
    digests = {found for found in existing.values() if found}
    if len(digests) > 1:
        raise PackageError(f'{name}: {version} and sha-{manifest["source_commit"][:7]} name different '
                           f'artifacts; resolve that by hand')
    if digests:
        found = digests.pop()
        published = same_content(name, found, manifest, archive, run)
        if published is None:
            raise PackageError(f'{name}:{version} already holds a different package; a published version '
                               f'is immutable, so ship changed content under a new version')
        if published['source_commit'] == manifest['source_commit']:
            # The same build from the same commit: complete its tags if one is missing.
            for tag, present in existing.items():
                if not present:
                    oras(run, 'tag', f'{name}@{found}', tag)
        # Otherwise a later commit produced identical content; the published
        # artifact, which records the commit that first built it, stays as is.
        manifest['source_commit'] = published['source_commit']
        (out / f'{component}.json').write_text(json.dumps(manifest, indent=2) + '\n')
        reused = True
    else:
        oras(run, 'push', f'{name}:{",".join(tags)}',
             '--image-spec', image_spec,
             '--artifact-type', ARTIFACT_TYPE,
             '--annotation', f'org.opencontainers.image.source=https://github.com/{manifest["repository"]}',
             '--annotation', f'org.opencontainers.image.revision={manifest["source_commit"]}',
             '--annotation', f'org.opencontainers.image.version={version}',
             f'{archive.name}:application/gzip', f'{component}.json:application/json',
             cwd=out)
        found = resolve(name, version, run)
        if found is None:
            raise PackageError(f'pushed {name}:{version} but it does not resolve')
        reused = False
    return {'component': component, 'version': version, 'commit': manifest['source_commit'],
            'name': name, 'digest': found, 'ref': f'{name}@{found}', 'reused': reused}


def main():
    parser = argparse.ArgumentParser(description=__doc__,
                                     formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument('--root', default='.', help='the component checkout (default: current directory)')
    parser.add_argument('--out', default='dist-package', help='where the pair is written')
    parser.add_argument('--repository', help='OWNER/NAME of the producer (default: GITHUB_REPOSITORY)')
    parser.add_argument('--commit', help='full producer commit (default: git HEAD of the root)')
    sub = parser.add_subparsers(dest='command', required=True)
    sub.add_parser('build', help='write the archive and manifest')
    p = sub.add_parser('publish', help='build and push to the organization registry')
    p.add_argument('--registry', help='registry namespace (default: ghcr.io/<GITHUB_REPOSITORY_OWNER>)')
    p.add_argument('--image-spec', choices=IMAGE_SPECS, default=IMAGE_SPECS[0])
    args = parser.parse_args()
    root, out = Path(args.root).resolve(), Path(args.out).resolve()
    try:
        if args.command == 'build':
            manifest, archive = build(root, out, args.repository, args.commit)
            print(f'  built {manifest["component"]:<12} v{manifest["version"]:<8} '
                  f'{len(manifest["files"]):>4} files  {manifest["archive_sha256"][:16]}…  {archive}')
            return
        registry = args.registry
        if not registry:
            owner = os.environ.get('GITHUB_REPOSITORY_OWNER')
            if not owner:
                raise PackageError('publish needs --registry or GITHUB_REPOSITORY_OWNER')
            registry = f'ghcr.io/{owner.lower()}'
        done = publish(root, out, registry, args.repository, args.commit, args.image_spec)
    except PackageError as error:
        sys.exit(f'FATAL: {error}')
    print(f'  {"reused" if done["reused"] else "published"} {done["component"]:<12} '
          f'v{done["version"]:<8} {done["ref"]}')
    output = os.environ.get('GITHUB_OUTPUT')
    if output:
        with open(output, 'a') as handle:
            for key in ('component', 'version', 'commit', 'name', 'digest', 'ref'):
                handle.write(f'{key}={done[key]}\n')
            handle.write(f'reused={"true" if done["reused"] else "false"}\n')


if __name__ == '__main__':
    main()
