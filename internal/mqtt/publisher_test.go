package mqtt

import (
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	paho "github.com/eclipse/paho.mqtt.golang"

	"nanit-bridge/internal/baby"
)

// mockToken implements paho.Token for test use.
type mockToken struct{}

func (mockToken) Wait() bool                  { return true }
func (mockToken) WaitTimeout(_ time.Duration) bool { return true }
func (mockToken) Done() <-chan struct{} {
	ch := make(chan struct{})
	close(ch)
	return ch
}
func (mockToken) Error() error { return nil }

// published records a single MQTT publish call.
type published struct {
	topic    string
	qos      byte
	retained bool
	payload  string
}

// mockClient implements paho.Client for test use.
type mockClient struct {
	mu   sync.Mutex
	pubs []published
}

func (m *mockClient) Publish(topic string, qos byte, retained bool, payload interface{}) paho.Token {
	m.mu.Lock()
	defer m.mu.Unlock()
	var s string
	switch v := payload.(type) {
	case string:
		s = v
	case []byte:
		s = string(v)
	}
	m.pubs = append(m.pubs, published{topic, qos, retained, s})
	return mockToken{}
}

func (m *mockClient) lastPayloads() map[string]string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]string, len(m.pubs))
	for _, p := range m.pubs {
		out[p.topic] = p.payload
	}
	return out
}

func (m *mockClient) reset() {
	m.mu.Lock()
	m.pubs = nil
	m.mu.Unlock()
}

// Stubs for the rest of paho.Client interface.
func (m *mockClient) IsConnected() bool                                                    { return true }
func (m *mockClient) IsConnectionOpen() bool                                               { return true }
func (m *mockClient) Connect() paho.Token                                                  { return mockToken{} }
func (m *mockClient) Disconnect(uint)                                                      {}
func (m *mockClient) Subscribe(string, byte, paho.MessageHandler) paho.Token               { return mockToken{} }
func (m *mockClient) SubscribeMultiple(map[string]byte, paho.MessageHandler) paho.Token    { return mockToken{} }
func (m *mockClient) Unsubscribe(...string) paho.Token                                     { return mockToken{} }
func (m *mockClient) AddRoute(string, paho.MessageHandler)                                 {}
func (m *mockClient) OptionsReader() paho.ClientOptionsReader                              { return paho.ClientOptionsReader{} }

func newTestPublisher() (*Publisher, *mockClient) {
	mc := &mockClient{}
	p := &Publisher{
		client:  mc,
		prefix:  "nanit",
		lastPub: make(map[string]string),
	}
	return p, mc
}

func TestPublishStateSensors(t *testing.T) {
	p, mc := newTestPublisher()
	state := baby.NewState("abc123", "cam1", "Leo")
	state.UpdateSensors(func(s *baby.SensorState) {
		s.Temperature = 22.5
		s.Humidity = 55.2
		s.Light = 120.0
		s.IsNight = true
		s.CryDetected = true
	})
	state.SetWSAlive(true)

	p.PublishState("abc123", "Leo", state)

	pubs := mc.lastPayloads()

	checks := map[string]string{
		"nanit/abc123/temperature":  "22.5",
		"nanit/abc123/humidity":     "55.2",
		"nanit/abc123/light":        "120.0",
		"nanit/abc123/is_night":     "true",
		"nanit/abc123/cry_detected": "true",
		"nanit/abc123/ws_alive":     "true",
	}
	for topic, want := range checks {
		if got := pubs[topic]; got != want {
			t.Errorf("%s = %q, want %q", topic, got, want)
		}
	}
}

