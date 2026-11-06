#!/bin/sh
# Build the bundled offline synthesizer. No audio device or RF access is used.
set -eu
if [ "$#" -ne 1 ]; then
    printf '%s\n' 'Usage: build-speech.sh BUILD_ROOT' >&2
    exit 2
fi
case "$(uname -sm)" in 'Linux aarch64') ;; *) printf '%s\n' 'Build host must be Linux arm64.' >&2; exit 1 ;; esac
speech_tools=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
mkdir -p "$1"
speech_root=$(CDPATH= cd -- "$1" && pwd)
speech_commit=4870adfa25b1a32b4361592f1be8a40337c58d6c
if [ ! -d "$speech_root/src/.git" ]; then
    git clone --depth 1 --branch 1.52.0 https://github.com/espeak-ng/espeak-ng.git "$speech_root/src"
fi
test "$(git -C "$speech_root/src" rev-parse HEAD)" = "$speech_commit"
test -z "$(git -C "$speech_root/src" status --porcelain)"
speech_cmake=${FRAMEWORK_SPEECH_CMAKE:-cmake}
"$speech_cmake" -S "$speech_root/src" -B "$speech_root/build" -G Ninja \
    -DCMAKE_BUILD_TYPE=Release -DCMAKE_INSTALL_PREFIX=/usr/local/trunksystem-speech \
    -DBUILD_SHARED_LIBS=OFF -DBUILD_TESTING=OFF -DCMAKE_SKIP_RPATH=ON \
    -DUSE_LIBPCAUDIO=OFF -DUSE_LIBSONIC=OFF -DUSE_MBROLA=OFF -DUSE_ASYNC=OFF \
    '-DCMAKE_EXE_LINKER_FLAGS=-static-libstdc++ -static-libgcc'
"$speech_cmake" --build "$speech_root/build" --target espeak-ng-bin data --parallel "${FRAMEWORK_SPEECH_JOBS:-2}"
"$speech_tools/collect-speech.sh" "$speech_root/src" "$speech_root/build" "$speech_root/payload"
