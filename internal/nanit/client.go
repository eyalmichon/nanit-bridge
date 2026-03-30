package nanit

import (
	"crypto/tls"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"google.golang.org/protobuf/proto"

	pb "nanit-bridge/internal/nanit/nanitpb"
)

var (
	keepaliveInterval = 25 * time.Second
	reconnectInterval = 5 * time.Second
	staleTimeout      = 90 * time.Second
	writeTimeout      = 5 * time.Second
)

// sleepModeIndicator is a substring the camera firmware includes in its
// status message when the stream request is rejected because sleep mode
// is active. If the firmware ever changes this wording, update here.
const sleepModeIndicator = "sleeping"

const (
	playbackTimerContinuous   = int32(-1)
	defaultSoundtrackCategory = int32(0)
	soundLowThresholdDefault  = int32(750)
	motionLowThresholdDefault = int32(2000)
	sensorSampleIntervalSec   = int32(1)
	sensorTriggerIntervalSec  = int32(0)
)

type SensorUpdate struct {
	CameraUID string
	Data      []*pb.SensorData
}

type StreamingUpdate struct {
	CameraUID string
	Streaming *pb.Streaming
}

type CameraClient struct {
	cameraUID string
	babyUID   string
	tokenMgr  *TokenManager
	rtmpURL   string

	sensorPollSec atomic.Int32

	conn      *websocket.Conn
	connMu    sync.Mutex
	requestID atomic.Int32
	lastRecv  atomic.Int64

	onSensor        func(SensorUpdate)
	onStreaming     func(StreamingUpdate)
	onSettings      func(*pb.Settings)
	onControl       func(*pb.Control)
	onPlaybackState func(*pb.Playback)
	onSoundtracks   func([]*pb.Soundtrack)
	onStingStatus   func(*pb.StingStatus)
	onStatus        func(*pb.Status)
	onFirmware      func(*pb.Firmware)
	onConnect       func()
	onDisconnect    func()

	done            chan struct{}
	wg              sync.WaitGroup
	streamRetryMu   sync.Mutex
	streamRetrying  bool
	lastWinLocation *pb.Point
}

func NewCameraClient(cameraUID, babyUID string, tokenMgr *TokenManager, rtmpURL string, sensorPollSec int) *CameraClient {
	if sensorPollSec <= 0 {
		sensorPollSec = 30
	}
	c := &CameraClient{
		cameraUID: cameraUID,
		babyUID:   babyUID,
		tokenMgr:  tokenMgr,
		rtmpURL:   rtmpURL,
		done:      make(chan struct{}),
	}
	c.sensorPollSec.Store(int32(sensorPollSec))
	return c
}

func (c *CameraClient) SetSensorPollInterval(seconds int) {
	if seconds < 5 {
		seconds = 5
	}
	c.sensorPollSec.Store(int32(seconds))
	log.Printf("[camera:%s] sensor poll interval set to %ds", c.cameraUID, seconds)
}

func (c *CameraClient) GetSensorPollInterval() int {
	return int(c.sensorPollSec.Load())
}

func (c *CameraClient) OnSensor(fn func(SensorUpdate))          { c.onSensor = fn }
func (c *CameraClient) OnStreaming(fn func(StreamingUpdate))    { c.onStreaming = fn }
func (c *CameraClient) OnSettings(fn func(*pb.Settings))        { c.onSettings = fn }
func (c *CameraClient) OnControl(fn func(*pb.Control))          { c.onControl = fn }
func (c *CameraClient) OnPlaybackState(fn func(*pb.Playback))   { c.onPlaybackState = fn }
func (c *CameraClient) OnSoundtracks(fn func([]*pb.Soundtrack)) { c.onSoundtracks = fn }
func (c *CameraClient) OnStingStatus(fn func(*pb.StingStatus))  { c.onStingStatus = fn }
func (c *CameraClient) OnStatus(fn func(*pb.Status))            { c.onStatus = fn }
func (c *CameraClient) OnFirmware(fn func(*pb.Firmware))        { c.onFirmware = fn }
func (c *CameraClient) OnConnect(fn func())                     { c.onConnect = fn }
func (c *CameraClient) OnDisconnect(fn func())                  { c.onDisconnect = fn }

