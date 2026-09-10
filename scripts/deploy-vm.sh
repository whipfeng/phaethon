#!/bin/bash
# VM (Windows) deployment script
# Usage: ./deploy-vm.sh
# Copy .env.example to .env and fill in your values

set -e

# Load environment from .env file if it exists
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
if [ -f "$SCRIPT_DIR/.env" ]; then
    source "$SCRIPT_DIR/.env"
fi

if [ -z "$VM_HOST" ] || [ -z "$VM_PORT" ] || [ -z "$VM_USER" ]; then
    echo "Error: VM_HOST, VM_PORT, and VM_USER must be set"
    echo "Copy .env.example to .env and fill in your values"
    exit 1
fi

VM_WORKSPACE="C:\\Users\\${VM_USER}\\Desktop\\Workspace\\phaethon"
LOCAL_BINARY="dist/windows-amd64/phaethon.exe"

echo "=== Deploying to VM ==="

# Check if local binary exists
if [ ! -f "$LOCAL_BINARY" ]; then
    echo "Error: Local binary not found at $LOCAL_BINARY"
    echo "Run 'make windows' first"
    exit 1
fi

# Stop the running process
echo "Stopping VM process..."
ssh -p $VM_PORT $VM_USER@$VM_HOST "powershell -Command 'Stop-Process -Name phaethon -Force -ErrorAction SilentlyContinue'" || true
sleep 2

# Upload binary to temp location
echo "Uploading binary..."
TEMP_NAME="phaethon-deploy-$(date +%s).exe"
scp -P $VM_PORT "$LOCAL_BINARY" "$VM_USER@$VM_HOST:$TEMP_NAME"

# Verify upload
echo "Verifying upload..."
ssh -p $VM_PORT $VM_USER@$VM_HOST "powershell -Command 'if (-not (Test-Path $TEMP_NAME)) { exit 1 }'"

# Replace binary
echo "Replacing binary..."
ssh -p $VM_PORT $VM_USER@$VM_HOST "powershell -Command 'Copy-Item $TEMP_NAME $VM_WORKSPACE\\phaethon.exe -Force; Remove-Item $TEMP_NAME'"

# Start via scheduled task
echo "Starting VM..."
ssh -p $VM_PORT $VM_USER@$VM_HOST "powershell -Command 'schtasks.exe /Run /TN PhaethonTUN'"

# Wait and verify
sleep 5
echo "Verifying process..."
ssh -p $VM_PORT $VM_USER@$VM_HOST "powershell -Command 'Get-Process -Name phaethon -ErrorAction SilentlyContinue | Select-Object Id,ProcessName'"

echo "=== VM deployment complete ==="
