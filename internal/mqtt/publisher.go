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

// NumberEntity defines a numeric HA entity with its valid range.
// Exported so command dispatch can read ranges from the same source.
type NumberEntity struct {
	Name string
	Key  string
	Min  int
	Max  int
	Step int
	Unit string
	Icon string
}

// NumberEntities lists all HA number slider entities and their valid ranges.
// Used by both discovery (publisher) and command validation (main.go dispatch).
var NumberEntities = []NumberEntity{
	{"Volume", "volume", 0, 100, 1, "%", "mdi:volume-high"},
	{"Night Light Timer", "night_light_timeout", 0, 900, 60, "s", "mdi:timer-outline"},
	{"Sound Sensitivity", "sound_sensitivity", 2, 9, 1, "", "mdi:ear-hearing"},
	{"Motion Sensitivity", "motion_sensitivity", 10000, 250000, 10000, "", "mdi:motion-sensor"},
}

// NumberRanges maps command key -> (min, max) for all numeric commands,
// including command-only keys that are not standalone HA number entities
// (e.g. night_light_brightness is part of the light entity).
var NumberRanges map[string][2]int

func init() {
	NumberRanges = make(map[string][2]int, len(NumberEntities)+1)
	for _, n := range NumberEntities {
		NumberRanges[n.Key] = [2]int{n.Min, n.Max}
	}
	NumberRanges["night_light_brightness"] = [2]int{0, 100}
}

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
	p.pubDedup(babyUID, "sound_sensitivity", fmt.Sprintf("%d", snap.Controls.SoundSensitivity))
	p.pubDedup(babyUID, "motion_sensitivity", fmt.Sprintf("%d", snap.Controls.MotionSensitivity))

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
			"state_topic":         p.stateTopic(babyUID, s.key),
			"unique_id":           fmt.Sprintf("%s_%s", deviceID, s.key),
			"device":              device,
			"device_class":        s.devClass,
			"unit_of_measurement": s.unit,
		}, avail)

		p.pubJSON(p.discoveryTopic("sensor", deviceID, s.key), config)
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
			"state_topic": p.stateTopic(babyUID, s.key),
			"unique_id":   fmt.Sprintf("%s_%s", deviceID, s.key),
			"device":      device,
			"payload_on":  s.payloadOn,
			"payload_off": s.payloadOff,
			"icon":        s.icon,
		}
		if s.hasAvail {
			mergeMaps(config, avail)
		}

		p.pubJSON(p.discoveryTopic("binary_sensor", deviceID, s.key), config)
	}

	// --- Light: Night Light (on/off + brightness) ---

	p.pubJSON(p.discoveryTopic("light", deviceID, "night_light"), mergeMaps(map[string]interface{}{
		"name":                     "Night Light",
		"unique_id":                fmt.Sprintf("%s_night_light", deviceID),
		"command_topic":            p.cmdTopic(babyUID, "night_light"),
		"state_topic":              p.stateTopic(babyUID, "night_light"),
		"brightness_command_topic": p.cmdTopic(babyUID, "night_light_brightness"),
		"brightness_state_topic":   p.stateTopic(babyUID, "night_light_brightness"),
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
			"command_topic": p.cmdTopic(babyUID, s.key),
			"state_topic":   p.stateTopic(babyUID, s.stateKey),
			"payload_on":    "ON",
			"payload_off":   "OFF",
			"icon":          s.icon,
			"device":        device,
		}, avail)

		p.pubJSON(p.discoveryTopic("switch", deviceID, s.key), config)
	}

	// --- Numbers (sliders) ---

	for _, n := range NumberEntities {
		config := mergeMaps(map[string]interface{}{
			"name":                n.Name,
			"unique_id":           fmt.Sprintf("%s_%s", deviceID, n.Key),
			"command_topic":       p.cmdTopic(babyUID, n.Key),
			"state_topic":         p.stateTopic(babyUID, n.Key),
			"min":                 n.Min,
			"max":                 n.Max,
			"step":               n.Step,
			"mode":               "slider",
			"unit_of_measurement": n.Unit,
			"icon":               n.Icon,
			"device":             device,
		}, avail)

		p.pubJSON(p.discoveryTopic("number", deviceID, n.Key), config)
	}

	// --- Select: Night Vision ---

	p.pubJSON(p.discoveryTopic("select", deviceID, "night_vision"), mergeMaps(map[string]interface{}{
		"name":          "Night Vision",
		"unique_id":     fmt.Sprintf("%s_night_vision", deviceID),
		"command_topic": p.cmdTopic(babyUID, "night_vision"),
		"state_topic":   p.stateTopic(babyUID, "night_vision"),
		"options":       []string{"off", "auto", "on"},
		"icon":          "mdi:eye-outline",
		"device":        device,
	}, avail))

	// --- Sensor: Breathing BPM ---

	p.pubJSON(p.discoveryTopic("sensor", deviceID, "breathing_bpm"), mergeMaps(map[string]interface{}{
		"name":                "Breathing Rate",
		"unique_id":           fmt.Sprintf("%s_breathing_bpm", deviceID),
		"state_topic":         p.stateTopic(babyUID, "breathing_bpm"),
		"unit_of_measurement": "bpm",
		"icon":                "mdi:lungs",
		"device":              device,
	}, avail))

	// select_track discovery is deferred until soundtracks are known (see PublishState).
}

// publishSelectTrackDiscovery sends/updates the HA select entity for soundtrack selection.
func (p *Publisher) publishSelectTrackDiscovery(babyUID, name string, trackNames []string) {
	deviceID := fmt.Sprintf("nanit_%s", babyUID)

	p.pubJSON(p.discoveryTopic("select", deviceID, "select_track"), mergeMaps(map[string]interface{}{
		"name":          "Sound Track",
		"unique_id":     fmt.Sprintf("%s_select_track", deviceID),
		"command_topic": p.cmdTopic(babyUID, "select_track"),
		"state_topic":   p.stateTopic(babyUID, "current_track"),
		"options":       trackNames,
		"icon":          "mdi:music-note",
		"device":        p.deviceBlock(babyUID, name),
	}, p.availabilityFields(babyUID)))
}

// --- Topic helpers ---

func (p *Publisher) stateTopic(babyUID, key string) string {
	return fmt.Sprintf("%s/%s/%s", p.prefix, babyUID, key)
}

func (p *Publisher) cmdTopic(babyUID, key string) string {
	return fmt.Sprintf("%s/%s/%s/set", p.prefix, babyUID, key)
}

func (p *Publisher) discoveryTopic(component, deviceID, key string) string {
	return fmt.Sprintf("homeassistant/%s/%s_%s/config", component, deviceID, key)
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
		"availability_topic":    p.stateTopic(babyUID, "ws_alive"),
		"payload_available":     "true",
		"payload_not_available": "false",
	}
}

// --- Publish helpers ---

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
	p.client.Publish(p.stateTopic(babyUID, key), 0, true, value)
}

func (p *Publisher) pubJSON(topic string, payload interface{}) {
	data, err := json.Marshal(payload)
	if err != nil {
		log.Printf("[mqtt] marshal error: %v", err)
		return
	}
	p.client.Publish(topic, 0, true, data)
}

// --- Formatting helpers ---

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