func (c *CameraClient) Start() {
	c.wg.Add(1)
	go c.connectLoop()
}

func (c *CameraClient) Stop() {
	select {
	case <-c.done:
		return
	default:
		close(c.done)
	}

	c.connMu.Lock()
	if c.conn != nil {
		c.conn.Close()
	}
	c.connMu.Unlock()

	ch := make(chan struct{})
	go func() { c.wg.Wait(); close(ch) }()
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		log.Printf("[camera:%s] stop: timed out waiting for goroutines", c.cameraUID)
	}
}

func (c *CameraClient) delayedAction(d time.Duration, fn func()) {
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		select {
		case <-c.done:
			return
		case <-time.After(d):
		}
		fn()
	}()
}

func (c *CameraClient) connectLoop() {
	defer c.wg.Done()

	for {
		select {
		case <-c.done:
			return
		default:
		}

		if err := c.connect(); err != nil {
			log.Printf("[camera:%s] connection error: %v", c.cameraUID, err)
		}

		select {
		case <-c.done:
			return
		case <-time.After(reconnectInterval):
		}
	}
}

func (c *CameraClient) connect() error {
	token, err := c.tokenMgr.GetAccessToken()
	if err != nil {
		return fmt.Errorf("get access token: %w", err)
	}

	wsBase := strings.Replace(apiBase, "https://", "wss://", 1)
	wsBase = strings.Replace(wsBase, "http://", "ws://", 1)
	url := fmt.Sprintf("%s/focus/cameras/%s/user_connect", wsBase, c.cameraUID)
	header := http.Header{
		"Authorization": []string{"Bearer " + token},
	}

	dialer := websocket.Dialer{
		TLSClientConfig:  &tls.Config{},
		HandshakeTimeout: 10 * time.Second,
	}

	conn, _, err := dialer.Dial(url, header)
	if err != nil {
		return fmt.Errorf("websocket dial: %w", err)
	}

	c.connMu.Lock()
	c.conn = conn
	c.connMu.Unlock()
	c.lastRecv.Store(time.Now().Unix())

	log.Printf("[camera:%s] connected to cloud WebSocket", c.cameraUID)

	if c.onConnect != nil {
		c.onConnect()
	}

	errCh := make(chan error, 2)

	c.wg.Add(3)
	go c.readLoop(conn, errCh)
	go c.keepaliveLoop(conn, errCh)
	go c.sensorPollLoop()

	if err := c.startStreaming(); err != nil {
		log.Printf("[camera:%s] failed to start streaming: %v", c.cameraUID, err)
		c.scheduleStreamRetry()
	}

	initRequests := []struct {
		name string
		fn   func() error
	}{
		{"sensor data", c.requestSensorData},
		{"settings", c.RequestSettings},
		{"control state", c.RequestControl},
		{"sensor push", c.enableSensorPush},
		{"playback state", c.RequestPlayback},
		{"soundtracks", c.RequestSoundtracks},
		{"sting status", c.RequestStingStatus},
		{"camera status", c.RequestStatus},
		{"firmware", c.RequestFirmware},
	}
	for _, r := range initRequests {
		if err := r.fn(); err != nil {
			log.Printf("[camera:%s] init %s: %v", c.cameraUID, r.name, err)
		}
	}

	var result error
	select {
	case result = <-errCh:
	case <-c.done:
	}

	c.stopStreaming()
	conn.Close()

	if c.onDisconnect != nil {
		c.onDisconnect()
	}

	return result
}

func (c *CameraClient) readLoop(conn *websocket.Conn, errCh chan<- error) {
	defer c.wg.Done()

	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			errCh <- fmt.Errorf("read: %w", err)
			return
		}

		c.lastRecv.Store(time.Now().Unix())

		msg := &pb.Message{}
		if err := proto.Unmarshal(data, msg); err != nil {
			log.Printf("[camera:%s] protobuf unmarshal error: %v", c.cameraUID, err)
			continue
		}

		c.handleMessage(msg)
	}
}

