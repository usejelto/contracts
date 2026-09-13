import hashlib
import io
import json
import os
from pathlib import Path
import subprocess
import sys
import tarfile
import tempfile
import unittest
from unittest.mock import patch

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))
import package_component as tool

COMMIT = 'a' * 40


def producer(root, component='docs', version='1.0.0', include=None):
    """A producer checkout with a built dist/ and the extras docs ships."""
    root.mkdir(parents=True, exist_ok=True)
    (root / 'dist').mkdir()
    (root / 'dist/index.html').write_bytes(b'<html>')
    (root / 'dist/assets').mkdir()
    (root / 'dist/assets/app.js').write_bytes(b'js')
    (root / 'content/guides').mkdir(parents=True)
    (root / 'content/guides/start.md').write_bytes(b'# start')
    (root / 'go.mod').write_bytes(b'module example\n')
    (root / 'server.go').write_bytes(b'package docs')
    (root / 'server_test.go').write_bytes(b'package docs')
    (root / 'package.json').write_text(json.dumps({'name': '@jelto/docs', 'version': version}))
    (root / tool.CONFIG).write_text(json.dumps({
        'component': component,
        'include': include or ['dist', 'content', 'go.mod', '*.go']}))
    return root


class BuildTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix='jelto-package-component-')
        self.addCleanup(self.temp.cleanup)
        self.root = producer(Path(self.temp.name) / 'docs')
        self.root.mkdir(exist_ok=True)

    def test_archive_is_deterministic_and_the_manifest_records_every_file(self):
        first, archive = tool.build(self.root, Path(self.temp.name) / 'one', 'usejelto/docs', COMMIT)
        second, again = tool.build(self.root, Path(self.temp.name) / 'two', 'usejelto/docs', COMMIT)
        self.assertEqual(archive.read_bytes(), again.read_bytes())
        self.assertEqual(first, second)
        self.assertEqual(archive.name, 'jelto-docs-1.0.0.tar.gz')
        self.assertEqual(first['component'], 'docs')
        self.assertEqual(first['version'], '1.0.0')
        self.assertEqual(first['source_commit'], COMMIT)
        self.assertEqual(first['repository'], 'usejelto/docs')
        self.assertFalse(first['published'])
        self.assertEqual(first['archive_sha256'], hashlib.sha256(archive.read_bytes()).hexdigest())
        expected = {'dist/index.html', 'dist/assets/app.js', 'content/guides/start.md',
                    'go.mod', 'server.go', 'server_test.go'}
        self.assertEqual(set(first['files']), expected)
        self.assertEqual(first['files']['dist/index.html'], hashlib.sha256(b'<html>').hexdigest())
        with tarfile.open(archive) as tar:
            members = tar.getmembers()
            self.assertEqual({m.name for m in members}, expected)
            for member in members:
                self.assertTrue(member.isfile())
                self.assertEqual((member.mtime, member.uid, member.gid, member.mode), (0, 0, 0, 0o644))
                self.assertEqual((member.uname, member.gname), ('', ''))
        manifest_on_disk = json.loads((Path(self.temp.name) / 'one/docs.json').read_text())
        self.assertEqual(manifest_on_disk, first)
        # Touching a file's mode or mtime changes nothing; changing its bytes changes everything.
        os.chmod(self.root / 'go.mod', 0o755)
        os.utime(self.root / 'go.mod', (1_700_000_000, 1_700_000_000))
        _, third = tool.build(self.root, Path(self.temp.name) / 'three', 'usejelto/docs', COMMIT)
        self.assertEqual(archive.read_bytes(), third.read_bytes())
        (self.root / 'go.mod').write_bytes(b'module changed\n')
        fourth, changed = tool.build(self.root, Path(self.temp.name) / 'four', 'usejelto/docs', COMMIT)
        self.assertNotEqual(archive.read_bytes(), changed.read_bytes())
        self.assertNotEqual(first['files']['go.mod'], fourth['files']['go.mod'])

    def test_every_include_entry_must_name_a_file_inside_the_root(self):
        out = Path(self.temp.name) / 'out'
        config = self.root / tool.CONFIG
        config.write_text(json.dumps({'component': 'docs', 'include': ['dist', 'LICENSE']}))
        with self.assertRaisesRegex(tool.PackageError, "'LICENSE' matches no file"):
            tool.build(self.root, out, commit=COMMIT)
        config.write_text(json.dumps({'component': 'docs', 'include': ['dist', '../secret']}))
        with self.assertRaisesRegex(tool.PackageError, 'relative path inside the root'):
            tool.build(self.root, out, commit=COMMIT)
        config.write_text(json.dumps({'component': 'docs', 'include': ['dist', '*.nothing']}))
        with self.assertRaisesRegex(tool.PackageError, "'\\*.nothing' matches no file"):
            tool.build(self.root, out, commit=COMMIT)
        config.write_text(json.dumps({'component': 'Docs', 'include': ['dist']}))
        with self.assertRaisesRegex(tool.PackageError, 'lowercase name'):
            tool.build(self.root, out, commit=COMMIT)
        config.write_text(json.dumps({'component': 'docs', 'include': ['dist'], 'version': '2'}))
        with self.assertRaisesRegex(tool.PackageError, 'unknown key'):
            tool.build(self.root, out, commit=COMMIT)
        config.write_text(json.dumps({'component': 'docs', 'include': ['dist']}))
        (self.root / 'package.json').write_text(json.dumps({'version': '1.0'}))
        with self.assertRaisesRegex(tool.PackageError, 'must be SemVer'):
            tool.build(self.root, out, commit=COMMIT)
        (self.root / 'package.json').write_text(json.dumps({'version': '1.0.0'}))
        with self.assertRaisesRegex(tool.PackageError, 'full 40-hex'):
            tool.build(self.root, out, commit='abc1234')
        self.assertFalse(out.exists() and any(out.iterdir()) and (out / 'docs.json').exists())

    def test_symlinks_are_rejected(self):
        try:
            os.symlink(self.root / 'go.mod', self.root / 'dist/link.mod')
        except (OSError, NotImplementedError):
            self.skipTest('symlinks unavailable here')
        with self.assertRaisesRegex(tool.PackageError, 'is a symlink'):
            tool.build(self.root, Path(self.temp.name) / 'out', commit=COMMIT)


