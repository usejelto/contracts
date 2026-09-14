#!/usr/bin/env python3
"""Standalone release tools. SDKs consume a checksummed copy; no backend imports."""
import argparse
import hashlib
import io
import http.client
import json
import os
from pathlib import Path, PurePosixPath
import re
import shlex
import shutil
import subprocess
import sys
import tarfile
import tempfile
import time
import tomllib
import urllib.error
import urllib.parse
import urllib.request
import xml.etree.ElementTree as ET
import zipfile

SEMVER = re.compile(r'(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)(?:-([0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*))?', re.ASCII)


def run(*args, cwd=None, env=None):
    return subprocess.check_output([shutil.which(args[0]) or args[0], *map(str, args[1:])],
                                   cwd=cwd, env=env, text=True).strip()


def version(value):
    match = SEMVER.fullmatch(value)
    if not match or (match[4] and any(re.fullmatch(r'0\d+', part) for part in match[4].split('.'))):
        raise ValueError('Expected SemVer without build metadata or leading zeroes')
    return value


def config(root):
    return json.loads((root / 'release.json').read_text())


def identity(root):
    component = config(root)['component']
    if component in {'analytics', 'crawler', 'electron', 'tauri'}:
        package = json.loads((root / 'package.json').read_text())
        if package['name'] != '@jelto/' + component:
            raise ValueError('Unexpected npm package identity')
        value = package['version']
        lock = json.loads((root / 'package-lock.json').read_text())
        if lock['version'] != value or lock['packages']['']['version'] != value:
            raise ValueError('npm lockfile version mismatch')
        if component == 'electron':
            reported = re.search(r"SDK_CLIENT_VERSION = 'electron/([^']+)'", (root / 'src/wire.ts').read_text())[1]
            if reported != value:
                raise ValueError('Electron client version mismatch')
        if component == 'tauri':
            crate = tomllib.loads((root / 'Cargo.toml').read_text())['package']
            locked = next(p for p in tomllib.loads((root / 'Cargo.lock').read_text())['package']
                          if p['name'] == 'tauri-plugin-jelto')
            if crate['name'] != 'tauri-plugin-jelto' or crate['version'] != value or locked['version'] != value:
                raise ValueError('Tauri npm/Rust/lockfile version mismatch')
    elif component == 'dotnet':
        project = ET.parse(root / 'Jelto/Jelto.csproj')
        if project.findtext('.//PackageId') != 'Jelto':
            raise ValueError('Unexpected NuGet package identity')
        value = project.findtext('.//Version')
        reported = re.search(r'versionOverride \?\? "dotnet/([^"]+)"', (root / 'Jelto/Engine.cs').read_text())[1]
        if reported != value:
            raise ValueError('.NET client version mismatch')
    elif component == 'swift':
        value = re.search(r'sdkClientVersion = "swift/([^"]+)"', (root / 'Sources/Jelto/Wire.swift').read_text())[1]
    elif component == 'contracts':
        manifest = json.loads((root / 'spec/contracts/manifest.json').read_text())
        if manifest['name'] != 'jelto-contracts':
            raise ValueError('Unexpected contracts identity')
        value = manifest['version']
    else:
        raise ValueError('Unknown release component')
    version(value)
    if component in {'electron', 'tauri', 'dotnet', 'swift'} and len(value) > 24:
        raise ValueError('SDK version exceeds the wire grammar')
    return component, value


def repository(value):
    if not re.fullmatch(r'[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+', value):
        raise ValueError('Configure the real owner/repository in release.json before tagging')
    return value


def validate(root, tag, repo, ancestry=True):
    component, value = identity(root)
    if not tag.startswith('v') or version(tag[1:]) != value:
        raise ValueError('Release tag and package version mismatch')
    if repository(config(root)['repository']) != repository(repo):
        raise ValueError('Release repository mismatch')
    url = 'https://github.com/' + repo
    if (root / 'package.json').exists():
        package = json.loads((root / 'package.json').read_text())
        if package.get('repository', {}).get('url') != 'git+' + url + '.git':
            raise ValueError('npm repository.url must match the standalone repository')
        if package.get('license') == 'MIT' and not (root / 'LICENSE').is_file():
            raise ValueError('Missing declared MIT license text')
    if component == 'tauri' and tomllib.loads((root / 'Cargo.toml').read_text())['package'].get('repository') != url:
        raise ValueError('Cargo repository metadata mismatch')
    if component == 'dotnet' and ET.parse(root / 'Jelto/Jelto.csproj').findtext('.//RepositoryUrl') != url:
        raise ValueError('NuGet repository metadata mismatch')
    if ancestry:
        if os.environ.get('GITHUB_REF_TYPE', 'tag') != 'tag':
            raise ValueError('Select a version tag, not a branch')
        head = run('git', 'rev-parse', 'HEAD', cwd=root)
        if run('git', 'rev-parse', 'refs/tags/' + tag + '^{commit}', cwd=root) != head:
            raise ValueError('Checkout does not match the release tag')
        run('git', 'merge-base', '--is-ancestor', head, 'origin/main', cwd=root)
        if run('git', 'status', '--porcelain', '--untracked-files=no', cwd=root):
            raise ValueError('Release source has tracked modifications')
    return component, value


class HTTPSRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        if urllib.parse.urlsplit(newurl).scheme != 'https':
            raise ValueError('Downloads must remain HTTPS')
        return super().redirect_request(req, fp, code, msg, headers, newurl)


# Five attempts with a doubling pause (2, 4, 8, 16 s): three attempts two and
# four seconds apart did not outlast a GitHub 504 on a release asset, and that
# one download cost a whole tauri release run (2026-09-14).
DOWNLOAD_ATTEMPTS = 5
DOWNLOAD_PAUSE = 2.0


def download(url, missing=False, accept=None):
    """Fetch an HTTPS URL, retrying transient failures; a definite answer is never retried.

    One connection reset from a release host must not fail a whole publication
    (tauri 1.0.1's checks did, 2026-09-12). Network errors, timeouts, truncated
    bodies and 5xx/429 answers are retried with backoff; 404 with missing=True
    returns None at once, every other 4xx is raised at once, and the size bound
    is not a retry.
    """
    parsed = urllib.parse.urlsplit(url)
    if parsed.scheme != 'https' or parsed.username or parsed.password:
        raise ValueError('Expected an HTTPS URL without credentials')
    headers = {'User-Agent': 'jelto-release/0.1.0'}
    if accept:
        headers['Accept'] = accept
    request = urllib.request.Request(url, headers=headers)
    last = None
    for attempt in range(1, DOWNLOAD_ATTEMPTS + 1):
        try:
            with urllib.request.build_opener(HTTPSRedirect()).open(request, timeout=60) as response:
                data = response.read(100 * 1024 * 1024 + 1)
                if len(data) > 100 * 1024 * 1024:
                    raise ValueError('Download exceeds 100 MiB')
                return data
        except urllib.error.HTTPError as error:
            if missing and error.code == 404:
                return None
            if error.code < 500 and error.code != 429:
                raise
            last = error
        except (urllib.error.URLError, TimeoutError, ConnectionError, http.client.HTTPException) as error:
            last = error
        if attempt < DOWNLOAD_ATTEMPTS:
            time.sleep(DOWNLOAD_PAUSE * 2 ** (attempt - 1))
    raise RuntimeError(f'Download failed after {DOWNLOAD_ATTEMPTS} attempts: {url} ({last})')


def install_contracts(root):
    pin = config(root).get('contracts') or {}
    if not pin.get('version') or not re.fullmatch(r'[a-f0-9]{64}', pin.get('sha256', '')):
        raise ValueError('Configure contracts version, URL and SHA-256 in release.json')
    version(pin['version'])
    data = download(pin['url'])
    with tempfile.TemporaryDirectory(prefix='jelto-contract-pin-') as temp:
        archive = Path(temp) / 'contracts.zip'
        archive.write_bytes(data)
        run(sys.executable, root / 'vendor/test-tools/install.py', archive, pin['sha256'], root / '.contracts',
            '--version', pin['version'])


def archive_files(data, kind):
    """Compare logical package contents; reject aliases and unsafe archive entries."""
    output = {}
    def add(name, body):
        path = PurePosixPath(name)
        if path.is_absolute() or '..' in path.parts or '\\' in name or ':' in name or name in output:
            raise ValueError('Unsafe or duplicate package entry')
        output[name] = body
    if kind in {'npm', 'cargo'}:
        with tarfile.open(fileobj=io.BytesIO(data), mode='r:gz') as archive:
            for entry in archive:
                if entry.isdir():
                    continue
                if not entry.isfile():
                    raise ValueError('Packages cannot contain links or special files')
                add(entry.name, archive.extractfile(entry).read())
    else:
        with zipfile.ZipFile(io.BytesIO(data)) as archive:
            for entry in archive.infolist():
                # ZipInfo.filename is normalised with the running platform's
                # separator, so on Windows a backslash entry is already read
                # as a forward slash and the guard could not see it. The raw
                # central-directory name is what the archive really carries.
                if not entry.is_dir():
                    add(entry.orig_filename, archive.read(entry))
        if kind == 'nuget':
            output.pop('.signature.p7s', None)
    return output


