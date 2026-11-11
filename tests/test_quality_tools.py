import hashlib
import importlib.util
import io
import json
import subprocess
import tarfile
import tempfile
import unittest
from pathlib import Path
from unittest import mock

ROOT = Path(__file__).resolve().parents[1]
QUALITY = ROOT / 'tools' / 'quality'

spec = importlib.util.spec_from_file_location('install_tools', QUALITY / 'install_tools.py')
install_tools = importlib.util.module_from_spec(spec)
spec.loader.exec_module(install_tools)
spec = importlib.util.spec_from_file_location('lint_baseline', QUALITY / 'lint_baseline.py')
lint_baseline = importlib.util.module_from_spec(spec)
spec.loader.exec_module(lint_baseline)


def tarball(name, data):
    buffer = io.BytesIO()
    with tarfile.open(fileobj=buffer, mode='w:gz') as archive:
        info = tarfile.TarInfo(f'pkg-1.0/{name}')
        info.size = len(data)
        archive.addfile(info, io.BytesIO(data))
    return buffer.getvalue()


class PinnedManifestTest(unittest.TestCase):
    def test_every_pin_is_complete(self):
        tools = install_tools.load_manifest()
        self.assertIn('gitleaks', tools)
        for name, tool in tools.items():
            for target, asset in tool.get('assets', {}).items():
                self.assertTrue(asset['url'].startswith('https://'), (name, target))
                self.assertRegex(asset['sha256'], r'^[0-9a-f]{64}$', (name, target))

    def test_every_binary_tool_covers_the_ci_and_developer_platforms(self):
        for name, tool in install_tools.load_manifest().items():
            if 'assets' in tool:
                self.assertLessEqual({'linux-x86_64', 'darwin-arm64'}, set(tool['assets']), name)


class InstallTest(unittest.TestCase):
    def setUp(self):
        self.cache = tempfile.TemporaryDirectory()
        self.addCleanup(self.cache.cleanup)
        self.environment = mock.patch.dict('os.environ', {'JELTO_TOOLS_DIR': self.cache.name})
        self.environment.start()
        self.addCleanup(self.environment.stop)

    def tools(self, payload, sha256=None):
        return {'demo': {
            'version': '1.0.0', 'binary': 'demo',
            'assets': {install_tools.current_platform(): {
                'url': 'https://example.invalid/demo-1.0.0.tar.gz',
                'sha256': sha256 or hashlib.sha256(payload).hexdigest()}}}}

    def test_installs_the_named_binary_after_the_checksum_matches(self):
        payload = tarball('demo', b'#!/bin/sh\necho demo\n')
        with mock.patch.object(install_tools, 'download', return_value=payload):
            path = install_tools.install('demo', self.tools(payload))
        self.assertEqual(path.read_bytes(), b'#!/bin/sh\necho demo\n')
        self.assertTrue(path.stat().st_mode & 0o100)

    def test_refuses_a_download_whose_checksum_differs(self):
        payload = tarball('demo', b'tampered')
        with mock.patch.object(install_tools, 'download', return_value=payload):
            with self.assertRaisesRegex(install_tools.ToolError, 'does not match the pin'):
                install_tools.install('demo', self.tools(payload, sha256='0' * 64))
        self.assertEqual(list(Path(self.cache.name).rglob('demo')), [Path(self.cache.name) / 'demo'],
                         'a refused download must leave no binary behind')

    def test_a_second_call_reuses_the_installed_copy(self):
        payload = tarball('demo', b'x')
        with mock.patch.object(install_tools, 'download', return_value=payload) as fetch:
            install_tools.install('demo', self.tools(payload))
            install_tools.install('demo', self.tools(payload))
        self.assertEqual(fetch.call_count, 1)

    def test_refuses_an_unpinned_platform(self):
        tools = {'demo': {'version': '1.0.0', 'binary': 'demo', 'assets': {'plan9-mips': {
            'url': 'https://example.invalid/x', 'sha256': '0' * 64}}}}
        with self.assertRaisesRegex(install_tools.ToolError, 'no pinned asset'):
            install_tools.install('demo', tools)

    def test_refuses_an_archive_without_exactly_one_binary(self):
        payload = tarball('other', b'x')
        with mock.patch.object(install_tools, 'download', return_value=payload):
            with self.assertRaisesRegex(install_tools.ToolError, 'expected one file named demo'):
                install_tools.install('demo', self.tools(payload))


class ManifestValidationTest(unittest.TestCase):
    def check(self, tools):
        with tempfile.TemporaryDirectory() as directory:
            manifest = Path(directory) / 'tools.json'
            manifest.write_text(json.dumps({'tools': tools}))
            with mock.patch.object(install_tools, 'MANIFEST', manifest):
                return install_tools.load_manifest()

    def test_rejects_plain_http(self):
        with self.assertRaisesRegex(install_tools.ToolError, 'HTTPS'):
            self.check({'x': {'version': '1', 'binary': 'x', 'assets': {'linux-x86_64': {
                'url': 'http://example.invalid/x', 'sha256': '0' * 64}}}})

    def test_rejects_a_git_tool_pinned_by_branch(self):
        with self.assertRaisesRegex(install_tools.ToolError, 'full commit id'):
            self.check({'x': {'version': 'main', 'git': 'https://example.invalid/x.git'}})


