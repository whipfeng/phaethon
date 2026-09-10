#!/bin/bash
# QG (Linux/Alpine) deployment script
# Usage: ./deploy-qg.sh
# Copy .env.example to .env and fill in your values

set -e

# Load environment from .env file if it exists
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
if [ -f "$SCRIPT_DIR/.env" ]; then
    source "$SCRIPT_DIR/.env"
fi

if [ -z "$QG_HOST" ] || [ -z "$QG_PORT" ] || [ -z "$QG_USER" ]; then
    echo "Error: QG_HOST, QG_PORT, and QG_USER must be set"
    echo "Copy .env.example to .env and fill in your values"
    exit 1
fi

LOCAL_BINARY="dist/linux-amd64/phaethon"

echo "=== Deploying to QG ==="

# Check if local binary exists
if [ ! -f "$LOCAL_BINARY" ]; then
    echo "Error: Local binary not found at $LOCAL_BINARY"
    echo "Run 'make linux' first"
    exit 1
fi

# Upload binary
echo "Uploading binary..."
scp -P $QG_PORT "$LOCAL_BINARY" "$QG_USER@$QG_HOST:/root/phaethon-new"

# Stop service
echo "Stopping service..."
ssh -p $QG_PORT $QG_USER@$QG_HOST "rc-service phaethon stop"

# Replace binary
echo "Replacing binary..."
ssh -p $QG_PORT $QG_USER@$QG_HOST "mv /root/phaethon-new /root/phaethon && chmod +x /root/phaethon"

# Start service
echo "Starting service..."
ssh -p $QG_PORT $QG_USER@$QG_HOST "rc-service phaethon start"

# Wait and verify
sleep 3
echo "Verifying service..."
ssh -p $QG_PORT $QG_USER@$QG_HOST "rc-service phaethon status"

echo "=== QG deployment complete ==="
