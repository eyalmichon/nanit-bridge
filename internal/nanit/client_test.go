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

func int32Ptr(v int32) *int32 {
	return &v
}
