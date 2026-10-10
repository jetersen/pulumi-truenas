"""Exercise provider archive selection without compiling Go or contacting a NAS."""

import os
from pathlib import Path
import shutil
import subprocess
import tarfile
import tempfile
import unittest


class ProviderPackagingTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        (self.root / "scripts").mkdir()
        (self.root / "provider").mkdir()
        shutil.copyfile(Path(__file__).with_name("package.sh"), self.root / "scripts/package.sh")
        tools = self.root / "tools"
        tools.mkdir()
        compiler = tools / "go"
        compiler.write_text('''#!/usr/bin/env bash
set -euo pipefail
[[ "$1" == build ]]
while [[ "$1" != -o ]]; do shift; done
shift
mkdir -p "$(dirname "$1")"
printf '%s/%s cgo=%s\\n' "$GOOS" "$GOARCH" "$CGO_ENABLED" > "$1"
printf '%s/%s\\n' "$GOOS" "$GOARCH" >> "$PACKAGE_BUILD_LOG"
''')
        compiler.chmod(0o755)
        self.env = dict(os.environ)
        for name in ("PROVIDER_OS", "PROVIDER_ARCH", "PACKAGE_PROVIDER_ARCHIVES"):
            self.env.pop(name, None)
        self.env.update(PATH=f"{tools}:{os.environ['PATH']}", VERSION="1.2.3",
                        PACKAGE_BUILD_LOG=str(self.root / "builds"))

    def package(self, **filters):
        return subprocess.run(["bash", str(self.root / "scripts/package.sh"), "provider"],
                              env={**self.env, **filters}, capture_output=True, text=True)

    def assert_archives(self, targets):
        expected = {f"pulumi-resource-truenas-v1.2.3-{os_name}-{arch}.tar.gz"
                    for os_name, arch in targets}
        self.assertEqual({p.name for p in (self.root / "dist").glob("*.tar.gz")}, expected)
        self.assertEqual(set((self.root / "builds").read_text().splitlines()),
                         {f"{os_name}/{arch}" for os_name, arch in targets})
        for os_name, arch in targets:
            with tarfile.open(self.root / "dist" /
                              f"pulumi-resource-truenas-v1.2.3-{os_name}-{arch}.tar.gz") as archive:
                binary = "pulumi-resource-truenas" + (".exe" if os_name == "windows" else "")
                self.assertEqual(archive.getnames(), [binary])
                self.assertEqual(archive.extractfile(binary).read(),
                                 f"{os_name}/{arch} cgo=0\n".encode())

    def test_default_packages_all_six_targets(self):
        result = self.package()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assert_archives([(os_name, arch) for os_name in ("linux", "darwin", "windows")
                              for arch in ("amd64", "arm64")])

    def test_selected_target_produces_only_one_archive(self):
        result = self.package(PROVIDER_OS="windows", PROVIDER_ARCH="arm64")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assert_archives([("windows", "arm64")])

    def test_invalid_target_fails_before_compilation(self):
        for filters in ({"PROVIDER_OS": "freebsd"}, {"PROVIDER_ARCH": "386"}):
            with self.subTest(filters=filters):
                result = self.package(**filters)
                self.assertNotEqual(result.returncode, 0)
                self.assertIn("Unsupported provider", result.stderr)
                self.assertFalse((self.root / "builds").exists())


if __name__ == "__main__":
    unittest.main()
