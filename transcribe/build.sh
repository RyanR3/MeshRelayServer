#!/bin/bash
# Builds the transcription helper for the release:
#   mrtranscribe-linux-amd64        x86-64 with AVX2/FMA (any server CPU since ~2013)
#   mrtranscribe-linux-amd64-basic  x86-64 without AVX (fallback, slower)
#   mrtranscribe-linux-arm64        ARMv8.2 + dot product (Raspberry Pi 5, Graviton 2+)
#   mrtranscribe-linux-arm64-basic  ARMv8.0 (Raspberry Pi 4 and older; fallback)
# Needs Docker (amd64 builds run under emulation on an ARM Mac, ~20 min each).
# Sources: whisper.cpp and libopus at pinned tags, fetched into vendor/.
set -euo pipefail
cd "$(dirname "$0")"
WHISPER_TAG=v1.9.5
OPUS_TAG=v1.6.1
mkdir -p vendor dist
[ -d vendor/whisper.cpp ] || git clone -q --depth 1 --branch $WHISPER_TAG https://github.com/ggml-org/whisper.cpp vendor/whisper.cpp
[ -d vendor/opus ] || git clone -q --depth 1 --branch $OPUS_TAG https://github.com/xiph/opus vendor/opus

build() { # platform output cpuflags
  docker run --rm --platform "$1" -v "$PWD":/src -w /src alpine:3.20 sh -ec "
    apk add -q --no-cache build-base cmake linux-headers >/dev/null
    cmake -S . -B build/$2 -DCMAKE_BUILD_TYPE=Release $3 >/dev/null
    cmake --build build/$2 -j4 --target mrtranscribe >/dev/null
    strip build/$2/mrtranscribe"
  cp "build/$2/mrtranscribe" "dist/$2"
  echo "built dist/$2 ($(du -h "dist/$2" | cut -f1))"
}
TARGETS="${*:-arm64 arm64-basic amd64 amd64-basic}"
for t in $TARGETS; do
  case $t in
    arm64)       build linux/arm64 mrtranscribe-linux-arm64 "-DGGML_CPU_ARM_ARCH=armv8.2-a+dotprod+fp16" ;;
    arm64-basic) build linux/arm64 mrtranscribe-linux-arm64-basic "" ;;
    amd64)       build linux/amd64 mrtranscribe-linux-amd64 "-DGGML_AVX=ON -DGGML_AVX2=ON -DGGML_FMA=ON -DGGML_F16C=ON" ;;
    amd64-basic) build linux/amd64 mrtranscribe-linux-amd64-basic "-DGGML_AVX=OFF -DGGML_AVX2=OFF -DGGML_FMA=OFF -DGGML_F16C=OFF" ;;
  esac
done