class FakeRegistry:
    """oras as a dictionary: tags -> digests, digests -> the pushed files."""

    def __init__(self):
        self.tags, self.artifacts, self.pushes = {}, {}, []

    def run(self, command, cwd=None, capture_output=False, text=False, **_):
        verb, rest = command[1], command[2:]
        if verb == 'resolve':
            found = self.tags.get(rest[0])
            return subprocess.CompletedProcess(command, 0 if found else 1, found or '',
                                               '' if found else f'Error: {rest[0]}: not found')
        if verb == 'pull':
            files = self.artifacts.get(rest[0].split('@', 1)[1])
            if files is None:
                return subprocess.CompletedProcess(command, 1, '', 'not found')
            for name, data in files.items():
                (Path(rest[2]) / name).write_bytes(data)
            return subprocess.CompletedProcess(command, 0, '', '')
        if verb == 'push':
            name, tags = rest[0].split(':', 1)
            files = {item.split(':')[0]: (Path(cwd) / item.split(':')[0]).read_bytes()
                     for item in rest if ':' in item and not item.startswith('--')
                     and (Path(cwd) / item.split(':')[0]).is_file()}
            digest = 'sha256:' + hashlib.sha256(b''.join(sorted(files.values()))).hexdigest()
            self.artifacts[digest] = files
            for tag in tags.split(','):
                self.tags[f'{name}:{tag}'] = digest
            self.pushes.append(command)
            return subprocess.CompletedProcess(command, 0, f'Digest: {digest}', '')
        if verb == 'tag':
            self.tags[rest[0].split('@')[0] + ':' + rest[1]] = rest[0].split('@', 1)[1]
            return subprocess.CompletedProcess(command, 0, '', '')
        raise AssertionError(command)


class PublishTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix='jelto-package-component-')
        self.addCleanup(self.temp.cleanup)
        self.root = producer(Path(self.temp.name) / 'docs')
        self.out = Path(self.temp.name) / 'out'
        self.registry = FakeRegistry()
        which = patch.object(tool.shutil, 'which', return_value='/usr/bin/oras')
        which.start()
        self.addCleanup(which.stop)

    def publish(self, **overrides):
        options = {'repository': 'usejelto/docs', 'commit': COMMIT, 'run': self.registry.run}
        options.update(overrides)
        return tool.publish(self.root, self.out, 'ghcr.io/usejelto', **options)

    def test_pushes_exactly_the_pair_with_both_tags_annotations_and_a_published_manifest(self):
        done = self.publish()
        (command,) = self.registry.pushes
        self.assertEqual(command[2], f'ghcr.io/usejelto/docs-package:1.0.0,sha-{COMMIT}')
        self.assertEqual(command[3:7], ['--image-spec', 'v1.1', '--artifact-type', tool.ARTIFACT_TYPE])
        self.assertIn('org.opencontainers.image.source=https://github.com/usejelto/docs', command)
        self.assertIn(f'org.opencontainers.image.revision={COMMIT}', command)
        self.assertEqual(command[-2:], ['jelto-docs-1.0.0.tar.gz:application/gzip', 'docs.json:application/json'])
        pushed = self.registry.artifacts[done['digest']]
        self.assertEqual(set(pushed), {'jelto-docs-1.0.0.tar.gz', 'docs.json'})
        manifest = json.loads(pushed['docs.json'])
        self.assertTrue(manifest['published'])
        self.assertEqual((manifest['repository'], manifest['source_commit']), ('usejelto/docs', COMMIT))
        self.assertNotIn('built_at', manifest)
        self.assertEqual(done['ref'], f'ghcr.io/usejelto/docs-package@{done["digest"]}')
        self.assertFalse(done['reused'])

    def test_republishing_identical_content_reuses_and_different_content_collides(self):
        first = self.publish()
        again = self.publish()
        self.assertTrue(again['reused'])
        self.assertEqual(again['digest'], first['digest'])
        self.assertEqual(len(self.registry.pushes), 1)
        (self.root / 'dist/index.html').write_bytes(b'<html>changed')
        with self.assertRaisesRegex(tool.PackageError, 'already holds a different package'):
            self.publish()
        self.assertEqual(len(self.registry.pushes), 1)
        # The same bytes from another commit are a different manifest: also a collision.
        (self.root / 'dist/index.html').write_bytes(b'<html>')
        with self.assertRaisesRegex(tool.PackageError, 'already holds a different package'):
            self.publish(commit='b' * 40)

    def test_a_missing_sha_tag_is_added_to_a_matching_existing_artifact(self):
        first = self.publish()
        del self.registry.tags[f'ghcr.io/usejelto/docs-package:sha-{COMMIT}']
        again = self.publish()
        self.assertTrue(again['reused'])
        self.assertEqual(self.registry.tags[f'ghcr.io/usejelto/docs-package:sha-{COMMIT}'], first['digest'])

    def test_unauthorized_resolve_is_not_treated_as_absent(self):
        def run(command, **kwargs):
            if command[1] == 'resolve':
                return subprocess.CompletedProcess(command, 1, '', 'Error: unauthorized: authentication required')
            return self.registry.run(command, **kwargs)
        with self.assertRaisesRegex(tool.PackageError, 'unauthorized'):
            self.publish(run=run)
        self.assertEqual(self.registry.pushes, [])

    def test_publish_needs_a_commit_and_a_repository(self):
        with patch.object(tool, 'git_head', return_value=None), patch.dict(os.environ, {}, clear=False):
            os.environ.pop('GITHUB_REPOSITORY', None)
            with self.assertRaisesRegex(tool.PackageError, 'needs the producer commit'):
                self.publish(commit=None)
            with self.assertRaisesRegex(tool.PackageError, 'needs the producer repository'):
                self.publish(repository=None)
        self.assertEqual(self.registry.pushes, [])


if __name__ == '__main__':
    unittest.main()