func (c *CameraClient) sensorPollLoop() {
	defer c.wg.Done()

	curInterval := c.sensorPollSec.Load()
	ticker := time.NewTicker(time.Duration(curInterval) * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			if err := c.requestSensorData(); err != nil {
				log.Printf("[camera:%s] sensor poll failed: %v", c.cameraUID, err)
			}
			if newInterval := c.sensorPollSec.Load(); newInterval != curInterval {
				curInterval = newInterval
				ticker.Reset(time.Duration(curInterval) * time.Second)
			}
		case <-c.done:
			return
		}
	}
}

func (c *CameraClient) keepaliveLoop(conn *websocket.Conn, errCh chan<- error) {
	defer c.wg.Done()

	ticker := time.NewTicker(keepaliveInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			if time.Since(time.Unix(c.lastRecv.Load(), 0)) > staleTimeout {
				errCh <- fmt.Errorf("stale connection (no data for %v)", staleTimeout)
				return
			}

			msg := &pb.Message{
				Type: pb.Message_KEEPALIVE.Enum(),
			}
			data, err := proto.Marshal(msg)
			if err != nil {
				continue
			}

			c.connMu.Lock()
			_ = conn.SetWriteDeadline(time.Now().Add(writeTimeout))
			err = conn.WriteMessage(websocket.BinaryMessage, data)
			c.connMu.Unlock()
			if err != nil {
				errCh <- fmt.Errorf("keepalive write: %w", err)
				return
			}

		case <-c.done:
			return
		}
	}
}

func (c *CameraClient) handleMessage(msg *pb.Message) {
	switch msg.GetType() {
	case pb.Message_REQUEST:
		c.handleRequest(msg.GetRequest())
	case pb.Message_RESPONSE:
		c.handleResponse(msg.GetResponse())
	case pb.Message_KEEPALIVE:
		// no-op
	}
}

func (c *CameraClient) handleRequest(req *pb.Request) {
	if req == nil {
		return
	}

	switch req.GetType() {
	case pb.RequestType_PUT_SENSOR_DATA:
		if c.onSensor != nil {
			c.onSensor(SensorUpdate{
				CameraUID: c.cameraUID,
				Data:      req.GetSensorData_(),
			})
		}

	case pb.RequestType_PUT_SETTINGS:
		if c.onSettings != nil && req.GetSettings() != nil {
			c.onSettings(req.GetSettings())
		}

	case pb.RequestType_PUT_CONTROL:
		if c.onControl != nil && req.GetControl() != nil {
			c.onControl(req.GetControl())
		}

	case pb.RequestType_PUT_PLAYBACK:
		if err := c.RequestPlayback(); err != nil {
			log.Printf("[camera:%s] failed to query playback after push: %v", c.cameraUID, err)
		}

	case pb.RequestType_PUT_STREAMING:
		if c.onStreaming != nil && req.GetStreaming() != nil {
			c.onStreaming(StreamingUpdate{
				CameraUID: c.cameraUID,
				Streaming: req.GetStreaming(),
			})
		}

	case pb.RequestType_PUT_STING_STATUS:
		if req.GetStingStatus() != nil {
			ss := req.GetStingStatus()
			log.Printf("[camera:%s] STING_STATUS push: state=%v breathing=%v bpm=%d",
				c.cameraUID, ss.GetState(), ss.GetBreathing(), ss.GetBreathsPerMin())
			switch ss.GetState() {
			case pb.StingStatus_RUNNING, pb.StingStatus_PAUSED, pb.StingStatus_RESUMING:
				if ss.GetWinLocation() != nil {
					c.lastWinLocation = ss.GetWinLocation()
				}
			case pb.StingStatus_INIT_FAILED, pb.StingStatus_COMPLETED_AFTER_INIT_FAILED:
				c.lastWinLocation = nil
			}
			if c.onStingStatus != nil {
				c.onStingStatus(ss)
			}
		}

	default:
	}
}

