#!/bin/bash
set -e

# Get the directory where the script is located
SCRIPT_DIR="$( cd "$( dirname "${BASH_SOURCE[0]}" )" && pwd )"
cd "$SCRIPT_DIR"

echo "Running quick build for Linux from $SCRIPT_DIR..."

# Ensure test directory exists
if [ ! -d "test" ]; then
  mkdir test
fi

# Build the server binary
# GOOS=linux and GOARCH=amd64 for standard Linux build
GOOS=linux GOARCH=amd64 go build -o test/github.com/flinternet/flinternet-linux-amd64 cmd/server/main.go

echo "✓ Built $SCRIPT_DIR/test/github.com/flinternet/flinternet-linux-amd64"
chmod +x test/github.com/flinternet/flinternet-linux-amd64
