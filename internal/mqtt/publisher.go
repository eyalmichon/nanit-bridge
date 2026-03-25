package mqtt

import (
	"encoding/json"
	"fmt"
	"log"
	"time"

	paho "github.com/eclipse/paho.mqtt.golang"

	"nanit-bridge/internal/baby"
)

type Config struct {
	BrokerURL string
	Username  string
	Password  string
	Prefix    string
}

type Publisher struct {
	client paho.Client
	prefix string
}

func NewPublisher(cfg Config) (*Publisher, error) {
	if cfg.BrokerURL == "" {
		return nil, nil
	}

	opts := paho.NewClientOptions().
		AddBroker(cfg.BrokerURL).
		SetClientID(fmt.Sprintf("%s-bridge", cfg.Prefix)).
		SetKeepAlive(30 * time.Second).
		SetAutoReconnect(true).
		SetConnectRetry(true).
		SetConnectRetryInterval(5 * time.Second).
		SetConnectionLostHandler(func(_ paho.Client, err error) {
			log.Printf("[mqtt] connection lost: %v", err)
		}).
		SetOnConnectHandler(func(_ paho.Client) {
			log.Printf("[mqtt] connected to broker")
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

	return &Publisher{
		client: client,
		prefix: cfg.Prefix,
	}, nil
}

func (p *Publisher) Close() {
	if p == nil || p.client == nil {
		return
	}
	p.client.Disconnect(1000)
}

func (p *Publisher) PublishState(babyUID string, state *baby.State) {
	if p == nil {
		return
	}

	sensors, _, stream, wsAlive := state.Snapshot()

	p.pub(babyUID, "temperature", fmt.Sprintf("%.1f", sensors.Temperature))
	p.pub(babyUID, "humidity", fmt.Sprintf("%.1f", sensors.Humidity))
	p.pub(babyUID, "light", fmt.Sprintf("%.1f", sensors.Light))
	p.pub(babyUID, "is_night", boolStr(sensors.IsNight))
	p.pub(babyUID, "cry_detected", boolStr(sensors.CryDetected))
	p.pub(babyUID, "sound_alert", boolStr(sensors.SoundAlert))
	p.pub(babyUID, "motion_alert", boolStr(sensors.MotionAlert))
	p.pub(babyUID, "stream_state", stream.String())
	p.pub(babyUID, "ws_alive", boolStr(wsAlive))
}

// PublishDiscovery sends Home Assistant MQTT auto-discovery messages for a baby.
func (p *Publisher) PublishDiscovery(babyUID, name string) {
	if p == nil {
		return
	}

	deviceID := fmt.Sprintf("nanit_%s", babyUID)
	device := map[string]interface{}{
		"identifiers":  []string{deviceID},
		"name":         fmt.Sprintf("Nanit %s", name),
		"manufacturer": "Nanit",
		"model":        "Baby Monitor",
	}

	sensors := []struct {
		name       string
		key        string
		unit       string
		devClass   string
		icon       string
	}{
		{"Temperature", "temperature", "°C", "temperature", ""},
		{"Humidity", "humidity", "%", "humidity", ""},
		{"Light", "light", "lx", "illuminance", ""},
	}

	for _, s := range sensors {
		config := map[string]interface{}{
			"name":                  fmt.Sprintf("%s %s", name, s.name),
			"state_topic":          fmt.Sprintf("%s/%s/%s", p.prefix, babyUID, s.key),
			"unique_id":            fmt.Sprintf("%s_%s", deviceID, s.key),
			"device":               device,
			"device_class":         s.devClass,
			"unit_of_measurement":  s.unit,
		}

		topic := fmt.Sprintf("homeassistant/sensor/%s_%s/config", deviceID, s.key)
		p.pubJSON(topic, config)
	}

	binarySensors := []struct {
		name string
		key  string
		icon string
	}{
		{"Night Mode", "is_night", "mdi:weather-night"},
		{"Cry Detected", "cry_detected", "mdi:emoticon-cry-outline"},
		{"Sound Alert", "sound_alert", "mdi:volume-high"},
		{"Motion Alert", "motion_alert", "mdi:motion-sensor"},
		{"Stream Active", "stream_state", "mdi:video"},
		{"WebSocket", "ws_alive", "mdi:lan-connect"},
	}

	for _, s := range binarySensors {
		payloadOn := "true"
		if s.key == "stream_state" {
			payloadOn = "active"
		}

		config := map[string]interface{}{
			"name":        fmt.Sprintf("%s %s", name, s.name),
			"state_topic": fmt.Sprintf("%s/%s/%s", p.prefix, babyUID, s.key),
			"unique_id":   fmt.Sprintf("%s_%s", deviceID, s.key),
			"device":      device,
			"payload_on":  payloadOn,
			"payload_off": "false",
			"icon":        s.icon,
		}
		if s.key == "stream_state" {
			config["payload_off"] = "stopped"
		}

		topic := fmt.Sprintf("homeassistant/binary_sensor/%s_%s/config", deviceID, s.key)
		p.pubJSON(topic, config)
	}
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
