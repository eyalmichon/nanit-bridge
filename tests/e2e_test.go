package tests

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"nanit-bridge/internal/api"
	"nanit-bridge/internal/baby"
	"nanit-bridge/internal/nanit"
	pb "nanit-bridge/internal/nanit/nanitpb"
	rtmpserver "nanit-bridge/internal/rtmp"
)

func TestE2EFullFlow(t *testing.T) {
	mock := newMockNanitCloud(t)
	restoreAPIBase := nanit.SetAPIBaseForTest(mock.URL())
	t.Cleanup(restoreAPIBase)

	httpPort := freePort(t)
	rtmpPort := freePort(t)
	cfg := buildTestConfig(t, httpPort, rtmpPort)

	tokenMgr := nanit.NewTokenManager(cfg.NanitEmail, cfg.NanitPassword, cfg.SessionFile)
	if _, err := tokenMgr.Login(); err != nil {
		t.Fatalf("Login() against mock cloud failed: %v", err)
	}

	rtmp := rtmpserver.NewServer(cfg.RTMPPort, cfg.RTMPToken)
	if err := rtmp.Start(); err != nil {
		t.Fatalf("rtmp.Start(): %v", err)
	}

	mgr := baby.NewManager(tokenMgr, cfg.RTMPAddr, cfg.RTMPToken, cfg.SensorPollSec, "", rtmp)
	startOrRestart := func() error {
		if mgr.IsStarted() {
			return mgr.Restart()
		}
		return mgr.Start()
	}

	logBcast := api.NewLogBroadcaster()
	apiServer := api.NewServer(
		cfg.HTTPPort,
		mgr,
		rtmp,
		logBcast,
		cfg.DashboardAuthFile,
		tokenMgr,
		startOrRestart,
		cfg.RTMPAddr,
		cfg.RTMPTokenFile,
		"test",
	)
	mgr.OnStateChange(func(uid string, st *baby.State) {
		apiServer.BroadcastState(uid, st)
	})
	if err := apiServer.Start(); err != nil {
		t.Fatalf("api.Start(): %v", err)
	}
	waitForHTTP(t, "http://127.0.0.1:"+itoa(httpPort)+"/login", 3*time.Second)

	if err := mgr.Start(); err != nil {
		t.Fatalf("mgr.Start(): %v", err)
	}
	t.Cleanup(mgr.Stop)

	if got := mock.waitForOutbound(3*time.Second, func(msg *pb.Message) bool {
		return msg.GetType() == pb.Message_REQUEST &&
			msg.GetRequest() != nil &&
			msg.GetRequest().GetType() == pb.RequestType_PUT_STREAMING
	}); got == nil {
		t.Fatalf("camera client did not connect/send initial stream request")
	}

	baseURL := "http://127.0.0.1:" + itoa(httpPort)

	// Auth middleware: unauthenticated API request before password is set -> 503 setup_required.
	unauthClient := &http.Client{Timeout: 2 * time.Second}
	req, _ := http.NewRequest(http.MethodGet, baseURL+"/api/babies", nil)
	req.Header.Set("Accept", "application/json")
	resp, err := unauthClient.Do(req)
	if err != nil {
		t.Fatalf("unauthenticated GET /api/babies: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("unauthenticated (no password set) status = %d, want 503", resp.StatusCode)
	}

	// Set up password + login, get an authenticated client.
	client := newAuthedHTTPClient(t, baseURL)

	// After password is set, unauthenticated API request -> 401.
	req2, _ := http.NewRequest(http.MethodGet, baseURL+"/api/babies", nil)
	req2.Header.Set("Accept", "application/json")
	resp2, err := unauthClient.Do(req2)
	if err != nil {
		t.Fatalf("unauthenticated GET /api/babies after setup: %v", err)
	}
	_ = resp2.Body.Close()
	if resp2.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated (with password set) status = %d, want 401", resp2.StatusCode)
	}

	var babiesResp struct {
		Babies []struct {
			UID       string `json:"uid"`
			Name      string `json:"name"`
			CameraUID string `json:"camera_uid"`
		} `json:"babies"`
	}
	doJSON(t, client, http.MethodGet, baseURL+"/api/babies", nil, http.StatusOK, &babiesResp)
	if len(babiesResp.Babies) != 1 || babiesResp.Babies[0].UID != "baby-1" || babiesResp.Babies[0].Name != "Test Baby" {
		t.Fatalf("unexpected babies payload: %+v", babiesResp.Babies)
	}

	wsURL := "ws://127.0.0.1:" + itoa(httpPort) + "/ws"
	wsHeader := http.Header{"Cookie": []string{cookieHeader(t, client, baseURL)}}
	wsConn, _, err := websocket.DefaultDialer.Dial(wsURL, wsHeader)
	if err != nil {
		t.Fatalf("dashboard ws dial: %v", err)
	}
	defer wsConn.Close()

	_, initialRaw, err := wsConn.ReadMessage()
	if err != nil {
		t.Fatalf("read ws initial: %v", err)
	}
	var initial struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(initialRaw, &initial); err != nil {
		t.Fatalf("unmarshal ws initial: %v", err)
	}
	if initial.Type != "initial" {
		t.Fatalf("ws initial type = %q, want initial", initial.Type)
	}

	tempType := pb.SensorType_TEMPERATURE
	humType := pb.SensorType_HUMIDITY
	tempMilli := int32(25100)
	humMilli := int32(64000)
	streamStarted := pb.Streaming_STARTED
	rtmpURL := "rtmp://127.0.0.1:1935/" + cfg.RTMPToken + "/baby-1"
	if err := mock.send(&pb.Message{
		Type: pb.Message_REQUEST.Enum(),
		Request: &pb.Request{
			Id:   int32Ptr(200),
			Type: pb.RequestType_PUT_SENSOR_DATA.Enum(),
			SensorData_: []*pb.SensorData{
				{SensorType: &tempType, ValueMilli: &tempMilli},
				{SensorType: &humType, ValueMilli: &humMilli},
			},
		},
	}); err != nil {
		t.Fatalf("mock send sensor update: %v", err)
	}
	if err := mock.send(&pb.Message{
		Type: pb.Message_REQUEST.Enum(),
		Request: &pb.Request{
			Id:        int32Ptr(201),
			Type:      pb.RequestType_PUT_STREAMING.Enum(),
			Streaming: &pb.Streaming{Id: pb.StreamIdentifier_MOBILE.Enum(), Status: &streamStarted, RtmpUrl: &rtmpURL},
		},
	}); err != nil {
		t.Fatalf("mock send streaming update: %v", err)
	}

	waitForBabyState(t, client, baseURL+"/api/babies/baby-1", func(b map[string]interface{}) bool {
		sensors, _ := b["sensors"].(map[string]interface{})
		stream, _ := b["stream"].(string)
		if sensors == nil {
			return false
		}
		temp, _ := sensors["temperature"].(float64)
		hum, _ := sensors["humidity"].(float64)
		return temp >= 25.0 && hum >= 64.0 && stream == "active"
	})

	_, stateRaw, err := wsConn.ReadMessage()
	if err != nil {
		t.Fatalf("read ws state update: %v", err)
	}
	var stateMsg struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal(stateRaw, &stateMsg)
	if stateMsg.Type != "state_update" {
		t.Fatalf("ws message type = %q, want state_update", stateMsg.Type)
	}

	doJSON(t, client, http.MethodPost, baseURL+"/api/babies/baby-1/control", map[string]interface{}{
		"action": "night_light",
		"value":  true,
	}, http.StatusOK, nil)
	if got := mock.waitForOutbound(3*time.Second, func(msg *pb.Message) bool {
		if msg.GetType() != pb.Message_REQUEST || msg.GetRequest() == nil || msg.GetRequest().GetType() != pb.RequestType_PUT_CONTROL {
			return false
		}
		ctrl := msg.GetRequest().GetControl()
		return ctrl != nil && ctrl.NightLight != nil && ctrl.GetNightLight() == pb.Control_LIGHT_ON
	}); got == nil {
		t.Fatalf("expected PUT_CONTROL night_light request to reach mock cloud")
	}
}

