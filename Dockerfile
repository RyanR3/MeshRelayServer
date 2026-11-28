# MeshRelay Server container image (used by the infra-TAK MeshRelay module).
#
#   docker build --build-arg VERSION=v4.10.2 -t meshrelay-server .
#
# Multi-arch (linux/amd64 + linux/arm64) is built by CI with buildx; a plain
# `docker build` produces the image for the machine it runs on.
#
# Three stages:
#   build    — the Go server (static, no cgo)
#   helpers  — the speech-to-text helper (whisper.cpp + libopus at pinned
#              commits), two builds per architecture: optimised and basic
#   runtime  — Alpine, non-root user, data in /data

FROM golang:1.26-alpine AS build
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download && go mod verify
COPY *.go ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.Version=${VERSION}" -o /out/ptt-server .

FROM alpine:3.20 AS helpers
# Pinned sources: a tag AND the commit it must resolve to.
ARG WHISPER_TAG=v1.9.5
ARG WHISPER_SHA=d1be6fde11ac6e0407606b4e42fe72d34add8037
ARG OPUS_TAG=v1.6.1
ARG OPUS_SHA=22244de5a79bd1d6d623c32e72bf1954b56235be
RUN apk add --no-cache build-base cmake linux-headers git
WORKDIR /src
COPY transcribe/CMakeLists.txt transcribe/mrtranscribe.c ./
RUN set -eu; \
    git clone -q --depth 1 --branch "$WHISPER_TAG" https://github.com/ggml-org/whisper.cpp vendor/whisper.cpp; \
    git clone -q --depth 1 --branch "$OPUS_TAG" https://github.com/xiph/opus vendor/opus; \
    test "$(git -C vendor/whisper.cpp rev-parse HEAD)" = "$WHISPER_SHA"; \
    test "$(git -C vendor/opus rev-parse HEAD)" = "$OPUS_SHA"
# Asset names match the GitHub release, so the server's helper selection
# (helperAssets) picks the best one that passes its self-test.
RUN set -eu; mkdir /helpers; \
    case "$(uname -m)" in \
      aarch64) arch=arm64; fast="-DGGML_CPU_ARM_ARCH=armv8.2-a+dotprod+fp16"; basic="" ;; \
      x86_64)  arch=amd64; fast="-DGGML_AVX=ON -DGGML_AVX2=ON -DGGML_FMA=ON -DGGML_F16C=ON"; \
               basic="-DGGML_AVX=OFF -DGGML_AVX2=OFF -DGGML_FMA=OFF -DGGML_F16C=OFF" ;; \
      *) echo "unsupported architecture $(uname -m)"; exit 1 ;; \
    esac; \
    cmake -S . -B build/fast -DCMAKE_BUILD_TYPE=Release $fast >/dev/null; \
    cmake --build build/fast -j"$(nproc)" --target mrtranscribe >/dev/null; \
    cmake -S . -B build/basic -DCMAKE_BUILD_TYPE=Release $basic >/dev/null; \
    cmake --build build/basic -j"$(nproc)" --target mrtranscribe >/dev/null; \
    strip build/fast/mrtranscribe build/basic/mrtranscribe; \
    cp build/fast/mrtranscribe  /helpers/mrtranscribe-linux-$arch; \
    cp build/basic/mrtranscribe /helpers/mrtranscribe-linux-$arch-basic; \
    chmod 755 /helpers/*

FROM alpine:3.20
ARG VERSION=dev
LABEL org.opencontainers.image.title="MeshRelay Server" \
      org.opencontainers.image.version="${VERSION}"
RUN apk add --no-cache ca-certificates tzdata \
 && addgroup -S -g 10001 meshrelay \
 && adduser -S -D -H -u 10001 -G meshrelay meshrelay \
 && mkdir -p /data && chown meshrelay:meshrelay /data && chmod 700 /data
COPY --from=build /out/ptt-server /app/ptt-server
COPY --from=helpers /helpers/ /app/helpers/
COPY web/ /app/web/
COPY THIRD_PARTY_LICENSES.txt /app/
ENV MESHRELAY_WEB_DIR=/app/web \
    MESHRELAY_TRANSCRIBE_HELPER_DIR=/app/helpers \
    MESHRELAY_TRANSCRIBE_DIR=/data/transcribe \
    MESHRELAY_RECORDING_DIR=/data/recordings \
    MESHRELAY_PTT_PORT=5000 \
    MESHRELAY_WEB_PORT=8770
USER meshrelay
WORKDIR /data
VOLUME ["/data"]
EXPOSE 5000/udp 8770/tcp
HEALTHCHECK --interval=30s --timeout=5s --start-period=20s --retries=3 \
  CMD wget -q --no-check-certificate -O /dev/null "https://127.0.0.1:${MESHRELAY_WEB_PORT}/health" 2>/dev/null \
   || wget -q -O /dev/null "http://127.0.0.1:${MESHRELAY_WEB_PORT}/health" || exit 1
ENTRYPOINT ["/app/ptt-server", "-config", "/data/config.json"]
