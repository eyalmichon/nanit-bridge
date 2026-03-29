package config

import "testing"

func TestLoadDefaults(t *testing.T) {
	t.Setenv("NANIT_RTMP_ADDR", "192.168.1.10:1935")
	t.Setenv("NANIT_RTMP_PORT", "")
	t.Setenv("NANIT_HTTP_PORT", "")
	t.Setenv("NANIT_SENSOR_POLL_SEC", "")
	t.Setenv("NANIT_MQTT_PREFIX", "")
	t.Setenv("NANIT_SESSION_FILE", "")
	t.Setenv("NANIT_PUSH_CREDS_FILE", "")
	t.Setenv("NANIT_DASHBOARD_AUTH_FILE", "")
	t.Setenv("NANIT_LOG_LEVEL", "")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	if cfg.RTMPPort != 1935 {
		t.Fatalf("RTMPPort = %d, want 1935", cfg.RTMPPort)
	}
	if cfg.HTTPPort != 8080 {
		t.Fatalf("HTTPPort = %d, want 8080", cfg.HTTPPort)
	}
	if cfg.SensorPollSec != 30 {
		t.Fatalf("SensorPollSec = %d, want 30", cfg.SensorPollSec)
	}
	if cfg.MQTTPrefix != "nanit" {
		t.Fatalf("MQTTPrefix = %q, want nanit", cfg.MQTTPrefix)
	}
	if cfg.SessionFile != "/data/session.json" {
		t.Fatalf("SessionFile = %q", cfg.SessionFile)
	}
	if cfg.PushCredsFile != "/data/push_creds.json" {
		t.Fatalf("PushCredsFile = %q", cfg.PushCredsFile)
	}
	if cfg.DashboardAuthFile != "/data/dashboard_password.hash" {
		t.Fatalf("DashboardAuthFile = %q", cfg.DashboardAuthFile)
	}
	if cfg.LogLevel != "info" {
		t.Fatalf("LogLevel = %q", cfg.LogLevel)
	}
}

func TestLoadOverrides(t *testing.T) {
	t.Setenv("NANIT_RTMP_ADDR", "10.0.0.1:11935")
	t.Setenv("NANIT_RTMP_PORT", "11935")
	t.Setenv("NANIT_HTTP_PORT", "18080")
	t.Setenv("NANIT_SENSOR_POLL_SEC", "12")
	t.Setenv("NANIT_MQTT_PREFIX", "home/nanit")
	t.Setenv("NANIT_SESSION_FILE", "/tmp/s.json")
	t.Setenv("NANIT_PUSH_CREDS_FILE", "/tmp/p.json")
	t.Setenv("NANIT_DASHBOARD_AUTH_FILE", "/tmp/a.hash")
	t.Setenv("NANIT_LOG_LEVEL", "debug")
	t.Setenv("NANIT_EMAIL", "user@example.com")
	t.Setenv("NANIT_PASSWORD", "pass")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	if cfg.RTMPAddr != "10.0.0.1:11935" || cfg.RTMPPort != 11935 {
		t.Fatalf("unexpected RTMP config: %s %d", cfg.RTMPAddr, cfg.RTMPPort)
	}
	if cfg.HTTPPort != 18080 || cfg.SensorPollSec != 12 {
		t.Fatalf("unexpected http/sensor config: %d %d", cfg.HTTPPort, cfg.SensorPollSec)
	}
	if cfg.MQTTPrefix != "home/nanit" || cfg.SessionFile != "/tmp/s.json" || cfg.PushCredsFile != "/tmp/p.json" {
		t.Fatalf("unexpected path/prefix config: %+v", cfg)
	}
	if cfg.DashboardAuthFile != "/tmp/a.hash" || cfg.LogLevel != "debug" {
		t.Fatalf("unexpected auth/log config: %+v", cfg)
	}
	if cfg.NanitEmail != "user@example.com" || cfg.NanitPassword != "pass" {
		t.Fatalf("unexpected nanit creds: %+v", cfg)
	}
}

func TestLoadMissingRTMPAddr(t *testing.T) {
	t.Setenv("NANIT_RTMP_ADDR", "")
	_, err := Load()
	if err == nil {
		t.Fatalf("expected error when NANIT_RTMP_ADDR is missing")
	}
}

func TestLoadInvalidPorts(t *testing.T) {
	t.Setenv("NANIT_RTMP_ADDR", "127.0.0.1:1935")
	t.Setenv("NANIT_RTMP_PORT", "bad")
	if _, err := Load(); err == nil {
		t.Fatalf("expected invalid NANIT_RTMP_PORT error")
	}

	t.Setenv("NANIT_RTMP_PORT", "1935")
	t.Setenv("NANIT_HTTP_PORT", "bad")
	if _, err := Load(); err == nil {
		t.Fatalf("expected invalid NANIT_HTTP_PORT error")
	}

	t.Setenv("NANIT_HTTP_PORT", "8080")
	t.Setenv("NANIT_SENSOR_POLL_SEC", "bad")
	if _, err := Load(); err == nil {
		t.Fatalf("expected invalid NANIT_SENSOR_POLL_SEC error")
	}
}