func newAuthedHTTPClient(t *testing.T, baseURL string) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookiejar.New: %v", err)
	}
	client := &http.Client{Jar: jar, Timeout: 3 * time.Second}

	doJSON(t, client, http.MethodPost, baseURL+"/api/auth/setup", map[string]string{
		"password": "test-pass",
		"confirm":  "test-pass",
	}, http.StatusOK, nil)
	doJSON(t, client, http.MethodPost, baseURL+"/api/auth/login", map[string]string{
		"password": "test-pass",
	}, http.StatusOK, nil)
	return client
}

func doJSON(t *testing.T, client *http.Client, method, url string, body interface{}, wantStatus int, out interface{}) {
	t.Helper()
	var payload []byte
	if body != nil {
		var err error
		payload, err = json.Marshal(body)
		if err != nil {
			t.Fatalf("json.Marshal: %v", err)
		}
	}
	req, err := http.NewRequest(method, url, bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("http.NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != wantStatus {
		t.Fatalf("%s %s status=%d want=%d", method, url, resp.StatusCode, wantStatus)
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			t.Fatalf("decode response %s %s: %v", method, url, err)
		}
	}
}

func cookieHeader(t *testing.T, client *http.Client, baseURL string) string {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, baseURL, nil)
	if err != nil {
		t.Fatalf("http.NewRequest for cookie header: %v", err)
	}
	cookies := client.Jar.Cookies(req.URL)
	parts := make([]string, 0, len(cookies))
	for _, c := range cookies {
		parts = append(parts, c.Name+"="+c.Value)
	}
	return strings.Join(parts, "; ")
}