def check_package(path, kind, name, value):
    files = archive_files(path.read_bytes(), kind)
    if kind == 'npm':
        manifest = json.loads(files['package/package.json'])
        if manifest['name'] != name or manifest['version'] != value:
            raise ValueError('npm artifact identity mismatch')
        if manifest.get('license') == 'MIT' and 'package/LICENSE' not in files:
            raise ValueError('npm artifact is missing LICENSE')
        paths = []
        def exports(node):
            if isinstance(node, str):
                paths.append(node)
            elif isinstance(node, dict):
                for child in node.values():
                    exports(child)
        exports(manifest.get('exports', {}))
        paths.extend(manifest[key] for key in ('main', 'module', 'types') if key in manifest)
        for entry in paths:
            if not files.get('package/' + entry.removeprefix('./')):
                raise ValueError('Missing npm package entry: ' + entry)
    elif kind == 'cargo':
        prefix = name + '-' + value + '/'
        allowed = {'Cargo.toml', 'Cargo.toml.orig', 'Cargo.lock', '.cargo_vcs_info.json', 'build.rs', 'README.md', 'LICENSE'}
        for filename in files:
            relative = filename.removeprefix(prefix)
            if not filename.startswith(prefix) or not (relative in allowed or relative.startswith(('src/', 'permissions/'))):
                raise ValueError('Unexpected file in Cargo package: ' + filename)
        manifest = tomllib.loads(files[prefix + 'Cargo.toml'].decode())['package']
        if manifest['name'] != name or manifest['version'] != value:
            raise ValueError('Cargo artifact identity mismatch')
        if prefix + 'LICENSE' not in files:
            raise ValueError('Cargo artifact is missing LICENSE')
    elif kind == 'nuget':
        manifest = ET.fromstring(files['Jelto.nuspec'])
        if manifest.findtext('.//{*}id') != name or manifest.findtext('.//{*}version') != value:
            raise ValueError('NuGet artifact identity mismatch')
        if not files.get('lib/net8.0/Jelto.dll'):
            raise ValueError('NuGet artifact has no SDK assembly')
    elif name == 'jelto-contracts':
        manifest = json.loads(files['manifest.json'])
        if manifest['name'] != name or manifest['version'] != value:
            raise ValueError('Contracts artifact identity mismatch')
        if set(files) != set(manifest['files']) | {'manifest.json'}:
            raise ValueError('Contracts artifact file list mismatch')
        for filename, digest in manifest['files'].items():
            if hashlib.sha256(files[filename]).hexdigest() != digest:
                raise ValueError('Contracts artifact checksum mismatch')
    else:
        if not files.get('Package.swift'):
            raise ValueError('Swift source archive must have Package.swift at its root')
        wire = files.get('Sources/Jelto/Wire.swift', b'').decode()
        match = re.search(r'sdkClientVersion = "swift/([^"]+)"', wire)
        if not match or match[1] != value:
            raise ValueError('Swift artifact version mismatch')


def stage(root, tag, repo):
    component, value = validate(root, tag, repo, ancestry=False)
    candidates = []
    if component in {'analytics', 'crawler', 'electron', 'tauri'}:
        filename = 'jelto-' + component + '-' + value + '.tgz'
        # Packaged by CI before credentials are available.
        candidates.append((root / (('artifacts/' if component == 'tauri' else '') + filename), 'npm', '@jelto/' + component))
    if component == 'tauri':
        # Cargo dry-run verifies the actual package from the tagged Git checkout.
        candidates.append((root / ('target/package/tauri-plugin-jelto-' + value + '.crate'), 'cargo', 'tauri-plugin-jelto'))
    elif component == 'dotnet':
        candidates.append((root / ('artifacts/Jelto.' + value + '.nupkg'), 'nuget', 'Jelto'))
    elif component in {'swift', 'contracts'}:
        folder = 'artifacts' if component == 'swift' else 'dist'
        candidates.append((root / (folder + '/jelto-' + component + '-' + value + '.zip'), 'zip', 'jelto-' + component))
    output = root / 'release-artifacts'
    output.mkdir(exist_ok=False)
    packages = []
    for path, kind, name in candidates:
        check_package(path, kind, name, value)
        shutil.copy2(path, output / path.name)
        packages.append({'file': path.name, 'kind': kind, 'name': name,
                         'sha256': hashlib.sha256(path.read_bytes()).hexdigest()})
    record = {'repository': repo, 'tag': tag, 'version': value, 'component': component,
              'commit': run('git', 'rev-parse', 'HEAD', cwd=root), 'packages': packages}
    (output / 'release.json').write_text(json.dumps(record, indent=2) + '\n')
    (output / 'CHECKSUMS').write_text(''.join(
        hashlib.sha256(path.read_bytes()).hexdigest() + '  ' + path.name + '\n'
        for path in sorted(output.iterdir())))
    # Preserve the conventional contracts checksum asset name.
    if component == 'contracts':
        item = packages[0]
        (output / (item['file'] + '.sha256')).write_text(item['sha256'] + '  ' + item['file'] + '\n')


def required_packages(component, value):
    """The (kind, registry name, file) triples a component's release ships."""
    required = set()
    if component in {'analytics', 'crawler', 'electron', 'tauri'}:
        required.add(('npm', '@jelto/' + component, 'jelto-' + component + '-' + value + '.tgz'))
    if component == 'tauri':
        required.add(('cargo', 'tauri-plugin-jelto', 'tauri-plugin-jelto-' + value + '.crate'))
    elif component == 'dotnet':
        required.add(('nuget', 'Jelto', 'Jelto.' + value + '.nupkg'))
    elif component in {'swift', 'contracts'}:
        required.add(('zip', 'jelto-' + component, 'jelto-' + component + '-' + value + '.zip'))
    return required


