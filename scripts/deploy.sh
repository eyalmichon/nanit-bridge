#!/usr/bin/env bash
# ──────────────────────────────────────────────────────────────────────────────
# nanit-bridge — deploy to Docker host
#
# Run from the Docker host LXC console:
#   bash -c "$(wget -qLO - https://raw.githubusercontent.com/eyalmichon/nanit-bridge/main/scripts/deploy.sh)"
# ──────────────────────────────────────────────────────────────────────────────
set -euo pipefail

REPO="https://github.com/eyalmichon/nanit-bridge.git"
SERVICE_NAME="nanit-bridge"
SERVICES_DIR="/opt/services"
DEPLOY_DIR="${SERVICES_DIR}/${SERVICE_NAME}"

GN="\033[1;32m"  YW="\033[33m"  BL="\033[36m"  RD="\033[01;31m"  CL="\033[m"
header()  { echo -e "\n${BL}──── $1 ────${CL}"; }
msg()     { echo -e " ${GN}✓${CL} $1"; }
info()    { echo -e " ${YW}→${CL} $1"; }
err()     { echo -e " ${RD}✗ $1${CL}" >&2; exit 1; }

command -v docker &>/dev/null || err "Docker not found. Run create-docker-host.sh on the Proxmox host first."
command -v git &>/dev/null    || err "git not found. Install with: apt install git"

header "nanit-bridge — Deploy"

# ── Clone or update repo ────────────────────────────────────────────────────
if [[ -d "${DEPLOY_DIR}/.git" ]]; then
  info "Updating existing installation..."
  cd "$DEPLOY_DIR"
  git pull --ff-only
  msg "Code updated"
else
  info "Cloning repository..."
  mkdir -p "$SERVICES_DIR"
  git clone "$REPO" "$DEPLOY_DIR"
  msg "Repository cloned"
fi

cd "$DEPLOY_DIR"

# ── Config ───────────────────────────────────────────────────────────────────
ENV_FILE="${DEPLOY_DIR}/.env"

if [[ -f "$ENV_FILE" ]]; then
  msg ".env already exists (not overwriting)"
else
  header "Configuration"
  echo ""

  read -rp " Nanit account email: " NANIT_EMAIL
  [[ -z "$NANIT_EMAIL" ]] && err "Nanit email is required."

  read -rsp " Nanit account password: " NANIT_PASSWORD
  echo ""
  [[ -z "$NANIT_PASSWORD" ]] && err "Nanit password is required."

  CT_IP=$(hostname -I 2>/dev/null | awk '{print $1}')
  echo ""
  echo " Your LAN IP must be reachable by the Nanit camera."
  read -rp " LAN IP for RTMP [${CT_IP}]: " RTMP_IP
  RTMP_IP="${RTMP_IP:-$CT_IP}"

  echo ""
  echo " MQTT is optional — enables Home Assistant sensor integration."
  read -rp " MQTT broker URL (leave empty to skip, e.g. tcp://192.168.1.50:1883): " MQTT_URL

  MQTT_USER=""
  MQTT_PASS=""
  if [[ -n "$MQTT_URL" ]]; then
    read -rp " MQTT username (leave empty if none): " MQTT_USER
    if [[ -n "$MQTT_USER" ]]; then
      read -rsp " MQTT password: " MQTT_PASS
      echo ""
    fi
  fi

  cat > "$ENV_FILE" << EOF
NANIT_EMAIL=${NANIT_EMAIL}
NANIT_PASSWORD=${NANIT_PASSWORD}
NANIT_RTMP_ADDR=${RTMP_IP}:1935
NANIT_MQTT_BROKER_URL=${MQTT_URL}
NANIT_MQTT_USERNAME=${MQTT_USER}
NANIT_MQTT_PASSWORD=${MQTT_PASS}
EOF
  chmod 600 "$ENV_FILE"
  msg ".env created"
fi

# ── Generate docker-compose.yml ──────────────────────────────────────────────
COMPOSE_FILE="${SERVICES_DIR}/docker-compose.yml"

header "Registering service"

# Append to shared compose file or create it
if [[ -f "$COMPOSE_FILE" ]] && grep -q "  ${SERVICE_NAME}:" "$COMPOSE_FILE"; then
  msg "Service already in ${COMPOSE_FILE}"
else
  # If no compose file exists, create with services: header
  if [[ ! -f "$COMPOSE_FILE" ]]; then
    echo "services:" > "$COMPOSE_FILE"
  fi

  cat >> "$COMPOSE_FILE" << EOF

  ${SERVICE_NAME}:
    build: ${DEPLOY_DIR}
    container_name: ${SERVICE_NAME}
    restart: unless-stopped
    ports:
      - "1935:1935"
      - "8080:8080"
    volumes:
      - ${DEPLOY_DIR}/.env:/app/.env:ro
      - nanit-data:/data
    env_file: ${DEPLOY_DIR}/.env
    environment:
      NANIT_SESSION_FILE: /data/session.json
      NANIT_PUSH_CREDS_FILE: /data/push_creds.json
      NANIT_DASHBOARD_AUTH_FILE: /data/dashboard_password.hash
      NANIT_RTMP_TOKEN_FILE: /data/rtmp_token
    healthcheck:
      test: ["CMD", "/nanit-bridge", "--healthcheck"]
      interval: 30s
      timeout: 5s
      retries: 3
      start_period: 15s
EOF

  # Add volume if not present
  if ! grep -q "^volumes:" "$COMPOSE_FILE"; then
    cat >> "$COMPOSE_FILE" << EOF

volumes:
  nanit-data:
EOF
  elif ! grep -q "nanit-data:" "$COMPOSE_FILE"; then
    echo "  nanit-data:" >> "$COMPOSE_FILE"
  fi

  msg "Written ${COMPOSE_FILE}"
fi

# ── Build and start ──────────────────────────────────────────────────────────
header "Building and starting"
cd "$SERVICES_DIR"
docker compose up -d --build "$SERVICE_NAME"
msg "Container is running"

# ── Done ─────────────────────────────────────────────────────────────────────
CT_IP=$(hostname -I 2>/dev/null | awk '{print $1}')
header "Deployed!"
echo ""
echo -e " ${GN}Dashboard:${CL}  http://${CT_IP}:8080"
echo -e " ${GN}RTMP:${CL}       rtmp://${CT_IP}:1935 (token on settings page)"
echo -e " ${GN}Status:${CL}     docker compose -f ${COMPOSE_FILE} ps"
echo -e " ${GN}Logs:${CL}       docker compose -f ${COMPOSE_FILE} logs -f ${SERVICE_NAME}"
echo -e " ${GN}Restart:${CL}    docker compose -f ${COMPOSE_FILE} restart ${SERVICE_NAME}"
echo ""
echo -e " ${YW}Open the dashboard to complete Nanit cloud authentication.${CL}"
echo -e " ${YW}To update later, run this same one-liner again.${CL}"
echo ""
