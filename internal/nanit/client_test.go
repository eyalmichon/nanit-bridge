package nanit

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"google.golang.org/protobuf/proto"

	pb "nanit-bridge/internal/nanit/nanitpb"
)

type wsCloud struct {
	t *testing.T

	srv      *httptest.Server
	upgrader websocket.Upgrader

	mu          sync.Mutex
	connections []*websocket.Conn
	recvCh      chan *pb.Message
}

func newWSCloud(t *testing.T) *wsCloud {
	t.Helper()
	c := &wsCloud{
		t:      t,
		recvCh: make(chan *pb.Message, 512),
		upgrader: websocket.Upgrader{
			CheckOrigin: func(r *http.Request) bool { return true },
		},
	}
	c.srv = httptest.NewServer(http.HandlerFunc(c.handle))
	t.Cleanup(c.srv.Close)
	return c
}

func (c *wsCloud) handle(w http.ResponseWriter, r *http.Request) {
	if !strings.Contains(r.URL.Path, "/focus/cameras/") || !strings.HasSuffix(r.URL.Path, "/user_connect") {
		http.NotFound(w, r)
		return
	}
	conn, err := c.upgrader.Upgrade(w, r, nil)
	if err != nil {
		c.t.Fatalf("upgrade error: %v", err)
	}

	c.mu.Lock()
	c.connections = append(c.connections, conn)
	c.mu.Unlock()

	go func() {
		for {
			_, data, err := conn.ReadMessage()
			if err != nil {
				return
			}
			msg := &pb.Message{}
			if err := proto.Unmarshal(data, msg); err != nil {
				continue
			}
			c.recvCh <- msg
		}
	}()
}

func (c *wsCloud) send(msg *pb.Message) error {
	data, err := proto.Marshal(msg)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.connections) == 0 {
		return nil
	}
	return c.connections[len(c.connections)-1].WriteMessage(websocket.BinaryMessage, data)
}

func (c *wsCloud) closeLatest() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.connections) == 0 {
		return
	}
	_ = c.connections[len(c.connections)-1].Close()
}

func setClientTimingForTest(t *testing.T, keepalive, reconnect, stale time.Duration) {
	t.Helper()
	oldKeepalive := keepaliveInterval
	oldReconnect := reconnectInterval
	oldStale := staleTimeout
	keepaliveInterval = keepalive
	reconnectInterval = reconnect
	staleTimeout = stale
	t.Cleanup(func() {
		keepaliveInterval = oldKeepalive
		reconnectInterval = oldReconnect
		staleTimeout = oldStale
	})
}

func await[T any](t *testing.T, ch <-chan T, timeout time.Duration, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(timeout):
		t.Fatalf("timed out waiting for %s", what)
		var zero T
		return zero
	}
}

func waitForOutbound(t *testing.T, ch <-chan *pb.Message, timeout time.Duration, match func(*pb.Message) bool, what string) *pb.Message {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case msg := <-ch:
			if match(msg) {
				return msg
			}
		case <-deadline:
			t.Fatalf("timed out waiting for outbound %s", what)
			return nil
		}
	}
}

