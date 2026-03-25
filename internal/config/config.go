package config

import (
	"fmt"
	"os"
	"strconv"
)

type Config struct {
	NanitEmail    string
	NanitPassword string

	// External address the camera will push RTMP to (must be reachable from the camera).
	RTMPAddr string
	RTMPPort int

	MQTTBrokerURL string
	MQTTUsername  string
	MQTTPassword  string
	MQTTPrefix    string

	SessionFile string
	LogLevel    string
}

func Load() (*Config, error) {
	c := &Config{
		NanitEmail:    os.Getenv("NANIT_EMAIL"),
		NanitPassword: os.Getenv("NANIT_PASSWORD"),
		RTMPAddr:      os.Getenv("NANIT_RTMP_ADDR"),
		MQTTBrokerURL: os.Getenv("NANIT_MQTT_BROKER_URL"),
		MQTTUsername:  os.Getenv("NANIT_MQTT_USERNAME"),
		MQTTPassword:  os.Getenv("NANIT_MQTT_PASSWORD"),
		MQTTPrefix:    envOrDefault("NANIT_MQTT_PREFIX", "nanit"),
		SessionFile:   envOrDefault("NANIT_SESSION_FILE", "/data/session.json"),
		LogLevel:      envOrDefault("NANIT_LOG_LEVEL", "info"),
	}

	port, err := strconv.Atoi(envOrDefault("NANIT_RTMP_PORT", "1935"))
	if err != nil {
		return nil, fmt.Errorf("invalid NANIT_RTMP_PORT: %w", err)
	}
	c.RTMPPort = port

	if c.RTMPAddr == "" {
		return nil, fmt.Errorf("NANIT_RTMP_ADDR is required (your LAN IP reachable by the camera, e.g. 192.168.1.100:1935)")
	}

	return c, nil
}

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
