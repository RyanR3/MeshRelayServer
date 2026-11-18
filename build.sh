#!/bin/bash

# MeshRelay Server Build Script

echo "=== MeshRelay Server Build Script ==="

# Check if Go is installed
if ! command -v go &> /dev/null; then
    echo "Error: Go is not installed"
    echo "Install Go from: https://go.dev/dl/"
    exit 1
fi

echo "Go version:"
go version

# Determine version from git tag
VERSION=$(git describe --tags --always 2>/dev/null || echo "dev")
echo "Building version: $VERSION"

echo ""
echo "Tidying dependencies..."
go mod tidy

echo ""
echo "Downloading dependencies..."
go mod download

echo ""
echo "Building server (macOS)..."
go build -ldflags "-X main.Version=${VERSION}" -o ptt-server

echo ""
echo "Building server (Linux amd64)..."
GOOS=linux GOARCH=amd64 go build -ldflags "-X main.Version=${VERSION}" -o ptt-server-linux

echo ""
echo "Building server (Linux arm64)..."
# Raspberry Pi deployments are aarch64. Releases have always shipped this
# artifact; it just used to be built by hand.
GOOS=linux GOARCH=arm64 go build -ldflags "-X main.Version=${VERSION}" -o ptt-server-linux-arm64

if [ $? -eq 0 ]; then
    echo ""
    echo "Build successful! Version: $VERSION"

    echo ""
    echo "Packaging web dashboard..."
    # The dashboard ships as web.tar.gz (the updater extracts it beside the
    # binary). It was hand-built before, which let a stale bundle reach a
    # release; build it here from the current web/ every time.
    # No macOS metadata (._* files, xattrs): Linux tar warns about them.
    COPYFILE_DISABLE=1 tar -czf web.tar.gz --no-xattrs --no-mac-metadata --exclude "*.bak" --exclude ".DS_Store" --exclude "._*" web/ 2>/dev/null \
        || tar -czf web.tar.gz --exclude "*.bak" --exclude ".DS_Store" web/

    # Speech-to-text helper (transcribe/, built separately with Docker by
    # transcribe/build.sh — slow, so it is not rebuilt here). The dashboard's
    # Transcription > Install downloads it from the release and checks it
    # against SHA256SUMS, so every release must carry it.
    HELPERS=""
    for h in mrtranscribe-linux-amd64 mrtranscribe-linux-amd64-basic mrtranscribe-linux-arm64 mrtranscribe-linux-arm64-basic; do
        if [ -f "transcribe/dist/$h" ]; then
            cp "transcribe/dist/$h" "$h"
            HELPERS="$HELPERS $h"
        else
            echo "  WARNING: transcribe/dist/$h missing — run transcribe/build.sh before releasing"
        fi
    done

    echo "Generating SHA256SUMS..."
    if command -v sha256sum >/dev/null 2>&1; then
        sha256sum ptt-server ptt-server-linux ptt-server-linux-arm64 web.tar.gz $HELPERS > SHA256SUMS
    else
        # macOS fallback (shasum -a 256), normalized to "<hash>  <file>" format
        shasum -a 256 ptt-server ptt-server-linux ptt-server-linux-arm64 web.tar.gz $HELPERS > SHA256SUMS
    fi
    [ -f THIRD_PARTY_LICENSES.txt ] || echo "  WARNING: THIRD_PARTY_LICENSES.txt missing — releases must include it"
    echo "  Attach to the GitHub release: binaries, web.tar.gz,$HELPERS, SHA256SUMS, THIRD_PARTY_LICENSES.txt"
    echo "  SHA256SUMS written:"
    cat SHA256SUMS

    echo ""
    echo "Run the server with:"
    echo "  ./ptt-server"
    echo ""
    echo "Access dashboard at:"
    echo "  https://localhost:8443  (configure TLS in config.json)"
else
    echo ""
    echo "Build failed"
    exit 1
fi
