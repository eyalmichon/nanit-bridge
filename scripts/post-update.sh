#!/usr/bin/env bash
# Resolve NANIT_RTMP_ADDR=auto to the host's LAN IP.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")/.." && pwd)"
ENV_FILE="${1:-${SCRIPT_DIR}/.env}"
LAN_IP=$(hostname -I | awk '{print $1}')

if grep -qiE "NANIT_RTMP_ADDR=['\"]?auto['\"]?" "$ENV_FILE" 2>/dev/null; then
  sed -i -E "s|NANIT_RTMP_ADDR=['\"]?auto['\"]?|NANIT_RTMP_ADDR=\"${LAN_IP}:1935\"|i" "$ENV_FILE"
  echo "  → Set NANIT_RTMP_ADDR to ${LAN_IP}:1935"
fi

# Migrate /data volume permissions for nonroot container (UID 65532).
# Docker Compose prefixes volumes with the project name (directory basename),
# so the real volume is e.g. "nanit-bridge_nanit-data", not "nanit-data".
if command -v docker >/dev/null 2>&1; then
  docker image inspect alpine &>/dev/null || docker pull alpine 2>/dev/null || true

  PROJECT_NAME="$(basename "$SCRIPT_DIR")"
  for VOLUME in "${PROJECT_NAME}_nanit-data" "nanit-data"; do
    if docker volume inspect "$VOLUME" >/dev/null 2>&1; then
      OWNER=$(docker run --rm -v "${VOLUME}:/data" alpine stat -c '%u' /data 2>/dev/null || echo "")
      if [ "$OWNER" != "65532" ]; then
        echo "  → Migrating $VOLUME permissions for nonroot container..."
        docker run --rm -v "${VOLUME}:/data" alpine chown -R 65532:65532 /data || true
      fi
      break
    fi
  done
fi
