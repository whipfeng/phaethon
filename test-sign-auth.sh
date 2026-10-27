#!/bin/bash
# Test script for signature-based admin authentication
# Usage: ./test-sign-auth.sh <method> <path> [body]

METHOD="${1:-GET}"
PATH_URL="${2:-/api/status}"
BODY="${3:-}"
KEY_FILE="admin.key"
BASE_URL="http://vm.phn"

# Generate signature
SIGN_OUTPUT=$(./dist/phaethon-sign sign "$METHOD" "$PATH_URL" "$KEY_FILE" "$BODY")
TIMESTAMP=$(echo "$SIGN_OUTPUT" | grep "X-Timestamp:" | awk '{print $2}')
SIGNATURE=$(echo "$SIGN_OUTPUT" | grep "X-Signature:" | awk '{print $2}')

echo "Testing signature authentication..."
echo "Method: $METHOD"
echo "Path: $PATH_URL"
echo "Timestamp: $TIMESTAMP"
echo "Signature: ${SIGNATURE:0:32}..."
echo ""

# Make request with curl
if [ -z "$BODY" ]; then
    curl -v \
        -H "X-Timestamp: $TIMESTAMP" \
        -H "X-Signature: $SIGNATURE" \
        "$BASE_URL$PATH_URL"
else
    curl -v \
        -H "X-Timestamp: $TIMESTAMP" \
        -H "X-Signature: $SIGNATURE" \
        -H "Content-Type: application/json" \
        -d "$BODY" \
        "$BASE_URL$PATH_URL"
fi