func (c *CameraClient) handleResponse(resp *pb.Response) {
	if resp == nil {
		return
	}

	switch resp.GetRequestType() {
	case pb.RequestType_GET_SENSOR_DATA:
		if c.onSensor != nil {
			c.onSensor(SensorUpdate{
				CameraUID: c.cameraUID,
				Data:      resp.GetSensorData(),
			})
		}

	case pb.RequestType_GET_SETTINGS:
		if c.onSettings != nil && resp.GetSettings() != nil {
			c.onSettings(resp.GetSettings())
		}

	case pb.RequestType_GET_CONTROL:
		if c.onControl != nil && resp.GetControl() != nil {
			c.onControl(resp.GetControl())
		}

	case pb.RequestType_GET_PLAYBACK:
		if c.onPlaybackState != nil && resp.GetPlayback() != nil {
			c.onPlaybackState(resp.GetPlayback())
		}

	case pb.RequestType_PUT_PLAYBACK:
		// Acknowledged; no action needed.

	case pb.RequestType_GET_SOUNDTRACKS:
		if c.onSoundtracks != nil {
			c.onSoundtracks(resp.GetSoundtracks())
		}

	case pb.RequestType_GET_STING_STATUS:
		ss := resp.GetStingStatus()
		if ss != nil {
			log.Printf("[camera:%s] GET_STING_STATUS: state=%v breathing=%v bpm=%d",
				c.cameraUID, ss.GetState(), ss.GetBreathing(), ss.GetBreathsPerMin())
		}
		if c.onStingStatus != nil && ss != nil {
			c.onStingStatus(ss)
		}

	case pb.RequestType_GET_STATUS:
		if c.onStatus != nil && resp.GetStatus() != nil {
			c.onStatus(resp.GetStatus())
		}

	case pb.RequestType_GET_FIRMWARE:
		if c.onFirmware != nil && resp.GetFirmware() != nil {
			c.onFirmware(resp.GetFirmware())
		}

	case pb.RequestType_PUT_STING_START:
		log.Printf("[camera:%s] PUT_STING_START response: status=%d %s",
			c.cameraUID, resp.GetStatusCode(), resp.GetStatusMessage())
		if resp.GetStatusCode() != 200 {
			if c.onStingStatus != nil {
				off := pb.StingStatus_OFF
				c.onStingStatus(&pb.StingStatus{State: &off})
			}
		} else {
			go c.RequestStingStatus()
		}

	case pb.RequestType_PUT_STING_STOP:
		if resp.GetStatusCode() != 200 {
			log.Printf("[camera:%s] PUT_STING_STOP: status=%d %s",
				c.cameraUID, resp.GetStatusCode(), resp.GetStatusMessage())
		} else {
			if c.onStingStatus != nil {
				off := pb.StingStatus_OFF
				c.onStingStatus(&pb.StingStatus{State: &off})
			}
		}

	case pb.RequestType_PUT_STREAMING:
		if resp.GetStatusCode() == 200 {
			c.streamRetryMu.Lock()
			c.streamRetrying = false
			c.streamRetryMu.Unlock()
			if c.onStreaming != nil {
				if streams := resp.GetStreaming(); len(streams) > 0 {
					for _, s := range streams {
						c.onStreaming(StreamingUpdate{
							CameraUID: c.cameraUID,
							Streaming: s,
						})
					}
				} else {
					started := pb.Streaming_STARTED
					c.onStreaming(StreamingUpdate{
						CameraUID: c.cameraUID,
						Streaming: &pb.Streaming{Status: &started},
					})
				}
			}
		} else {
			log.Printf("[camera:%s] PUT_STREAMING failed: status=%d %s",
				c.cameraUID, resp.GetStatusCode(), resp.GetStatusMessage())
			if strings.Contains(resp.GetStatusMessage(), sleepModeIndicator) {
				log.Printf("[camera:%s] camera is in sleep mode, stopping stream retry", c.cameraUID)
				c.streamRetryMu.Lock()
				c.streamRetrying = false
				c.streamRetryMu.Unlock()
			} else {
				c.scheduleStreamRetry()
			}
		}

	default:
		if resp.GetStatusCode() != 200 {
			log.Printf("[camera:%s] %v: status=%d %s",
				c.cameraUID, resp.GetRequestType(), resp.GetStatusCode(), resp.GetStatusMessage())
		}
	}
}