def verify(root, tag, repo):
    validate(root, tag, repo)
    folder = root / 'release-artifacts'
    record = json.loads((folder / 'release.json').read_text())
    if (record['tag'], record['repository'], record['commit']) != (tag, repo, run('git', 'rev-parse', 'HEAD', cwd=root)):
        raise ValueError('Artifact source identity mismatch')
    if record['component'] != identity(root)[0] or record['version'] != tag[1:]:
        raise ValueError('Artifact component/version mismatch')
    component, value = record['component'], record['version']
    required = required_packages(component, value)
    if (len(record['packages']) != len(required)
            or {(item['kind'], item['name'], item['file']) for item in record['packages']} != required):
        raise ValueError('Release packages do not match the component')
    expected = {'release.json', 'CHECKSUMS'}
    for item in record['packages']:
        filename = item['file']
        if Path(filename).name != filename:
            raise ValueError('Unsafe artifact filename')
        expected.add(filename)
        path = folder / filename
        if hashlib.sha256(path.read_bytes()).hexdigest() != item['sha256']:
            raise ValueError('Artifact checksum mismatch')
        check_package(path, item['kind'], item['name'], record['version'])
    if record['component'] == 'contracts':
        item = record['packages'][0]
        expected.add(item['file'] + '.sha256')
        if (folder / (item['file'] + '.sha256')).read_text() != item['sha256'] + '  ' + item['file'] + '\n':
            raise ValueError('Contracts checksum asset mismatch')
    if {p.name for p in folder.iterdir()} != expected:
        raise ValueError('Unexpected release artifact files')
    checksums = ''.join(hashlib.sha256((folder / name).read_bytes()).hexdigest() + '  ' + name + '\n'
                        for name in sorted(expected - {'CHECKSUMS'} - {n for n in expected if n.endswith('.sha256')}))
    if (folder / 'CHECKSUMS').read_text() != checksums:
        raise ValueError('Release checksum manifest mismatch')
    return record


def registry_data(item, value):
    name = urllib.parse.quote(item['name'], safe='')
    if item['kind'] == 'npm':
        metadata = download('https://registry.npmjs.org/' + name + '/' + value, missing=True)
        return None if metadata is None else download(json.loads(metadata)['dist']['tarball'])
    if item['kind'] == 'cargo':
        # API metadata distinguishes missing versions from transport/auth failures.
        metadata = download('https://crates.io/api/v1/crates/' + name + '/' + value, missing=True)
        return None if metadata is None else download('https://static.crates.io/crates/' + name + '/' + name + '-' + value + '.crate')
    if item['kind'] == 'nuget':
        name, value = name.lower(), value.lower()
        return download('https://api.nuget.org/v3-flatcontainer/' + name + '/' + value + '/' + name + '.' + value + '.nupkg', missing=True)
    return None


def registry_indexed(item, value):
    """Whether the registry's install path resolves this version yet.

    The immutable bytes registry_data reads appear seconds to minutes before the
    index installers resolve: npm's abbreviated packument, the crates.io sparse
    index and NuGet's flat-container version list. Waiting on the bytes alone sent
    the installation check after a version the registry did not list yet
    (crawler and analytics 1.0.1: npm ETARGET; tauri 1.0.0: Cargo "did not match
    any packages"), so every first tag run needed a second dispatch.
    """
    name = item['name']
    if item['kind'] == 'npm':
        # The representation npm's resolver requests; the CDN caches it on its own.
        data = download('https://registry.npmjs.org/' + urllib.parse.quote(name, safe=''), missing=True,
                        accept='application/vnd.npm.install-v1+json; q=1.0, application/json; q=0.8, */*')
        return data is not None and value in json.loads(data).get('versions', {})
    if item['kind'] == 'cargo':
        lower = name.lower()
        prefix = {1: '1', 2: '2', 3: '3/' + lower[0]}.get(len(lower), lower[:2] + '/' + lower[2:4])
        data = download('https://index.crates.io/' + prefix + '/' + lower, missing=True)
        return data is not None and any(
            json.loads(line)['vers'] == value for line in data.decode().splitlines() if line.strip())
    if item['kind'] == 'nuget':
        lower = name.lower()
        data = download('https://api.nuget.org/v3-flatcontainer/' + lower + '/index.json', missing=True)
        return data is not None and value.lower() in json.loads(data).get('versions', [])
    return True


def registry_matches(folder, item, value):
    data = registry_data(item, value)
    if data is None:
        return False
    # npm and Cargo bytes are immutable; NuGet adds a repository signature.
    local = (folder / item['file']).read_bytes()
    same = (archive_files(local, 'nuget') == archive_files(data, 'nuget')
            if item['kind'] == 'nuget' else local == data)
    if not same:
        raise ValueError('Published version has different contents: ' + item['name'])
    return True


# A registry lists a published version seconds to minutes after it holds the
# bytes; npm took over ten minutes for @jelto/tauri 1.0.2 (2026-09-14), past the
# earlier five-minute wait, and that run had to be dispatched again for nothing.
# A quarter of an hour with a growing pause covers what has been observed.
WAIT_PAUSES = [10, 15, 20, 30, 45] + [60] * 13


