package api

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"nanit-bridge/internal/baby"
	"nanit-bridge/internal/nanit"
	"nanit-bridge/internal/rtmp"
)

func newTestServer() *Server {
	return &Server{
		manager:    baby.NewManager(nil, "127.0.0.1:1935", "testtoken", 30, "", nil),
		rtmpServer: rtmp.NewServer(1935, "testtoken"),
		logBcast:   NewLogBroadcaster(),
		clients:    make(map[*wsClient]struct{}),
	}
}

func TestBuildBabyJSONShape(t *testing.T) {
	s := newTestServer()
	st := baby.NewState("baby-1", "cam-1", "Ava")
	st.UpdateSensors(func(ss *baby.SensorState) {
		ss.Temperature = 22.1
		ss.IsNight = true
	})
	st.UpdateControls(func(c *baby.ControlState) {
		c.NightLight = true
		c.Breathing.Active = true
		c.Breathing.BreathsPerMin = 31
	})
	st.SetStreamState(baby.StreamActive)
	st.SetWSAlive(true)

	payload := s.buildBabyJSON("baby-1", st)
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal buildBabyJSON: %v", err)
	}

	var got map[string]interface{}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	for _, key := range []string{
		"uid", "camera_uid", "name", "ws_alive", "stream", "rtmp_active",
		"sensor_poll_sec", "push_active", "sensors", "controls", "camera",
	} {
		if _, ok := got[key]; !ok {
			t.Fatalf("missing key %q in payload", key)
		}
	}
	if got["uid"] != "baby-1" || got["camera_uid"] != "cam-1" {
		t.Fatalf("unexpected id fields: %+v", got)
	}
}

func TestHandleBabiesReturnsJSON(t *testing.T) {
	s := newTestServer()

	req := httptest.NewRequest(http.MethodGet, "/api/babies", nil)
	rr := httptest.NewRecorder()
	s.handleBabies(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	var body struct {
		Babies []json.RawMessage `json:"babies"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if len(body.Babies) != 0 {
		t.Fatalf("expected empty babies list, got %d", len(body.Babies))
	}
}

func TestHandleBabyOrControlNotFound(t *testing.T) {
	s := newTestServer()
	req := httptest.NewRequest(http.MethodGet, "/api/babies/unknown", nil)
	rr := httptest.NewRecorder()

	s.handleBabyOrControl(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rr.Code)
	}
}

func TestHandleControlValidationAndUnknownAction(t *testing.T) {
	s := newTestServer()

	invalidJSONReq := httptest.NewRequest(http.MethodPost, "/api/babies/x/control", strings.NewReader("{"))
	invalidJSONRR := httptest.NewRecorder()
	s.handleControl(invalidJSONRR, invalidJSONReq, "x")
	if invalidJSONRR.Code != http.StatusBadRequest {
		t.Fatalf("invalid JSON status = %d, want 400", invalidJSONRR.Code)
	}

	unknownReq := httptest.NewRequest(http.MethodPost, "/api/babies/x/control", strings.NewReader(`{"action":"nope","value":true}`))
	unknownRR := httptest.NewRecorder()
	s.handleControl(unknownRR, unknownReq, "x")
	if unknownRR.Code != http.StatusBadRequest {
		t.Fatalf("unknown action status = %d, want 400", unknownRR.Code)
	}

	wrongMethodReq := httptest.NewRequest(http.MethodGet, "/api/babies/x/control", nil)
	wrongMethodRR := httptest.NewRecorder()
	s.handleControl(wrongMethodRR, wrongMethodReq, "x")
	if wrongMethodRR.Code != http.StatusMethodNotAllowed {
		t.Fatalf("wrong method status = %d, want 405", wrongMethodRR.Code)
	}
}

func TestCheckWSOriginSameHost(t *testing.T) {
	tests := []struct {
		name   string
		origin string
		host   string
		want   bool
	}{
		{"no origin header (non-browser client)", "", "localhost:8080", true},
		{"exact match", "http://localhost:8080", "localhost:8080", true},
		{"https scheme", "https://myhost:443", "myhost:443", true},
		{"case insensitive", "http://LocalHost:8080", "localhost:8080", true},
		{"host without port", "http://myhost", "myhost", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/ws", nil)
			r.Host = tt.host
			if tt.origin != "" {
				r.Header.Set("Origin", tt.origin)
			}
			if got := checkWSOrigin(r); got != tt.want {
				t.Fatalf("checkWSOrigin() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestCheckWSOriginCrossOriginRejected(t *testing.T) {
	tests := []struct {
		name   string
		origin string
		host   string
	}{
		{"different host", "http://evil.com", "localhost:8080"},
		{"different port", "http://localhost:9999", "localhost:8080"},
		{"subdomain mismatch", "http://sub.localhost:8080", "localhost:8080"},
		{"malformed origin", "://not-a-url", "localhost:8080"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/ws", nil)
			r.Host = tt.host
			r.Header.Set("Origin", tt.origin)
			if checkWSOrigin(r) {
				t.Fatalf("checkWSOrigin() = true, want false for origin=%q host=%q", tt.origin, tt.host)
			}
		})
	}
}

func TestSecurityHeadersPresent(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	handler := securityHeaders(inner)

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, r)

	csp := rr.Header().Get("Content-Security-Policy")
	if csp == "" {
		t.Fatal("Content-Security-Policy header missing")
	}
	for _, directive := range []string{"default-src", "script-src", "style-src", "connect-src", "frame-ancestors"} {
		if !strings.Contains(csp, directive) {
			t.Errorf("CSP missing directive %q: %s", directive, csp)
		}
	}

	xcto := rr.Header().Get("X-Content-Type-Options")
	if xcto != "nosniff" {
		t.Fatalf("X-Content-Type-Options = %q, want %q", xcto, "nosniff")
	}
}

func TestServerStopGracefulShutdown(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for free port: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()

	tm := nanit.NewTokenManager("", "", filepath.Join(t.TempDir(), "session.json"))
	mgr := baby.NewManager(tm, "127.0.0.1:1935", "testtoken", 30, "", nil)
	s := NewServer(
		port,
		mgr,
		rtmp.NewServer(1935, "testtoken"),
		NewLogBroadcaster(),
		filepath.Join(t.TempDir(), "dashboard.hash"),
		tm,
		nil,
		"127.0.0.1:1935",
		filepath.Join(t.TempDir(), "rtmp_token"),
		"test",
	)
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	addr := "127.0.0.1:" + strconv.Itoa(port)
	deadline := time.Now().Add(2 * time.Second)
	for {
		conn, dialErr := net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if dialErr == nil {
			_ = conn.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("server did not start listening: %v", dialErr)
		}
		time.Sleep(25 * time.Millisecond)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := s.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	conn, dialErr := net.DialTimeout("tcp", addr, 200*time.Millisecond)
	if dialErr == nil {
		_ = conn.Close()
		t.Fatalf("expected server listener to be closed after Stop")
	}
}
