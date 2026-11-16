#!/usr/bin/env python3
"""Exercise notice packaging without compiling or running native radio software."""

import hashlib
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import unittest


REPO = Path(__file__).resolve().parents[2]


def manifest(directory):
    paths = sorted(path for path in directory.rglob("*") if path.is_file())
    (directory / "SHA256SUMS").write_text("".join(
        f"{hashlib.sha256(path.read_bytes()).hexdigest()}  {path.relative_to(directory)}\n"
        for path in paths
    ))


class PackageNoticesTest(unittest.TestCase):
    def test_collector_selects_repository_toolchain_from_another_directory(self):
        with tempfile.TemporaryDirectory(prefix="open25-notice-toolchain-") as temporary:
            root = Path(temporary)
            shim = root / "bin"
            shim.mkdir()
            (shim / "go").write_text("""#!/bin/sh
if [ "$PWD" != "$PACKAGE_TEST_REPO" ] || [ "$CGO_ENABLED/$GOOS/$GOARCH" != 0/linux/arm64 ]; then
    printf '%s\\n' 'Go invoked outside the selected repository/toolchain context.' >&2
    exit 1
fi
exec "$PACKAGE_TEST_REAL_GO" "$@"
""")
            (shim / "go").chmod(0o755)
            output = root / "Go-dependencies.txt"
            subprocess.run(
                [sys.executable, str(REPO / "tools/framework/collect-go-notices.py"),
                 "--output", str(output)], cwd=root,
                env={**os.environ, "PATH": f"{shim}{os.pathsep}{os.environ['PATH']}",
                     "PACKAGE_TEST_REAL_GO": shutil.which("go"), "PACKAGE_TEST_REPO": str(REPO)},
                check=True, capture_output=True, text=True,
            )
            self.assertIn("===== Go go", output.read_text())

    def test_complete_package_keeps_component_notices_and_source(self):
        with tempfile.TemporaryDirectory(prefix="open25-package-notices-") as temporary:
            root = Path(temporary)
            native = root / "native-input"
            speech = root / "speech-input"
            for directory in (native, speech):
                (directory / "source").mkdir(parents=True)
                (directory / "licenses").mkdir()
            (native / "licenses/DVM-GPL-2.0.txt").write_bytes(
                (REPO / "patches/dvmhost/LICENSE").read_bytes()
            )
            (native / "source/dvmhost.tar.gz").write_bytes(b"synthetic native source fixture")
            (speech / "source/espeak-ng.tar.gz").write_bytes(b"synthetic speech source fixture")
            (speech / "licenses/speech-test-license.txt").write_text("synthetic speech notice")
            (speech / "espeak-ng").write_text("#!/bin/sh\nexit 1\n")
            (speech / "espeak-ng").chmod(0o755)
            manifest(native)
            manifest(speech)

            # Only the Go binary build is stubbed; notice collection uses the real
            # selected package graph, module files, and toolchain license files.
            shim = root / "bin"
            shim.mkdir()
            (shim / "go").write_text("""#!/bin/sh
if [ "$1" = build ]; then
    shift
    while [ "$#" -gt 0 ]; do
        if [ "$1" = -o ]; then
            shift
            printf '%s\\n' 'synthetic test executable' > "$1"
            exit 0
        fi
        shift
    done
    exit 1
fi
exec "$PACKAGE_TEST_REAL_GO" "$@"
""")
            (shim / "go").chmod(0o755)
            output = root / "package"
            subprocess.run(
                ["sh", "tools/framework/package.sh", str(output)], cwd=REPO,
                env={**os.environ, "PATH": f"{shim}{os.pathsep}{os.environ['PATH']}",
                     "PACKAGE_TEST_REAL_GO": shutil.which("go"),
                     "FRAMEWORK_NATIVE_DIR": str(native), "FRAMEWORK_SPEECH_DIR": str(speech)},
                check=True, capture_output=True, text=True,
            )
            for relative, original in {
                "LICENSE": "LICENSE", "THIRD_PARTY.md": "THIRD_PARTY.md",
                "LICENSE.lucide": "internal/frameworkui/LICENSE.lucide",
                "licenses/sipgo-LICENSE": "third_party/sipgo-LICENSE",
                "licenses/sipgo-dependency-LICENSES": "third_party/sipgo-dependency-LICENSES",
            }.items():
                self.assertEqual((output / relative).read_bytes(), (REPO / original).read_bytes())
            self.assertEqual((output / "native/source/dvmhost.tar.gz").read_bytes(),
                             (native / "source/dvmhost.tar.gz").read_bytes())
            self.assertEqual((output / "speech/licenses/speech-test-license.txt").read_bytes(),
                             (speech / "licenses/speech-test-license.txt").read_bytes())
            notices = (output / "licenses/Go-dependencies.txt").read_text()
            for component in ("github.com/emiago/sipgo@", "github.com/pion/webrtc/v4@",
                              "modernc.org/sqlite@", "===== Go go", "--- LICENSE-3RD-PARTY.md ---"):
                self.assertIn(component, notices)
            self.assertIn((REPO / "third_party/sipgo-LICENSE").read_text(), notices)
            self.assertNotIn(str(Path.home()), notices)


if __name__ == "__main__":
    unittest.main()