func TestPublishStateControls(t *testing.T) {
	p, mc := newTestPublisher()
	state := baby.NewState("abc123", "cam1", "Leo")
	state.UpdateControls(func(c *baby.ControlState) {
		c.NightLight = true
		c.NightLightBrightness = 75
		c.NightLightTimeout = 300
		c.Volume = 50
		c.PlaybackActive = true
		c.CurrentTrack = "White Noise"
		c.SleepMode = false
		c.NightVision = 1
		c.StatusLight = true
		c.MicMute = false
		c.Breathing.Active = true
		c.Breathing.BreathsPerMin = 14
	})

	p.PublishState("abc123", "Leo", state)

	pubs := mc.lastPayloads()

	checks := map[string]string{
		"nanit/abc123/night_light":            "ON",
		"nanit/abc123/night_light_brightness":  "75",
		"nanit/abc123/night_light_timeout":     "300",
		"nanit/abc123/volume":                  "50",
		"nanit/abc123/playback":                "ON",
		"nanit/abc123/current_track":           "White Noise",
		"nanit/abc123/sleep_mode":              "OFF",
		"nanit/abc123/night_vision":            "auto",
		"nanit/abc123/status_light":            "ON",
		"nanit/abc123/mic_mute":                "OFF",
		"nanit/abc123/breathing_active":        "ON",
		"nanit/abc123/breathing_bpm":           "14",
	}
	for topic, want := range checks {
		if got := pubs[topic]; got != want {
			t.Errorf("%s = %q, want %q", topic, got, want)
		}
	}
}

func TestPublishStateDedup(t *testing.T) {
	p, mc := newTestPublisher()
	state := baby.NewState("abc123", "cam1", "Leo")
	state.UpdateSensors(func(s *baby.SensorState) {
		s.Temperature = 22.5
	})

	p.PublishState("abc123", "Leo", state)
	count1 := len(mc.pubs)

	mc.reset()
	p.PublishState("abc123", "Leo", state)
	count2 := len(mc.pubs)

	if count2 != 0 {
		t.Errorf("second PublishState with same values published %d messages, want 0", count2)
	}

	state.UpdateSensors(func(s *baby.SensorState) {
		s.Temperature = 23.0
	})
	mc.reset()
	p.PublishState("abc123", "Leo", state)
	count3 := len(mc.pubs)

	if count3 == 0 {
		t.Errorf("PublishState after temperature change published 0 messages, want > 0")
	}
	_ = count1
}