class CommitMessageHookTest(unittest.TestCase):
    def verdict(self, message):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / 'COMMIT_EDITMSG'
            path.write_text(message)
            return subprocess.run(['sh', str(QUALITY / 'githooks' / 'commit-msg'), str(path)],
                                  capture_output=True, text=True, cwd=directory).returncode

    def test_accepts_conventional_subjects(self):
        for subject in ['fix(query): keep a failed read out of the zero bucket',
                        'feature(tools): pin the quality tools',
                        'spec(public-site): v0.16 -- page scrolls pass over the demo preview',
                        'chore: release 1.2.3',
                        'feat(api)!: drop the v0 route',
                        'chore(deps, ci): bump both']:
            self.assertEqual(self.verdict(subject + '\n\nBody.\n'), 0, subject)

    def test_rejects_other_subjects(self):
        for subject in ['Fix dashboard loading rows', 'fix: ', 'fix(Query): x', 'update(x): y',
                        'fix(query) missing colon', 'fix(dashboard v1.21): spaces in the scope']:
            self.assertEqual(self.verdict(subject + '\n'), 1, subject)

    def test_skips_comments_and_git_written_subjects(self):
        self.assertEqual(self.verdict('# Please enter the commit message\n\nfix(x): y\n'), 0)
        for subject in ["Merge branch 'main'", 'Revert "fix(x): y"', 'fixup! fix(x): y']:
            self.assertEqual(self.verdict(subject + '\n'), 0, subject)


class LintBaselineTest(unittest.TestCase):
    def setUp(self):
        directory = tempfile.TemporaryDirectory()
        self.addCleanup(directory.cleanup)
        self.root = Path(directory.name)
        self.cwd = mock.patch('pathlib.Path.cwd', return_value=self.root)
        self.cwd.start()
        self.addCleanup(self.cwd.stop)

    def report(self, *issues):
        path = self.root / 'report.json'
        path.write_text(json.dumps({'Issues': [
            {'FromLinter': linter, 'Text': text, 'Pos': {'Filename': name, 'Line': line}}
            for name, line, linter, text in issues]}))
        return path

    def run_check(self, report, baseline):
        with mock.patch('sys.stderr'), mock.patch('sys.stdout'):
            return lint_baseline.main(['check', str(report), str(baseline)])

    def test_a_finding_outside_the_baseline_fails(self):
        baseline = self.root / 'baseline.json'
        lint_baseline.write(lint_baseline.findings(self.report(('a.go', 3, 'errcheck', 'x')))[0], baseline)
        self.assertEqual(self.run_check(self.report(('a.go', 3, 'errcheck', 'x'), ('b.go', 1, 'unused', 'y')), baseline), 1)

    def test_known_findings_pass_even_when_their_lines_move(self):
        baseline = self.root / 'baseline.json'
        with mock.patch('sys.stdout'):
            lint_baseline.write(lint_baseline.findings(self.report(('a.go', 3, 'errcheck', 'x')))[0], baseline)
        self.assertEqual(self.run_check(self.report(('a.go', 40, 'errcheck', 'x')), baseline), 0)

    def test_a_fixed_finding_still_listed_fails_until_the_baseline_shrinks(self):
        baseline = self.root / 'baseline.json'
        with mock.patch('sys.stdout'):
            lint_baseline.write(lint_baseline.findings(self.report(('a.go', 3, 'errcheck', 'x')))[0], baseline)
        self.assertEqual(self.run_check(self.report(), baseline), 1)

    def test_a_second_occurrence_of_a_known_finding_is_new(self):
        baseline = self.root / 'baseline.json'
        with mock.patch('sys.stdout'):
            lint_baseline.write(lint_baseline.findings(self.report(('a.go', 3, 'errcheck', 'x')))[0], baseline)
        self.assertEqual(self.run_check(self.report(('a.go', 3, 'errcheck', 'x'), ('a.go', 9, 'errcheck', 'x')), baseline), 1)


class KnipBaselineTest(unittest.TestCase):
    def test_reads_knip_reports(self):
        with tempfile.TemporaryDirectory() as directory:
            report = Path(directory) / 'knip.json'
            report.write_text(json.dumps({'issues': [{
                'file': 'src/a.ts', 'exports': [{'name': 'unused', 'line': 4}], 'files': [],
                'duplicates': [[{'name': 'a', 'line': 1}, {'name': 'b', 'line': 2}]]}]}))
            with mock.patch('pathlib.Path.cwd', return_value=Path(directory)):
                found, _ = lint_baseline.findings(report)
        self.assertEqual(set(found), {('src/a.ts', 'knip:exports', 'unused'),
                                      ('src/a.ts', 'knip:duplicates', 'a|b')})


if __name__ == '__main__':
    unittest.main()