func TestCameraClientWebSocketRoutingAndReconnect(t *testing.T) {
	cloud := newWSCloud(t)
	withTestAPIBase(t, cloud.srv.URL)
	setClientTimingForTest(t, 200*time.Millisecond, 200*time.Millisecond, 3*time.Second)

	tm := NewTokenManager("u@example.com", "pw", t.TempDir()+"/session.json")
	tm.session.AccessToken = "token-1"
	tm.session.ExpiresAt = time.Now().Add(30 * time.Minute)

	client := NewCameraClient("cam-1", "baby-1", tm, "rtmp://127.0.0.1:1935/local/baby-1", 3600)
	defer client.Stop()

	connectCh := make(chan struct{}, 4)
	disconnectCh := make(chan struct{}, 4)
	sensorCh := make(chan SensorUpdate, 4)
	settingsCh := make(chan *pb.Settings, 2)
	controlCh := make(chan *pb.Control, 2)
	streamingCh := make(chan StreamingUpdate, 4)
	playbackCh := make(chan *pb.Playback, 2)

	client.OnConnect(func() { connectCh <- struct{}{} })
	client.OnDisconnect(func() { disconnectCh <- struct{}{} })
	client.OnSensor(func(s SensorUpdate) { sensorCh <- s })
	client.OnSettings(func(s *pb.Settings) { settingsCh <- s })
	client.OnControl(func(c *pb.Control) { controlCh <- c })
	client.OnStreaming(func(s StreamingUpdate) { streamingCh <- s })
	client.OnPlaybackState(func(p *pb.Playback) { playbackCh <- p })

	client.Start()
	_ = await(t, connectCh, 3*time.Second, "initial connect")

	tempType := pb.SensorType_TEMPERATURE
	tempMilli := int32(23500)
	vol := int32(45)
	nl := pb.Control_LIGHT_ON
	streamStarted := pb.Streaming_STARTED
	rtmpURL := "rtmp://127.0.0.1:1935/local/baby-1"
	playbackStarted := pb.Playback_STARTED

	if err := cloud.send(&pb.Message{
		Type: pb.Message_REQUEST.Enum(),
		Request: &pb.Request{
			Id:          int32Ptr(101),
			Type:        pb.RequestType_PUT_SENSOR_DATA.Enum(),
			SensorData_: []*pb.SensorData{{SensorType: &tempType, ValueMilli: &tempMilli}},
		},
	}); err != nil {
		t.Fatalf("send sensor request: %v", err)
	}
	if got := await(t, sensorCh, 2*time.Second, "sensor callback from PUT_SENSOR_DATA"); got.CameraUID != "cam-1" || len(got.Data) != 1 {
		t.Fatalf("unexpected sensor callback: %+v", got)
	}

	if err := cloud.send(&pb.Message{
		Type: pb.Message_REQUEST.Enum(),
		Request: &pb.Request{
			Id:       int32Ptr(102),
			Type:     pb.RequestType_PUT_SETTINGS.Enum(),
			Settings: &pb.Settings{Volume: &vol},
		},
	}); err != nil {
		t.Fatalf("send settings request: %v", err)
	}
	if got := await(t, settingsCh, 2*time.Second, "settings callback"); got.GetVolume() != vol {
		t.Fatalf("settings volume = %d, want %d", got.GetVolume(), vol)
	}

	if err := cloud.send(&pb.Message{
		Type: pb.Message_REQUEST.Enum(),
		Request: &pb.Request{
			Id:      int32Ptr(103),
			Type:    pb.RequestType_PUT_CONTROL.Enum(),
			Control: &pb.Control{NightLight: &nl},
		},
	}); err != nil {
		t.Fatalf("send control request: %v", err)
	}
	if got := await(t, controlCh, 2*time.Second, "control callback"); got.GetNightLight() != pb.Control_LIGHT_ON {
		t.Fatalf("control night_light = %v, want LIGHT_ON", got.GetNightLight())
	}

	if err := cloud.send(&pb.Message{
		Type: pb.Message_REQUEST.Enum(),
		Request: &pb.Request{
			Id:        int32Ptr(104),
			Type:      pb.RequestType_PUT_STREAMING.Enum(),
			Streaming: &pb.Streaming{Id: pb.StreamIdentifier_MOBILE.Enum(), Status: &streamStarted, RtmpUrl: &rtmpURL},
		},
	}); err != nil {
		t.Fatalf("send streaming request: %v", err)
	}
	if got := await(t, streamingCh, 2*time.Second, "streaming callback from request"); got.Streaming.GetStatus() != pb.Streaming_STARTED {
		t.Fatalf("streaming status = %v, want STARTED", got.Streaming.GetStatus())
	}

	if err := cloud.send(&pb.Message{
		Type: pb.Message_RESPONSE.Enum(),
		Response: &pb.Response{
			RequestId:   int32Ptr(105),
			RequestType: pb.RequestType_GET_PLAYBACK.Enum(),
			StatusCode:  int32Ptr(200),
			Playback:    &pb.Playback{Status: &playbackStarted},
		},
	}); err != nil {
		t.Fatalf("send playback response: %v", err)
	}
	if got := await(t, playbackCh, 2*time.Second, "playback callback"); got.GetStatus() != pb.Playback_STARTED {
		t.Fatalf("playback status = %v, want STARTED", got.GetStatus())
	}

	if err := cloud.send(&pb.Message{
		Type: pb.Message_RESPONSE.Enum(),
		Response: &pb.Response{
			RequestId:   int32Ptr(106),
			RequestType: pb.RequestType_GET_SENSOR_DATA.Enum(),
			StatusCode:  int32Ptr(200),
			SensorData:  []*pb.SensorData{{SensorType: &tempType, ValueMilli: &tempMilli}},
		},
	}); err != nil {
		t.Fatalf("send sensor response: %v", err)
	}
	_ = await(t, sensorCh, 2*time.Second, "sensor callback from GET_SENSOR_DATA")

	if err := client.SetNightLight(true); err != nil {
		t.Fatalf("SetNightLight(true): %v", err)
	}
	_ = waitForOutbound(t, cloud.recvCh, 2*time.Second, func(msg *pb.Message) bool {
		if msg.GetType() != pb.Message_REQUEST || msg.GetRequest() == nil || msg.GetRequest().GetType() != pb.RequestType_PUT_CONTROL {
			return false
		}
		ctrl := msg.GetRequest().GetControl()
		return ctrl != nil && ctrl.NightLight != nil && ctrl.GetNightLight() == pb.Control_LIGHT_ON
	}, "PUT_CONTROL night_light request")

	_ = waitForOutbound(t, cloud.recvCh, 2*time.Second, func(msg *pb.Message) bool {
		return msg.GetType() == pb.Message_KEEPALIVE
	}, "keepalive")

	cloud.closeLatest()
	_ = await(t, disconnectCh, 3*time.Second, "disconnect callback")
	_ = await(t, connectCh, 3*time.Second, "reconnect callback")
}

