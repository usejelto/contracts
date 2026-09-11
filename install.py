#!/usr/bin/env python3
"""Install a checksum-pinned Jelto contracts archive into an empty directory."""
import argparse
import hashlib
import json
from pathlib import Path, PurePosixPath
import shutil
import stat
import tempfile
import zipfile

VERSION_FILE = Path(__file__).resolve().parent / 'spec/contracts/manifest.json'
VERSION = json.loads(VERSION_FILE.read_text())['version'] if VERSION_FILE.exists() else '0.1.0'


def install(archive, checksum, destination, expected_version=VERSION):
    if hashlib.sha256(archive.read_bytes()).hexdigest() != checksum.lower():
        raise ValueError('Contracts archive SHA-256 mismatch')
    if destination.exists():
        raise ValueError('Choose an empty contracts destination')
    with zipfile.ZipFile(archive) as package:
        entries = package.infolist()
        names = [entry.filename for entry in entries]
        if len(names) != len(set(names)) or sum(entry.file_size for entry in entries) > 100 * 1024 * 1024:
            raise ValueError('Invalid contracts archive entries/size')
        for entry in entries:
            path = PurePosixPath(entry.filename)
            if path.is_absolute() or '..' in path.parts or '\\' in entry.filename or ':' in entry.filename or stat.S_ISLNK(entry.external_attr >> 16):
                raise ValueError('Unsafe contracts archive path')
        manifest = json.loads(package.read('manifest.json'))
        if manifest.get('name') != 'jelto-contracts' or manifest.get('version') != expected_version:
            raise ValueError(f'Expected Jelto contracts {expected_version}')
        if set(names) != set(manifest['files']) | {'manifest.json'}:
            raise ValueError('Contracts archive disagrees with its file manifest')
        for name, expected in manifest['files'].items():
            if hashlib.sha256(package.read(name)).hexdigest() != expected:
                raise ValueError(f'Contracts file checksum mismatch: {name}')
        destination.parent.mkdir(parents=True, exist_ok=True)
        staging = Path(tempfile.mkdtemp(prefix='.contracts-', dir=destination.parent))
        try:
            package.extractall(staging)
            staging.rename(destination)
        finally:
            if staging.exists():
                shutil.rmtree(staging)


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('archive', type=Path)
    parser.add_argument('sha256')
    parser.add_argument('destination', type=Path)
    parser.add_argument('--version', default=VERSION)
    args = parser.parse_args()
    install(args.archive, args.sha256, args.destination, args.version)
    print(f'Installed Jelto contracts {args.version} in {args.destination}')
