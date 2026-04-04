package mqtt

import (
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	paho "github.com/eclipse/paho.mqtt.golang"

	"nanit-bridge/internal/baby"
)

const (
	mqttKeepalive    = 30 * time.Second
	mqttConnectRetry = 5 * time.Second
)

type Config struct {
	BrokerURL string
	Username  string
	Password  string
	Prefix    string
}

// CommandHandler is called when an MQTT command is received on {prefix}/{babyUID}/{key}/set.
type CommandHandler func(babyUID, key, payload string)

type Publisher struct {
	client     paho.Client
	prefix     string
	cmdHandler CommandHandler
	mu         sync.Mutex
	lastPub    map[string]string // "{babyUID}/{key}" -> last published value
}

// NewPublisher connects to the MQTT broker. If cfg.BrokerURL is empty, MQTT
// is disabled and a nil *Publisher is returned. All Publisher methods are
// nil-receiver safe, so callers may use the result without nil checks.
func NewPublisher(cfg Config) (*Publisher, error) {
	if cfg.BrokerURL == "" {
		log.Printf("[mqtt] no broker URL configured, MQTT disabled")
		return nil, nil
	}

	p := &Publisher{
		prefix:  cfg.Prefix,
		lastPub: make(map[string]string),
	}

	opts := paho.NewClientOptions().
		AddBroker(cfg.BrokerURL).
		SetClientID(fmt.Sprintf("%s-bridge", cfg.Prefix)).
		SetKeepAlive(mqttKeepalive).
		SetAutoReconnect(true).
		SetConnectRetry(true).
		SetConnectRetryInterval(mqttConnectRetry).
		SetConnectionLostHandler(func(_ paho.Client, err error) {
			log.Printf("[mqtt] connection lost: %v", err)
		}).
		SetOnConnectHandler(func(c paho.Client) {
			log.Printf("[mqtt] connected to broker")
			if p.cmdHandler != nil {
				topic := fmt.Sprintf("%s/+/+/set", p.prefix)
				c.Subscribe(topic, 1, p.handleCommand)
			}
		})

	if cfg.Username != "" {
		opts.SetUsername(cfg.Username)
		opts.SetPassword(cfg.Password)
	}

	client := paho.NewClient(opts)
	token := client.Connect()
	token.Wait()
	if err := token.Error(); err != nil {
		return nil, fmt.Errorf("mqtt connect: %w", err)
	}

	p.client = client
	return p, nil
}

// SetCommandHandler registers a callback for incoming MQTT commands and
// subscribes to the wildcard command topic. Safe to call after NewPublisher.
func (p *Publisher) SetCommandHandler(handler CommandHandler) {
	if p == nil {
		return
	}
	p.cmdHandler = handler
	if p.client != nil && p.client.IsConnected() {
		topic := fmt.Sprintf("%s/+/+/set", p.prefix)
		p.client.Subscribe(topic, 1, p.handleCommand)
	}
}

func (p *Publisher) handleCommand(_ paho.Client, msg paho.Message) {
	if p.cmdHandler == nil {
		return
	}

	// Topic format: {prefix}/{babyUID}/{key}/set
	topic := msg.Topic()
	trimmed := strings.TrimPrefix(topic, p.prefix+"/")
	trimmed = strings.TrimSuffix(trimmed, "/set")
	parts := strings.SplitN(trimmed, "/", 2)
	if len(parts) != 2 {
		log.Printf("[mqtt] ignoring malformed command topic: %s", topic)
		return
	}

	babyUID := parts[0]
	key := parts[1]
	payload := strings.TrimSpace(string(msg.Payload()))

	log.Printf("[mqtt] command received: %s/%s = %s", babyUID, key, payload)
	p.cmdHandler(babyUID, key, payload)
}

func (p *Publisher) Close() {
	if p == nil || p.client == nil {
		return
	}
	p.client.Disconnect(1000)
}