func TestCameraClientStopCloseDoneFirst(t *testing.T) {
	cloud := newWSCloud(t)
	withTestAPIBase(t, cloud.srv.URL)
	setClientTimingForTest(t, 200*time.Millisecond, 200*time.Millisecond, 3*time.Second)

	tm := NewTokenManager("u@example.com", "pw", t.TempDir()+"/session.json")
	tm.session.AccessToken = "token-1"
	tm.session.ExpiresAt = time.Now().Add(30 * time.Minute)

	client := NewCameraClient("cam-stop", "baby-stop", tm, "rtmp://127.0.0.1:1935/local/baby-stop", 3600)
	connectCh := make(chan struct{}, 1)
	client.OnConnect(func() { connectCh <- struct{}{} })

	client.Start()
	_ = await(t, connectCh, 3*time.Second, "connect before stop")

	stopped := make(chan struct{})
	go func() {
		client.Stop()
		close(stopped)
	}()

	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatalf("Stop() timed out")
	}

	select {
	case <-client.done:
	default:
		t.Fatalf("done channel should be closed after Stop")
	}
}

func TestStreamRetryLoopTrackedInWaitGroup(t *testing.T) {
	tm := NewTokenManager("u@example.com", "pw", t.TempDir()+"/session.json")
	tm.session.AccessToken = "token-1"
	tm.session.ExpiresAt = time.Now().Add(30 * time.Minute)

	client := NewCameraClient("cam-retry", "baby-retry", tm, "rtmp://127.0.0.1:1935/local/baby-retry", 3600)
	client.scheduleStreamRetry()

	waitDone := make(chan struct{})
	go func() {
		client.wg.Wait()
		close(waitDone)
	}()

	select {
	case <-waitDone:
		t.Fatalf("waitgroup finished early; streamRetryLoop is not tracked")
	case <-time.After(150 * time.Millisecond):
		// Expected: retry loop is running and tracked by waitgroup.
	}

	client.Stop()
	select {
	case <-waitDone:
	case <-time.After(2 * time.Second):
		t.Fatalf("waitgroup did not finish after Stop")
	}
}

