#!/usr/bin/env python3
"""Install and run the exact quality and security programs tools.json pins.

    install_tools.py install [tool ...]   install the named tools (default: all)
    install_tools.py path <tool>          print the tool's path, installing it first if needed
    install_tools.py run <tool> [arg ...] run the tool with the given arguments
    install_tools.py verify               check tools.json itself and exit

Each tool lives in a per-user cache (JELTO_TOOLS_DIR, else $XDG_CACHE_HOME/jelto-tools,
else ~/.cache/jelto-tools) under its name, version and platform, so two versions never
mix and nothing is written into a repository. A download is refused unless its SHA-256
equals the pinned value; nothing falls back to a program found on PATH, because an
unpinned version would make a local result disagree with CI.

Standard library only, so it runs before anything else is installed.
"""

import hashlib
import io
import json
import os
import platform
import shutil
import subprocess
import sys
import tarfile
import tempfile
import time
import urllib.request
from pathlib import Path

HERE = Path(__file__).resolve().parent
MANIFEST = HERE / 'tools.json'
# A release asset bigger than this is not one of the tools pinned here.
MAX_DOWNLOAD = 400 * 1024 * 1024
ATTEMPTS = 3
COMPLETE = '.complete'


class ToolError(Exception):
    pass


def cache_root():
    explicit = os.environ.get('JELTO_TOOLS_DIR')
    if explicit:
        return Path(explicit).expanduser()
    xdg = os.environ.get('XDG_CACHE_HOME')
    base = Path(xdg).expanduser() if xdg else Path.home() / '.cache'
    return base / 'jelto-tools'


def current_platform():
    system = platform.system().lower()
    machine = platform.machine().lower()
    machine = {'aarch64': 'arm64', 'amd64': 'x86_64'}.get(machine, machine)
    return f'{system}-{machine}'


def load_manifest():
    data = json.loads(MANIFEST.read_text())
    tools = data.get('tools')
    if not isinstance(tools, dict) or not tools:
        raise ToolError(f'{MANIFEST}: no tools')
    for name, tool in tools.items():
        if not tool.get('version'):
            raise ToolError(f'{name}: no version')
        kinds = [k for k in ('assets', 'go', 'git') if k in tool]
        if len(kinds) != 1:
            raise ToolError(f'{name}: exactly one of assets, go or git is required, found {kinds}')
        if 'assets' in tool:
            if not tool.get('binary'):
                raise ToolError(f'{name}: an asset tool names its binary')
            for target, asset in tool['assets'].items():
                url, sha = asset.get('url', ''), asset.get('sha256', '')
                if not url.startswith('https://'):
                    raise ToolError(f'{name} {target}: the URL must be HTTPS: {url!r}')
                if len(sha) != 64 or any(c not in '0123456789abcdef' for c in sha):
                    raise ToolError(f'{name} {target}: the SHA-256 must be 64 lowercase hex digits')
        if 'git' in tool:
            commit = tool['version']
            if len(commit) != 40 or any(c not in '0123456789abcdef' for c in commit):
                raise ToolError(f'{name}: a Git tool is pinned by its full commit id')
            if not tool['git'].startswith('https://'):
                raise ToolError(f'{name}: the repository URL must be HTTPS')
        if 'go' in tool and not tool.get('binary'):
            raise ToolError(f'{name}: a Go tool names its binary')
    return tools


def download(url):
    last = None
    for attempt in range(ATTEMPTS):
        try:
            request = urllib.request.Request(url, headers={'User-Agent': 'jelto-install-tools'})
            with urllib.request.urlopen(request, timeout=120) as response:
                data = response.read(MAX_DOWNLOAD + 1)
            if len(data) > MAX_DOWNLOAD:
                raise ToolError(f'{url}: larger than {MAX_DOWNLOAD} bytes')
            return data
        except (OSError, ValueError) as error:
            last = error
            time.sleep(2 ** attempt)
    raise ToolError(f'{url}: download failed: {last}')


def extract_binary(data, binary, url):
    """Return the bytes of the one regular file named `binary` in a tar.gz, or the
    download itself when the asset is the bare executable."""
    if not url.endswith(('.tar.gz', '.tgz')):
        return data
    with tarfile.open(fileobj=io.BytesIO(data), mode='r:gz') as archive:
        members = [m for m in archive.getmembers()
                   if m.isreg() and Path(m.name).name == binary]
        if len(members) != 1:
            raise ToolError(f'{url}: expected one file named {binary}, found {len(members)}')
        return archive.extractfile(members[0]).read()


