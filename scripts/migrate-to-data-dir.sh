#!/bin/bash
# migrate-to-data-dir.sh
# Migrate from .phaethon/ to data/ directory structure

set -e

echo "Migrating to data/ directory structure..."

# 1. Create new directory structure
echo "Creating directory structure..."
mkdir -p data/packages
mkdir -p data/worker
mkdir -p data/state
mkdir -p data/logs
mkdir -p data/cache
mkdir -p data/certs
mkdir -p data/setup

# 2. Migrate from .phaethon/ if it exists
if [ -d ".phaethon" ]; then
    echo "Migrating from .phaethon/..."
    
    # Move packages
    if [ -d ".phaethon/packages" ]; then
        echo "  Moving packages..."
        mv .phaethon/packages/* data/packages/ 2>/dev/null || true
    fi
    
    # Move worker binaries
    if [ -d ".phaethon/worker" ]; then
        echo "  Moving worker binaries..."
        mv .phaethon/worker/* data/worker/ 2>/dev/null || true
    fi
    
    # Move state files
    if [ -f ".phaethon/mesh-state.json" ]; then
        echo "  Moving mesh-state.json..."
        mv .phaethon/mesh-state.json data/state/ 2>/dev/null || true
    fi
    
    # Move setup files
    if [ -d ".phaethon/setup" ]; then
        echo "  Moving setup files..."
        mv .phaethon/setup/* data/setup/ 2>/dev/null || true
    fi
    
    # Move p2p-cache files
    if [ -d ".phaethon/p2p-cache" ]; then
        echo "  Moving p2p-cache files..."
        mv .phaethon/p2p-cache data/cache/ 2>/dev/null || true
    fi
    
    # Remove old .phaethon directory (only if empty or nearly empty)
    echo "  Removing .phaethon/..."
    # Check if directory is empty or only contains empty directories
    if [ -z "$(find .phaethon -type f)" ]; then
        rm -rf .phaethon
    else
        echo "  WARNING: .phaethon/ still contains files, not removing:"
        find .phaethon -type f
    fi
fi

# 3. Move scattered pkg files from root directory
if ls *.pkg 1> /dev/null 2>&1; then
    echo "Moving pkg files from root..."
    mv *.pkg data/packages/ 2>/dev/null || true
fi

# 4. Move log files (only phaethon-related logs)
echo "Moving log files..."
for logfile in phaethon.log access.log error.log debug.log; do
    if [ -f "$logfile" ]; then
        mv "$logfile" data/logs/ 2>/dev/null || true
    fi
done

# 5. Move certificate files
if ls admin.{crt,key} 1> /dev/null 2>&1; then
    echo "Moving certificate files..."
    mv admin.crt admin.key data/certs/ 2>/dev/null || true
fi

# 6. Move PID file
if [ -f "pid" ]; then
    echo "Moving PID file..."
    mv pid data/state/phaethon.pid 2>/dev/null || true
fi

# 7. Move mesh-state.json from root
if [ -f "mesh-state.json" ]; then
    echo "Moving mesh-state.json..."
    mv mesh-state.json data/state/ 2>/dev/null || true
fi

# 8. Move config backups
if ls config.yaml.bak* 1> /dev/null 2>&1; then
    echo "Moving config backups..."
    mkdir -p data/backups
    mv config.yaml.bak* data/backups/ 2>/dev/null || true
fi

# 9. Clean up old worker binaries (keep only recent 5)
echo "Cleaning up old worker binaries..."
cd data/worker
ls -t | tail -n +6 | xargs rm -f 2>/dev/null || true
cd ../..

# 10. Update OpenRC service script if it exists
if [ -f "/etc/init.d/phaethon" ]; then
    echo "Updating OpenRC service script..."
    # The service script should already use the correct working directory
    # No changes needed here
fi

echo ""
echo "Migration complete!"
echo ""
echo "New directory structure:"
echo "  data/packages/  - pkg files"
echo "  data/worker/    - worker binaries"
echo "  data/state/     - state files (mesh-state.json, pid, stopped)"
echo "  data/logs/      - log files"
echo "  data/cache/     - cache files"
echo "  data/certs/     - certificates"
echo "  data/setup/     - setup files (reverse-id, profile)"
echo ""
echo "Next steps:"
echo "  1. Review the migrated files"
echo "  2. Restart phaethon service"
echo "  3. Verify everything works"
