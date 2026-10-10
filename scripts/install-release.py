#!/usr/bin/env python3
"""Install a verified GitLab package and restore binary/metadata on failure."""
import argparse
import datetime
import fcntl
import hashlib
import os
from pathlib import Path
import platform
import re
import shutil
import subprocess
import tempfile
import time
import urllib.parse
import urllib.request


class SameOriginRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, request, fp, code, msg, headers, newurl):
        old = urllib.parse.urlsplit(request.full_url)
        new = urllib.parse.urlsplit(newurl)
        if (old.scheme, old.netloc) != (new.scheme, new.netloc):
            raise RuntimeError("Cross-origin download redirect rejected")
        return super().redirect_request(request, fp, code, msg, headers, newurl)


def download(url, destination, token):
    opener = urllib.request.build_opener(
        urllib.request.ProxyHandler({}), SameOriginRedirect()
    )
    request = urllib.request.Request(url, headers={"DEPLOY-TOKEN": token})
    with opener.open(request, timeout=60) as response, destination.open("wb") as output:
        shutil.copyfileobj(response, output)


def verify_checksum(binary, manifest):
    checksums = {}
    for line in manifest.read_text().splitlines():
        fields = line.split()
        if len(fields) != 2 or not re.fullmatch(r"[0-9a-f]{64}", fields[0]):
            raise RuntimeError("Invalid SHA256SUMS manifest")
        if fields[1] in checksums:
            raise RuntimeError("Duplicate checksum filename")
        checksums[fields[1]] = fields[0]
    with binary.open("rb") as source:
        actual = hashlib.file_digest(source, "sha256").hexdigest()
    if checksums.get(binary.name) != actual:
        raise RuntimeError("Binary checksum mismatch")


def command(*args, check=True):
    return subprocess.run(args, check=check, capture_output=True, text=True)


def wait_healthy(url):
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    for _ in range(20):
        if command("systemctl", "is-active", "--quiet", "content-cache", check=False).returncode == 0:
            try:
                with opener.open(url, timeout=1) as response:
                    if response.status == 200:
                        return
            except OSError:
                pass
        time.sleep(1)
    raise RuntimeError("content-cache did not become healthy")


def replace_binary(source, target):
    temporary = target.with_name(target.name + ".new")
    try:
        shutil.copyfile(source, temporary)
        temporary.chmod(0o755)
        with temporary.open("rb") as f:
            os.fsync(f.fileno())
        os.replace(temporary, target)
    finally:
        temporary.unlink(missing_ok=True)


def install(binary, target, metadata, backup_root, health_url):
    stamp = datetime.datetime.now(datetime.timezone.utc).strftime("%Y%m%dT%H%M%S%fZ")
    backup = backup_root / stamp
    backup.mkdir(parents=True, mode=0o700)
    backup.chmod(0o700)
    was_active = command("systemctl", "is-active", "--quiet", "content-cache", check=False).returncode == 0
    command("systemctl", "stop", "content-cache")
    replaced = False
    metadata_existed = metadata.exists()
    try:
        if target.exists():
            shutil.copy2(target, backup / "content-cache")
        if metadata_existed:
            # bbolt must be closed before copying its metadata database.
            shutil.copy2(metadata, backup / "metadata.db")
        replace_binary(binary, target)
        replaced = True
        command("systemctl", "restart", "content-cache")
        wait_healthy(health_url)
    except Exception:
        if replaced:
            command("systemctl", "stop", "content-cache")
            previous = backup / "content-cache"
            if previous.exists():
                replace_binary(previous, target)
            else:
                target.unlink(missing_ok=True)
            if metadata_existed:
                shutil.copy2(backup / "metadata.db", metadata)
            else:
                metadata.unlink(missing_ok=True)
        if was_active:
            command("systemctl", "start", "content-cache")
        raise
    return backup


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--gitlab-url", default="https://gitlab.campbox.tech")
    parser.add_argument("--project", required=True, help="Project ID or tlcamp/content-cache")
    parser.add_argument("--version", required=True, help="1.4.0-campbox.1")
    parser.add_argument("--binary-path", type=Path, default=Path("/usr/local/bin/content-cache"))
    parser.add_argument("--metadata-file", type=Path, default=Path("/var/lib/content-cache/metadata.db"))
    parser.add_argument("--backup-dir", type=Path, default=Path("/var/lib/content-cache/recovery/releases"))
    parser.add_argument("--health-url", default="http://172.17.0.1:4874/health")
    parser.add_argument("--dry-run", action="store_true", help="Download and verify without installing")
    args = parser.parse_args()
    args.version = args.version.removeprefix("v")
    if not re.fullmatch(r"[0-9]+\.[0-9]+\.[0-9]+-campbox\.[0-9]+", args.version):
        parser.error("Expected a version such as 1.4.0-campbox.1")
    if urllib.parse.urlsplit(args.gitlab_url).scheme != "https":
        parser.error("--gitlab-url must use HTTPS")
    token = os.environ.get("CONTENT_CACHE_DEPLOY_TOKEN")
    if not token:
        parser.error("CONTENT_CACHE_DEPLOY_TOKEN is required (read_package_registry)")
    arch = {"x86_64": "amd64", "aarch64": "arm64"}.get(platform.machine())
    if platform.system() != "Linux" or not arch:
        parser.error("Only Linux amd64/arm64 are supported")
    if not args.dry_run and os.geteuid() != 0:
        parser.error("Run as root to install the systemd service binary")
    base = args.gitlab_url.rstrip("/") + "/api/v4/projects/" + urllib.parse.quote(args.project, safe="")
    base += "/packages/generic/content-cache/" + args.version
    with tempfile.TemporaryDirectory(prefix="content-cache-install-") as directory:
        binary = Path(directory) / ("content-cache_linux_" + arch)
        manifest = Path(directory) / "SHA256SUMS"
        download(base + "/" + binary.name, binary, token)
        download(base + "/SHA256SUMS", manifest, token)
        verify_checksum(binary, manifest)
        binary.chmod(0o755)
        if command(str(binary), "--version").stdout.strip() != args.version:
            raise RuntimeError("Downloaded binary version does not match the requested release")
        if args.dry_run:
            print("Verified content-cache", args.version, arch, "(installation skipped)")
            return
        with open("/run/lock/content-cache-install.lock", "w") as lock:
            fcntl.flock(lock, fcntl.LOCK_EX)
            backup = install(binary, args.binary_path, args.metadata_file, args.backup_dir, args.health_url)
        print("Installed content-cache", args.version, "backup:", backup)


if __name__ == "__main__":
    try:
        main()
    except Exception as error:
        raise SystemExit("Installation failed: " + str(error)) from None
