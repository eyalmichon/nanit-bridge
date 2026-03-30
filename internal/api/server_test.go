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
		manager:    baby.NewManager(nil, "127.0.0.1:1935", 30, "", nil),
		rtmpServer: rtmp.NewServer(1935),
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

func TestServerStopGracefulShutdown(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for free port: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()

	tm := nanit.NewTokenManager("", "", filepath.Join(t.TempDir(), "session.json"))
	mgr := baby.NewManager(tm, "127.0.0.1:1935", 30, "", nil)
	s := NewServer(
		port,
		mgr,
		rtmp.NewServer(1935),
		NewLogBroadcaster(),
		filepath.Join(t.TempDir(), "dashboard.hash"),
		tm,
		nil,
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
