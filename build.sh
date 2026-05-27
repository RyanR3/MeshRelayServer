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

if [ $? -eq 0 ]; then
    echo ""
    echo "Build successful! Version: $VERSION"

    echo ""
    echo "Generating SHA256SUMS..."
    if command -v sha256sum >/dev/null 2>&1; then
        sha256sum ptt-server ptt-server-linux > SHA256SUMS
    else
        # macOS fallback (shasum -a 256), normalized to "<hash>  <file>" format
        shasum -a 256 ptt-server ptt-server-linux > SHA256SUMS
    fi
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
