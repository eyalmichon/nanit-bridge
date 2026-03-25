package nanit

import (
	"crypto/tls"
	"fmt"
	"log"
	"math"
	"math/rand"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"google.golang.org/protobuf/proto"

	pb "nanit-bridge/internal/nanit/nanitpb"
)

const (
	keepaliveInterval = 25 * time.Second
	maxBackoff        = 5 * time.Minute
	baseBackoff       = 2 * time.Second
	staleTimeout      = 5 * time.Minute
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

	conn      *websocket.Conn
	connMu    sync.Mutex
	requestID atomic.Int32
	lastRecv  atomic.Int64

	onSensor    func(SensorUpdate)
	onStreaming func(StreamingUpdate)
	onSettings  func(*pb.Settings)
	onControl   func(*pb.Control)

	done chan struct{}
	wg   sync.WaitGroup
}

func NewCameraClient(cameraUID, babyUID string, tokenMgr *TokenManager, rtmpURL string) *CameraClient {
	return &CameraClient{
		cameraUID: cameraUID,
		babyUID:   babyUID,
		tokenMgr:  tokenMgr,
		rtmpURL:   rtmpURL,
		done:      make(chan struct{}),
	}
}

func (c *CameraClient) OnSensor(fn func(SensorUpdate))       { c.onSensor = fn }
func (c *CameraClient) OnStreaming(fn func(StreamingUpdate))  { c.onStreaming = fn }
func (c *CameraClient) OnSettings(fn func(*pb.Settings))      { c.onSettings = fn }
func (c *CameraClient) OnControl(fn func(*pb.Control))        { c.onControl = fn }

func (c *CameraClient) Start() {
	c.wg.Add(1)
	go c.connectLoop()
}

func (c *CameraClient) Stop() {
	close(c.done)
	c.connMu.Lock()
	if c.conn != nil {
		c.conn.Close()
	}
	c.connMu.Unlock()
	c.wg.Wait()
}

func (c *CameraClient) connectLoop() {
	defer c.wg.Done()

	attempt := 0
	for {
		select {
		case <-c.done:
			return
		default:
		}

		err := c.connect()
		if err != nil {
			log.Printf("[camera:%s] connection error: %v", c.cameraUID, err)
		}

		select {
		case <-c.done:
			return
		case <-time.After(backoff(attempt)):
			attempt++
		}
	}
}

func (c *CameraClient) connect() error {
	token, err := c.tokenMgr.GetAccessToken()
	if err != nil {
		return fmt.Errorf("get access token: %w", err)
	}

	url := fmt.Sprintf("wss://api.nanit.com/focus/cameras/%s/user_connect", c.cameraUID)
	header := http.Header{
		"Authorization": []string{"Bearer " + token},
	}

	dialer := websocket.Dialer{
		TLSClientConfig: &tls.Config{},
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

	errCh := make(chan error, 2)

	c.wg.Add(2)
	go c.readLoop(conn, errCh)
	go c.keepaliveLoop(conn, errCh)

	if err := c.requestSensorData(); err != nil {
		log.Printf("[camera:%s] failed to request sensor data: %v", c.cameraUID, err)
	}

	log.Printf("[camera:%s] sending PUT_STREAMING with RTMP URL: %s", c.cameraUID, c.rtmpURL)
	if err := c.startStreaming(); err != nil {
		log.Printf("[camera:%s] failed to start streaming: %v", c.cameraUID, err)
	} else {
		log.Printf("[camera:%s] PUT_STREAMING sent successfully", c.cameraUID)
	}

	select {
	case err := <-errCh:
		c.stopStreaming()
		conn.Close()
		return err
	case <-c.done:
		c.stopStreaming()
		conn.Close()
		return nil
	}
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

	case pb.RequestType_PUT_STREAMING:
		if c.onStreaming != nil && req.GetStreaming() != nil {
			c.onStreaming(StreamingUpdate{
				CameraUID: c.cameraUID,
				Streaming: req.GetStreaming(),
			})
		}

	default:
		log.Printf("[camera:%s] unhandled push request type: %v", c.cameraUID, req.GetType())
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

	default:
		log.Printf("[camera:%s] response for %v: status=%d %s",
			c.cameraUID, resp.GetRequestType(), resp.GetStatusCode(), resp.GetStatusMessage())
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
	return c.conn.WriteMessage(websocket.BinaryMessage, data)
}

func (c *CameraClient) startStreaming() error {
	return c.sendRequest(pb.RequestType_PUT_STREAMING, func(req *pb.Request) {
		started := pb.Streaming_STARTED
		mobile := pb.StreamIdentifier_MOBILE
		attempts := int32(1)
		req.Streaming = &pb.Streaming{
			Id:      &mobile,
			Status:  &started,
			RtmpUrl: &c.rtmpURL,
			Attempts: &attempts,
		}
	})
}

func (c *CameraClient) stopStreaming() {
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

func (c *CameraClient) requestSensorData() error {
	return c.sendRequest(pb.RequestType_GET_SENSOR_DATA, func(req *pb.Request) {
		all := true
		// Proto field name may be GetSensorData_ due to getter collision; adjust after codegen.
		req.GetSensorData = &pb.GetSensorData{All: &all}
	})
}

// backoff computes delay with exponential backoff + jitter, capped at maxBackoff.
func backoff(attempt int) time.Duration {
	if attempt == 0 {
		return 0
	}
	exp := math.Min(float64(maxBackoff), float64(baseBackoff)*math.Pow(2, float64(attempt-1)))
	jitter := rand.Float64() * 0.3 * exp
	return time.Duration(exp + jitter)
}
