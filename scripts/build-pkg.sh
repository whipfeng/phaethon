#!/bin/bash
# Build and package phaethon into a .pkg file for admin upload
# Usage: ./scripts/build-pkg.sh [platform] [arch]
# Example: ./scripts/build-pkg.sh linux amd64

set -e

# Load .env if exists
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
if [ -f "$SCRIPT_DIR/.env" ]; then
    source "$SCRIPT_DIR/.env"
fi

# Parse arguments
PLATFORM=${1:-$(go env GOOS)}
ARCH=${2:-$(go env GOARCH)}
VERSION=$(git describe --tags --always --dirty 2>/dev/null || echo "dev")

# Map platform for windows7 special case
if [ "$PLATFORM" = "windows7" ]; then
    BUILD_OS="windows"
    BUILD_ARCH="amd64"
    DIST_DIR="windows7-amd64"
else
    BUILD_OS="$PLATFORM"
    BUILD_ARCH="$ARCH"
    DIST_DIR="${PLATFORM}-${ARCH}"
fi

echo "=== Building phaethon pkg ==="
echo "Platform: $PLATFORM"
echo "Arch: $ARCH"
echo "Version: $VERSION"

# Build binary
OUTPUT_NAME="phaethon"
if [ "$BUILD_OS" = "windows" ]; then
    OUTPUT_NAME="phaethon.exe"
fi

echo "Building binary..."
if [ "$PLATFORM" = "windows7" ]; then
    if [ -z "$GO_LEGACY_WIN7" ]; then
        echo "Error: GO_LEGACY_WIN7 not set for windows7 build"
        exit 1
    fi
    $GO_LEGACY_WIN7 build -ldflags "-s -w -X main.Version=$VERSION -X main.Platform=$PLATFORM -X main.Arch=$ARCH" \
        -o "dist/$DIST_DIR/$OUTPUT_NAME" .
else
    GOOS=$BUILD_OS GOARCH=$BUILD_ARCH go build \
        -ldflags "-s -w -X main.Version=$VERSION -X main.Platform=$PLATFORM -X main.Arch=$ARCH" \
        -o "dist/$DIST_DIR/$OUTPUT_NAME" .
fi

# Create meta.json
BUILD_TIME=$(date -u +"%Y-%m-%dT%H:%M:%SZ")
GIT_COMMIT=$(git rev-parse --short HEAD 2>/dev/null || echo "unknown")
GO_VERSION=$(go version | awk '{print $3}')

META_FILE="dist/$DIST_DIR/meta.json"
cat > "$META_FILE" << EOF
{
  "version": "$VERSION",
  "platform": "$PLATFORM",
  "arch": "$ARCH",
  "buildTime": "$BUILD_TIME",
  "gitCommit": "$GIT_COMMIT",
  "goVersion": "$GO_VERSION"
}
EOF

echo "Created meta.json"

# Sign and package
if [ -z "$PHAETHON_SIGNING_KEY" ]; then
    echo "Warning: PHAETHON_SIGNING_KEY not set, creating unsigned pkg"
    PKG_NAME="phaethon_${PLATFORM}_${ARCH}_${VERSION}.pkg"
    cd "dist/$DIST_DIR"
    zip -q "$PKG_NAME" "$OUTPUT_NAME" meta.json
    cd ../..
    echo "Created unsigned package: dist/$DIST_DIR/$PKG_NAME"
else
    if [ ! -f "$PHAETHON_SIGNING_KEY" ]; then
        echo "Error: Signing key not found: $PHAETHON_SIGNING_KEY"
        exit 1
    fi

    # Use Go to sign and create pkg
    echo "Signing package..."
    go run "$SCRIPT_DIR/../pkg/signing/cmd/sign/main.go" \
        -binary "dist/$DIST_DIR/$OUTPUT_NAME" \
        -meta "$META_FILE" \
        -key "$PHAETHON_SIGNING_KEY" \
        -output "dist/$DIST_DIR/phaethon_${PLATFORM}_${ARCH}_${VERSION}.pkg"

    echo "Created signed package: dist/$DIST_DIR/phaethon_${PLATFORM}_${ARCH}_${VERSION}.pkg"
fi

echo "=== Done ==="