def registry_status(root, tag, repo, wait=False, kind=None):
    record = verify(root, tag, repo)
    for item in record['packages']:
        if item['kind'] == 'zip' or (kind and item['kind'] != kind):
            continue
        exists = ready = False
        pauses = WAIT_PAUSES if wait else []
        for attempt in range(len(pauses) + 1):
            exists = registry_matches(root / 'release-artifacts', item, record['version'])
            # The installation check follows a wait, so a wait also needs the index installers
            # resolve; status keeps answering about the bytes, which decide whether to publish.
            ready = exists and (not wait or registry_indexed(item, record['version']))
            if ready or attempt == len(pauses):
                break
            time.sleep(pauses[attempt])
        if wait and not ready:
            raise ValueError('Registry indexing timed out: ' + item['name'])
        line = item['kind'] + '_exists=' + str(exists).lower() + '\n'
        if os.environ.get('GITHUB_OUTPUT'):
            with open(os.environ['GITHUB_OUTPUT'], 'a') as output:
                output.write(line)
        print(line, end='')


def invoke(*args):
    """Run a command and return its completed process; unlike run(), a failure is reported, not raised."""
    return subprocess.run([shutil.which(args[0]) or args[0], *map(str, args[1:])], capture_output=True, text=True)


def gh_json(*args):
    return json.loads(run('gh', 'api', *args))


def find_release(tag, repo):
    """The GitHub Release for this tag, draft or published, or None."""
    releases = gh_json('--paginate', '--slurp', 'repos/' + repo + '/releases')
    return next((r for page in releases for r in page if r['tag_name'] == tag), None)


def release_notes(record):
    return ('Source: ' + record['commit'] + '\n\nComponent CI, package checks and conformance twice passed.'
            '\n\nPublished artifacts and SHA-256 checksums are attached. '
            'For bootstrap, publish only these verified packages from this exact tag.\n')


def github_draft(root, tag, repo):
    """Open the GitHub Release as a draft before anything immutable is published.

    Creating the release is the one step a rerun could not always repeat: on
    2026-09-14 electron v1.0.1 and tauri v1.0.2 got "HTTP 403: Resource not
    accessible by integration" from workflow_dispatch runs, while every tag-push
    run had opened its draft. Opening the draft first, before any registry
    receives bytes, leaves a later dispatch only assets to upload and the draft
    to publish; and when the token cannot open it, the run stops before it has
    published anything and prints the one command that opens the draft by hand.
    """
    record = verify(root, tag, repo)
    existing = find_release(tag, repo)
    if existing is not None:
        if existing['target_commitish'] not in {record['commit'], 'main'}:
            raise ValueError('Existing GitHub Release source differs')
        print('GitHub Release ' + ('draft ' if existing['draft'] else '') + 'exists: ' + tag)
        return existing
    args = ['gh', 'release', 'create', tag, '--repo', repo, '--verify-tag', '--draft',
            '--target', record['commit'], '--title', tag, '--notes', release_notes(record)]
    if '-' in record['version']:
        args.append('--prerelease')
    result = invoke(*args)
    if result.returncode:
        detail = (result.stderr or result.stdout or '').strip()
        if 'HTTP 403' in detail:
            raise ValueError(
                detail + '\n\nThe workflow token could not open the draft release. Open it once by hand, '
                'then rerun this tag; the rerun uploads the assets and publishes the draft:\n'
                '  gh api -X POST repos/' + repo + '/releases -f tag_name=' + tag
                + ' -f target_commitish=' + record['commit'] + ' -f name=' + tag
                + ' -F draft=true -f body=' + shlex.quote('Source: ' + record['commit']))
        raise ValueError(detail or ('gh release create failed with status ' + str(result.returncode)))
    print('GitHub Release draft opened: ' + tag)
    return find_release(tag, repo)


def github_release(root, tag, repo):
    record = verify(root, tag, repo)
    # A partially completed registry release must never become a completed GitHub Release.
    for item in record['packages']:
        if item['kind'] != 'zip' and not registry_matches(root / 'release-artifacts', item, record['version']):
            raise ValueError('Registry package is not available: ' + item['name'])
    github_draft(root, tag, repo)
    folder = root / 'release-artifacts'
    with tempfile.TemporaryDirectory(prefix='jelto-release-assets-') as temp:
        details = json.loads(run('gh', 'release', 'view', tag, '--repo', repo, '--json', 'assets,isDraft'))
        assets = {a['name']: a for a in details['assets']}
        for path in sorted(folder.iterdir()):
            if path.name in assets:
                run('gh', 'release', 'download', tag, '--repo', repo, '--pattern', path.name, '--dir', temp)
                if (Path(temp) / path.name).read_bytes() != path.read_bytes():
                    raise ValueError('Existing release asset differs: ' + path.name)
            else:
                if not details['isDraft']:
                    raise ValueError('Completed release is missing an expected asset')
                run('gh', 'release', 'upload', tag, path, '--repo', repo)
        if details['isDraft']:
            run('gh', 'release', 'edit', tag, '--repo', repo, '--draft=false',
                '--prerelease=' + str('-' in record['version']).lower(), '--latest=' + str('-' not in record['version']).lower())