// PublishState publishes sensor and control state for a baby. Values are
// deduplicated: only changed values are sent to the broker.
func (p *Publisher) PublishState(babyUID, name string, state *baby.State) {
	if p == nil {
		return
	}

	snap := state.Snapshot()

	// Sensor data
	p.pubDedup(babyUID, "temperature", fmt.Sprintf("%.1f", snap.Sensors.Temperature))
	p.pubDedup(babyUID, "humidity", fmt.Sprintf("%.1f", snap.Sensors.Humidity))
	p.pubDedup(babyUID, "light", fmt.Sprintf("%.1f", snap.Sensors.Light))
	p.pubDedup(babyUID, "is_night", boolStr(snap.Sensors.IsNight))
	p.pubDedup(babyUID, "cry_detected", boolStr(snap.Sensors.CryDetected))
	p.pubDedup(babyUID, "sound_alert", boolStr(snap.Sensors.SoundAlert))
	p.pubDedup(babyUID, "motion_alert", boolStr(snap.Sensors.MotionAlert))
	p.pubDedup(babyUID, "stream_state", snap.Stream.String())
	p.pubDedup(babyUID, "ws_alive", boolStr(snap.WSAlive))

	// Control state
	p.pubDedup(babyUID, "night_light", onOffStr(snap.Controls.NightLight))
	p.pubDedup(babyUID, "night_light_brightness", fmt.Sprintf("%d", snap.Controls.NightLightBrightness))
	p.pubDedup(babyUID, "night_light_timeout", fmt.Sprintf("%d", snap.Controls.NightLightTimeout))
	p.pubDedup(babyUID, "volume", fmt.Sprintf("%d", snap.Controls.Volume))
	p.pubDedup(babyUID, "playback", onOffStr(snap.Controls.PlaybackActive))
	p.pubDedup(babyUID, "current_track", snap.Controls.CurrentTrack)
	p.pubDedup(babyUID, "sleep_mode", onOffStr(snap.Controls.SleepMode))
	p.pubDedup(babyUID, "night_vision", nightVisionStr(snap.Controls.NightVision))
	p.pubDedup(babyUID, "status_light", onOffStr(snap.Controls.StatusLight))
	p.pubDedup(babyUID, "mic_mute", onOffStr(snap.Controls.MicMute))
	p.pubDedup(babyUID, "breathing_active", onOffStr(snap.Controls.Breathing.Active))
	p.pubDedup(babyUID, "breathing_bpm", fmt.Sprintf("%d", snap.Controls.Breathing.BreathsPerMin))

	// Re-publish select_track discovery when the soundtrack list changes.
	if len(snap.Controls.Soundtracks) > 0 {
		trackNames := make([]string, len(snap.Controls.Soundtracks))
		for i, t := range snap.Controls.Soundtracks {
			trackNames[i] = t.Name
		}
		tracksJSON, _ := json.Marshal(trackNames)
		tracksKey := babyUID + "/soundtracks"
		p.mu.Lock()
		changed := p.lastPub[tracksKey] != string(tracksJSON)
		if changed {
			p.lastPub[tracksKey] = string(tracksJSON)
		}
		p.mu.Unlock()
		if changed {
			p.publishSelectTrackDiscovery(babyUID, name, trackNames)
		}
	}
}

