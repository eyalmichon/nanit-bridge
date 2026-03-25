# nanit-bridge

A lightweight Go service that connects to a Nanit baby monitor, receives local RTMP video, and publishes sensor data via MQTT. Designed for integration with Frigate NVR and Home Assistant.

## How it works

```
Nanit Camera ──RTMP (local LAN)──► nanit-bridge ──RTMP──► Frigate (go2rtc)
                                        │
                                        └──MQTT──► Home Assistant
```

1. Authenticates with Nanit's cloud API (`api.nanit.com`)
2. Opens a WebSocket to signal the camera via protobuf
3. Tells the camera to push RTMP to this service's local IP
4. Camera streams video directly over your LAN (low latency)
5. Frigate's go2rtc pulls the RTMP stream
6. Sensor data (temperature, humidity, light) published to MQTT

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

### 3. Configure Frigate

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

### 4. Sensor data in Home Assistant

If MQTT is configured, HA auto-discovery creates entities automatically:
- `sensor.nanit_<name>_temperature`
- `sensor.nanit_<name>_humidity`
- `sensor.nanit_<name>_light`
- `binary_sensor.nanit_<name>_night_mode`
- `binary_sensor.nanit_<name>_sound_alert`
- `binary_sensor.nanit_<name>_motion_alert`

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
| `NANIT_MQTT_BROKER_URL` | No | | MQTT broker (e.g. `tcp://192.168.1.10:1883`) |
| `NANIT_MQTT_USERNAME` | No | | MQTT username |
| `NANIT_MQTT_PASSWORD` | No | | MQTT password |
| `NANIT_MQTT_PREFIX` | No | `nanit` | MQTT topic prefix |
| `NANIT_SESSION_FILE` | No | `/data/session.json` | Token persistence file |
| `NANIT_LOG_LEVEL` | No | `info` | Log level |

*Not required after initial login if a valid session file exists.

## Protocol reference

Based on reverse-engineered protocol from open-source projects (indiefan/home_assistant_nanit, aionanit, homebridge-nanit).
See `internal/nanit/proto/nanit.proto` for the full protobuf schema.

## License

MIT
