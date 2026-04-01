#!/usr/bin/env bash
# Resolve NANIT_RTMP_ADDR=auto to the host's LAN IP.
set -euo pipefail

ENV_FILE="${1:-.env}"
LAN_IP=$(hostname -I | awk '{print $1}')

if grep -qiE "NANIT_RTMP_ADDR=['\"]?auto['\"]?" "$ENV_FILE" 2>/dev/null; then
  sed -i -E "s|NANIT_RTMP_ADDR=['\"]?auto['\"]?|NANIT_RTMP_ADDR=\"${LAN_IP}:1935\"|i" "$ENV_FILE"
  echo "  → Set NANIT_RTMP_ADDR to ${LAN_IP}:1935"
fi
