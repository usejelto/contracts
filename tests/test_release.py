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
import urllib.error
import zipfile

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))
import release


def unsafe_zip(name, body=b'unsafe'):
    """A zip whose central directory carries `name` byte for byte.

    writestr() would normalise the platform separator on the way in, so on
    Windows a backslash fixture would never reach the archive.
    """
    data = io.BytesIO()
    with zipfile.ZipFile(data, 'w') as archive:
        entry = zipfile.ZipInfo('placeholder')
        entry.filename = name
        archive.writestr(entry, body)
    raw = data.getvalue()
    assert name.encode() in raw, name
    return raw


def npm_archive(value='1.2.3', body=b'export const ready = true'):
    files = {'package/package.json': json.dumps({
        'name': '@jelto/analytics', 'version': value, 'license': 'MIT',
        'exports': {'.': './dist/browser.js', './server': './dist/server.js'},
    }).encode(), 'package/LICENSE': b'MIT', 'package/dist/browser.js': body,
        'package/dist/server.js': b'export const server = true'}
    data = io.BytesIO()
    with tarfile.open(fileobj=data, mode='w:gz') as archive:
        for name, content in files.items():
            info = tarfile.TarInfo(name)
            info.size = len(content)
            archive.addfile(info, io.BytesIO(content))
    return data.getvalue()


class ReleaseTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        # A workflow run exports GITHUB_REF_TYPE, so the ancestry guard would otherwise read
        # the ref the runner happens to be on instead of the one a test selects.
        ref_type = patch.dict('os.environ', {'GITHUB_REF_TYPE': 'tag'})
        ref_type.start()
        self.addCleanup(ref_type.stop)
        self.root = Path(self.temp.name)
        self.repo = 'example/jelto-analytics'
        (self.root / 'release.json').write_text(json.dumps({'component': 'analytics', 'repository': self.repo}))
        self.package = {'name': '@jelto/analytics', 'version': '1.2.3', 'license': 'MIT',
                        'repository': {'url': 'git+https://github.com/' + self.repo + '.git'}}
        (self.root / 'package.json').write_text(json.dumps(self.package))
        (self.root / 'package-lock.json').write_text(json.dumps({
            'version': '1.2.3', 'packages': {'': {'version': '1.2.3'}}}))
        (self.root / 'LICENSE').write_text('MIT')

    def git(self, *args):
        return release.run('git', *args, cwd=self.root)

    def commit(self):
        self.git('init', '-b', 'main')
        self.git('add', '.')
        self.git('-c', 'user.name=Test', '-c', 'user.email=test@localhost', 'commit', '-m', 'Initial')
        self.git('update-ref', 'refs/remotes/origin/main', 'HEAD')
        self.git('tag', 'v1.2.3')

    def prepare(self):
        self.commit()
        (self.root / 'jelto-analytics-1.2.3.tgz').write_bytes(npm_archive())
        release.stage(self.root, 'v1.2.3', self.repo)

    def test_semver_stable_and_prereleases(self):
        for value in ['0.0.0', '1.2.3', '12.0.3-rc.1', '1.0.0-0', '1.0.0-a-1']:
            self.assertEqual(release.version(value), value)
        for value in ['1.2', '01.2.3', '1.2.3-01', '1.2.3-', '1.2.3+x', 'v1.2.3', '1.2.3\n', '１.2.3']:
            with self.subTest(value=value), self.assertRaises(ValueError):
                release.version(value)

    def test_tag_and_metadata_must_match(self):
        release.validate(self.root, 'v1.2.3', self.repo, ancestry=False)
        for tag, repo in [('v1.2.4', self.repo), ('v1.2.3', 'other/repo'), ('main', self.repo)]:
            with self.assertRaises(ValueError):
                release.validate(self.root, tag, repo, ancestry=False)
        self.package['repository']['url'] = 'git+https://github.com/private/backend.git'
        (self.root / 'package.json').write_text(json.dumps(self.package))
        with self.assertRaisesRegex(ValueError, 'repository.url'):
            release.validate(self.root, 'v1.2.3', self.repo, ancestry=False)

    def test_lockfile_mismatch_fails(self):
        (self.root / 'package-lock.json').write_text('{"version":"1.2.2","packages":{"":{"version":"1.2.2"}}}')
        with self.assertRaisesRegex(ValueError, 'lockfile'):
            release.identity(self.root)

    def test_branch_dirty_checkout_and_outside_main_fail(self):
        self.commit()
        release.validate(self.root, 'v1.2.3', self.repo)
        with patch.dict('os.environ', {'GITHUB_REF_TYPE': 'branch'}), self.assertRaisesRegex(ValueError, 'branch'):
            release.validate(self.root, 'v1.2.3', self.repo)
        (self.root / 'LICENSE').write_text('changed')
        with self.assertRaisesRegex(ValueError, 'tracked modifications'):
            release.validate(self.root, 'v1.2.3', self.repo)
        self.git('add', '.')
        self.git('-c', 'user.name=Test', '-c', 'user.email=test@localhost', 'commit', '-m', 'Outside main')
        self.git('tag', '-f', 'v1.2.3')
        with self.assertRaises(subprocess.CalledProcessError):
            release.validate(self.root, 'v1.2.3', self.repo)

    def test_stage_and_verify_reject_tampered_artifacts(self):
        self.prepare()
        record = release.verify(self.root, 'v1.2.3', self.repo)
        self.assertEqual(record['version'], '1.2.3')
        artifact = self.root / 'release-artifacts/jelto-analytics-1.2.3.tgz'
        artifact.write_bytes(npm_archive(body=b'tampered'))
        with self.assertRaisesRegex(ValueError, 'checksum'):
            release.verify(self.root, 'v1.2.3', self.repo)

    def test_registry_duplicates_only_skip_matching_bytes(self):
        self.prepare()
        record = release.verify(self.root, 'v1.2.3', self.repo)
        item = record['packages'][0]
        folder = self.root / 'release-artifacts'
        data = (folder / item['file']).read_bytes()
        with patch.object(release, 'registry_data', return_value=None):
            self.assertFalse(release.registry_matches(folder, item, '1.2.3'))
        with patch.object(release, 'registry_data', return_value=data):
            self.assertTrue(release.registry_matches(folder, item, '1.2.3'))
        with patch.object(release, 'registry_data', return_value=npm_archive(body=b'collision')):
            with self.assertRaisesRegex(ValueError, 'different contents'):
                release.registry_matches(folder, item, '1.2.3')

    def test_nuget_comparison_excludes_only_signature(self):
        def package(signature, assembly=b'assembly'):
            data = io.BytesIO()
            with zipfile.ZipFile(data, 'w') as archive:
                archive.writestr('lib/net8.0/Jelto.dll', assembly)
                if signature:
                    archive.writestr('.signature.p7s', signature)
            return data.getvalue()
        self.assertEqual(release.archive_files(package(None), 'nuget'),
                         release.archive_files(package(b'registry'), 'nuget'))
        self.assertNotEqual(release.archive_files(package(None), 'nuget'),
                            release.archive_files(package(b'registry', b'changed'), 'nuget'))

    def test_unsafe_archives_fail(self):
        for name in ['../escape', '/absolute', 'C:/file', 'folder\\file']:
            data = unsafe_zip(name)
            with self.assertRaises(ValueError):
                release.archive_files(data, 'zip')

    def test_backslash_entry_fails_where_the_platform_separator_is_a_backslash(self):
        # zipfile normalises os.sep to '/' in ZipInfo.filename while reading,
        # so on Windows the guard must look at the raw name or it accepts an
        # archive that Linux rejects. Simulate that reader here so every
        # platform's CI covers it.
        data = unsafe_zip('folder\\file')
        with patch.object(os, 'sep', '\\'):
            with zipfile.ZipFile(io.BytesIO(data)) as archive:
                self.assertEqual(archive.infolist()[0].filename, 'folder/file')
            with self.assertRaises(ValueError):
                release.archive_files(data, 'zip')
        data = io.BytesIO()
        with tarfile.open(fileobj=data, mode='w:gz') as archive:
            entry = tarfile.TarInfo('package/link')
            entry.type = tarfile.SYMTYPE
            entry.linkname = '../../secret'
            archive.addfile(entry)
        with self.assertRaises(ValueError):
            release.archive_files(data.getvalue(), 'npm')

    def test_cargo_archive_excludes_dependency_and_contracts_files(self):
        for unwanted in ['node_modules/README.md', '.contracts/README.md', 'example/LICENSE']:
            data = io.BytesIO()
            with tarfile.open(fileobj=data, mode='w:gz') as archive:
                entry = tarfile.TarInfo('tauri-plugin-jelto-1.0.0/' + unwanted)
                entry.size = 1
                archive.addfile(entry, io.BytesIO(b'x'))
            path = self.root / 'plugin.crate'
            path.write_bytes(data.getvalue())
            with self.assertRaisesRegex(ValueError, 'Unexpected file'):
                release.check_package(path, 'cargo', 'tauri-plugin-jelto', '1.0.0')

    def test_missing_pins_and_non_https_downloads_fail_before_network(self):
        with self.assertRaisesRegex(ValueError, 'Configure contracts'):
            release.install_contracts(self.root)
        for url in ['http://example.com/file', 'https://user:secret@example.com/file']:
            with self.assertRaises(ValueError):
                release.download(url)

    def test_transport_errors_are_not_missing_versions(self):
        error = urllib.error.HTTPError('https://example.com', 403, 'forbidden', {}, None)
        self.addCleanup(error.close)
        with patch('urllib.request.OpenerDirector.open', side_effect=error) as opened, \
                patch('release.time.sleep') as slept:
            with self.assertRaises(urllib.error.HTTPError):
                release.download('https://example.com', missing=True)
        # A definite client-side answer is final: no retry, no pause.
        self.assertEqual(opened.call_count, 1)
        slept.assert_not_called()

    def test_transient_download_failures_are_retried_with_backoff_then_given_up(self):
        class Response(io.BytesIO):
            def __enter__(self):
                return self

            def __exit__(self, *exc):
                return False

        reset = urllib.error.URLError(ConnectionResetError(104, 'Connection reset by peer'))
        unavailable = urllib.error.HTTPError('https://example.com', 503, 'unavailable', {}, None)
        self.addCleanup(unavailable.close)
        with patch('urllib.request.OpenerDirector.open', side_effect=[reset, unavailable, Response(b'archive')]) as opened, \
                patch('release.time.sleep') as slept:
            self.assertEqual(release.download('https://example.com/file'), b'archive')
        self.assertEqual(opened.call_count, 3)
        self.assertEqual([call.args[0] for call in slept.call_args_list],
                         [release.DOWNLOAD_PAUSE, release.DOWNLOAD_PAUSE * 2])
        with patch('urllib.request.OpenerDirector.open', side_effect=[reset, reset, reset]) as opened, \
                patch('release.time.sleep'):
            with self.assertRaisesRegex(RuntimeError, 'Download failed after 3 attempts'):
                release.download('https://example.com/file')
        self.assertEqual(opened.call_count, 3)
        missing = urllib.error.HTTPError('https://example.com', 404, 'missing', {}, None)
        self.addCleanup(missing.close)
        with patch('urllib.request.OpenerDirector.open', side_effect=missing) as opened, \
                patch('release.time.sleep') as slept:
            self.assertIsNone(release.download('https://example.com/file', missing=True))
        self.assertEqual(opened.call_count, 1)
        slept.assert_not_called()

    def test_artifact_record_cannot_omit_a_package(self):
        self.prepare()
        path = self.root / 'release-artifacts/release.json'
        record = json.loads(path.read_text())
        record['packages'] = []
        path.write_text(json.dumps(record))
        with self.assertRaisesRegex(ValueError, 'do not match'):
            release.verify(self.root, 'v1.2.3', self.repo)

    def test_wait_can_select_only_rust_for_partial_tauri_release(self):
        record = {'version': '1.0.0', 'packages': [
            {'kind': 'cargo', 'name': 'tauri-plugin-jelto'}, {'kind': 'npm', 'name': '@jelto/tauri'}]}
        with patch.object(release, 'verify', return_value=record), patch.object(release, 'registry_matches', return_value=True) as matches:
            release.registry_status(self.root, 'v1.0.0', self.repo, wait=True, kind='cargo')
            self.assertEqual(matches.call_count, 1)
            self.assertEqual(matches.call_args.args[1]['kind'], 'cargo')

    def test_wait_awaits_the_install_index_after_the_bytes(self):
        record = {'version': '1.0.1', 'packages': [{'kind': 'npm', 'name': '@jelto/crawler'}]}
        with patch.object(release, 'verify', return_value=record), \
                patch.object(release, 'registry_matches', return_value=True), \
                patch.object(release, 'registry_indexed', side_effect=[False, False, True]) as indexed, \
                patch.object(release.time, 'sleep') as sleep:
            release.registry_status(self.root, 'v1.0.1', self.repo, wait=True)
            self.assertEqual(indexed.call_count, 3)
            self.assertEqual(sleep.call_count, 2)
        with patch.object(release, 'verify', return_value=record), \
                patch.object(release, 'registry_matches', return_value=True), \
                patch.object(release, 'registry_indexed', return_value=False), \
                patch.object(release.time, 'sleep'):
            with self.assertRaisesRegex(ValueError, 'indexing timed out'):
                release.registry_status(self.root, 'v1.0.1', self.repo, wait=True)
        # status answers about the bytes alone: a published, not yet indexed version exists.
        with patch.object(release, 'verify', return_value=record), \
                patch.object(release, 'registry_matches', return_value=True), \
                patch.object(release, 'registry_indexed') as indexed:
            release.registry_status(self.root, 'v1.0.1', self.repo, wait=False)
            indexed.assert_not_called()

    def test_registry_indexed_reads_what_each_installer_resolves(self):
        bodies = {
            'https://registry.npmjs.org/%40jelto%2Fcrawler': json.dumps({'versions': {'1.0.0': {}, '1.0.1': {}}}).encode(),
            'https://index.crates.io/ta/ur/tauri-plugin-jelto':
                b'{"name":"tauri-plugin-jelto","vers":"1.0.0"}\n{"name":"tauri-plugin-jelto","vers":"1.0.1"}\n',
            'https://api.nuget.org/v3-flatcontainer/jelto/index.json': json.dumps({'versions': ['0.1.0']}).encode(),
        }
        calls = []

        def fake(url, missing=False, accept=None):
            calls.append((url, missing, accept))
            return bodies.get(url)

        with patch.object(release, 'download', side_effect=fake):
            self.assertTrue(release.registry_indexed({'kind': 'npm', 'name': '@jelto/crawler'}, '1.0.1'))
            self.assertFalse(release.registry_indexed({'kind': 'npm', 'name': '@jelto/crawler'}, '1.0.2'))
            self.assertTrue(release.registry_indexed({'kind': 'cargo', 'name': 'tauri-plugin-jelto'}, '1.0.1'))
            self.assertFalse(release.registry_indexed({'kind': 'cargo', 'name': 'tauri-plugin-jelto'}, '1.0.2'))
            self.assertTrue(release.registry_indexed({'kind': 'nuget', 'name': 'Jelto'}, '0.1.0'))
            self.assertFalse(release.registry_indexed({'kind': 'nuget', 'name': 'Jelto'}, '0.1.1'))
            self.assertFalse(release.registry_indexed({'kind': 'npm', 'name': '@jelto/missing'}, '1.0.0'))
        self.assertTrue(all(missing for _, missing, _ in calls))
        self.assertEqual(calls[0][2], 'application/vnd.npm.install-v1+json; q=1.0, application/json; q=0.8, */*')
        self.assertIsNone(calls[2][2])

    def test_completed_release_requires_all_registries(self):
        self.prepare()
        with patch.object(release, 'registry_data', return_value=None), self.assertRaisesRegex(ValueError, 'not available'):
            release.github_release(self.root, 'v1.2.3', self.repo)

    def test_missing_exports_fail_package_check(self):
        path = self.root / 'package.tgz'
        path.write_bytes(npm_archive(body=b''))
        with self.assertRaisesRegex(ValueError, 'Missing npm package entry'):
            release.check_package(path, 'npm', '@jelto/analytics', '1.2.3')

    def test_configure_preserves_package_identity(self):
        release.configure(self.root, 'owner/new-repo')
        self.assertEqual(release.identity(self.root), ('analytics', '1.2.3'))
        release.validate(self.root, 'v1.2.3', 'owner/new-repo', ancestry=False)

    def test_electron_metadata_links_to_the_existing_framework_guide(self):
        (self.root / 'release.json').write_text(json.dumps({'component': 'electron'}))
        self.package['name'] = '@jelto/electron'
        (self.root / 'package.json').write_text(json.dumps(self.package))
        release.configure(self.root, 'usejelto/jelto-electron')
        package = json.loads((self.root / 'package.json').read_text())
        self.assertEqual(package['homepage'], 'https://jelto.io/docs/sdk/electron-forge')
        self.assertEqual(package['bugs']['url'], 'https://github.com/usejelto/jelto-electron/issues')
        self.assertEqual((package['name'], package['version']), ('@jelto/electron', '1.2.3'))


if __name__ == '__main__':
    unittest.main()
