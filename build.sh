#!/bin/bash
set -e

# Configuration
BINARY_NAME="github.com/flinternet/flinternet"
MAIN_PATH="cmd/server/main.go"
OUTPUT_DIR="build"

# Platforms to build for (OS/ARCH)
PLATFORMS=(
    "linux/amd64"
    "linux/arm64"
    "windows/amd64"
    "darwin/amd64"
    "darwin/arm64"
)

echo "Started building $BINARY_NAME..."

# Tidy dependencies
echo "Tidying dependencies..."
go mod tidy

# Create output directory
mkdir -p "$OUTPUT_DIR"

# Build for each platform
for PLATFORM in "${PLATFORMS[@]}"; do
    OS="${PLATFORM%%/*}"
    ARCH="${PLATFORM##*/}"
    OUTPUT_NAME="$BINARY_NAME-$OS-$ARCH"
    
    if [ "$OS" = "windows" ]; then
        OUTPUT_NAME+=".exe"
    fi

    echo "Building for $OS/$ARCH..."
    env GOOS=$OS GOARCH=$ARCH go build -trimpath -ldflags="-s -w" -o "$OUTPUT_DIR/$OUTPUT_NAME" "$MAIN_PATH"
    
    if [ $? -eq 0 ]; then
        echo "✓ Built $OUTPUT_DIR/$OUTPUT_NAME"
    else
        echo "✗ Failed to build for $OS/$ARCH"
        exit 1
    fi
done

echo "All builds complete! Artifacts are in the '$OUTPUT_DIR' directory."
ls -lh "$OUTPUT_DIR"
