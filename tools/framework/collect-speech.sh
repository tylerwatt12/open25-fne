#!/bin/sh
# Collect the offline speech runtime and its corresponding source.
set -eu
if [ "$#" -ne 3 ]; then
    printf '%s\n' 'Usage: collect-speech.sh ESPEAK_SOURCE ESPEAK_BUILD OUTPUT' >&2
    exit 2
fi
case "$(uname -sm)" in 'Linux aarch64') ;; *) printf '%s\n' 'Build host must be Linux arm64.' >&2; exit 1 ;; esac
speech_source=$(CDPATH= cd -- "$1" && pwd)
speech_build=$(CDPATH= cd -- "$2" && pwd)
mkdir -p "$3"
speech_output=$(CDPATH= cd -- "$3" && pwd)
if [ -e "$speech_output/SHA256SUMS" ]; then
    printf '%s\n' 'Speech payload already exists; choose an empty output folder.' >&2
    exit 1
fi
test "$(git -C "$speech_source" rev-parse HEAD)" = 4870adfa25b1a32b4361592f1be8a40337c58d6c
test -z "$(git -C "$speech_source" status --porcelain)"
for speech_option in USE_LIBPCAUDIO USE_LIBSONIC USE_MBROLA USE_ASYNC BUILD_SHARED_LIBS BUILD_TESTING; do
    grep -q "^$speech_option:BOOL=OFF$" "$speech_build/CMakeCache.txt"
done
test -x "$speech_build/src/espeak-ng"
readelf -h "$speech_build/src/espeak-ng" | grep -q 'Machine:.*AArch64'
if readelf -d "$speech_build/src/espeak-ng" | grep -Eq 'RPATH|RUNPATH'; then
    printf '%s\n' 'Speech executable has a nonportable search path.' >&2
    exit 1
fi
speech_dependencies=$(LD_LIBRARY_PATH= ldd "$speech_build/src/espeak-ng")
if printf '%s\n' "$speech_dependencies" | grep -Eq 'not found|libespeak|libstdc\+\+|libgcc|pcaudio|libsonic|asound|pulse'; then
    printf '%s\n' 'Speech executable has an unexpected runtime dependency.' >&2
    exit 1
fi
# Only the operating system C runtime, mathematics library and loader may remain.
speech_extra=$(printf '%s\n' "$speech_dependencies" | awk '$2 == "=>" && $1 != "libc.so.6" && $1 != "libm.so.6" { print $1 }')
test -z "$speech_extra"
mkdir -p "$speech_output/bin" "$speech_output/licenses" "$speech_output/source"
cp "$speech_build/src/espeak-ng" "$speech_output/bin/espeak-ng"
cp -R "$speech_build/espeak-ng-data" "$speech_output/espeak-ng-data"
cp "$speech_source/COPYING" "$speech_output/licenses/eSpeak-NG-GPL-3.0.txt"
cp "$speech_source/COPYING.APACHE" "$speech_output/licenses/eSpeak-NG-Apache-2.0.txt"
cp "$speech_source/COPYING.BSD2" "$speech_output/licenses/eSpeak-NG-BSD-2-Clause.txt"
cp "$speech_source/COPYING.UCD" "$speech_output/licenses/Unicode.txt"
cp "$speech_source/src/ucd-tools/COPYING" "$speech_output/licenses/ucd-tools.txt"
cp -L /usr/share/doc/libstdc++6/copyright "$speech_output/licenses/GCC-runtime.txt"
for speech_license in GPL-3 LGPL-2.1 LGPL-3 Apache-2.0; do
    cp -L "/usr/share/common-licenses/$speech_license" "$speech_output/licenses/$speech_license.txt"
done
git -C "$speech_source" archive --format=tar HEAD | gzip -n > "$speech_output/source/espeak-ng.tar.gz"
cat > "$speech_output/espeak-ng" <<'EOF'
#!/bin/sh
set -eu
speech_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
ESPEAK_DATA_PATH="$speech_dir"
export ESPEAK_DATA_PATH
unset LD_LIBRARY_PATH LD_PRELOAD
exec "$speech_dir/bin/espeak-ng" "$@"
EOF
chmod 0755 "$speech_output/espeak-ng" "$speech_output/bin/espeak-ng"
cat > "$speech_output/README.txt" <<'EOF'
Offline eSpeak NG speech runtime for Open25 FNE

Run espeak-ng in this directory. Its launcher selects the included voice and
dictionary data. Sound-device, MBROLA, async output and external Sonic support
are disabled. The application generates WAV files; it does not use a sound card.

Target: Linux arm64, Raspberry Pi OS / Debian 13 (trixie), glibc 2.41 or later.
Only the operating system libc, libm and ELF loader are required. C++ and GCC
support are linked statically. No package installation or network synthesis
service is required on the target.

Source: https://github.com/espeak-ng/espeak-ng
Upstream tag: 1.52.0
Commit: 4870adfa25b1a32b4361592f1be8a40337c58d6c
Corresponding source: source/espeak-ng.tar.gz. License notices: licenses/.
Rebuild using tools/framework/build-speech.sh on a Linux arm64 build host.
SHA256SUMS covers every included file except the checksum file itself.
EOF
printf '%s\n' 'Radio test ready. Please check the console.' | "$speech_output/espeak-ng" -v en-us -s 175 -w "$speech_output/test.wav" --stdin
python3 - "$speech_output/test.wav" <<'PY'
import sys, wave
with wave.open(sys.argv[1], 'rb') as wav:
    assert (wav.getnchannels(), wav.getsampwidth(), wav.getframerate()) == (1, 2, 22050)
    assert wav.getnframes() > 22050
    samples = wav.readframes(wav.getnframes())
    assert any(samples)
PY
rm "$speech_output/test.wav"
(
    cd "$speech_output"
    find . -type f ! -name SHA256SUMS -print0 | LC_ALL=C sort -z | xargs -0 sha256sum > SHA256SUMS
    sha256sum -c SHA256SUMS >/dev/null
)
printf '%s\n' "Speech runtime collected: $speech_output"
