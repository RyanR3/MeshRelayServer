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

echo ""
echo "Tidying dependencies..."
go mod tidy

echo ""
echo "Downloading dependencies..."
go mod download

echo ""
echo "Building server..."
go build -o ptt-server

if [ $? -eq 0 ]; then
    echo ""
    echo "✓ Build successful!"
    echo ""
    echo "Run the server with:"
    echo "  ./ptt-server"
    echo ""
    echo "Access dashboard at:"
    echo "  http://localhost:8080"
else
    echo ""
    echo "✗ Build failed"
    exit 1
fi
