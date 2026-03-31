# nanit-bridge

Local bridge for Nanit baby monitors. Connects to the Nanit cloud, relays the camera's RTMP stream locally, publishes sensor data over MQTT, and serves a real-time web dashboard — no Nanit subscription required for local video.

## Features

- **Local RTMP relay** — camera publishes via Nanit cloud WebSocket, bridge re-publishes as a standard RTMP stream (~0.5s latency)
- **Frigate / go2rtc compatible** — plug the RTMP URL into go2rtc for RTSP conversion, then into Frigate for recording + detection
- **MQTT → Home Assistant** — temperature, humidity, light level, night mode, cry/sound/motion alerts with auto-discovery
- **Web dashboard** — live video, sensor gauges, camera controls (night light, sound machine, volume, etc.)
- **Token-authenticated RTMP** — all stream connections require a token in the URL path
- **Dashboard auth** — password-protected with bcrypt + session cookies

## Quick Start

### Homelab CLI (recommended)

If you're using the [proxmox-homelab](https://github.com/eyalmichon/proxmox-homelab) Docker host:

```bash
homelab install nanit-bridge
```

The CLI will prompt for your Nanit credentials, LAN IP, and optional MQTT config, then build and start the container.

### Manual Docker Compose

1. Clone the repo and create a `.env` file:

```bash
git clone https://github.com/eyalmichon/nanit-bridge.git
cd nanit-bridge
cp .env.example .env
```

2. Edit `.env` with your details:

```env
NANIT_EMAIL=you@example.com
NANIT_PASSWORD=your-nanit-password
NANIT_RTMP_ADDR=192.168.1.100:1935   # your LAN IP (must be reachable by the camera)
```

3. Start:

```bash
docker compose up -d --build
```

4. Open `http://<your-ip>:8080` — the setup page will guide you through Nanit cloud authentication (including MFA if enabled).

5. Once authenticated, the RTMP token and per-baby stream URLs are on the **Settings** page.

## Environment Variables

| Variable | Required | Default | Description |
|---|---|---|---|
| `NANIT_EMAIL` | Yes* | — | Nanit account email |
| `NANIT_PASSWORD` | Yes* | — | Nanit account password |
| `NANIT_RTMP_ADDR` | Yes | — | LAN IP + port the camera can reach (e.g. `192.168.1.100:1935`) |
| `NANIT_RTMP_PORT` | No | `1935` | RTMP listen port |
| `NANIT_HTTP_PORT` | No | `8080` | Dashboard / API listen port |
| `NANIT_SENSOR_POLL_SEC` | No | `30` | Sensor polling interval in seconds |
| `NANIT_MQTT_BROKER_URL` | No | — | MQTT broker (e.g. `tcp://192.168.1.50:1883`) |
| `NANIT_MQTT_USERNAME` | No | — | MQTT username |
| `NANIT_MQTT_PASSWORD` | No | — | MQTT password |
| `NANIT_MQTT_PREFIX` | No | `nanit` | MQTT topic prefix |
| `NANIT_LOG_LEVEL` | No | `info` | Log level (`debug`, `info`, `warn`, `error`) |
| `NANIT_RTMP_TOKEN` | No | auto-generated | Override the RTMP auth token (min 8 chars) |
| `NANIT_RTMP_TOKEN_FILE` | No | `/data/rtmp_token` | Path to persist the RTMP token |
| `NANIT_DASHBOARD_PASSWORD` | No | — | Set dashboard password on first start |
| `NANIT_DASHBOARD_AUTH_FILE` | No | `/data/dashboard_password.hash` | Path to dashboard password hash |
| `NANIT_SESSION_FILE` | No | `/data/session.json` | Path to persist Nanit session |
| `NANIT_PUSH_CREDS_FILE` | No | `/data/push_creds.json` | Path to persist push notification credentials |

*Email/password can also be entered via the web setup page after first start.

## Frigate / go2rtc Integration

Get your RTMP stream URL from the Settings page (`http://<ip>:8080/settings`), then configure go2rtc and Frigate:

```yaml
# Frigate config
go2rtc:
  streams:
    nanit:
      - rtmp://192.168.1.100:1935/{token}/{baby_uid}

cameras:
  nanit:
    ffmpeg:
      inputs:
        - path: rtsp://127.0.0.1:8554/nanit
          roles: [detect, record]
```

Replace `{token}` and `{baby_uid}` with the values from the Settings page (or use the copy button).

## Home Assistant MQTT

When `NANIT_MQTT_BROKER_URL` is set, the bridge publishes sensor data with HA auto-discovery:

- `nanit/{baby_uid}/temperature` — room temperature (°C)
- `nanit/{baby_uid}/humidity` — relative humidity (%)
- `nanit/{baby_uid}/light` — light level (lux)
- `nanit/{baby_uid}/night` — night mode (on/off)
- `nanit/{baby_uid}/cry_detected` — cry alert
- `nanit/{baby_uid}/sound_alert` — sound alert
- `nanit/{baby_uid}/motion_alert` — motion alert

Entities appear automatically in Home Assistant under the device name.

## Dashboard

The web dashboard at `http://<ip>:8080` provides:

- Live video stream with audio
- Sensor readings (temperature, humidity, light)
- Camera controls: night light (on/off, brightness, timer), sound machine (play/pause, track selection, volume), night vision, status LED, mic mute, sleep mode
- Breathing monitor status
- Camera info (firmware, connection state, logs)
- RTMP token management on the Settings page

## RTMP Authentication

All RTMP connections require a token in the URL: `rtmp://host:port/{token}/{baby_uid}`.

- A 32-character hex token is auto-generated on first start and saved to `/data/rtmp_token`
- Override with `NANIT_RTMP_TOKEN` env var (minimum 8 characters)
- View and regenerate the token from the dashboard Settings page
- After regenerating, update your Frigate/go2rtc config with the new URL

## Reset Dashboard Password

```bash
docker compose exec nanit-bridge /nanit-bridge --reset-dashboard-password
```

## Building from Source

```bash
make setup
make build
./bin/nanit-bridge
```

Requires Go 1.25+, `protoc`, and `protoc-gen-go`.

## License

MIT
