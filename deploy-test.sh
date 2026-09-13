#!/bin/sh
# Deploy new binary with 60-second auto-rollback
set -e

WORKDIR=/root
OLD_BIN=$WORKDIR/phaethon.old
NEW_BIN=$WORKDIR/phaethon.new
CUR_BIN=$WORKDIR/phaethon
KEEP_FLAG=$WORKDIR/.keep-new-bin

# Clean up any previous flag
rm -f "$KEEP_FLAG"

# Backup current working binary
cp "$CUR_BIN" "$OLD_BIN"

# Stop service
rc-service phaethon stop
sleep 1

# Swap binary
cp "$NEW_BIN" "$CUR_BIN"
chmod +x "$CUR_BIN"

# Start service
rc-service phaethon start

echo "New version deployed. Auto-rollback in 120 seconds."
echo "To keep: touch $KEEP_FLAG"

# Wait 120 seconds, then rollback unless flag exists
sleep 120

if [ -f "$KEEP_FLAG" ]; then
    echo "Keep flag found. Staying on new version."
    rm -f "$KEEP_FLAG"
else
    echo "Auto-rolling back to old version..."
    rc-service phaethon stop
    sleep 1
    cp "$OLD_BIN" "$CUR_BIN"
    rc-service phaethon start
    echo "Rolled back to old version."
fi
