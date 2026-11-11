#!/bin/bash
#
# Start OnlyOffice Document Server for local development
#
# This script:
# 1. Reads the JWT secret shared with cozy-stack from ~/.cozy/onlyoffice.env
# 2. Stops and removes any existing onlyoffice-ds container
# 3. Collects all instance hostnames from cozy-stack
# 4. Starts OnlyOffice with proper host mappings so it can reach cozy-stack
#
# Usage: ./scripts/start-oo.sh
#
# See docs/docker.md for more information

set -e

CONTAINER_NAME="onlyoffice-ds"
PORT="${OO_PORT:-8000}"
SECRET_FILE="${OO_SECRET_FILE:-$HOME/.cozy/onlyoffice.env}"

if [ ! -f "$SECRET_FILE" ]; then
  echo "Error: $SECRET_FILE not found. Create it with:" >&2
  echo "  (umask 077; printf 'JWT_SECRET=%s\\n' \"\$(openssl rand -hex 32)\" > \"$SECRET_FILE\")" >&2
  exit 1
fi
. "$SECRET_FILE"
if [ -z "$JWT_SECRET" ]; then
  echo "Error: JWT_SECRET is empty in $SECRET_FILE" >&2
  exit 1
fi

echo "Stopping existing OnlyOffice container..."
docker stop $CONTAINER_NAME 2>/dev/null || true
docker rm $CONTAINER_NAME 2>/dev/null || true

echo "Collecting instance hostnames..."
HOSTS=""
for domain in $(go run . instances ls 2>/dev/null | awk '{print $1}' | sed 's/:[0-9]*$//'); do
  HOSTS="$HOSTS --add-host=$domain:host-gateway"
done

if [ -z "$HOSTS" ]; then
  echo "Warning: No instances found. Adding default cozy.localhost"
  HOSTS="--add-host=cozy.localhost:host-gateway"
fi

echo "Starting OnlyOffice Document Server on port $PORT..."
docker run -d --name $CONTAINER_NAME \
  -p $PORT:80 \
  -e JWT_ENABLED=true \
  -e JWT_SECRET="$JWT_SECRET" \
  -e ALLOW_PRIVATE_IP_ADDRESS=true \
  -e ALLOW_META_IP_ADDRESS=true \
  $HOSTS \
  onlyoffice/documentserver:9.4

echo "Waiting for OnlyOffice to be ready..."
for i in {1..60}; do
  if curl -s http://localhost:$PORT/healthcheck 2>/dev/null | grep -q "true"; then
    echo "OnlyOffice is ready at http://localhost:$PORT"
    exit 0
  fi
  sleep 2
  echo -n "."
done

echo ""
echo "Warning: OnlyOffice did not become ready in time. Check logs with:"
echo "  docker logs $CONTAINER_NAME"