func (c *CameraClient) sendRequest(reqType pb.RequestType, populate func(*pb.Request)) error {
	id := c.requestID.Add(1)
	req := &pb.Request{
		Id:   &id,
		Type: reqType.Enum(),
	}
	if populate != nil {
		populate(req)
	}

	msg := &pb.Message{
		Type:    pb.Message_REQUEST.Enum(),
		Request: req,
	}

	data, err := proto.Marshal(msg)
	if err != nil {
		return fmt.Errorf("marshal request: %w", err)
	}

	c.connMu.Lock()
	defer c.connMu.Unlock()
	if c.conn == nil {
		return fmt.Errorf("not connected")
	}
	_ = c.conn.SetWriteDeadline(time.Now().Add(writeTimeout))
	return c.conn.WriteMessage(websocket.BinaryMessage, data)
}

func (c *CameraClient) startStreaming() error {
	return c.sendRequest(pb.RequestType_PUT_STREAMING, func(req *pb.Request) {
		started := pb.Streaming_STARTED
		mobile := pb.StreamIdentifier_MOBILE
		attempts := int32(1)
		req.Streaming = &pb.Streaming{
			Id:       &mobile,
			Status:   &started,
			RtmpUrl:  &c.rtmpURL,
			Attempts: &attempts,
		}
	})
}

func (c *CameraClient) stopStreaming() {
	c.streamRetryMu.Lock()
	c.streamRetrying = false
	c.streamRetryMu.Unlock()

	select {
	case <-c.done:
		return // shutting down, skip the network write
	default:
	}

	empty := ""
	_ = c.sendRequest(pb.RequestType_PUT_STREAMING, func(req *pb.Request) {
		stopped := pb.Streaming_STOPPED
		mobile := pb.StreamIdentifier_MOBILE
		req.Streaming = &pb.Streaming{
			Id:      &mobile,
			Status:  &stopped,
			RtmpUrl: &empty,
		}
	})
}

// RestartStreaming re-requests the video stream from the camera.
// It sends an immediate request, then enters the retry loop which keeps
// retrying until the camera confirms the stream is active.
func (c *CameraClient) RestartStreaming() {
	log.Printf("[camera:%s] re-requesting stream", c.cameraUID)
	if err := c.startStreaming(); err != nil {
		log.Printf("[camera:%s] stream re-request failed: %v", c.cameraUID, err)
	}
	c.scheduleStreamRetry()
}

func (c *CameraClient) scheduleStreamRetry() {
	c.streamRetryMu.Lock()
	if c.streamRetrying {
		c.streamRetryMu.Unlock()
		return
	}
	c.streamRetrying = true
	c.streamRetryMu.Unlock()

	c.wg.Add(1)
	go c.streamRetryLoop()
}

func (c *CameraClient) streamRetryLoop() {
	defer c.wg.Done()
	log.Printf("[camera:%s] stream unavailable, retrying every 5s...", c.cameraUID)

	for attempt := 1; ; attempt++ {
		select {
		case <-c.done:
			return
		case <-time.After(5 * time.Second):
		}

		c.streamRetryMu.Lock()
		if !c.streamRetrying {
			c.streamRetryMu.Unlock()
			return
		}
		c.streamRetryMu.Unlock()

		if err := c.startStreaming(); err != nil {
			log.Printf("[camera:%s] stream retry send failed: %v", c.cameraUID, err)
			continue
		}

		// Wait for the async response.
		select {
		case <-c.done:
			return
		case <-time.After(3 * time.Second):
		}

		c.streamRetryMu.Lock()
		still := c.streamRetrying
		c.streamRetryMu.Unlock()
		if !still {
			log.Printf("[camera:%s] stream recovered on attempt %d", c.cameraUID, attempt)
			return
		}
	}
}

func (c *CameraClient) requestSensorData() error {
	return c.sendRequest(pb.RequestType_GET_SENSOR_DATA, func(req *pb.Request) {
		all := true
		req.GetSensorData = &pb.GetSensorData{All: &all}
	})
}

func (c *CameraClient) RequestSettings() error {
	return c.sendRequest(pb.RequestType_GET_SETTINGS, nil)
}

func (c *CameraClient) RequestControl() error {
	return c.sendRequest(pb.RequestType_GET_CONTROL, func(req *pb.Request) {
		t := true
		req.GetControl_ = &pb.GetControl{
			NightLight:           &t,
			NightLightTimeout:    &t,
			SensorDataTransferEn: &t,
		}
	})
}

