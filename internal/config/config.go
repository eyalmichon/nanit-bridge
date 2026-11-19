package config

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/joho/godotenv"
)

type Config struct {
	NanitEmail    string
	NanitPassword string

	// External address the camera will push RTMP to (must be reachable from the camera).
	RTMPAddr string
	RTMPPort int

	// Stream ingest mode: "auto" (WebRTC with RTMP fallback), "webrtc", or "rtmp".
	StreamMode string
	// TCP port for the built-in RTSP server (0 disables RTSP).
	RTSPPort int

	MQTTBrokerURL string
	MQTTUsername  string
	MQTTPassword  string
	MQTTPrefix    string

	HTTPPort int

	SensorPollSec int

	SessionFile       string
	PushCredsFile     string
	DashboardAuthFile string
	DashboardPassword string
	LogLevel          string

	RTMPToken     string
	RTMPTokenFile string
}

// LoadEnvFile loads .env if present without overriding existing env vars.
// Useful for CLI subcommands that need env defaults without full config validation.
func LoadEnvFile() error {
	return godotenv.Load()
}

func Load() (*Config, error) {
	_ = LoadEnvFile()
	c := &Config{
		NanitEmail:    os.Getenv("NANIT_EMAIL"),
		NanitPassword: os.Getenv("NANIT_PASSWORD"),
		RTMPAddr:      os.Getenv("NANIT_RTMP_ADDR"),
		MQTTBrokerURL: os.Getenv("NANIT_MQTT_BROKER_URL"),
		MQTTUsername:  os.Getenv("NANIT_MQTT_USERNAME"),
		MQTTPassword:  os.Getenv("NANIT_MQTT_PASSWORD"),
		MQTTPrefix:    envOrDefault("NANIT_MQTT_PREFIX", "nanit"),
		SessionFile:   envOrDefault("NANIT_SESSION_FILE", "/data/session.json"),
		PushCredsFile: envOrDefault("NANIT_PUSH_CREDS_FILE", "/data/push_creds.json"),
		DashboardAuthFile: envOrDefault("NANIT_DASHBOARD_AUTH_FILE", "/data/dashboard_password.hash"),
		DashboardPassword: os.Getenv("NANIT_DASHBOARD_PASSWORD"),
		LogLevel:          envOrDefault("NANIT_LOG_LEVEL", "info"),
	}

	port, err := strconv.Atoi(envOrDefault("NANIT_RTMP_PORT", "1935"))
	if err != nil {
		return nil, fmt.Errorf("invalid NANIT_RTMP_PORT: %w", err)
	}
	c.RTMPPort = port

	httpPort, err := strconv.Atoi(envOrDefault("NANIT_HTTP_PORT", "8080"))
	if err != nil {
		return nil, fmt.Errorf("invalid NANIT_HTTP_PORT: %w", err)
	}
	c.HTTPPort = httpPort

	sensorPoll, err := strconv.Atoi(envOrDefault("NANIT_SENSOR_POLL_SEC", "30"))
	if err != nil {
		return nil, fmt.Errorf("invalid NANIT_SENSOR_POLL_SEC: %w", err)
	}
	c.SensorPollSec = sensorPoll

	c.StreamMode = strings.ToLower(strings.TrimSpace(envOrDefault("NANIT_STREAM_MODE", "auto")))
	switch c.StreamMode {
	case "auto", "webrtc", "rtmp":
	default:
		return nil, fmt.Errorf("invalid NANIT_STREAM_MODE %q (expected auto, webrtc, or rtmp)", c.StreamMode)
	}

	rtspPort, err := strconv.Atoi(envOrDefault("NANIT_RTSP_PORT", "8554"))
	if err != nil || rtspPort < 0 {
		return nil, fmt.Errorf("invalid NANIT_RTSP_PORT: %q", os.Getenv("NANIT_RTSP_PORT"))
	}
	c.RTSPPort = rtspPort

	if c.StreamMode != "webrtc" {
		if c.RTMPAddr == "" {
			return nil, fmt.Errorf("NANIT_RTMP_ADDR is required (your LAN IP reachable by the camera, e.g. 192.168.1.100:1935)")
		}
		if strings.EqualFold(c.RTMPAddr, "auto") {
			return nil, fmt.Errorf("NANIT_RTMP_ADDR is still set to 'auto'; set it to your LAN IP (e.g. 192.168.1.100:1935)")
		}
	}

	c.RTMPTokenFile = envOrDefault("NANIT_RTMP_TOKEN_FILE", "/data/rtmp_token")

	if envToken := os.Getenv("NANIT_RTMP_TOKEN"); envToken != "" {
		if len(envToken) < 8 {
			return nil, fmt.Errorf("NANIT_RTMP_TOKEN must be at least 8 characters")
		}
		c.RTMPToken = envToken
	} else {
		token, err := loadOrGenerateToken(c.RTMPTokenFile)
		if err != nil {
			return nil, fmt.Errorf("rtmp token: %w", err)
		}
		c.RTMPToken = token
	}

	return c, nil
}

func loadOrGenerateToken(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err == nil {
		token := strings.TrimSpace(string(data))
		if len(token) >= 8 {
			return token, nil
		}
	}
	token, err := GenerateRTMPToken()
	if err != nil {
		return "", err
	}
	if err := WriteRTMPToken(path, token); err != nil {
		return "", err
	}
	return token, nil
}

func GenerateRTMPToken() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("crypto/rand failed: %w", err)
	}
	return hex.EncodeToString(b), nil
}

func WriteRTMPToken(filePath, token string) error {
	if dir := filepath.Dir(filePath); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	return os.WriteFile(filePath, []byte(token+"\n"), 0o600)
}

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
