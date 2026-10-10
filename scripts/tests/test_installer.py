import hashlib
import importlib.util
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location(
    "installer", Path(__file__).resolve().parents[1] / "install-release.py"
)
installer = importlib.util.module_from_spec(spec)
spec.loader.exec_module(installer)


class InstallerTests(unittest.TestCase):
    def test_integrity_failure_prevents_install(self):
        with tempfile.TemporaryDirectory() as directory:
            binary = Path(directory) / "content-cache_linux_amd64"
            binary.write_bytes(b"release")
            manifest = Path(directory) / "SHA256SUMS"
            manifest.write_text(hashlib.sha256(b"release").hexdigest() + "  " + binary.name + "\n")
            installer.verify_checksum(binary, manifest)
            binary.write_bytes(b"corrupted")
            with self.assertRaisesRegex(RuntimeError, "checksum mismatch"):
                installer.verify_checksum(binary, manifest)

    def test_failed_health_restores_binary_and_metadata(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            binary, target, metadata = [root / name for name in ["release", "installed", "metadata.db"]]
            binary.write_bytes(b"new binary")
            target.write_bytes(b"old binary")
            metadata.write_bytes(b"original metadata")
            calls = []

            def run(*args, **kwargs):
                calls.append(args)
                if args == ("systemctl", "restart", "content-cache"):
                    metadata.write_bytes(b"new incompatible metadata")
                return subprocess.CompletedProcess(args, 0)

            with patch.object(installer, "command", side_effect=run), patch.object(
                installer, "wait_healthy", side_effect=RuntimeError("unhealthy")
            ):
                with self.assertRaisesRegex(RuntimeError, "unhealthy"):
                    installer.install(binary, target, metadata, root / "backups", "http://unused")
            self.assertEqual(target.read_bytes(), b"old binary")
            self.assertEqual(metadata.read_bytes(), b"original metadata")
            self.assertEqual(calls[-1], ("systemctl", "start", "content-cache"))
            self.assertFalse(target.with_name("installed.new").exists())

    def test_success_keeps_cache_and_previous_release(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            binary, target, metadata = [root / name for name in ["release", "installed", "metadata.db"]]
            binary.write_bytes(b"new binary")
            target.write_bytes(b"old binary")
            metadata.write_bytes(b"existing metadata")
            with patch.object(installer, "command", return_value=subprocess.CompletedProcess([], 0)), patch.object(installer, "wait_healthy"):
                backup = installer.install(binary, target, metadata, root / "backups", "http://unused")
            self.assertEqual(target.read_bytes(), b"new binary")
            self.assertEqual(metadata.read_bytes(), b"existing metadata")
            self.assertEqual((backup / "content-cache").read_bytes(), b"old binary")
            self.assertEqual((backup / "metadata.db").read_bytes(), b"existing metadata")
