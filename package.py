#!/usr/bin/env python3
"""Export the versioned wire/conformance tools without the backend checkout."""
import argparse
import hashlib
import json
from pathlib import Path
import re
import stat
import zipfile


def source_file(root, relative):
    candidate = root / relative
    if Path(relative).is_absolute() or not candidate.resolve().is_relative_to(root.resolve()):
        raise ValueError(f'Contract source must stay inside its root: {relative}')
    for parent in (candidate, *candidate.parents):
        if parent == root:
            break
        if parent.is_symlink():
            raise ValueError(f'Contract sources must not contain symlinks: {relative}')
    return candidate


def build_archive(root, destination):
    config = json.loads((root / 'spec/contracts/manifest.json').read_text())
    semver = r'(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-([0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*))?'
    match = re.fullmatch(semver, config['version'])
    if (config['name'] != 'jelto-contracts' or not match
            or (match[4] and any(re.fullmatch(r'0[0-9]+', part) for part in match[4].split('.')))):
        raise ValueError('Invalid contracts package identity/version')
    files = dict(config['files'])
    for directory in config['directories']:
        for source in sorted(source_file(root, directory).rglob('*')):
            relative = source.relative_to(root).as_posix()
            source_file(root, relative)
            if source.is_file() and source.suffix in {'.go', '.yaml', '.json', '.md', '.txt'} and source.name != 'TODO.md':
                if relative in files:
                    raise ValueError(f'Duplicate contract source: {relative}')
                files[relative] = relative
    content = {}
    for name, source in files.items():
        if name == 'manifest.json' or Path(name).is_absolute() or '..' in Path(name).parts:
            raise ValueError(f'Invalid contract artifact path: {name}')
        content[name] = source_file(root, source).read_bytes()
    manifest = {
        'name': config['name'], 'version': config['version'],
        'files': {name: hashlib.sha256(data).hexdigest() for name, data in sorted(content.items())},
    }
    content['manifest.json'] = (json.dumps(manifest, indent=2) + '\n').encode()
    destination.mkdir(parents=True, exist_ok=True)
    archive = destination / f"jelto-contracts-{config['version']}.zip"
    temporary = archive.with_suffix('.tmp')
    try:
        with zipfile.ZipFile(temporary, 'w', compression=zipfile.ZIP_DEFLATED, compresslevel=9) as output:
            for name, data in sorted(content.items()):
                entry = zipfile.ZipInfo(name, date_time=(1980, 1, 1, 0, 0, 0))
                entry.create_system = 3
                entry.external_attr = (stat.S_IFREG | 0o644) << 16
                entry.compress_type = zipfile.ZIP_DEFLATED
                output.writestr(entry, data, compresslevel=9)
        temporary.replace(archive)
    finally:
        temporary.unlink(missing_ok=True)
    checksum = hashlib.sha256(archive.read_bytes()).hexdigest()
    archive.with_suffix('.zip.sha256').write_text(f'{checksum}  {archive.name}\n')
    return archive


if __name__ == '__main__':
    root = Path(__file__).resolve().parent
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--output', type=Path, default=root / 'dist')
    args = parser.parse_args()
    print(build_archive(root, args.output.resolve()))