def previous_run(repo, sha, workflow_file, current):
    """An earlier completed run of this workflow on this commit whose checks all passed
    and whose release artifacts still exist; the newest such run, or None.

    A rerun of the same tag then publishes from those artifacts instead of
    verifying the identical commit again: the publish job's verify step still
    checks the artifact's recorded tag, repository and commit against the checkout.
    """
    runs = gh_json('repos/' + repo + '/actions/workflows/' + workflow_file + '/runs?head_sha=' + sha + '&per_page=50')
    for entry in sorted(runs['workflow_runs'], key=lambda r: r['run_number'], reverse=True):
        if str(entry['id']) == str(current) or entry['status'] != 'completed':
            continue
        jobs = gh_json('repos/' + repo + '/actions/runs/' + str(entry['id']) + '/jobs?per_page=100')['jobs']
        checks = [job for job in jobs if job['name'].startswith('checks')]
        if not checks or any(job['conclusion'] != 'success' for job in checks):
            continue
        artifacts = gh_json('repos/' + repo + '/actions/runs/' + str(entry['id']) + '/artifacts?per_page=100')['artifacts']
        if not any(a['name'].startswith('release-') and not a['expired'] for a in artifacts):
            continue
        return entry['id']
    return None


def previous(root, tag, repo):
    validate(root, tag, repo)
    workflow_file = os.environ['GITHUB_WORKFLOW_REF'].split('@')[0].rsplit('/', 1)[1]
    found = previous_run(repo, run('git', 'rev-parse', 'HEAD', cwd=root), workflow_file, os.environ.get('GITHUB_RUN_ID', ''))
    line = 'run_id=' + (str(found) if found else '') + '\n'
    if os.environ.get('GITHUB_OUTPUT'):
        with open(os.environ['GITHUB_OUTPUT'], 'a') as output:
            output.write(line)
    print(('Reusing the verified artifacts of run ' + str(found)) if found
          else 'No earlier verified run for this commit; the checks run now.')


def trust_evidence(kind, name):
    """What a registry shows about trusted publishing for this package: (level, message).

    No registry exposes its trusted-publisher policy, but every version it
    published through OIDC carries provenance, so one such version proves the
    policy is in place; a package with none needs the one-time configuration
    RELEASING.md describes before its first automated release.
    """
    if kind == 'npm':
        data = download('https://registry.npmjs.org/' + urllib.parse.quote(name, safe=''), missing=True)
        if data is None:
            return 'note', name + ': not on npm yet; the first publication is the manual bootstrap'
        versions = json.loads(data).get('versions', {})
        proven = sorted(v for v, meta in versions.items() if meta.get('dist', {}).get('attestations'))
        if proven:
            return 'ok', name + ': trusted publishing proven by ' + proven[-1]
        return 'warn', (name + ': no version published with provenance yet; configure the Trusted Publisher at '
                        'https://www.npmjs.com/package/' + name + '/access (GitHub Actions, workflow release.yml, environment release)')
    if kind == 'cargo':
        data = download('https://crates.io/api/v1/crates/' + name + '/versions', missing=True)
        if data is None:
            return 'note', name + ': not on crates.io yet; the first publication is the manual bootstrap'
        proven = sorted(v['num'] for v in json.loads(data).get('versions', []) if v.get('trustpub_data'))
        if proven:
            return 'ok', name + ': trusted publishing proven by ' + proven[-1]
        return 'warn', (name + ': no version published through trusted publishing yet; configure it at '
                        'https://crates.io/crates/' + name + '/settings/trusted-publishing')
    if kind == 'nuget':
        return 'note', name + ': NuGet exposes no provenance; confirm the trusted policy and NUGET_USER at nuget.org'
    return 'ok', name + ': GitHub Release asset only'


def doctor(root, repo):
    """Report what a tag push needs before it can publish: variables, environment, registry trust."""
    component, value = identity(root)
    findings = []

    def gh_ok(*args):
        return subprocess.run([shutil.which('gh') or 'gh', *args], capture_output=True, text=True)

    variable = gh_ok('variable', 'get', 'RELEASE_PUBLISH_ENABLED', '--repo', repo)
    findings.append(('ok' if variable.stdout.strip() == 'true' else 'fail',
                     'RELEASE_PUBLISH_ENABLED is ' + (variable.stdout.strip() or 'unset') + ' on ' + repo))
    environment = gh_ok('api', 'repos/' + repo + '/environments/release')
    findings.append(('ok' if environment.returncode == 0 else 'fail',
                     'environment "release" ' + ('exists' if environment.returncode == 0 else 'is missing') + ' on ' + repo))
    if component == 'dotnet':
        user = gh_ok('variable', 'get', 'NUGET_USER', '--repo', repo)
        findings.append(('ok' if user.stdout.strip() else 'fail', 'NUGET_USER is ' + (user.stdout.strip() or 'unset')))
    for kind, name, _ in sorted(required_packages(component, value)):
        findings.append(trust_evidence(kind, name))
    width = max(len(level) for level, _ in findings)
    for level, message in findings:
        print(level.upper().ljust(width) + '  ' + message)
    if any(level == 'fail' for level, _ in findings):
        raise ValueError('Fix the FAIL lines before tagging ' + component + ' v' + value)


