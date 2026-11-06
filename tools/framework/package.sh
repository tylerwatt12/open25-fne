#!/bin/sh
# Build all four services with their native runtime and embedded frontend.
# Modem firmware and private operational settings are not included.
set -eu
framework_repo=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
framework_output=${1:-"$framework_repo/build/open25-fne-package"}
framework_native=${FRAMEWORK_NATIVE_DIR:-"$framework_repo/build/native-runtime-linux-arm64"}
framework_speech=${FRAMEWORK_SPEECH_DIR:-"$framework_repo/build/speech-runtime-linux-arm64"}
if [ ! -f "$framework_native/SHA256SUMS" ]; then
    printf '%s\n' 'A verified Linux arm64 native payload is required. Run tools/framework/collect-native.sh on the build Pi and set FRAMEWORK_NATIVE_DIR.' >&2
    exit 1
fi
if [ ! -f "$framework_speech/SHA256SUMS" ] || [ ! -x "$framework_speech/espeak-ng" ]; then
    printf '%s\n' 'A verified Linux arm64 speech payload is required. Run tools/framework/build-speech.sh on the build Pi and set FRAMEWORK_SPEECH_DIR.' >&2
    exit 1
fi
if [ "${FRAMEWORK_GOOS:-linux}/${FRAMEWORK_GOARCH:-arm64}" != linux/arm64 ]; then
    printf '%s\n' 'The complete package currently supports Linux arm64.' >&2
    exit 1
fi
(
    cd "$framework_native"
    if command -v sha256sum >/dev/null 2>&1; then
        sha256sum -c SHA256SUMS >/dev/null
    else
        shasum -a 256 -c SHA256SUMS >/dev/null
    fi
)
(
    cd "$framework_speech"
    if command -v sha256sum >/dev/null 2>&1; then
        sha256sum -c SHA256SUMS >/dev/null
    else
        shasum -a 256 -c SHA256SUMS >/dev/null
    fi
)
mkdir -p "$framework_output/deploy/framework" "$framework_output/tools/framework" "$framework_output/docs"
framework_output=$(CDPATH= cd -- "$framework_output" && pwd)
cd "$framework_repo"
CGO_ENABLED=0 GOOS="${FRAMEWORK_GOOS:-linux}" GOARCH="${FRAMEWORK_GOARCH:-arm64}" \
    go build -mod=readonly -trimpath -o "$framework_output/open25-fne" ./cmd/open25-fne
cp deploy/framework/*.service "$framework_output/deploy/framework/"
cp tools/framework/install-services.sh "$framework_output/tools/framework/"
cp tools/framework/collect-native.sh "$framework_output/tools/framework/"
cp tools/framework/build-speech.sh tools/framework/collect-speech.sh "$framework_output/tools/framework/"
if [ -e "$framework_output/native" ] || [ -e "$framework_output/speech" ]; then
    printf '%s\n' 'Output already contains runtime files; use a fresh output folder.' >&2
    exit 1
fi
cp -R "$framework_native" "$framework_output/native"
cp -R "$framework_speech" "$framework_output/speech"
cp README.md "$framework_output/"
cp docs/installation.md "$framework_output/docs/"
cp internal/frameworkui/LICENSE.lucide "$framework_output/LICENSE.lucide"
printf '%s\n' "Open25 FNE package: $framework_output"
