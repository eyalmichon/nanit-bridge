# nanit-bridge

A lightweight Go service that connects to a Nanit baby monitor, receives local RTMP video, and publishes sensor data via MQTT. Includes a web dashboard for live monitoring and camera control. Designed for integration with Frigate NVR and Home Assistant.

## How it works

```
Nanit Camera ──RTMP (local LAN)──► nanit-bridge ──RTMP──► Frigate (go2rtc)
                                        │
                                        ├──MQTT──► Home Assistant
                                        │
                                        └──HTTP──► Web Dashboard
```

1. Authenticates with Nanit's cloud API (`api.nanit.com`)
2. Opens a WebSocket to signal the camera via protobuf
3. Tells the camera to push RTMP to this service's local IP
4. Camera streams video directly over your LAN (low latency)
5. Frigate's go2rtc pulls the RTMP stream
6. Sensor data (temperature, humidity, light) published to MQTT
7. Push notifications (sound, motion, cry) via Firebase Cloud Messaging

## Features

- **Local RTMP video relay** — camera streams directly over your LAN
- **HTTP-FLV live stream** — watch in the dashboard without any extra software
- **Web dashboard** — real-time sensor data, camera controls, live video
- **Camera controls** — night light, sound machine (with track selection), volume, sensitivity
- **Push notifications** — instant sound, motion, and cry alerts via FCM (no polling)
- **Notification toggles** — enable/disable sound, motion, and cry alerts
- **Sensitivity sliders** — adjust sound and motion detection thresholds
- **MQTT publishing** — sensor data + HA auto-discovery
- **Auto-reconnect** — exponential backoff with jitter on WebSocket disconnect

## Quick start

### 1. Copy the env file and fill in your credentials

```bash
cp .env.example .env
# Edit .env with your Nanit email, password, LAN IP, and MQTT broker
```

### 2. Run with Docker Compose

```bash
docker compose up -d
```

On first run, if MFA is required, run interactively to enter the code:

```bash
docker compose run --rm nanit-bridge
```

### 3. Open the dashboard

Navigate to `http://NANIT_BRIDGE_IP:8080` to see the live dashboard with sensor readings, video stream, and camera controls.

### 4. Configure Frigate

```yaml
go2rtc:
  streams:
    nanit:
      - rtmp://NANIT_BRIDGE_IP:1935/local/YOUR_BABY_UID

cameras:
  nanit:
    ffmpeg:
      inputs:
        - path: rtsp://127.0.0.1:8554/nanit
          roles:
            - detect
            - record
```

### 5. Sensor data in Home Assistant

If MQTT is configured, HA auto-discovery creates entities automatically:
- `sensor.nanit_<name>_temperature`
- `sensor.nanit_<name>_humidity`
- `sensor.nanit_<name>_light`
- `binary_sensor.nanit_<name>_night_mode`
- `binary_sensor.nanit_<name>_sound_alert`
- `binary_sensor.nanit_<name>_motion_alert`

## Push notifications (FCM)

nanit-bridge can register as an FCM client to receive instant sound, motion, and cry alerts — the same mechanism the official Nanit app uses. This eliminates polling and gives sub-second alert latency.

On first start, the bridge automatically registers with FCM and stores credentials in the push creds file. No manual setup is required.

## Commands

The project has two binaries:

| Command | Description |
|---|---|
| `cmd/nanit-bridge` | Main service — runs the RTMP relay, dashboard, MQTT, and FCM receiver |
| `cmd/nanit-debug` | Diagnostic tool — connects to the camera WebSocket and dumps all raw protobuf messages for reverse engineering |

## Development (devcontainer)

Open in VS Code / Cursor and "Reopen in Container". The devcontainer has Go, protoc, and all tools pre-installed.

```bash
make setup    # generate proto + tidy modules
make build    # compile
make run      # compile + run
```

## Environment variables

| Variable | Required | Default | Description |
|---|---|---|---|
| `NANIT_EMAIL` | Yes* | | Nanit account email |
| `NANIT_PASSWORD` | Yes* | | Nanit account password |
| `NANIT_RTMP_ADDR` | Yes | | LAN IP:port the camera pushes RTMP to (e.g. `192.168.1.100:1935`) |
| `NANIT_RTMP_PORT` | No | `1935` | RTMP listen port |
| `NANIT_HTTP_PORT` | No | `8080` | Web dashboard port |
| `NANIT_SENSOR_POLL_SEC` | No | `30` | Sensor data poll interval in seconds |
| `NANIT_MQTT_BROKER_URL` | No | | MQTT broker (e.g. `tcp://192.168.1.10:1883`) |
| `NANIT_MQTT_USERNAME` | No | | MQTT username |
| `NANIT_MQTT_PASSWORD` | No | | MQTT password |
| `NANIT_MQTT_PREFIX` | No | `nanit` | MQTT topic prefix |
| `NANIT_SESSION_FILE` | No | `/data/session.json` | Token persistence file |
| `NANIT_PUSH_CREDS_FILE` | No | `/data/push_creds.json` | FCM credentials persistence file |
| `NANIT_LOG_LEVEL` | No | `info` | Log level |

*Not required after initial login if a valid session file exists.

## Protocol reference

Based on reverse-engineered protocol from open-source projects (indiefan/home_assistant_nanit, aionanit, homebridge-nanit).
See `internal/nanit/proto/nanit.proto` for the full protobuf schema.

## License

MIT