// PublishDiscovery sends Home Assistant MQTT auto-discovery messages for a baby.
func (p *Publisher) PublishDiscovery(babyUID, name string) {
	if p == nil {
		return
	}

	deviceID := fmt.Sprintf("nanit_%s", babyUID)
	device := p.deviceBlock(babyUID, name)
	avail := p.availabilityFields(babyUID)

	// --- Sensors (temperature, humidity, light) ---

	sensors := []struct {
		name     string
		key      string
		unit     string
		devClass string
	}{
		{"Temperature", "temperature", "°C", "temperature"},
		{"Humidity", "humidity", "%", "humidity"},
		{"Light", "light", "lx", "illuminance"},
	}

	for _, s := range sensors {
		config := mergeMaps(map[string]interface{}{
			"name":                s.name,
			"state_topic":         fmt.Sprintf("%s/%s/%s", p.prefix, babyUID, s.key),
			"unique_id":           fmt.Sprintf("%s_%s", deviceID, s.key),
			"device":              device,
			"device_class":        s.devClass,
			"unit_of_measurement": s.unit,
		}, avail)

		topic := fmt.Sprintf("homeassistant/sensor/%s_%s/config", deviceID, s.key)
		p.pubJSON(topic, config)
	}

	// --- Binary sensors ---

	binarySensors := []struct {
		name       string
		key        string
		icon       string
		payloadOn  string
		payloadOff string
		hasAvail   bool // false for ws_alive — it IS the availability signal
	}{
		{"Night Mode", "is_night", "mdi:weather-night", "true", "false", true},
		{"Cry Detected", "cry_detected", "mdi:emoticon-cry-outline", "true", "false", true},
		{"Sound Alert", "sound_alert", "mdi:volume-high", "true", "false", true},
		{"Motion Alert", "motion_alert", "mdi:motion-sensor", "true", "false", true},
		{"Stream Active", "stream_state", "mdi:video", "active", "stopped", true},
		{"WebSocket", "ws_alive", "mdi:lan-connect", "true", "false", false},
	}

	for _, s := range binarySensors {
		config := map[string]interface{}{
			"name":        s.name,
			"state_topic": fmt.Sprintf("%s/%s/%s", p.prefix, babyUID, s.key),
			"unique_id":   fmt.Sprintf("%s_%s", deviceID, s.key),
			"device":      device,
			"payload_on":  s.payloadOn,
			"payload_off": s.payloadOff,
			"icon":        s.icon,
		}
		if s.hasAvail {
			mergeMaps(config, avail)
		}

		topic := fmt.Sprintf("homeassistant/binary_sensor/%s_%s/config", deviceID, s.key)
		p.pubJSON(topic, config)
	}

	// --- Light: Night Light (on/off + brightness) ---

	p.pubJSON(fmt.Sprintf("homeassistant/light/%s_night_light/config", deviceID), mergeMaps(map[string]interface{}{
		"name":                     "Night Light",
		"unique_id":                fmt.Sprintf("%s_night_light", deviceID),
		"command_topic":            fmt.Sprintf("%s/%s/night_light/set", p.prefix, babyUID),
		"state_topic":              fmt.Sprintf("%s/%s/night_light", p.prefix, babyUID),
		"brightness_command_topic": fmt.Sprintf("%s/%s/night_light_brightness/set", p.prefix, babyUID),
		"brightness_state_topic":   fmt.Sprintf("%s/%s/night_light_brightness", p.prefix, babyUID),
		"brightness_scale":         100,
		"payload_on":               "ON",
		"payload_off":              "OFF",
		"icon":                     "mdi:lightbulb-night",
		"device":                   device,
	}, avail))

	// --- Switches ---

	switches := []struct {
		name     string
		key      string
		stateKey string
		icon     string
	}{
		{"Sound Machine", "playback", "playback", "mdi:music"},
		{"Sleep Mode", "sleep_mode", "sleep_mode", "mdi:sleep"},
		{"Status LED", "status_light", "status_light", "mdi:led-on"},
		{"Mic Mute", "mic_mute", "mic_mute", "mdi:microphone-off"},
		{"Breathing Monitor", "breathing_monitoring", "breathing_active", "mdi:lungs"},
	}

	for _, s := range switches {
		config := mergeMaps(map[string]interface{}{
			"name":          s.name,
			"unique_id":     fmt.Sprintf("%s_%s", deviceID, s.key),
			"command_topic": fmt.Sprintf("%s/%s/%s/set", p.prefix, babyUID, s.key),
			"state_topic":   fmt.Sprintf("%s/%s/%s", p.prefix, babyUID, s.stateKey),
			"payload_on":    "ON",
			"payload_off":   "OFF",
			"icon":          s.icon,
			"device":        device,
		}, avail)

		topic := fmt.Sprintf("homeassistant/switch/%s_%s/config", deviceID, s.key)
		p.pubJSON(topic, config)
	}

	// --- Numbers (sliders) ---

	numbers := []struct {
		name string
		key  string
		min  int
		max  int
		step int
		unit string
		icon string
	}{
		{"Volume", "volume", 0, 100, 1, "%", "mdi:volume-high"},
		{"Night Light Timer", "night_light_timeout", 0, 900, 60, "s", "mdi:timer-outline"},
	}

	for _, n := range numbers {
		config := mergeMaps(map[string]interface{}{
			"name":                n.name,
			"unique_id":           fmt.Sprintf("%s_%s", deviceID, n.key),
			"command_topic":       fmt.Sprintf("%s/%s/%s/set", p.prefix, babyUID, n.key),
			"state_topic":         fmt.Sprintf("%s/%s/%s", p.prefix, babyUID, n.key),
			"min":                 n.min,
			"max":                 n.max,
			"step":               n.step,
			"mode":               "slider",
			"unit_of_measurement": n.unit,
			"icon":               n.icon,
			"device":             device,
		}, avail)

		topic := fmt.Sprintf("homeassistant/number/%s_%s/config", deviceID, n.key)
		p.pubJSON(topic, config)
	}

	// --- Select: Night Vision ---

	p.pubJSON(fmt.Sprintf("homeassistant/select/%s_night_vision/config", deviceID), mergeMaps(map[string]interface{}{
		"name":          "Night Vision",
		"unique_id":     fmt.Sprintf("%s_night_vision", deviceID),
		"command_topic": fmt.Sprintf("%s/%s/night_vision/set", p.prefix, babyUID),
		"state_topic":   fmt.Sprintf("%s/%s/night_vision", p.prefix, babyUID),
		"options":       []string{"off", "auto", "on"},
		"icon":          "mdi:eye-outline",
		"device":        device,
	}, avail))

	// --- Sensor: Breathing BPM ---

	p.pubJSON(fmt.Sprintf("homeassistant/sensor/%s_breathing_bpm/config", deviceID), mergeMaps(map[string]interface{}{
		"name":                "Breathing Rate",
		"unique_id":           fmt.Sprintf("%s_breathing_bpm", deviceID),
		"state_topic":         fmt.Sprintf("%s/%s/breathing_bpm", p.prefix, babyUID),
		"unit_of_measurement": "bpm",
		"icon":                "mdi:lungs",
		"device":              device,
	}, avail))

	// select_track discovery is deferred until soundtracks are known (see PublishState).
}

