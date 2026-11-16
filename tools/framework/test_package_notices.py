#!/usr/bin/env python3
"""Exercise notice packaging without compiling or running native radio software."""

import hashlib
import json
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
    def test_incomplete_native_licensing_is_refused_before_go_build(self):
        required = ("licenses/Open25-native-components.md", "licenses/LICENSE-CLARIFICATION.json",
                    "licenses/DVM-GPL-2.0.txt", "licenses/DVM-GPL-3.0-or-later.txt",
                    "source-provenance.json", "source/dvmhost.tar.gz", "source/asio.tar.gz")
        with tempfile.TemporaryDirectory(prefix="open25-incomplete-native-") as temporary:
            root = Path(temporary)
            native, speech = root / "native", root / "speech"
            for relative in required:
                path = native / relative
                path.parent.mkdir(parents=True, exist_ok=True)
                path.write_text("synthetic required payload file\n")
            speech.mkdir()
            (speech / "espeak-ng").write_text("#!/bin/sh\nexit 1\n")
            (speech / "espeak-ng").chmod(0o755)
            manifest(native)
            manifest(speech)
            shim = root / "bin"
            shim.mkdir()
            marker = root / "go-was-called"
            (shim / "go").write_text('#!/bin/sh\nprintf called > "$PACKAGE_TEST_GO_MARKER"\nexit 23\n')
            (shim / "go").chmod(0o755)
            for index, relative in enumerate(required):
                with self.subTest(missing=relative):
                    path = native / relative
                    contents = path.read_bytes()
                    path.unlink()
                    output = root / f"package-{index}"
                    try:
                        result = subprocess.run(
                            ["sh", "tools/framework/package.sh", str(output)], cwd=REPO,
                            env={**os.environ, "PATH": f"{shim}{os.pathsep}{os.environ['PATH']}",
                                 "PACKAGE_TEST_GO_MARKER": str(marker),
                                 "FRAMEWORK_NATIVE_DIR": str(native), "FRAMEWORK_SPEECH_DIR": str(speech)},
                            capture_output=True, text=True,
                        )
                    finally:
                        path.write_bytes(contents)
                    self.assertNotEqual(result.returncode, 0)
                    self.assertIn(f"Native payload lacks {relative}", result.stderr)
                    self.assertFalse(marker.exists(), "incomplete licensing reached the Go build")
                    self.assertFalse(output.exists(), "incomplete licensing created a package output")

    def test_native_provenance_distinguishes_git_source_from_nested_export(self):
        # Run the collector's production Python step without the Linux-only
        # ELF/library copy stages. This checks real Git metadata and archive
        # hashes, including an export nested inside an unrelated repository.
        script = (REPO / "tools/framework/collect-native.sh").read_text()
        marker = 'python3 - "$native_source" "$native_build" "$native_output" <<\'PY\'\n'
        step = script.split(marker, 1)[1].split("\nPY\n", 1)[0]
        with tempfile.TemporaryDirectory(prefix="open25-native-provenance-") as temporary:
            root = Path(temporary)
            parent = root / "unrelated-repository"
            parent.mkdir()
            source = parent / "source-export"
            source.mkdir()
            (source / "source.cpp").write_text("synthetic source\n")
            build = root / "build"
            build.mkdir()
            (build / "CMakeCache.txt").write_text("synthetic build metadata\n")
            payload = root / "payload"
            for part in ("source", "bin", "licenses"):
                (payload / part).mkdir(parents=True)
            (payload / "source/dvmhost.tar.gz").write_bytes(b"synthetic corresponding source archive")
            for name in ("dvmhost", "dvmfne", "dvmbridge"):
                (payload / "bin" / name).write_bytes(("synthetic " + name).encode())
            for name in ("DVM-GPL-2.0.txt", "DVM-GPL-3.0-or-later.txt",
                         "Open25-native-components.md", "LICENSE-CLARIFICATION.json"):
                (payload / "licenses" / name).write_text("synthetic retained notice\n")

            def git(directory, *args):
                return subprocess.check_output(
                    ["git", "-c", "core.hooksPath=/dev/null", "-c", "commit.gpgsign=false",
                     "-C", str(directory), *args], text=True, stderr=subprocess.DEVNULL,
                ).strip()

            git(parent, "init", "-q")
            git(parent, "add", ".")
            git(parent, "-c", "user.name=Package fixture", "-c", "user.email=fixture@example.invalid",
                "commit", "-qm", "Unrelated parent repository")

            def collect():
                subprocess.run([sys.executable, "-", str(source), str(build), str(payload)],
                               input=step, text=True, check=True, capture_output=True)
                return json.loads((payload / "source-provenance.json").read_text())

            exported = collect()
            self.assertIsNone(exported["source_git"])
            self.assertEqual(exported["source_archive"]["sha256"],
                             hashlib.sha256((payload / "source/dvmhost.tar.gz").read_bytes()).hexdigest())
            self.assertEqual(exported["build_metadata_sha256"]["CMakeCache.txt"],
                             hashlib.sha256((build / "CMakeCache.txt").read_bytes()).hexdigest())
            git(source, "init", "-q")
            git(source, "add", ".")
            git(source, "-c", "user.name=Package fixture", "-c", "user.email=fixture@example.invalid",
                "commit", "-qm", "Native source fixture")
            committed = collect()
            self.assertEqual(committed["source_git"]["commit"], git(source, "rev-parse", "HEAD"))
            self.assertEqual(committed["source_git"]["tree"], git(source, "rev-parse", "HEAD^{tree}"))
            self.assertEqual(committed["source_git"]["worktree_status"], "")
            (source / "source.cpp").write_text("modified synthetic source\n")
            self.assertIn("source.cpp", collect()["source_git"]["worktree_status"])

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
            (native / "licenses/DVM-GPL-3.0-or-later.txt").write_text("synthetic GPLv3 license-text fixture")
            (native / "licenses/Open25-native-components.md").write_text(
                "Synthetic native component inventory: Host/FNE GPL-2.0-only; combined Bridge GPL-3.0-or-later.\n"
            )
            (native / "licenses/LICENSE-CLARIFICATION.json").write_text(
                '{"upstream_commit":"b819cda22f7d22c87f344492a626d8d562b9ea01","fixture":true}\n'
            )
            (native / "source/dvmhost.tar.gz").write_bytes(b"synthetic native source fixture")
            (native / "source/asio.tar.gz").write_bytes(b"synthetic Asio source fixture")
            (native / "source-provenance.json").write_text(
                '{"source_git":null,"fixture":true,"source_archive_sha256":"'
                + hashlib.sha256((native / "source/dvmhost.tar.gz").read_bytes()).hexdigest()
                + '"}\n'
            )
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
            for relative in ("licenses/DVM-GPL-2.0.txt", "licenses/DVM-GPL-3.0-or-later.txt",
                             "licenses/Open25-native-components.md", "licenses/LICENSE-CLARIFICATION.json",
                             "source-provenance.json"):
                self.assertEqual((output / "native" / relative).read_bytes(), (native / relative).read_bytes())
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
