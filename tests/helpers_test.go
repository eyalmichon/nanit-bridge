package tests

import (
	"net"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"nanit-bridge/internal/config"
)

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatalf("listen :0: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func waitForHTTP(t *testing.T, url string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	client := &http.Client{Timeout: 500 * time.Millisecond}
	for time.Now().Before(deadline) {
		resp, err := client.Get(url) //nolint:noctx
		if err == nil {
			_ = resp.Body.Close()
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for HTTP endpoint: %s", url)
}

func buildTestConfig(t *testing.T, httpPort, rtmpPort int) *config.Config {
	t.Helper()
	tmp := t.TempDir()
	return &config.Config{
		NanitEmail:        "u@example.com",
		NanitPassword:     "pw",
		RTMPAddr:          "127.0.0.1:" + itoa(rtmpPort),
		RTMPPort:          rtmpPort,
		HTTPPort:          httpPort,
		SensorPollSec:     3600,
		SessionFile:       filepath.Join(tmp, "session.json"),
		PushCredsFile:     filepath.Join(tmp, "push_creds.json"),
		DashboardAuthFile: filepath.Join(tmp, "dashboard_password.hash"),
		DashboardPassword: "",
		RTMPToken:         "e2etoken",
		RTMPTokenFile:     filepath.Join(tmp, "rtmp_token"),
	}
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var b [20]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