func int32Ptr(v int32) *int32 {
	return &v
}

func setupConnectedClient(t *testing.T) (*CameraClient, *wsCloud) {
	t.Helper()
	cloud := newWSCloud(t)
	withTestAPIBase(t, cloud.srv.URL)
	setClientTimingForTest(t, 200*time.Millisecond, 200*time.Millisecond, 3*time.Second)

	tm := NewTokenManager("u@example.com", "pw", t.TempDir()+"/session.json")
	tm.session.AccessToken = "token-1"
	tm.session.ExpiresAt = time.Now().Add(30 * time.Minute)

	client := NewCameraClient("cam-proto", "baby-proto", tm, "rtmp://127.0.0.1:1935/local/baby-proto", 3600)
	connectCh := make(chan struct{}, 1)
	client.OnConnect(func() { connectCh <- struct{}{} })
	client.Start()
	_ = await(t, connectCh, 3*time.Second, "connect for proto test")
	return client, cloud
}

func captureRequest(t *testing.T, cloud *wsCloud, fn func() error) *pb.Request {
	t.Helper()
	// drain any pending messages (keepalives, initial requests)
	for {
		select {
		case <-cloud.recvCh:
		default:
			goto drained
		}
	}
drained:
	if err := fn(); err != nil {
		t.Fatalf("function under test failed: %v", err)
	}
	msg := await(t, cloud.recvCh, 2*time.Second, "outbound request")
	if msg.GetType() != pb.Message_REQUEST {
		t.Fatalf("expected REQUEST, got %v", msg.GetType())
	}
	return msg.GetRequest()
}

