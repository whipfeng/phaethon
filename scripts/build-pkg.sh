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
VERSION=$(git describe --tags --match 'v*' --always --dirty 2>/dev/null || echo "dev")

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

# Windows amd64 packages carry the winpty fallback so Server 2016 / Win7 nodes
# recover PTY on self-upgrade. Other platforms never get assets (and no arm64
# winpty 0.4.3 build exists).
ASSET_ARGS=()
if [ "$BUILD_OS" = "windows" ] && [ "$BUILD_ARCH" = "amd64" ]; then
    for asset in winpty.dll winpty-agent.exe; do
        asset_path="$SCRIPT_DIR/../third_party/winpty/amd64/$asset"
        if [ ! -f "$asset_path" ]; then
            echo "Error: missing asset: $asset_path"
            exit 1
        fi
        ASSET_ARGS+=(-asset "$asset=$asset_path")
    done
fi

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

    # Use Go to sign and create pkg. Pass -version explicitly: cross-compiled
    # binaries cannot be executed here, and `strings` may not be installed on
    # Windows build hosts.
    echo "Signing package..."
    go run "$SCRIPT_DIR/../pkg/signing/cmd/sign/main.go" \
        -binary "dist/$DIST_DIR/$OUTPUT_NAME" \
        -meta "$META_FILE" \
        -key "$PHAETHON_SIGNING_KEY" \
        -version "$VERSION" \
        "${ASSET_ARGS[@]}" \
        -output "dist/$DIST_DIR/phaethon_${PLATFORM}_${ARCH}_${VERSION}.pkg"

    echo "Created signed package: dist/$DIST_DIR/phaethon_${PLATFORM}_${ARCH}_${VERSION}.pkg"
fi

echo "=== Done ==="
