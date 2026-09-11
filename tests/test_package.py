import hashlib
import importlib.util
import json
from pathlib import Path
import subprocess
import tempfile
import sys
import unittest
import zipfile

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))
from package import build_archive


class ContractsPackageTests(unittest.TestCase):
    def test_archive_is_reproducible_self_describing_and_excludes_backend(self):
        root = Path(__file__).resolve().parent.parent
        with tempfile.TemporaryDirectory(prefix='jelto-contract-package-') as temp:
            first = build_archive(root, Path(temp) / 'one')
            second = build_archive(root, Path(temp) / 'two')
            self.assertEqual(first.read_bytes(), second.read_bytes())
            with zipfile.ZipFile(first) as archive:
                manifest = json.loads(archive.read('manifest.json'))
                self.assertEqual(set(archive.namelist()), set(manifest['files']) | {'manifest.json'})
                for name, checksum in manifest['files'].items():
                    self.assertEqual(hashlib.sha256(archive.read(name)).hexdigest(), checksum)
                    self.assertFalse(name.startswith(('internal/', 'sdk/', 'docs/', 'web/')), name)
                self.assertIn('spec/conformance/runner/main.go', manifest['files'])
                self.assertIn('spec/wire-v1.schema.json', manifest['files'])
                for name in ('LICENSE', 'CONTRIBUTING.md', 'CODE_OF_CONDUCT.md',
                             'SECURITY.md', 'SUPPORT.md', '.github/PULL_REQUEST_TEMPLATE.md',
                             '.github/ISSUE_TEMPLATE/bug_report.yml',
                             '.github/ISSUE_TEMPLATE/feature_request.yml',
                             '.github/ISSUE_TEMPLATE/config.yml'):
                    self.assertEqual(archive.read(name), (root / name).read_bytes())
                self.assertNotIn('internal/', archive.namelist())
                self.assertNotIn('replace ', archive.read('go.mod').decode())
                isolated = Path(temp) / 'isolated'
                archive.extractall(isolated)
            # A release must carry its own packager and every declared input.
            # Repacking from an unrelated working directory must be identical.
            subprocess.run([sys.executable, str(isolated / 'package.py'), '--output',
                            str(Path(temp) / 'repacked')], cwd=temp, check=True, capture_output=True)
            repacked = Path(temp) / 'repacked' / first.name
            self.assertEqual(first.read_bytes(), repacked.read_bytes())

    def test_installer_checks_archive_and_files_before_writing(self):
        root = Path(__file__).resolve().parent.parent
        spec = importlib.util.spec_from_file_location('contract_installer', root / 'install.py')
        installer = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(installer)
        with tempfile.TemporaryDirectory(prefix='jelto-contract-install-') as temp:
            temp = Path(temp)
            archive = build_archive(root, temp / 'packages')
            checksum = hashlib.sha256(archive.read_bytes()).hexdigest()
            destination = temp / 'installed'
            with self.assertRaisesRegex(ValueError, 'SHA-256'):
                installer.install(archive, '0' * 64, destination)
            self.assertFalse(destination.exists())
            installer.install(archive, checksum, destination)
            self.assertTrue((destination / 'go.mod').is_file())
            self.assertTrue((destination / 'spec/conformance/runner/main.go').is_file())
            with self.assertRaisesRegex(ValueError, 'empty'):
                installer.install(archive, checksum, destination)
            unsafe = temp / 'unsafe.zip'
            with zipfile.ZipFile(unsafe, 'w') as output:
                output.writestr('../outside', 'escape')
            with self.assertRaisesRegex(ValueError, 'Unsafe'):
                installer.install(unsafe, hashlib.sha256(unsafe.read_bytes()).hexdigest(), temp / 'unsafe')
            self.assertFalse((temp / 'outside').exists())


if __name__ == '__main__':
    unittest.main()
