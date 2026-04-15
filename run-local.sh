#!/bin/bash
set -e

IMAGE_NAME="smartrenew"
CONTAINER_NAME="smartrenew"
CONFIG_FILE="${1:-./config.json}"
DATA_DIR="${2:-./data}"

if [ ! -f "$CONFIG_FILE" ]; then
    echo "Config file not found: $CONFIG_FILE"
    echo "Usage: $0 [config.json path] [data dir]"
    echo ""
    echo "Quick start:"
    echo "  cp config.example.json config.json"
    echo "  # Edit config.json with your accounts and webhook"
    echo "  ./run-local.sh"
    exit 1
fi

mkdir -p "$DATA_DIR"

# Build image if not exists
if ! docker image inspect "$IMAGE_NAME" > /dev/null 2>&1; then
    echo "Building image..."
    docker build -t "$IMAGE_NAME" .
fi

# Stop existing container
docker rm -f "$CONTAINER_NAME" 2>/dev/null || true

echo "Starting SmartRenew..."
docker run -d \
    --name "$CONTAINER_NAME" \
    -p 5000:5000 \
    -v "$(realpath "$CONFIG_FILE")":/etc/smartrenew/config.json:ro \
    -v "$(realpath "$DATA_DIR")":/data \
    -e SMARTRENEW_CONFIG_FILE=/etc/smartrenew/config.json \
    --restart unless-stopped \
    "$IMAGE_NAME"

echo ""
echo "SmartRenew running at http://localhost:5000"
echo "Logs: docker logs -f $CONTAINER_NAME"
