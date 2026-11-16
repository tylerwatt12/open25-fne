#!/bin/sh
# Run on the Linux arm64 build host after building the reviewed DVM sources.
# This collects runtime libraries, notices and the corresponding source; it
# never starts a radio engine or accesses a modem.
set -eu
if [ "$#" -ne 3 ]; then
    printf '%s\n' 'Usage: collect-native.sh DVM_SOURCE DVM_BUILD OUTPUT' >&2
    exit 2
fi
native_source=$(CDPATH= cd -- "$1" && pwd)
native_build=$(CDPATH= cd -- "$2" && pwd)
mkdir -p "$3"
native_output=$(CDPATH= cd -- "$3" && pwd)
case "$(uname -sm)" in 'Linux aarch64') ;; *) printf '%s\n' 'Build host must be Linux arm64.' >&2; exit 1 ;; esac
if [ -e "$native_output/SHA256SUMS" ]; then
    printf '%s\n' 'Output already contains a native payload; choose an empty folder.' >&2
    exit 1
fi
for native_notice in OPEN25-LICENSING.md LICENSE-CLARIFICATION.json LICENSES/GPL-3.0-or-later.txt; do
    if [ ! -f "$native_source/$native_notice" ]; then
        printf '%s\n' "Native source lacks $native_notice; select the reviewed licensing-clarification source." >&2
        exit 1
    fi
done
mkdir -p "$native_output/bin" "$native_output/lib" "$native_output/licenses" "$native_output/source"
for native_program in dvmhost dvmfne dvmbridge; do
    test -x "$native_build/$native_program"
    readelf -h "$native_build/$native_program" | grep -q 'Machine:.*AArch64'
    # Require relocatable linker paths; no build-machine directory is shipped.
    readelf -d "$native_build/$native_program" | grep 'RUNPATH' | grep -Fq '$ORIGIN/../lib'
    cp "$native_build/$native_program" "$native_output/bin/$native_program"
    cat > "$native_output/$native_program" <<'EOF'