def tool_dir(name, tool, target):
    return cache_root() / name / tool['version'] / target


def installed_path(name, tool, target):
    directory = tool_dir(name, tool, target)
    if not (directory / COMPLETE).is_file():
        return None
    return directory if 'git' in tool else directory / tool['binary']


def place(staging, directory):
    """Move a finished staging directory into place; a concurrent install of the
    same pinned bytes may already have won, which is equally correct."""
    directory.parent.mkdir(parents=True, exist_ok=True)
    (staging / COMPLETE).write_text('')
    try:
        staging.rename(directory)
    except OSError:
        if not (directory / COMPLETE).is_file():
            raise
        shutil.rmtree(staging, ignore_errors=True)


def install(name, tools):
    if name not in tools:
        raise ToolError(f'unknown tool {name!r}; tools.json pins: {", ".join(sorted(tools))}')
    tool = tools[name]
    target = 'any' if 'git' in tool else current_platform()
    existing = installed_path(name, tool, target)
    if existing:
        return existing
    directory = tool_dir(name, tool, target)
    directory.parent.mkdir(parents=True, exist_ok=True)
    staging = Path(tempfile.mkdtemp(prefix=f'.{name}-', dir=directory.parent))
    try:
        if 'assets' in tool:
            asset = tool['assets'].get(target)
            if not asset:
                raise ToolError(f'{name} {tool["version"]} has no pinned asset for {target}; '
                                f'pinned: {", ".join(sorted(tool["assets"]))}')
            data = download(asset['url'])
            digest = hashlib.sha256(data).hexdigest()
            if digest != asset['sha256']:
                raise ToolError(f'{asset["url"]}: SHA-256 {digest} does not match the pin {asset["sha256"]}')
            path = staging / tool['binary']
            path.write_bytes(extract_binary(data, tool['binary'], asset['url']))
            path.chmod(0o755)
        elif 'go' in tool:
            env = {k: v for k, v in os.environ.items() if k not in {'GOFLAGS', 'GOWORK'}}
            env.update(GOBIN=str(staging), GOWORK='off', GOFLAGS='-mod=mod')
            subprocess.run(['go', 'install', f'{tool["go"]}@{tool["version"]}'],
                           check=True, env=env, cwd=staging)
            if not (staging / tool['binary']).is_file():
                raise ToolError(f'go install {tool["go"]} produced no {tool["binary"]}')
        else:
            commit = tool['version']
            git = ['git', '-C', str(staging), '-c', 'advice.detachedHead=false']
            subprocess.run(['git', 'init', '--quiet', str(staging)], check=True)
            subprocess.run([*git, 'fetch', '--quiet', '--depth', '1', tool['git'], commit], check=True)
            subprocess.run([*git, 'checkout', '--quiet', 'FETCH_HEAD'], check=True)
            head = subprocess.run([*git, 'rev-parse', 'HEAD'], check=True,
                                  capture_output=True, text=True).stdout.strip()
            if head != commit:
                raise ToolError(f'{tool["git"]}: fetched {head}, pinned {commit}')
            shutil.rmtree(staging / '.git')
        place(staging, directory)
    except BaseException:
        shutil.rmtree(staging, ignore_errors=True)
        raise
    return installed_path(name, tool, target)


def main(argv):
    if not argv or argv[0] in {'-h', '--help'}:
        print(__doc__.strip())
        return 0 if argv else 2
    command, rest = argv[0], argv[1:]
    try:
        tools = load_manifest()
        if command == 'verify':
            print(f'{MANIFEST.name}: {len(tools)} tools pinned')
            return 0
        if command == 'install':
            for name in rest or sorted(tools):
                print(f'{name} {tools.get(name, {}).get("version", "?")}: {install(name, tools)}')
            return 0
        if command == 'path' and len(rest) == 1:
            print(install(rest[0], tools))
            return 0
        if command == 'run' and rest:
            path = install(rest[0], tools)
            if 'git' in tools[rest[0]]:
                raise ToolError(f'{rest[0]} is a directory of data, not a program')
            os.execv(str(path), [str(path), *rest[1:]])
    except ToolError as error:
        print(f'install_tools: {error}', file=sys.stderr)
        return 1
    except subprocess.CalledProcessError as error:
        print(f'install_tools: {error}', file=sys.stderr)
        return 1
    print(__doc__.strip(), file=sys.stderr)
    return 2


if __name__ == '__main__':
    sys.exit(main(sys.argv[1:]))