// publishSelectTrackDiscovery sends/updates the HA select entity for soundtrack selection.
func (p *Publisher) publishSelectTrackDiscovery(babyUID, name string, trackNames []string) {
	deviceID := fmt.Sprintf("nanit_%s", babyUID)

	p.pubJSON(fmt.Sprintf("homeassistant/select/%s_select_track/config", deviceID), mergeMaps(map[string]interface{}{
		"name":          "Sound Track",
		"unique_id":     fmt.Sprintf("%s_select_track", deviceID),
		"command_topic": fmt.Sprintf("%s/%s/select_track/set", p.prefix, babyUID),
		"state_topic":   fmt.Sprintf("%s/%s/current_track", p.prefix, babyUID),
		"options":       trackNames,
		"icon":          "mdi:music-note",
		"device":        p.deviceBlock(babyUID, name),
	}, p.availabilityFields(babyUID)))
}

func (p *Publisher) deviceBlock(babyUID, name string) map[string]interface{} {
	return map[string]interface{}{
		"identifiers":  []string{fmt.Sprintf("nanit_%s", babyUID)},
		"name":         fmt.Sprintf("Nanit %s", name),
		"manufacturer": "Nanit",
		"model":        "Baby Monitor",
	}
}

func (p *Publisher) availabilityFields(babyUID string) map[string]interface{} {
	return map[string]interface{}{
		"availability_topic":    fmt.Sprintf("%s/%s/ws_alive", p.prefix, babyUID),
		"payload_available":     "true",
		"payload_not_available": "false",
	}
}

func (p *Publisher) pubDedup(babyUID, key, value string) {
	cacheKey := babyUID + "/" + key
	p.mu.Lock()
	if p.lastPub[cacheKey] == value {
		p.mu.Unlock()
		return
	}
	p.lastPub[cacheKey] = value
	p.mu.Unlock()
	p.pub(babyUID, key, value)
}

func (p *Publisher) pub(babyUID, key, value string) {
	topic := fmt.Sprintf("%s/%s/%s", p.prefix, babyUID, key)
	p.client.Publish(topic, 0, true, value)
}

func (p *Publisher) pubJSON(topic string, payload interface{}) {
	data, err := json.Marshal(payload)
	if err != nil {
		log.Printf("[mqtt] marshal error: %v", err)
		return
	}
	p.client.Publish(topic, 0, true, data)
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

func onOffStr(b bool) string {
	if b {
		return "ON"
	}
	return "OFF"
}

func nightVisionStr(mode int32) string {
	switch mode {
	case 1:
		return "auto"
	case 2:
		return "on"
	default:
		return "off"
	}
}

func mergeMaps(base, extra map[string]interface{}) map[string]interface{} {
	for k, v := range extra {
		base[k] = v
	}
	return base
}