def configure(root, repo, url=None, checksum=None, contracts_version='0.1.3'):
    repository(repo)
    settings = config(root)
    settings['repository'] = repo
    if url is not None or checksum is not None:
        if not url or not url.startswith('https://') or not re.fullmatch(r'[a-f0-9]{64}', checksum or ''):
            raise ValueError('Supply both the contracts HTTPS URL and SHA-256')
        settings['contracts'] = {'version': version(contracts_version), 'url': url, 'sha256': checksum}
    (root / 'release.json').write_text(json.dumps(settings, indent=2) + '\n')
    address = 'https://github.com/' + repo
    readme = root / 'README.md'
    if readme.exists():
        content = readme.read_text().replace('](RELEASING.md)', '](' + address + '/blob/main/RELEASING.md)')
        content = re.sub(r'https://github.com/[^/]+/[^/]+/blob/main/RELEASING\.md',
                         address + '/blob/main/RELEASING.md', content)
        readme.write_text(content)
    if (root / 'package.json').exists():
        package = json.loads((root / 'package.json').read_text())
        package['repository'] = {'type': 'git', 'url': 'git+' + address + '.git'}
        guide = 'electron-forge' if settings['component'] == 'electron' else settings['component']
        package['homepage'] = 'https://jelto.io/docs/sdk/' + guide
        package['bugs'] = {'url': address + '/issues'}
        package['publishConfig'] = {'access': 'public', 'registry': 'https://registry.npmjs.org'}
        # ensure_ascii would escape the em dash in a description and turn a repin into a noisy diff.
        (root / 'package.json').write_text(json.dumps(package, indent=2, ensure_ascii=False) + '\n')
    if settings['component'] == 'tauri':
        path = root / 'Cargo.toml'
        content = re.sub(r'^repository = .*\n', '', path.read_text(), flags=re.M)
        path.write_text(content.replace('[package]\n', '[package]\nrepository = "' + address + '"\n', 1))
    if settings['component'] == 'dotnet':
        path = root / 'Jelto/Jelto.csproj'
        content = re.sub(r'    <RepositoryUrl>.*</RepositoryUrl>\n', '', path.read_text())
        path.write_text(content.replace('  <PropertyGroup>\n', '  <PropertyGroup>\n    <RepositoryUrl>' + address + '</RepositoryUrl>\n', 1))
    print('Configured release metadata. Review and commit these files before tagging.')