func TestNightVisionStr(t *testing.T) {
	tests := []struct {
		in   int32
		want string
	}{
		{0, "off"},
		{1, "auto"},
		{2, "on"},
		{99, "off"},
	}
	for _, tc := range tests {
		if got := nightVisionStr(tc.in); got != tc.want {
			t.Errorf("nightVisionStr(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestOnOffStr(t *testing.T) {
	if got := onOffStr(true); got != "ON" {
		t.Errorf("onOffStr(true) = %q", got)
	}
	if got := onOffStr(false); got != "OFF" {
		t.Errorf("onOffStr(false) = %q", got)
	}
}

// parseDiscovery unmarshals a JSON discovery payload from the published messages.
func parseDiscovery(t *testing.T, pubs map[string]string, topic string) map[string]interface{} {
	t.Helper()
	raw, ok := pubs[topic]
	if !ok {
		t.Fatalf("no message published on %s", topic)
	}
	var cfg map[string]interface{}
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		t.Fatalf("unmarshal %s: %v", topic, err)
	}
	return cfg
}

func TestDiscoveryEntityNamingNoChildName(t *testing.T) {
	p, mc := newTestPublisher()
	p.PublishDiscovery("abc123", "Leo")

	pubs := mc.lastPayloads()

	cfg := parseDiscovery(t, pubs, "homeassistant/sensor/nanit_abc123_temperature/config")
	name, _ := cfg["name"].(string)
	if name != "Temperature" {
		t.Errorf("sensor name = %q, want %q (should not contain child name)", name, "Temperature")
	}

	cfg = parseDiscovery(t, pubs, "homeassistant/binary_sensor/nanit_abc123_is_night/config")
	name, _ = cfg["name"].(string)
	if name != "Night Mode" {
		t.Errorf("binary_sensor name = %q, want %q", name, "Night Mode")
	}
}

func TestDiscoveryAvailability(t *testing.T) {
	p, mc := newTestPublisher()
	p.PublishDiscovery("abc123", "Leo")

	pubs := mc.lastPayloads()

	topics := []string{
		"homeassistant/sensor/nanit_abc123_temperature/config",
		"homeassistant/binary_sensor/nanit_abc123_is_night/config",
		"homeassistant/light/nanit_abc123_night_light/config",
		"homeassistant/switch/nanit_abc123_playback/config",
		"homeassistant/number/nanit_abc123_volume/config",
		"homeassistant/select/nanit_abc123_night_vision/config",
		"homeassistant/sensor/nanit_abc123_breathing_bpm/config",
	}

	for _, topic := range topics {
		cfg := parseDiscovery(t, pubs, topic)
		if cfg["availability_topic"] != "nanit/abc123/ws_alive" {
			t.Errorf("%s: availability_topic = %v", topic, cfg["availability_topic"])
		}
		if cfg["payload_available"] != "true" {
			t.Errorf("%s: payload_available = %v", topic, cfg["payload_available"])
		}
		if cfg["payload_not_available"] != "false" {
			t.Errorf("%s: payload_not_available = %v", topic, cfg["payload_not_available"])
		}
	}
}

func TestDiscoveryWSAliveNoAvailability(t *testing.T) {
	p, mc := newTestPublisher()
	p.PublishDiscovery("abc123", "Leo")

	pubs := mc.lastPayloads()
	cfg := parseDiscovery(t, pubs, "homeassistant/binary_sensor/nanit_abc123_ws_alive/config")

	if _, ok := cfg["availability_topic"]; ok {
		t.Error("ws_alive should not have availability_topic (its state_topic IS the availability topic for other entities)")
	}
}

func TestDiscoveryLight(t *testing.T) {
	p, mc := newTestPublisher()
	p.PublishDiscovery("abc123", "Leo")

	pubs := mc.lastPayloads()
	cfg := parseDiscovery(t, pubs, "homeassistant/light/nanit_abc123_night_light/config")

	checks := map[string]interface{}{
		"name":                     "Night Light",
		"unique_id":                "nanit_abc123_night_light",
		"command_topic":            "nanit/abc123/night_light/set",
		"state_topic":              "nanit/abc123/night_light",
		"brightness_command_topic": "nanit/abc123/night_light_brightness/set",
		"brightness_state_topic":   "nanit/abc123/night_light_brightness",
		"payload_on":               "ON",
		"payload_off":              "OFF",
		"icon":                     "mdi:lightbulb-night",
	}
	for key, want := range checks {
		got := cfg[key]
		if fmt.Sprintf("%v", got) != fmt.Sprintf("%v", want) {
			t.Errorf("light config %s = %v, want %v", key, got, want)
		}
	}

	bs, _ := cfg["brightness_scale"].(float64)
	if bs != 100 {
		t.Errorf("brightness_scale = %v, want 100", cfg["brightness_scale"])
	}
}

func TestDiscoverySwitches(t *testing.T) {
	p, mc := newTestPublisher()
	p.PublishDiscovery("abc123", "Leo")

	pubs := mc.lastPayloads()

	switches := []struct {
		key      string
		name     string
		stateKey string
		icon     string
	}{
		{"playback", "Sound Machine", "playback", "mdi:music"},
		{"sleep_mode", "Sleep Mode", "sleep_mode", "mdi:sleep"},
		{"status_light", "Status LED", "status_light", "mdi:led-on"},
		{"mic_mute", "Mic Mute", "mic_mute", "mdi:microphone-off"},
		{"breathing_monitoring", "Breathing Monitor", "breathing_active", "mdi:lungs"},
	}

	for _, s := range switches {
		topic := fmt.Sprintf("homeassistant/switch/nanit_abc123_%s/config", s.key)
		cfg := parseDiscovery(t, pubs, topic)

		if cfg["name"] != s.name {
			t.Errorf("%s name = %v, want %s", s.key, cfg["name"], s.name)
		}
		wantCmd := fmt.Sprintf("nanit/abc123/%s/set", s.key)
		if cfg["command_topic"] != wantCmd {
			t.Errorf("%s command_topic = %v, want %s", s.key, cfg["command_topic"], wantCmd)
		}
		wantState := fmt.Sprintf("nanit/abc123/%s", s.stateKey)
		if cfg["state_topic"] != wantState {
			t.Errorf("%s state_topic = %v, want %s", s.key, cfg["state_topic"], wantState)
		}
		if cfg["icon"] != s.icon {
			t.Errorf("%s icon = %v, want %s", s.key, cfg["icon"], s.icon)
		}
		if cfg["payload_on"] != "ON" || cfg["payload_off"] != "OFF" {
			t.Errorf("%s payloads = %v/%v", s.key, cfg["payload_on"], cfg["payload_off"])
		}
	}
}

func TestDiscoveryNumbers(t *testing.T) {
	p, mc := newTestPublisher()
	p.PublishDiscovery("abc123", "Leo")

	pubs := mc.lastPayloads()

	numbers := []struct {
		key  string
		name string
		min  float64
		max  float64
		step float64
		unit string
	}{
		{"volume", "Volume", 0, 100, 1, "%"},
		{"night_light_timeout", "Night Light Timer", 0, 900, 60, "s"},
	}

	for _, n := range numbers {
		topic := fmt.Sprintf("homeassistant/number/nanit_abc123_%s/config", n.key)
		cfg := parseDiscovery(t, pubs, topic)

		if cfg["name"] != n.name {
			t.Errorf("%s name = %v, want %s", n.key, cfg["name"], n.name)
		}
		if v, _ := cfg["min"].(float64); v != n.min {
			t.Errorf("%s min = %v, want %v", n.key, v, n.min)
		}
		if v, _ := cfg["max"].(float64); v != n.max {
			t.Errorf("%s max = %v, want %v", n.key, v, n.max)
		}
		if v, _ := cfg["step"].(float64); v != n.step {
			t.Errorf("%s step = %v, want %v", n.key, v, n.step)
		}
		if cfg["unit_of_measurement"] != n.unit {
			t.Errorf("%s unit = %v, want %s", n.key, cfg["unit_of_measurement"], n.unit)
		}
		if cfg["mode"] != "slider" {
			t.Errorf("%s mode = %v, want slider", n.key, cfg["mode"])
		}
	}
}

func TestDiscoverySelectNightVision(t *testing.T) {
	p, mc := newTestPublisher()
	p.PublishDiscovery("abc123", "Leo")

	pubs := mc.lastPayloads()
	cfg := parseDiscovery(t, pubs, "homeassistant/select/nanit_abc123_night_vision/config")

	if cfg["name"] != "Night Vision" {
		t.Errorf("name = %v", cfg["name"])
	}
	opts, ok := cfg["options"].([]interface{})
	if !ok || len(opts) != 3 {
		t.Fatalf("options = %v", cfg["options"])
	}
	want := []string{"off", "auto", "on"}
	for i, w := range want {
		if opts[i] != w {
			t.Errorf("options[%d] = %v, want %s", i, opts[i], w)
		}
	}
}

func TestDiscoveryBreathingBPM(t *testing.T) {
	p, mc := newTestPublisher()
	p.PublishDiscovery("abc123", "Leo")

	pubs := mc.lastPayloads()
	cfg := parseDiscovery(t, pubs, "homeassistant/sensor/nanit_abc123_breathing_bpm/config")

	if cfg["name"] != "Breathing Rate" {
		t.Errorf("name = %v", cfg["name"])
	}
	if cfg["unit_of_measurement"] != "bpm" {
		t.Errorf("unit = %v", cfg["unit_of_measurement"])
	}
	if cfg["icon"] != "mdi:lungs" {
		t.Errorf("icon = %v", cfg["icon"])
	}
	if cfg["state_topic"] != "nanit/abc123/breathing_bpm" {
		t.Errorf("state_topic = %v", cfg["state_topic"])
	}
}

func TestDiscoveryDeviceBlock(t *testing.T) {
	p, mc := newTestPublisher()
	p.PublishDiscovery("abc123", "Leo")

	pubs := mc.lastPayloads()
	cfg := parseDiscovery(t, pubs, "homeassistant/sensor/nanit_abc123_temperature/config")

	dev, ok := cfg["device"].(map[string]interface{})
	if !ok {
		t.Fatalf("device is not a map: %T", cfg["device"])
	}
	if dev["name"] != "Nanit Leo" {
		t.Errorf("device name = %v", dev["name"])
	}
	if dev["manufacturer"] != "Nanit" {
		t.Errorf("device manufacturer = %v", dev["manufacturer"])
	}
	ids, ok := dev["identifiers"].([]interface{})
	if !ok || len(ids) != 1 || ids[0] != "nanit_abc123" {
		t.Errorf("device identifiers = %v", dev["identifiers"])
	}
}

func TestDynamicSoundtrackDiscovery(t *testing.T) {
	p, mc := newTestPublisher()
	state := baby.NewState("abc123", "cam1", "Leo")
	state.UpdateControls(func(c *baby.ControlState) {
		c.Soundtracks = []baby.SoundtrackInfo{
			{Name: "White Noise"},
			{Name: "Ocean Waves"},
			{Name: "Rain"},
		}
		c.CurrentTrack = "White Noise"
	})

	p.PublishState("abc123", "Leo", state)

	pubs := mc.lastPayloads()
	cfg := parseDiscovery(t, pubs, "homeassistant/select/nanit_abc123_select_track/config")

	if cfg["name"] != "Sound Track" {
		t.Errorf("name = %v", cfg["name"])
	}
	opts, ok := cfg["options"].([]interface{})
	if !ok {
		t.Fatalf("options type = %T", cfg["options"])
	}
	want := []string{"White Noise", "Ocean Waves", "Rain"}
	if len(opts) != len(want) {
		t.Fatalf("options count = %d, want %d", len(opts), len(want))
	}
	for i, w := range want {
		if opts[i] != w {
			t.Errorf("options[%d] = %v, want %s", i, opts[i], w)
		}
	}
	if cfg["state_topic"] != "nanit/abc123/current_track" {
		t.Errorf("state_topic = %v", cfg["state_topic"])
	}

	// Second call with same soundtracks should NOT re-publish discovery.
	mc.reset()
	p.PublishState("abc123", "Leo", state)
	pubs2 := mc.lastPayloads()
	if _, ok := pubs2["homeassistant/select/nanit_abc123_select_track/config"]; ok {
		t.Error("soundtrack discovery re-published with unchanged list")
	}
}

func TestHandleCommandParsing(t *testing.T) {
	p, _ := newTestPublisher()

	var gotUID, gotKey, gotPayload string
	p.cmdHandler = func(babyUID, key, payload string) {
		gotUID = babyUID
		gotKey = key
		gotPayload = payload
	}

	msg := &fakeMessage{
		topic:   "nanit/abc123/night_light/set",
		payload: []byte("ON"),
	}
	p.handleCommand(nil, msg)

	if gotUID != "abc123" {
		t.Errorf("babyUID = %q, want abc123", gotUID)
	}
	if gotKey != "night_light" {
		t.Errorf("key = %q, want night_light", gotKey)
	}
	if gotPayload != "ON" {
		t.Errorf("payload = %q, want ON", gotPayload)
	}
}

func TestHandleCommandMalformed(t *testing.T) {
	p, _ := newTestPublisher()

	called := false
	p.cmdHandler = func(_, _, _ string) { called = true }

	msg := &fakeMessage{
		topic:   "nanit/badtopic/set",
		payload: []byte("ON"),
	}
	p.handleCommand(nil, msg)

	if called {
		t.Error("handler was called for malformed topic")
	}
}

func TestHandleCommandTrimPayload(t *testing.T) {
	p, _ := newTestPublisher()

	var gotPayload string
	p.cmdHandler = func(_, _, payload string) { gotPayload = payload }

	msg := &fakeMessage{
		topic:   "nanit/abc123/volume/set",
		payload: []byte("  50 \n"),
	}
	p.handleCommand(nil, msg)

	if gotPayload != "50" {
		t.Errorf("payload = %q, want %q", gotPayload, "50")
	}
}

func TestNilPublisherSafety(t *testing.T) {
	var p *Publisher
	p.Close()
	p.PublishState("abc", "Leo", baby.NewState("abc", "cam", "Leo"))
	p.PublishDiscovery("abc", "Leo")
	p.SetCommandHandler(func(_, _, _ string) {})
}

// fakeMessage implements paho.Message for testing.
type fakeMessage struct {
	topic   string
	payload []byte
}

func (m *fakeMessage) Duplicate() bool    { return false }
func (m *fakeMessage) Qos() byte          { return 0 }
func (m *fakeMessage) Retained() bool     { return false }
func (m *fakeMessage) Topic() string      { return m.topic }
func (m *fakeMessage) MessageID() uint16  { return 0 }
func (m *fakeMessage) Payload() []byte    { return m.payload }
func (m *fakeMessage) Ack()               {}
