#!/bin/bash
# Create a version tag with proper format validation
# Usage: ./scripts/create-tag.sh <version> [message]
# Example: ./scripts/create-tag.sh v1.0.0+mesh "Initial mesh release"

set -e

if [ -z "$1" ]; then
    echo "Usage: $0 <version> [message]"
    echo "Example: $0 v1.0.0+mesh \"Initial mesh release\""
    echo ""
    echo "Version format: v<major>.<minor>.<patch>[+<build>]"
    echo "  - Use + for build metadata (e.g., +mesh, +tun)"
    echo "  - Do NOT use - in tag name (conflicts with git describe)"
    exit 1
fi

VERSION="$1"
MESSAGE="${2:-Release $VERSION}"

# Validate version format: v<major>.<minor>.<patch>[+<build>]
# Examples: v1.0.0, v1.0.0+mesh, v2.1.3+tun-v2
if ! echo "$VERSION" | grep -qE '^v[0-9]+\.[0-9]+\.[0-9]+(\+[a-zA-Z0-9._-]+)?$'; then
    echo "Error: Invalid version format: $VERSION"
    echo ""
    echo "Expected format: v<major>.<minor>.<patch>[+<build>]"
    echo "Examples:"
    echo "  v1.0.0          - plain release"
    echo "  v1.0.0+mesh     - with build metadata"
    echo "  v2.1.3+tun-v2   - build metadata can contain dashes"
    echo ""
    echo "Do NOT use dash (-) before the + sign!"
    echo "  BAD:  v1.0.0-mesh"
    echo "  GOOD: v1.0.0+mesh"
    exit 1
fi

# Check if tag already exists
if git rev-parse "$VERSION" >/dev/null 2>&1; then
    echo "Error: Tag $VERSION already exists"
    exit 1
fi

echo "Creating tag: $VERSION"
echo "Message: $MESSAGE"
echo ""

# Create annotated tag
git tag -a "$VERSION" -m "$MESSAGE"

echo ""
echo "Tag created successfully!"
echo ""
echo "To push:"
echo "  git push origin $VERSION"
echo ""
echo "To verify git describe output:"
echo "  git describe --tags --always --dirty"