func waitForBabyState(t *testing.T, client *http.Client, url string, cond func(map[string]interface{}) bool) {
	t.Helper()
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		var b map[string]interface{}
		doJSON(t, client, http.MethodGet, url, nil, http.StatusOK, &b)
		if cond(b) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for baby state condition")
}

func int32Ptr(v int32) *int32 { return &v }

func TestE2EMFAHandoffFlow(t *testing.T) {
	mock := newMockNanitCloud(t)
	mock.mfaEnabled = true
	restoreAPIBase := nanit.SetAPIBaseForTest(mock.URL())
	t.Cleanup(restoreAPIBase)

	httpPort := freePort(t)
	rtmpPort := freePort(t)
	cfg := buildTestConfig(t, httpPort, rtmpPort)

	tokenMgr := nanit.NewTokenManager(cfg.NanitEmail, cfg.NanitPassword, cfg.SessionFile)

	rtmp := rtmpserver.NewServer(cfg.RTMPPort, cfg.RTMPToken)
	if err := rtmp.Start(); err != nil {
		t.Fatalf("rtmp.Start(): %v", err)
	}
	t.Cleanup(rtmp.Stop)

	mgr := baby.NewManager(tokenMgr, cfg.RTMPAddr, cfg.RTMPToken, cfg.SensorPollSec, "", rtmp)
	startOrRestart := func() error {
		if mgr.IsStarted() {
			return mgr.Restart()
		}
		return mgr.Start()
	}

	logBcast := api.NewLogBroadcaster()
	apiServer := api.NewServer(
		cfg.HTTPPort,
		mgr,
		rtmp,
		logBcast,
		cfg.DashboardAuthFile,
		tokenMgr,
		startOrRestart,
		cfg.RTMPAddr,
		cfg.RTMPTokenFile,
		"test",
	)
	if err := apiServer.Start(); err != nil {
		t.Fatalf("api.Start(): %v", err)
	}

	baseURL := "http://127.0.0.1:" + itoa(httpPort)
	waitForHTTP(t, baseURL+"/login", 3*time.Second)

	// Simulate ensureAuth in non-interactive mode: Login() returns MFA token,
	// hand it off to the API server instead of blocking on stdin.
	mfaToken, err := tokenMgr.Login()
	if err != nil {
		t.Fatalf("Login() should succeed with MFA token: %v", err)
	}
	if mfaToken == "" {
		t.Fatalf("expected non-empty MFA token from mock cloud")
	}
	apiServer.SetPendingMFA(mfaToken)

	// Manager should NOT be started yet.
	if mgr.IsStarted() {
		t.Fatalf("manager should not be started before MFA completion")
	}

	// Auth as dashboard user.
	client := newAuthedHTTPClient(t, baseURL)

	// Verify /api/nanit/status reports mfa_pending.
	var status map[string]interface{}
	doJSON(t, client, http.MethodGet, baseURL+"/api/nanit/status", nil, http.StatusOK, &status)
	if status["mfa_pending"] != true {
		t.Fatalf("expected mfa_pending=true, got %v", status["mfa_pending"])
	}
	if status["connected"] != false {
		t.Fatalf("expected connected=false, got %v", status["connected"])
	}

	// Submit MFA code via dashboard endpoint.
	var mfaResp map[string]string
	doJSON(t, client, http.MethodPost, baseURL+"/api/nanit/mfa",
		map[string]string{"code": "123456"}, http.StatusOK, &mfaResp)
	if mfaResp["status"] != "ok" {
		t.Fatalf("MFA response status = %q, want ok", mfaResp["status"])
	}

	// Manager should now be started.
	if !mgr.IsStarted() {
		t.Fatalf("manager should be started after MFA completion")
	}

	// Verify status shows connected and no pending MFA.
	var status2 map[string]interface{}
	doJSON(t, client, http.MethodGet, baseURL+"/api/nanit/status", nil, http.StatusOK, &status2)
	if status2["connected"] != true {
		t.Fatalf("expected connected=true after MFA, got %v", status2["connected"])
	}
	if status2["mfa_pending"] == true {
		t.Fatalf("expected mfa_pending=false after MFA completion")
	}

	t.Cleanup(mgr.Stop)
}