def smoke(root, registry=False):
    """Install packages outside their source tree, with fresh dependency caches."""
    record = json.loads((root / 'release-artifacts/release.json').read_text())
    component, value = record['component'], record['version']
    with tempfile.TemporaryDirectory(prefix='jelto-release-consumer-') as temp:
        temp = Path(temp)
        env = {**os.environ, 'npm_config_cache': str(temp / 'npm-cache')}
        for item in record['packages']:
            if item['kind'] != 'npm':
                continue
            target = item['name'] + '@' + value if registry else str(root / 'release-artifacts' / item['file'])
            (temp / 'package.json').write_text('{"private":true,"type":"module"}')
            run('npm', 'install', '--ignore-scripts', '--no-audit', '--no-fund', '--save-exact',
                '--registry', 'https://registry.npmjs.org', target, cwd=temp, env=env)
            package = json.loads((temp / 'node_modules' / item['name'] / 'package.json').read_text())
            statements = []
            for export in package.get('exports', {'.': None}):
                specifier = item['name'] + (export[1:] if export != '.' else '')
                statements.append('await import(' + json.dumps(specifier) + ');')
            run('node', '--input-type=module', '-e', '\n'.join(statements), cwd=temp, env=env)
            if component == 'electron':
                run('node', '-e', 'require("@jelto/electron")', cwd=temp, env=env)
        if component == 'dotnet':
            run(sys.executable, root / 'verify-examples.py', *(['--registry'] if registry else []), cwd=root)
        elif component == 'swift':
            if registry:
                address = 'https://github.com/' + record['repository'] + '.git'
            else:
                source = temp / 'source'
                source.mkdir()
                archive = root / 'release-artifacts' / record['packages'][0]['file']
                # check_package validated the paths before this extraction.
                with zipfile.ZipFile(archive) as package:
                    package.extractall(source)
                run('git', 'init', '-b', 'main', cwd=source)
                run('git', 'add', '.', cwd=source)
                run('git', '-c', 'user.name=Release test', '-c', 'user.email=release-test@localhost',
                    'commit', '-m', 'Test packaged sources', cwd=source)
                run('git', 'tag', record['tag'], cwd=source)
                address = source.as_uri()
            package_identity = Path(urllib.parse.urlsplit(address).path).name.removesuffix('.git').lower()
            consumer = temp / 'consumer'
            (consumer / 'Sources/Consumer').mkdir(parents=True)
            (consumer / 'Sources/Consumer/main.swift').write_text('import Jelto\n')
            (consumer / 'Package.swift').write_text(
                '// swift-tools-version: 6.0\nimport PackageDescription\n'
                'let package = Package(name: "Consumer", platforms: [.macOS(.v12)], dependencies: ['
                '.package(url: ' + json.dumps(address) + ', exact: ' + json.dumps(value) + ')],'
                'targets: [.executableTarget(name: "Consumer", dependencies: [.product(name: "Jelto", package: '
                + json.dumps(package_identity) + ')])])\n')
            run('swift', 'build', cwd=consumer)
        elif component == 'tauri' and registry:
            example = temp / 'example'
            shutil.copytree(root / 'example', example,
                            ignore=shutil.ignore_patterns('node_modules', 'target', 'dist'))
            package_path = example / 'package.json'
            package = json.loads(package_path.read_text())
            package['dependencies']['@jelto/tauri'] = value
            package_path.write_text(json.dumps(package, indent=2) + '\n')
            lock_path = example / 'package-lock.json'
            lock = json.loads(lock_path.read_text())
            metadata = json.loads(download('https://registry.npmjs.org/@jelto%2Ftauri/' + value))
            lock['packages']['']['dependencies']['@jelto/tauri'] = value
            entry = lock['packages']['node_modules/@jelto/tauri']
            entry.update(version=value, resolved=metadata['dist']['tarball'], integrity=metadata['dist']['integrity'])
            lock_path.write_text(json.dumps(lock, indent=2) + '\n')
            cargo_path = example / 'src-tauri/Cargo.toml'
            content, changed = re.subn(r'tauri-plugin-jelto\s*=\s*\{[^}]*path[^}]*\}',
                                      'tauri-plugin-jelto = "=' + value + '"', cargo_path.read_text())
            if changed != 1:
                raise ValueError('Cannot select registry crate in the native example')
            cargo_path.write_text(content)
            # Resolve against the example's own lockfile: only the plugin moves
            # from the path source to the registry, every other version stays
            # pinned. `cargo update -p` cannot do this -- the path package is
            # gone from the manifest, so cargo drops it from the previous
            # resolve and the spec matches nothing.
            run('cargo', 'fetch', '--manifest-path', cargo_path, cwd=example)
            crate = next(item for item in record['packages'] if item['kind'] == 'cargo')
            locked = re.search(r'name = "tauri-plugin-jelto"\nversion = "([^"]+)"\nsource = "([^"]+)"\nchecksum = "([a-f0-9]{64})"',
                               (example / 'src-tauri/Cargo.lock').read_text())
            if not locked or locked.group(1) != value or 'crates.io' not in locked.group(2) or locked.group(3) != crate['sha256']:
                raise ValueError('The native example did not resolve the published crate: ' + repr(locked and locked.groups()))
            run('npm', 'ci', cwd=example, env=env)
            run('npm', 'run', 'tauri', 'build', '--', '--no-bundle', cwd=example, env=env)
        print('PASS isolated ' + component + (' registry' if registry else ' package') + ' consumer')


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('command', choices=['validate', 'contracts', 'stage', 'verify', 'status', 'wait', 'draft', 'github',
                                            'previous', 'doctor', 'configure', 'smoke'])
    parser.add_argument('--tag', default=os.environ.get('GITHUB_REF_NAME', ''))
    parser.add_argument('--repository', default=os.environ.get('GITHUB_REPOSITORY', ''))
    parser.add_argument('--contracts-url')
    parser.add_argument('--contracts-sha256')
    parser.add_argument('--contracts-version', default='0.1.3')
    parser.add_argument('--kind', choices=['npm', 'cargo', 'nuget'])
    parser.add_argument('--registry', action='store_true')
    args = parser.parse_args()
    root = Path.cwd()
    if args.command == 'smoke':
        smoke(root, args.registry)
    elif args.command == 'doctor':
        doctor(root, args.repository or config(root)['repository'])
    elif args.command == 'configure':
        configure(root, args.repository, args.contracts_url, args.contracts_sha256, args.contracts_version)
    elif args.command == 'contracts':
        install_contracts(root)
    elif args.command in {'status', 'wait'}:
        registry_status(root, args.tag, args.repository, args.command == 'wait', args.kind)
    else:
        {'validate': validate, 'stage': stage, 'verify': verify, 'draft': github_draft, 'github': github_release,
         'previous': previous}[args.command](root, args.tag, args.repository)


if __name__ == '__main__':
    try:
        main()
    except subprocess.CalledProcessError as error:
        sys.exit((error.output or '') + '\n' + str(error))
    except (ValueError, KeyError, OSError) as error:
        sys.exit(str(error))