func (c *CameraClient) enableSensorPush() error {
	return c.sendRequest(pb.RequestType_PUT_CONTROL, func(req *pb.Request) {
		t := true
		req.Control = &pb.Control{
			SensorDataTransfer: &pb.Control_SensorDataTransfer{
				Sound:       &t,
				Motion:      &t,
				Temperature: &t,
				Humidity:    &t,
				Light:       &t,
				Night:       &t,
			},
		}
	})
}

func (c *CameraClient) SetNightLight(on bool) error {
	return c.sendRequest(pb.RequestType_PUT_CONTROL, func(req *pb.Request) {
		v := pb.Control_LIGHT_OFF
		if on {
			v = pb.Control_LIGHT_ON
		}
		req.Control = &pb.Control{NightLight: &v}
	})
}

func (c *CameraClient) SetNightLightTimeout(seconds int) error {
	return c.sendRequest(pb.RequestType_PUT_CONTROL, func(req *pb.Request) {
		v := int32(seconds)
		req.Control = &pb.Control{NightLightTimeout: &v}
	})
}

func (c *CameraClient) SetPlayback(on bool) error {
	return c.SetPlaybackTrack(on, "")
}

func (c *CameraClient) SetPlaybackTrack(on bool, trackName string) error {
	err := c.sendRequest(pb.RequestType_PUT_PLAYBACK, func(req *pb.Request) {
		s := pb.Playback_STOPPED
		if on {
			s = pb.Playback_STARTED
		}
		playback := &pb.Playback{Status: &s}
		if on {
			continuous := playbackTimerContinuous
			playback.Timer = &continuous
			if trackName != "" {
				cat := defaultSoundtrackCategory
				playback.Soundtracks = []*pb.Soundtrack{{
					Category: &cat,
					Name:     &trackName,
				}}
			}
		}
		req.Playback = playback
	})
	if err != nil {
		return err
	}
	c.delayedAction(2*time.Second, func() {
		if err := c.RequestPlayback(); err != nil {
			log.Printf("[camera:%s] failed to query playback state: %v", c.cameraUID, err)
		}
	})
	return nil
}

func (c *CameraClient) SetVolume(level int) error {
	v := int32(level)
	settings := &pb.Settings{Volume: &v}
	err := c.sendRequest(pb.RequestType_PUT_SETTINGS, func(req *pb.Request) {
		req.Settings = settings
	})
	if err == nil && c.onSettings != nil {
		c.onSettings(settings)
	}
	return err
}

func (c *CameraClient) SetNightLightBrightness(level int) error {
	v := int32(level)
	settings := &pb.Settings{NightLightBrightness: &v}
	err := c.sendRequest(pb.RequestType_PUT_SETTINGS, func(req *pb.Request) {
		req.Settings = settings
	})
	if err == nil && c.onSettings != nil {
		c.onSettings(settings)
	}
	return err
}

func (c *CameraClient) SetSleepMode(on bool) error {
	settings := &pb.Settings{SleepMode: &on}
	err := c.sendRequest(pb.RequestType_PUT_SETTINGS, func(req *pb.Request) {
		req.Settings = settings
	})
	if err == nil && c.onSettings != nil {
		c.onSettings(settings)
	}
	if err == nil && !on {
		c.delayedAction(2*time.Second, func() {
			log.Printf("[camera:%s] sleep mode off — restarting stream", c.cameraUID)
			c.startStreaming()
		})
	}
	return err
}

func (c *CameraClient) SetNightVision(on bool) error {
	settings := &pb.Settings{NightVision: &on}
	err := c.sendRequest(pb.RequestType_PUT_SETTINGS, func(req *pb.Request) {
		req.Settings = settings
	})
	if err == nil && c.onSettings != nil {
		c.onSettings(settings)
	}
	return err
}

func (c *CameraClient) SetStatusLight(on bool) error {
	settings := &pb.Settings{StatusLightOn: &on}
	err := c.sendRequest(pb.RequestType_PUT_SETTINGS, func(req *pb.Request) {
		req.Settings = settings
	})
	if err == nil && c.onSettings != nil {
		c.onSettings(settings)
	}
	return err
}

