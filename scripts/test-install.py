"""Exercise the published installer through curl | sh against a local HTTPS release."""

import base64
import functools
import hashlib
import http.server
import io
import os
from pathlib import Path
import platform
import shutil
import ssl
import subprocess
import tarfile
import tempfile
import threading
import unittest


ROOT = Path(__file__).resolve().parents[1]
TAG = "v0.1.0-alpha.99"
ORIGIN = "https://github.com/friesencr/transcoop/releases/download"
STUB = f'#!/bin/sh\necho "transcoop {TAG}"\n'.encode()


def signing_tool():
    candidates = [shutil.which("openssl")]
    if shutil.which("brew"):
        brew = subprocess.run(["brew", "--prefix", "openssl@3"], capture_output=True, text=True)
        if brew.returncode == 0:
            candidates.append(Path(brew.stdout.strip()) / "bin/openssl")
    for tool in candidates:
        if tool and Path(tool).is_file():
            help_text = subprocess.run([tool, "pkeyutl", "-help"], capture_output=True)
            if b"-rawin" in help_text.stderr + help_text.stdout:
                return tool
    raise RuntimeError("OpenSSL 3 is required for installer tests")


class AssetHandler(http.server.SimpleHTTPRequestHandler):
    def do_GET(self):
        self.server.requests.append(self.path)
        if self.path.endswith(".tar.gz") and self.server.barrier:
            self.server.barrier.wait(timeout=15)
        super().do_GET()

    def log_message(self, fmt, *args):
        pass


class InstallerTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        tmp = tempfile.TemporaryDirectory()
        cls.addClassCleanup(tmp.cleanup)
        root = Path(tmp.name)
        cls.openssl = signing_tool()
        cls.signer = root / "signing.pem"
        subprocess.run([cls.openssl, "genpkey", "-algorithm", "ED25519", "-out", cls.signer], check=True)
        der = subprocess.check_output([cls.openssl, "pkey", "-in", cls.signer, "-pubout", "-outform", "DER"])
        cls.public = base64.b64encode(der).decode()
        cls.cert, cls.tls_key = root / "loopback.pem", root / "loopback.key"
        subprocess.run([cls.openssl, "req", "-x509", "-newkey", "rsa:2048", "-noenc",
                        "-subj", "/CN=localhost", "-addext", "subjectAltName=IP:127.0.0.1",
                        "-days", "1", "-keyout", cls.tls_key, "-out", cls.cert],
                       check=True, stderr=subprocess.DEVNULL)
        cls.native = root / "transcoop"
        flags = f"-s -w -X main.version={TAG} -X github.com/friesencr/transcoop/internal/update.PublicKey={cls.public}"
        subprocess.run(["go", "build", "-trimpath", "-ldflags", flags, "-o", cls.native, "./cmd/transcoop"],
                       env=dict(os.environ, CGO_ENABLED="0"), cwd=ROOT, check=True)

    def setUp(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        self.root = Path(tmp.name)
        self.home = self.root / "home with spaces"
        self.target = self.home / ".local/bin/transcoop"
        self.tools = self.root / "tools"
        self.tools.mkdir()
        for name in ("awk", "chmod", "curl", "grep", "gzip", "ln", "mkdir", "mktemp", "rm", "sh", "tar"):
            (self.tools / name).symlink_to(shutil.which(name))
        (self.tools / "openssl").symlink_to(self.openssl)
        uname = self.tools / "uname"
        uname.write_text('#!/bin/sh\ncase "$1" in -s) echo "$TEST_OS";; -m) echo "$TEST_ARCH";; esac\n')
        uname.chmod(0o755)
        self.env = dict(os.environ, PATH=str(self.tools), HOME=str(self.home), CURL_CA_BUNDLE=str(self.cert),
                        NO_PROXY="127.0.0.1", no_proxy="127.0.0.1", TEST_OS=platform.system(),
                        TEST_ARCH=platform.machine())
        self.os = {"Darwin": "darwin", "Linux": "linux"}[platform.system()]
        self.arch = {"x86_64": "amd64", "amd64": "amd64", "arm64": "arm64", "aarch64": "arm64"}[platform.machine()]
        feed = self.root / "releases"
        self.assets = feed / TAG
        self.assets.mkdir(parents=True)
        server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), functools.partial(AssetHandler, directory=feed))
        self.addCleanup(server.server_close)
        tls = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
        tls.load_cert_chain(self.cert, self.tls_key)
        server.socket = tls.wrap_socket(server.socket, server_side=True)
        server.requests = []
        server.barrier = None
        self.server = server
        self.url = f"https://127.0.0.1:{server.server_port}"
        self.script = self.assets / "install.sh"
        template = (ROOT / "scripts/install.sh").read_text()
        self.script.write_text(template.replace("@TRANSCOOP_VERSION@", TAG)
                               .replace("@TRANSCOOP_UPDATE_PUBLIC_KEY@", self.public).replace(ORIGIN, self.url))
        threading.Thread(target=server.serve_forever, kwargs={"poll_interval": 0.02}, daemon=True).start()
        self.addCleanup(server.shutdown)

    def sign(self, manifest):
        sums = self.assets / "SHA256SUMS"
        sums.write_text(manifest)
        subprocess.run([self.openssl, "pkeyutl", "-sign", "-rawin", "-inkey", self.signer,
                        "-in", sums, "-out", self.assets / "SHA256SUMS.sig"], check=True)

    def release(self, data=STUB, member="transcoop", os_name=None, arch=None):
        stem = f"transcoop_{TAG}_{os_name or self.os}_{arch or self.arch}"
        archive = self.assets / (stem + ".tar.gz")
        with tarfile.open(archive, "w:gz", compresslevel=1) as tar:
            item = tarfile.TarInfo(f"{stem}/{member}")
            item.size = len(data)
            tar.addfile(item, io.BytesIO(data))
        self.sign("".join(f"{hashlib.sha256(p.read_bytes()).hexdigest()}  {p.name}\n"
                          for p in (archive, self.script)))
        return archive

    def start_install(self):
        return subprocess.Popen(["/bin/sh", "-c", 'curl --disable -fsSL --proto "=https" --proto-redir "=https" "$1" | sh',
                                 "installer-test", f"{self.url}/{TAG}/install.sh"], env=self.env,
                                stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True)

    def run_install(self, message, *, success=True):
        with self.start_install() as process:
            output, _ = process.communicate(timeout=30)
        self.assertEqual(process.returncode == 0, success, output)
        self.assertIn(message, output)
        self.assertEqual(list(self.target.parent.glob(".transcoop-install.*")), [])
        if not success:
            self.assertFalse(self.target.exists())

    def test_native_install_and_rerun(self):
        self.release(self.native.read_bytes())
        self.run_install('"$HOME/.local/bin/transcoop" setup')
        self.assertEqual(self.target.read_bytes(), self.native.read_bytes())
        self.assertEqual(subprocess.check_output([self.target, "version"], text=True).strip(), f"transcoop {TAG}")
        self.server.requests.clear()
        self.run_install(str(self.target))
        self.assertEqual(self.server.requests, [f"/{TAG}/install.sh"])

    def test_platforms(self):
        for system, machine, os_name, arch in [("Linux", "x86_64", "linux", "amd64"),
                                               ("Linux", "aarch64", "linux", "arm64"),
                                               ("Darwin", "x86_64", "darwin", "amd64"),
                                               ("Darwin", "arm64", "darwin", "arm64")]:
            with self.subTest(system=system, machine=machine):
                self.env.update(TEST_OS=system, TEST_ARCH=machine)
                archive = self.release(os_name=os_name, arch=arch)
                self.run_install(f"Installed transcoop {TAG}")
                self.assertEqual(self.server.requests[-1], f"/{TAG}/{archive.name}")
                self.target.unlink()

    def test_verification_failures(self):
        archive = self.release()
        (self.assets / "SHA256SUMS.sig").write_bytes(bytes(64))
        self.run_install("signature could not be verified", success=False)
        self.assertFalse(any(path.endswith(".tar.gz") for path in self.server.requests))
        for manifest in ("0" * 64 + "  other.tar.gz\n", ("0" * 64 + f"  {archive.name}\n") * 2):
            self.sign(manifest)
            self.run_install("Expected one signed SHA-256", success=False)
        self.sign("0" * 64 + f"  {archive.name}\n")
        self.run_install("checksum mismatch", success=False)
        archive = self.release()
        archive.write_bytes(archive.read_bytes()[:20])
        self.run_install("checksum mismatch", success=False)
        self.release(member="other")
        self.run_install("Cannot extract", success=False)
        self.release(data=b'#!/bin/sh\necho "transcoop dev"\n')
        self.run_install("did not report", success=False)

    def test_requirements_and_partial_script(self):
        self.env["TEST_ARCH"] = "i386"
        self.run_install("Unsupported architecture", success=False)
        self.env["TEST_OS"] = "FreeBSD"
        self.run_install("Unsupported operating system", success=False)
        self.env.update(TEST_OS=platform.system(), TEST_ARCH=platform.machine())
        (self.tools / "openssl").unlink()
        self.run_install("Install OpenSSL 3", success=False)
        self.assertFalse(self.target.parent.exists())
        self.script.write_text(self.script.read_text().rsplit("\ninstall_transcoop", 1)[0])
        self.run_install("")
        self.assertFalse(self.target.parent.exists())

    def test_existing_alpha_on_path(self):
        legacy = self.tools / "transcoop"
        legacy.write_text('#!/bin/sh\necho "transcoop v0.1.0-alpha.1"\n')
        legacy.chmod(0o755)
        original = legacy.read_bytes()
        self.run_install("For v0.1.0-alpha.1")
        self.assertEqual(legacy.read_bytes(), original)
        self.assertFalse(self.target.exists())
        self.assertEqual(self.server.requests, [f"/{TAG}/install.sh"])

    def test_dangling_symlink(self):
        self.target.parent.mkdir(parents=True)
        destination = self.root / "missing"
        self.target.symlink_to(destination)
        self.run_install("install path is occupied", success=False)
        self.assertEqual(self.target.readlink(), destination)
        self.assertFalse(destination.exists())

    def test_homebrew_openssl(self):
        self.release()
        system_ssl = self.tools / "openssl"
        system_ssl.unlink()
        system_ssl.write_text("#!/bin/sh\necho 'usage: openssl pkeyutl' >&2\nexit 1\n")
        system_ssl.chmod(0o755)
        prefix = self.root / "brew prefix"
        (prefix / "bin").mkdir(parents=True)
        (prefix / "bin/openssl").symlink_to(self.openssl)
        brew = self.tools / "brew"
        brew.write_text('#!/bin/sh\n[ "$1" = --prefix ] && [ "$2" = openssl@3 ] && echo "$TEST_PREFIX"\n')
        brew.chmod(0o755)
        self.env["TEST_PREFIX"] = str(prefix)
        self.run_install(f"Installed transcoop {TAG}")

    def test_interrupted_extraction(self):
        self.release()
        tar = self.tools / "tar"
        tar.unlink()
        tar.write_text('#!/bin/sh\nkill -TERM "$PPID"\n')
        tar.chmod(0o755)
        self.run_install("", success=False)

    def test_concurrent_install(self):
        self.release()
        self.server.barrier = threading.Barrier(2)
        with self.start_install() as first, self.start_install() as second:
            output = [proc.communicate(timeout=30)[0] for proc in (first, second)]
        self.assertEqual((first.returncode, second.returncode), (0, 0), output)
        self.assertEqual(sum("Installed transcoop" in out for out in output), 1, output)
        self.assertEqual(sum("already installed" in out for out in output), 1, output)
        self.assertEqual(list(self.target.parent.iterdir()), [self.target])
        self.assertEqual(self.target.read_bytes(), STUB)


if __name__ == "__main__":
    unittest.main(verbosity=2)