#!/bin/sh
set -eu
native_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
native_program=${0##*/}
# Do not inherit system or build-machine library search paths.
LD_LIBRARY_PATH="$native_dir/lib"
export LD_LIBRARY_PATH
exec "$native_dir/bin/$native_program" "$@"
EOF
    chmod 0755 "$native_output/$native_program" "$native_output/bin/$native_program"
done
# Resolve the exact libraries used by this build. FRAMEWORK_NATIVE_LIBRARY_PATH
# may point to a private compiler sysroot; it is not embedded in the payload.
native_dependencies=$(LD_LIBRARY_PATH="${FRAMEWORK_NATIVE_LIBRARY_PATH:-}" ldd "$native_build/dvmhost" "$native_build/dvmfne" "$native_build/dvmbridge")
if printf '%s\n' "$native_dependencies" | grep -q 'not found'; then
    printf '%s\n' 'Native build has unresolved shared libraries.' >&2
    exit 1
fi
for native_library in libssl.so.3 libcrypto.so.3 libstdc++.so.6 libgcc_s.so.1 libz.so.1 libzstd.so.1; do
    native_path=$(printf '%s\n' "$native_dependencies" | awk -v name="$native_library" '$1 == name && $2 == "=>" { print $3; exit }')
    test -n "$native_path"
    cp -L "$native_path" "$native_output/lib/$native_library"
    chmod 0644 "$native_output/lib/$native_library"
done
cp "$native_source/LICENSE" "$native_output/licenses/DVM-GPL-2.0.txt"
if [ -f "$native_source/COPYING" ]; then cp "$native_source/COPYING" "$native_output/licenses/DVM-COPYING.txt"; fi
cp "$native_source/LICENSES/GPL-3.0-or-later.txt" "$native_output/licenses/DVM-GPL-3.0-or-later.txt"
cp "$native_source/OPEN25-LICENSING.md" "$native_output/licenses/Open25-native-components.md"
cp "$native_source/LICENSE-CLARIFICATION.json" "$native_output/licenses/LICENSE-CLARIFICATION.json"
native_ssl_notice=${FRAMEWORK_NATIVE_SSL_NOTICE:-/usr/share/doc/libssl3t64/copyright}
cp -L "$native_ssl_notice" "$native_output/licenses/OpenSSL.txt"
cp -L /usr/share/doc/libstdc++6/copyright "$native_output/licenses/GCC-runtime.txt"
cp -L /usr/share/doc/zlib1g/copyright "$native_output/licenses/zlib.txt"
cp -L /usr/share/doc/libzstd1/copyright "$native_output/licenses/zstd.txt"
for native_license in GPL-1 GPL-2 GPL-3 LGPL-2.1 LGPL-3 Apache-2.0 Artistic; do
    cp -L "/usr/share/common-licenses/$native_license" "$native_output/licenses/$native_license.txt"
done
native_asio=${FRAMEWORK_NATIVE_ASIO_SOURCE:-"$native_build/_deps/asio-src/asio"}
test -f "$native_asio/LICENSE_1_0.txt"
cp "$native_asio/LICENSE_1_0.txt" "$native_output/licenses/Asio-Boost-1.0.txt"
cp "$native_asio/COPYING" "$native_output/licenses/Asio-COPYING.txt"
tar --exclude=.git --exclude=build --exclude=.cache --exclude='._*' -C "$native_source" -czf "$native_output/source/dvmhost.tar.gz" .
tar --exclude=.git --exclude='._*' -C "$native_asio" -czf "$native_output/source/asio.tar.gz" include LICENSE_1_0.txt COPYING
python3 - "$native_source" "$native_build" "$native_output" <<'PY'
from pathlib import Path
import hashlib
import json
import os
import shutil
import subprocess
import sys

source, build, output = (Path(value).resolve() for value in sys.argv[1:])
def digest(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()

provenance = {
    "source_archive": {"path": "source/dvmhost.tar.gz", "sha256": digest(output / "source/dvmhost.tar.gz")},
    "source_git": None,
    "supplied_source_directory": str(source),
    "supplied_build_directory": str(build),
    "build_metadata_sha256": {name: digest(build / name) for name in ("CMakeCache.txt", "compile_commands.json") if (build / name).is_file()},
    "binary_sha256": {name: digest(output / "bin" / name) for name in ("dvmhost", "dvmfne", "dvmbridge")},
    "license_files_sha256": {name: digest(output / "licenses" / name) for name in ("DVM-GPL-2.0.txt", "DVM-GPL-3.0-or-later.txt", "Open25-native-components.md", "LICENSE-CLARIFICATION.json")},
    "source_binary_relationship": "Builder-supplied corresponding source. Archive and metadata hashes record inputs; they do not independently prove source/binary equivalence. Retain the matching build qualification.",
}
if shutil.which("git"):
    def git(*args):
        return subprocess.run(["git", "-C", str(source), *args], env={**os.environ, "GIT_OPTIONAL_LOCKS": "0"}, capture_output=True, text=True)
    top = git("rev-parse", "--show-toplevel")
    if top.returncode == 0 and Path(top.stdout.strip()).resolve() == source:
        commit = git("rev-parse", "HEAD")
        tree = git("rev-parse", "HEAD^{tree}")
        state = git("status", "--porcelain", "--untracked-files=all")
        if commit.returncode == tree.returncode == state.returncode == 0:
            provenance["source_git"] = {"commit": commit.stdout.strip(), "tree": tree.stdout.strip(), "worktree_status": state.stdout}
(output / "source-provenance.json").write_text(json.dumps(provenance, indent=2) + "\n")
PY
cat > "$native_output/README.txt" <<'EOF'
DVM native runtime for Open25 FNE

Use dvmhost, dvmfne and dvmbridge in this directory. These launchers select the
bundled runtime libraries and then replace themselves with the native process.
No native executable depends on an earlier Open25 FNE installation.

Target: Linux arm64, Raspberry Pi OS / Debian 13 (trixie), glibc 2.41 or later.
The operating system supplies libc, libm and the ELF loader. Other shared
libraries are included in lib/. No package-manager installation is required on
that supported OS. Hardware firmware is separate from these host executables.

Corresponding DVM source is in source/dvmhost.tar.gz; its Asio headers are in
source/asio.tar.gz. License notices are in licenses/. SHA256SUMS covers every
shipped file except the checksum file itself.

Per-component terms are in licenses/Open25-native-components.md, copied from
the native source owner, with its LICENSE-CLARIFICATION.json provenance and
both applicable GPLv2/GPLv3 texts. Original third-party notices remain in source.
source-provenance.json records the supplied source archive, available Git/build
metadata and collected binaries; retain the corresponding build qualification.
EOF
(
    cd "$native_output"
    find . -type f ! -name SHA256SUMS -print0 | LC_ALL=C sort -z | xargs -0 sha256sum > SHA256SUMS
    sha256sum -c SHA256SUMS >/dev/null
    LD_LIBRARY_PATH="$native_output/lib" ldd bin/dvmhost bin/dvmfne bin/dvmbridge > dependency-check.txt
    if grep -q 'not found' dependency-check.txt; then exit 1; fi
    rm dependency-check.txt
)
printf '%s\n' "Native runtime collected: $native_output"