func (c *CameraClient) SetMicMute(on bool) error {
	settings := &pb.Settings{MicMuteOn: &on}
	err := c.sendRequest(pb.RequestType_PUT_SETTINGS, func(req *pb.Request) {
		req.Settings = settings
	})
	if err == nil && c.onSettings != nil {
		c.onSettings(settings)
	}
	return err
}

func (c *CameraClient) SetSoundSensitivity(value int32) error {
	sType := pb.SensorType_SOUND
	useLow := true
	useHigh := true
	low := soundLowThresholdDefault
	high := value
	sampleSec := sensorSampleIntervalSec
	triggerSec := sensorTriggerIntervalSec
	settings := &pb.Settings{
		Sensors: []*pb.Settings_SensorSettings{{
			SensorType:         &sType,
			UseLowThreshold:    &useLow,
			UseHighThreshold:   &useHigh,
			LowThreshold:       &low,
			HighThreshold:      &high,
			SampleIntervalSec:  &sampleSec,
			TriggerIntervalSec: &triggerSec,
		}},
	}
	err := c.sendRequest(pb.RequestType_PUT_SETTINGS, func(req *pb.Request) {
		req.Settings = settings
	})
	if err == nil && c.onSettings != nil {
		c.onSettings(settings)
	}
	return err
}

func (c *CameraClient) SetMotionSensitivity(value int32) error {
	sType := pb.SensorType_MOTION
	useLow := true
	useHigh := true
	low := motionLowThresholdDefault
	high := value
	sampleSec := sensorSampleIntervalSec
	triggerSec := sensorTriggerIntervalSec
	settings := &pb.Settings{
		Sensors: []*pb.Settings_SensorSettings{{
			SensorType:         &sType,
			UseLowThreshold:    &useLow,
			UseHighThreshold:   &useHigh,
			LowThreshold:       &low,
			HighThreshold:      &high,
			SampleIntervalSec:  &sampleSec,
			TriggerIntervalSec: &triggerSec,
		}},
	}
	err := c.sendRequest(pb.RequestType_PUT_SETTINGS, func(req *pb.Request) {
		req.Settings = settings
	})
	if err == nil && c.onSettings != nil {
		c.onSettings(settings)
	}
	return err
}

func (c *CameraClient) RequestPlayback() error {
	return c.sendRequest(pb.RequestType_GET_PLAYBACK, nil)
}

func (c *CameraClient) RequestSoundtracks() error {
	return c.sendRequest(pb.RequestType_GET_SOUNDTRACKS, nil)
}

func (c *CameraClient) RequestStingStatus() error {
	return c.sendRequest(pb.RequestType_GET_STING_STATUS, nil)
}

func (c *CameraClient) StartBreathingMonitoring(point *BmmPatternPoint) error {
	return c.sendRequest(pb.RequestType_PUT_STING_START, func(req *pb.Request) {
		showOverlay := true
		ss := &pb.StingStart{
			DisplayData: &showOverlay,
		}
		if point != nil {
			x := int32(point.X)
			y := int32(point.Y)
			ss.WinLocation = &pb.Point{X: &x, Y: &y}
			log.Printf("[camera:%s] sting start with BMM point: x=%d y=%d", c.cameraUID, point.X, point.Y)
		} else if c.lastWinLocation != nil {
			log.Printf("[camera:%s] sting start with cached win_location: %v", c.cameraUID, c.lastWinLocation)
			ss.WinLocation = c.lastWinLocation
		} else {
			log.Printf("[camera:%s] sting start with no coordinates", c.cameraUID)
		}
		req.StingStart = ss
	})
}

func (c *CameraClient) StopBreathingMonitoring() error {
	return c.sendRequest(pb.RequestType_PUT_STING_STOP, nil)
}

func (c *CameraClient) RequestStatus() error {
	return c.sendRequest(pb.RequestType_GET_STATUS, func(req *pb.Request) {
		all := true
		req.GetStatus_ = &pb.GetStatus{All: &all}
	})
}

func (c *CameraClient) RequestFirmware() error {
	return c.sendRequest(pb.RequestType_GET_FIRMWARE, nil)
}
