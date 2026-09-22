#!/bin/bash
# Create a version tag with version bump validation
# Usage: ./scripts/create-tag.sh <version> [message]
# Example: ./scripts/create-tag.sh v0.3.3 "Bug fixes"

set -e

if [ -z "$1" ]; then
    echo "Usage: $0 <version> [message]"
    echo "Example: $0 v0.3.3 \"Bug fixes\""
    echo ""
    echo "Version format: v<major>.<minor>.<patch>[+<build>]"
    echo "  - Use + for build metadata (e.g., +mesh, +tun)"
    echo "  - Do NOT use - in tag name (conflicts with git describe)"
    echo ""
    echo "Version bump rules (Semantic Versioning):"
    echo "  - PATCH (v0.x.X+1): bug fixes, performance, cleanup"
    echo "  - MINOR (v0.X+1.0): new features, new APIs"
    echo "  - MAJOR (vX+1.0.0): breaking changes"
    exit 1
fi

VERSION="$1"
MESSAGE="${2:-Release $VERSION}"

# Get last tag
LAST_TAG=$(git describe --tags --abbrev=0 2>/dev/null || echo "")

if [ -n "$LAST_TAG" ]; then
    echo "上一个 tag: $LAST_TAG"
    echo ""
    
    # Show commits since last tag
    echo "自上个 tag 以来的变更："
    COMMITS=$(git log $LAST_TAG..HEAD --oneline)
    echo "$COMMITS"
    echo ""
    
    # Analyze commit types (format: "hash type:" or "hash type(scope):")
    HAS_FEAT=$(echo "$COMMITS" | grep -cE "^[a-f0-9]+ feat(\(.*\))?:" || true)
    HAS_FIX=$(echo "$COMMITS" | grep -cE "^[a-f0-9]+ fix(\(.*\))?:" || true)
    HAS_PERF=$(echo "$COMMITS" | grep -cE "^[a-f0-9]+ perf(\(.*\))?:" || true)
    HAS_REFACTOR=$(echo "$COMMITS" | grep -cE "^[a-f0-9]+ refactor(\(.*\))?:" || true)
    HAS_CHORE=$(echo "$COMMITS" | grep -cE "^[a-f0-9]+ chore(\(.*\))?:" || true)
    HAS_DOCS=$(echo "$COMMITS" | grep -cE "^[a-f0-9]+ docs(\(.*\))?:" || true)
    HAS_BREAKING=$(echo "$COMMITS" | grep -c "BREAKING CHANGE" || true)
    
    echo "变更分析："
    [ $HAS_FEAT -gt 0 ] && echo "  新功能 (feat:): $HAS_FEAT"
    [ $HAS_FIX -gt 0 ] && echo "  修复 (fix:): $HAS_FIX"
    [ $HAS_PERF -gt 0 ] && echo "  性能 (perf:): $HAS_PERF"
    [ $HAS_REFACTOR -gt 0 ] && echo "  重构 (refactor:): $HAS_REFACTOR"
    [ $HAS_CHORE -gt 0 ] && echo "  清理 (chore:): $HAS_CHORE"
    [ $HAS_DOCS -gt 0 ] && echo "  文档 (docs:): $HAS_DOCS"
    [ $HAS_BREAKING -gt 0 ] && echo "  破坏性变更: $HAS_BREAKING"
    echo ""
    
    # Parse last version
    LAST_VERSION=${LAST_TAG#v}
    LAST_VERSION=${LAST_VERSION%%+*}  # Remove build metadata
    IFS='.' read -r LAST_MAJOR LAST_MINOR LAST_PATCH <<< "$LAST_VERSION"
    
    # Parse new version
    NEW_VERSION=${VERSION#v}
    NEW_VERSION=${NEW_VERSION%%+*}
    IFS='.' read -r NEW_MAJOR NEW_MINOR NEW_PATCH <<< "$NEW_VERSION"
    
    # Determine recommended bump
    if [ $HAS_BREAKING -gt 0 ]; then
        RECOMMENDED="MAJOR"
        REC_VERSION="v$((LAST_MAJOR + 1)).0.0"
    elif [ $HAS_FEAT -gt 0 ]; then
        RECOMMENDED="MINOR"
        REC_VERSION="v${LAST_MAJOR}.$((LAST_MINOR + 1)).0"
    else
        RECOMMENDED="PATCH"
        REC_VERSION="v${LAST_MAJOR}.${LAST_MINOR}.$((LAST_PATCH + 1))"
    fi
    
    echo "建议: $RECOMMENDED 版本 → $REC_VERSION"
    echo ""
    
    # Check if version bump matches recommendation
    if [ "$NEW_MAJOR" -gt "$LAST_MAJOR" ]; then
        ACTUAL_BUMP="MAJOR"
    elif [ "$NEW_MINOR" -gt "$LAST_MINOR" ]; then
        ACTUAL_BUMP="MINOR"
    elif [ "$NEW_PATCH" -gt "$LAST_PATCH" ]; then
        ACTUAL_BUMP="PATCH"
    else
        ACTUAL_BUMP="NONE"
    fi
    
    if [ "$ACTUAL_BUMP" != "$RECOMMENDED" ]; then
        echo "⚠️  警告：版本号升级类型不匹配！"
        echo "   实际: $ACTUAL_BUMP ($LAST_TAG → $VERSION)"
        echo "   建议: $RECOMMENDED ($LAST_TAG → $REC_VERSION)"
        echo ""
        read -p "确认继续？(y/N) " -n 1 -r
        echo
        if [[ ! $REPLY =~ ^[Yy]$ ]]; then
            exit 1
        fi
    else
        echo "✓ 版本号升级类型正确"
        echo ""
    fi
fi

# Validate version format: v<major>.<minor>.<patch>[+<build>]
# Build metadata cannot contain dash (to avoid ambiguity with git describe)
# Examples: v1.0.0, v1.0.0+mesh, v2.1.3+tun.v2
if ! echo "$VERSION" | grep -qE '^v[0-9]+\.[0-9]+\.[0-9]+(\+[a-zA-Z0-9._]+)?$'; then
    echo "Error: Invalid version format: $VERSION"
    echo ""
    echo "Expected format: v<major>.<minor>.<patch>[+<build>]"
    echo "Examples:"
    echo "  v1.0.0          - plain release"
    echo "  v1.0.0+mesh     - with build metadata"
    echo "  v2.1.3+tun.v2   - build metadata with dots"
    echo ""
    echo "Rules:"
    echo "  - Do NOT use dash (-) in tag name (conflicts with git describe)"
    echo "  - Build metadata cannot contain dash"
    echo "  - BAD:  v1.0.0-mesh, v1.0.0+mesh-build"
    echo "  - GOOD: v1.0.0+mesh, v1.0.0+mesh.build"
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