func TestProtoMessageConstruction(t *testing.T) {
	client, cloud := setupConnectedClient(t)
	defer client.Stop()

	// Allow initial connect messages to flow
	time.Sleep(300 * time.Millisecond)

	t.Run("startStreaming", func(t *testing.T) {
		req := captureRequest(t, cloud, func() error { return client.startStreaming() })
		if req.GetType() != pb.RequestType_PUT_STREAMING {
			t.Fatalf("type = %v, want PUT_STREAMING", req.GetType())
		}
		s := req.GetStreaming()
		if s == nil {
			t.Fatal("streaming field is nil")
		}
		if s.GetStatus() != pb.Streaming_STARTED {
			t.Errorf("status = %v, want STARTED", s.GetStatus())
		}
		if s.GetId() != pb.StreamIdentifier_MOBILE {
			t.Errorf("id = %v, want MOBILE", s.GetId())
		}
		if s.GetRtmpUrl() != "rtmp://127.0.0.1:1935/local/baby-proto" {
			t.Errorf("rtmp_url = %q", s.GetRtmpUrl())
		}
		if s.GetAttempts() != 1 {
			t.Errorf("attempts = %d, want 1", s.GetAttempts())
		}
	})

	t.Run("stopStreaming", func(t *testing.T) {
		req := captureRequest(t, cloud, func() error { client.stopStreaming(); return nil })
		if req.GetType() != pb.RequestType_PUT_STREAMING {
			t.Fatalf("type = %v, want PUT_STREAMING", req.GetType())
		}
		s := req.GetStreaming()
		if s == nil {
			t.Fatal("streaming field is nil")
		}
		if s.GetStatus() != pb.Streaming_STOPPED {
			t.Errorf("status = %v, want STOPPED", s.GetStatus())
		}
		if s.GetId() != pb.StreamIdentifier_MOBILE {
			t.Errorf("id = %v, want MOBILE", s.GetId())
		}
	})

	t.Run("requestSensorData", func(t *testing.T) {
		req := captureRequest(t, cloud, func() error { return client.requestSensorData() })
		if req.GetType() != pb.RequestType_GET_SENSOR_DATA {
			t.Fatalf("type = %v, want GET_SENSOR_DATA", req.GetType())
		}
		if req.GetGetSensorData() == nil || !req.GetGetSensorData().GetAll() {
			t.Error("GetSensorData.All should be true")
		}
	})

	t.Run("RequestSettings", func(t *testing.T) {
		req := captureRequest(t, cloud, func() error { return client.RequestSettings() })
		if req.GetType() != pb.RequestType_GET_SETTINGS {
			t.Fatalf("type = %v, want GET_SETTINGS", req.GetType())
		}
		if req.GetGetSettings_() == nil || !req.GetGetSettings_().GetAll() {
			t.Error("GetSettings_.All should be true")
		}
	})

	t.Run("RequestControl", func(t *testing.T) {
		req := captureRequest(t, cloud, func() error { return client.RequestControl() })
		if req.GetType() != pb.RequestType_GET_CONTROL {
			t.Fatalf("type = %v, want GET_CONTROL", req.GetType())
		}
		gc := req.GetGetControl_()
		if gc == nil {
			t.Fatal("GetControl_ is nil")
		}
		if !gc.GetNightLight() || !gc.GetNightLightTimeout() || !gc.GetSensorDataTransferEn() {
			t.Error("expected all GetControl fields to be true")
		}
	})

	t.Run("enableSensorPush", func(t *testing.T) {
		req := captureRequest(t, cloud, func() error { return client.enableSensorPush() })
		if req.GetType() != pb.RequestType_PUT_CONTROL {
			t.Fatalf("type = %v, want PUT_CONTROL", req.GetType())
		}
		sdt := req.GetControl().GetSensorDataTransfer()
		if sdt == nil {
			t.Fatal("SensorDataTransfer is nil")
		}
		if !sdt.GetSound() || !sdt.GetMotion() || !sdt.GetTemperature() || !sdt.GetHumidity() || !sdt.GetLight() || !sdt.GetNight() {
			t.Error("expected all SensorDataTransfer fields to be true")
		}
	})

	t.Run("RequestStatus", func(t *testing.T) {
		req := captureRequest(t, cloud, func() error { return client.RequestStatus() })
		if req.GetType() != pb.RequestType_GET_STATUS {
			t.Fatalf("type = %v, want GET_STATUS", req.GetType())
		}
		if req.GetGetStatus_() == nil || !req.GetGetStatus_().GetAll() {
			t.Error("GetStatus_.All should be true")
		}
	})

	t.Run("RequestPlayback", func(t *testing.T) {
		req := captureRequest(t, cloud, func() error { return client.RequestPlayback() })
		if req.GetType() != pb.RequestType_GET_PLAYBACK {
			t.Fatalf("type = %v, want GET_PLAYBACK", req.GetType())
		}
	})

	t.Run("RequestSoundtracks", func(t *testing.T) {
		req := captureRequest(t, cloud, func() error { return client.RequestSoundtracks() })
		if req.GetType() != pb.RequestType_GET_SOUNDTRACKS {
			t.Fatalf("type = %v, want GET_SOUNDTRACKS", req.GetType())
		}
	})

	t.Run("RequestStingStatus", func(t *testing.T) {
		req := captureRequest(t, cloud, func() error { return client.RequestStingStatus() })
		if req.GetType() != pb.RequestType_GET_STING_STATUS {
			t.Fatalf("type = %v, want GET_STING_STATUS", req.GetType())
		}
	})

	t.Run("RequestFirmware", func(t *testing.T) {
		req := captureRequest(t, cloud, func() error { return client.RequestFirmware() })
		if req.GetType() != pb.RequestType_GET_FIRMWARE {
			t.Fatalf("type = %v, want GET_FIRMWARE", req.GetType())
		}
	})

	t.Run("SetNightLight_on", func(t *testing.T) {
		req := captureRequest(t, cloud, func() error { return client.SetNightLight(true) })
		if req.GetType() != pb.RequestType_PUT_CONTROL {
			t.Fatalf("type = %v, want PUT_CONTROL", req.GetType())
		}
		if req.GetControl().GetNightLight() != pb.Control_LIGHT_ON {
			t.Errorf("NightLight = %v, want LIGHT_ON", req.GetControl().GetNightLight())
		}
	})

	t.Run("SetNightLight_off", func(t *testing.T) {
		req := captureRequest(t, cloud, func() error { return client.SetNightLight(false) })
		if req.GetType() != pb.RequestType_PUT_CONTROL {
			t.Fatalf("type = %v, want PUT_CONTROL", req.GetType())
		}
		if req.GetControl().GetNightLight() != pb.Control_LIGHT_OFF {
			t.Errorf("NightLight = %v, want LIGHT_OFF", req.GetControl().GetNightLight())
		}
	})

	t.Run("SetNightLightTimeout", func(t *testing.T) {
		req := captureRequest(t, cloud, func() error { return client.SetNightLightTimeout(300) })
		if req.GetType() != pb.RequestType_PUT_CONTROL {
			t.Fatalf("type = %v, want PUT_CONTROL", req.GetType())
		}
		if req.GetControl().GetNightLightTimeout() != 300 {
			t.Errorf("timeout = %d, want 300", req.GetControl().GetNightLightTimeout())
		}
	})

	t.Run("SetVolume", func(t *testing.T) {
		req := captureRequest(t, cloud, func() error { return client.SetVolume(75) })
		if req.GetType() != pb.RequestType_PUT_SETTINGS {
			t.Fatalf("type = %v, want PUT_SETTINGS", req.GetType())
		}
		if req.GetSettings().GetVolume() != 75 {
			t.Errorf("volume = %d, want 75", req.GetSettings().GetVolume())
		}
	})

	t.Run("SetNightLightBrightness", func(t *testing.T) {
		req := captureRequest(t, cloud, func() error { return client.SetNightLightBrightness(50) })
		if req.GetType() != pb.RequestType_PUT_SETTINGS {
			t.Fatalf("type = %v, want PUT_SETTINGS", req.GetType())
		}
		if req.GetSettings().GetNightLightBrightness() != 50 {
			t.Errorf("brightness = %d, want 50", req.GetSettings().GetNightLightBrightness())
		}
	})

	t.Run("SetSleepMode_on", func(t *testing.T) {
		req := captureRequest(t, cloud, func() error { return client.SetSleepMode(true) })
		if req.GetType() != pb.RequestType_PUT_SETTINGS {
			t.Fatalf("type = %v, want PUT_SETTINGS", req.GetType())
		}
		if !req.GetSettings().GetSleepMode() {
			t.Error("SleepMode should be true")
		}
	})

	t.Run("SetNightVision", func(t *testing.T) {
		req := captureRequest(t, cloud, func() error { return client.SetNightVision(2) })
		if req.GetType() != pb.RequestType_PUT_SETTINGS {
			t.Fatalf("type = %v, want PUT_SETTINGS", req.GetType())
		}
		if req.GetSettings().GetNightVision() != pb.Settings_NV_ON {
			t.Errorf("NightVision = %v, want NV_ON", req.GetSettings().GetNightVision())
		}
	})

	t.Run("SetStatusLight", func(t *testing.T) {
		req := captureRequest(t, cloud, func() error { return client.SetStatusLight(true) })
		if req.GetType() != pb.RequestType_PUT_SETTINGS {
			t.Fatalf("type = %v, want PUT_SETTINGS", req.GetType())
		}
		if !req.GetSettings().GetStatusLightOn() {
			t.Error("StatusLightOn should be true")
		}
	})

	t.Run("SetMicMute", func(t *testing.T) {
		req := captureRequest(t, cloud, func() error { return client.SetMicMute(true) })
		if req.GetType() != pb.RequestType_PUT_SETTINGS {
			t.Fatalf("type = %v, want PUT_SETTINGS", req.GetType())
		}
		if !req.GetSettings().GetMicMuteOn() {
			t.Error("MicMuteOn should be true")
		}
	})

	t.Run("SetSoundSensitivity", func(t *testing.T) {
		req := captureRequest(t, cloud, func() error { return client.SetSoundSensitivity(500) })
		if req.GetType() != pb.RequestType_PUT_SETTINGS {
			t.Fatalf("type = %v, want PUT_SETTINGS", req.GetType())
		}
		sensors := req.GetSettings().GetSensors()
		if len(sensors) != 1 {
			t.Fatalf("sensors count = %d, want 1", len(sensors))
		}
		s := sensors[0]
		if s.GetSensorType() != pb.SensorType_SOUND {
			t.Errorf("sensor type = %v, want SOUND", s.GetSensorType())
		}
		if s.GetHighThreshold() != 500 {
			t.Errorf("high threshold = %d, want 500", s.GetHighThreshold())
		}
	})

	t.Run("SetMotionSensitivity", func(t *testing.T) {
		req := captureRequest(t, cloud, func() error { return client.SetMotionSensitivity(3000) })
		if req.GetType() != pb.RequestType_PUT_SETTINGS {
			t.Fatalf("type = %v, want PUT_SETTINGS", req.GetType())
		}
		sensors := req.GetSettings().GetSensors()
		if len(sensors) != 1 {
			t.Fatalf("sensors count = %d, want 1", len(sensors))
		}
		s := sensors[0]
		if s.GetSensorType() != pb.SensorType_MOTION {
			t.Errorf("sensor type = %v, want MOTION", s.GetSensorType())
		}
		if s.GetHighThreshold() != 3000 {
			t.Errorf("high threshold = %d, want 3000", s.GetHighThreshold())
		}
	})

	t.Run("SetPlaybackTrack_on", func(t *testing.T) {
		req := captureRequest(t, cloud, func() error { return client.SetPlaybackTrack(true, "White Noise") })
		if req.GetType() != pb.RequestType_PUT_PLAYBACK {
			t.Fatalf("type = %v, want PUT_PLAYBACK", req.GetType())
		}
		p := req.GetPlayback()
		if p.GetStatus() != pb.Playback_STARTED {
			t.Errorf("status = %v, want STARTED", p.GetStatus())
		}
		if p.GetTimer() != -1 {
			t.Errorf("timer = %d, want -1 (continuous)", p.GetTimer())
		}
		if len(p.GetSoundtracks()) != 1 || p.GetSoundtracks()[0].GetName() != "White Noise" {
			t.Errorf("soundtracks = %v, want [{White Noise}]", p.GetSoundtracks())
		}
	})

	t.Run("SetPlaybackTrack_off", func(t *testing.T) {
		req := captureRequest(t, cloud, func() error { return client.SetPlaybackTrack(false, "") })
		if req.GetType() != pb.RequestType_PUT_PLAYBACK {
			t.Fatalf("type = %v, want PUT_PLAYBACK", req.GetType())
		}
		if req.GetPlayback().GetStatus() != pb.Playback_STOPPED {
			t.Errorf("status = %v, want STOPPED", req.GetPlayback().GetStatus())
		}
	})

	t.Run("StartBreathingMonitoring", func(t *testing.T) {
		req := captureRequest(t, cloud, func() error { return client.StartBreathingMonitoring(nil) })
		if req.GetType() != pb.RequestType_PUT_STING_START {
			t.Fatalf("type = %v, want PUT_STING_START", req.GetType())
		}
		ss := req.GetStingStart()
		if ss == nil {
			t.Fatal("StingStart is nil")
		}
		if !ss.GetDisplayData() {
			t.Error("DisplayData should be true")
		}
	})

	t.Run("StartBreathingMonitoring_withPoint", func(t *testing.T) {
		req := captureRequest(t, cloud, func() error {
			return client.StartBreathingMonitoring(&BmmPatternPoint{X: 100, Y: 200})
		})
		ss := req.GetStingStart()
		if ss.GetWinLocation() == nil {
			t.Fatal("WinLocation is nil")
		}
		if ss.GetWinLocation().GetX() != 100 || ss.GetWinLocation().GetY() != 200 {
			t.Errorf("WinLocation = (%d,%d), want (100,200)",
				ss.GetWinLocation().GetX(), ss.GetWinLocation().GetY())
		}
	})

	t.Run("StopBreathingMonitoring", func(t *testing.T) {
		req := captureRequest(t, cloud, func() error { return client.StopBreathingMonitoring() })
		if req.GetType() != pb.RequestType_PUT_STING_STOP {
			t.Fatalf("type = %v, want PUT_STING_STOP", req.GetType())
		}
	})
}
