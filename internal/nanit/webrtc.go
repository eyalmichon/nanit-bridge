package nanit

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"google.golang.org/protobuf/proto"

	pb "nanit-bridge/internal/nanit/nanitpb"
)

var webrtcOfferTimeout = 5 * time.Second

// WebRTCCandidate represents a remote ICE candidate trickled from the camera.
type WebRTCCandidate struct {
	SessionID     int64
	Candidate     string
	SDPMid        *string
	SDPMLineIndex *int32
}

// WebRTCClosed represents a camera-initiated close for a WebRTC session.
type WebRTCClosed struct {
	SessionID int64
	Error     pb.WebRtc_Error
	Message   string
}

// WebRTCRefusedError is returned when the camera explicitly rejects a WebRTC
// offer with a non-200 status and sends no SDP answer.
type WebRTCRefusedError struct {
	StatusCode    int32
	StatusMessage string
}

func (e *WebRTCRefusedError) Error() string {
	if e.StatusMessage != "" {
		return fmt.Sprintf("camera refused WebRTC offer: status=%d %s", e.StatusCode, e.StatusMessage)
	}
	return fmt.Sprintf("camera refused WebRTC offer: status=%d", e.StatusCode)
}

// IsUnsupported reports whether the refusal indicates firmware does not
// support native WebRTC signaling (e.g. 501 Not Implemented or 404/400).
func (e *WebRTCRefusedError) IsUnsupported() bool {
	return e.StatusCode == 501 || e.StatusCode == 404 || e.StatusCode == 400
}

type webrtcSession struct {
	sessionID   int64
	offerReqID  int32
	answerCh    chan string
	closeCh     chan WebRTCClosed
	settledCh   chan struct{}
	settleOnce  sync.Once
	onCandidate func(WebRTCCandidate)
	onClose     func(WebRTCClosed)

	mu            sync.Mutex
	answered      bool
	closed        bool
	refusedStatus int32
	refusedMsg    string
}

func (s *webrtcSession) markSettled() {
	s.settleOnce.Do(func() { close(s.settledCh) })
}

// IsConnected reports whether the camera control WebSocket is currently open.
func (c *CameraClient) IsConnected() bool {
	c.connMu.Lock()
	ok := c.conn != nil
	c.connMu.Unlock()
	return ok
}

// CameraUID returns the camera identifier managed by this client.
func (c *CameraClient) CameraUID() string {
	return c.cameraUID
}

// SendWebRTCOffer sends a PUT_WEBRTC_SERVICE_MESSAGE OFFER and waits for the
// camera's SDP ANSWER. Remote ICE candidates and camera-initiated close events
// for this session are delivered to onCandidate and onClose.
func (c *CameraClient) SendWebRTCOffer(
	ctx context.Context,
	sessionID int64,
	offerSDP string,
	onCandidate func(WebRTCCandidate),
	onClose func(WebRTCClosed),
) (string, error) {
	if sessionID <= 0 {
		return "", fmt.Errorf("invalid WebRTC session id: %d", sessionID)
	}

	reqID := c.requestID.Add(1)
	sess := &webrtcSession{
		sessionID:   sessionID,
		offerReqID:  reqID,
		answerCh:    make(chan string, 1),
		closeCh:     make(chan WebRTCClosed, 1),
		settledCh:   make(chan struct{}),
		onCandidate: onCandidate,
		onClose:     onClose,
	}

	c.webrtcMu.Lock()
	if c.webrtcSessions == nil {
		c.webrtcSessions = make(map[int64]*webrtcSession)
	}
	if c.webrtcOffers == nil {
		c.webrtcOffers = make(map[int32]*webrtcSession)
	}
	if _, exists := c.webrtcSessions[sessionID]; exists {
		c.webrtcMu.Unlock()
		return "", fmt.Errorf("WebRTC session %d already active", sessionID)
	}
	c.webrtcSessions[sessionID] = sess
	c.webrtcOffers[reqID] = sess
	c.webrtcMu.Unlock()

	defer func() {
		c.webrtcMu.Lock()
		delete(c.webrtcOffers, reqID)
		c.webrtcMu.Unlock()
	}()

	offerType := pb.WebRtc_SessionDescription_OFFER
	err := c.writeWebRTCRequest(reqID, &pb.WebRtc{
		Command: &pb.WebRtc_SessionDescription_{
			SessionDescription: &pb.WebRtc_SessionDescription{
				SessionId:   &sessionID,
				SdpType:     &offerType,
				Description: &offerSDP,
			},
		},
	})
	if err != nil {
		c.forgetWebRTCSession(sessionID)
		sess.markSettled()
		return "", fmt.Errorf("send WebRTC offer: %w", err)
	}

	timer := time.NewTimer(webrtcOfferTimeout)
	defer timer.Stop()

	select {
	case ans := <-sess.answerCh:
		sess.markSettled()
		return ans, nil

	case cl := <-sess.closeCh:
		c.forgetWebRTCSession(sessionID)
		sess.markSettled()
		return "", fmt.Errorf("camera closed WebRTC session %d before answer (error=%v %s)", sessionID, cl.Error, cl.Message)

	case <-ctx.Done():
		c.forgetWebRTCSession(sessionID)
		sess.markSettled()
		return "", ctx.Err()

	case <-c.done:
		c.forgetWebRTCSession(sessionID)
		sess.markSettled()
		return "", fmt.Errorf("camera client stopped")

	case <-timer.C:
		c.forgetWebRTCSession(sessionID)
		sess.markSettled()
		sess.mu.Lock()
		code, msg := sess.refusedStatus, sess.refusedMsg
		sess.mu.Unlock()
		if code != 0 && code != 200 {
			return "", &WebRTCRefusedError{StatusCode: code, StatusMessage: msg}
		}
		return "", fmt.Errorf("timed out waiting for WebRTC answer from camera %s", c.cameraUID)
	}
}

// SendWebRTCCandidate sends a local ICE candidate to the camera for an active
// session. It waits until the offer has been accepted (or answered) so a
// candidate never overtakes the offer.
func (c *CameraClient) SendWebRTCCandidate(
	ctx context.Context,
	sessionID int64,
	candidate string,
	sdpMid *string,
	sdpMLineIndex *int32,
) error {
	if candidate == "" {
		return nil
	}

	c.webrtcMu.Lock()
	sess := c.webrtcSessions[sessionID]
	c.webrtcMu.Unlock()
	if sess == nil {
		return nil
	}

	select {
	case <-sess.settledCh:
	case <-ctx.Done():
		return ctx.Err()
	case <-c.done:
		return fmt.Errorf("camera client stopped")
	}

	c.webrtcMu.Lock()
	stillActive := c.webrtcSessions[sessionID] == sess
	c.webrtcMu.Unlock()
	if !stillActive {
		return nil
	}

	reqID := c.requestID.Add(1)
	return c.writeWebRTCRequest(reqID, &pb.WebRtc{
		Command: &pb.WebRtc_IceCandidate_{
			IceCandidate: &pb.WebRtc_IceCandidate{
				SessionId:     &sessionID,
				SdpMid:        sdpMid,
				SdpMLineIndex: sdpMLineIndex,
				Sdp:           &candidate,
			},
		},
	})
}

// SendWebRTCClose removes local tracking for sessionID and sends a best-effort
// WebRtc.Close message to the camera.
func (c *CameraClient) SendWebRTCClose(sessionID int64) error {
	if sess := c.forgetWebRTCSession(sessionID); sess != nil {
		sess.markSettled()
	}

	reqID := c.requestID.Add(1)
	return c.writeWebRTCRequest(reqID, &pb.WebRtc{
		Command: &pb.WebRtc_Close_{
			Close: &pb.WebRtc_Close{
				SessionId: &sessionID,
			},
		},
	})
}

func (c *CameraClient) writeWebRTCRequest(reqID int32, w *pb.WebRtc) error {
	reqType := pb.RequestType_PUT_WEBRTC_SERVICE_MESSAGE
	msg := &pb.Message{
		Type: pb.Message_REQUEST.Enum(),
		Request: &pb.Request{
			Id:     &reqID,
			Type:   &reqType,
			Webrtc: w,
		},
	}
	data, err := proto.Marshal(msg)
	if err != nil {
		return fmt.Errorf("marshal WebRTC request: %w", err)
	}

	c.connMu.Lock()
	if c.conn == nil {
		c.connMu.Unlock()
		return fmt.Errorf("not connected")
	}
	_ = c.conn.SetWriteDeadline(time.Now().Add(writeTimeout))
	err = c.conn.WriteMessage(websocket.BinaryMessage, data)
	c.connMu.Unlock()
	return err
}

func (c *CameraClient) forgetWebRTCSession(sessionID int64) *webrtcSession {
	c.webrtcMu.Lock()
	sess := c.webrtcSessions[sessionID]
	delete(c.webrtcSessions, sessionID)
	c.webrtcMu.Unlock()
	return sess
}

func (c *CameraClient) cancelWebRTCSessions() {
	c.webrtcMu.Lock()
	sessions := make([]*webrtcSession, 0, len(c.webrtcSessions))
	for id, sess := range c.webrtcSessions {
		sessions = append(sessions, sess)
		delete(c.webrtcSessions, id)
	}
	for id := range c.webrtcOffers {
		delete(c.webrtcOffers, id)
	}
	c.webrtcMu.Unlock()

	for _, sess := range sessions {
		sess.markSettled()
	}
}

// handleWebRTCRequest routes camera-originated PUT_WEBRTC_SERVICE_MESSAGE
// requests by session_id. Messages for unknown sessions (relayed from other
// apps connected to the same camera) are ignored. Camera WebRTC requests are
// never acknowledged, matching the official Nanit app.
func (c *CameraClient) handleWebRTCRequest(w *pb.WebRtc) {
	if w == nil {
		return
	}

	switch cmd := w.Command.(type) {
	case *pb.WebRtc_SessionDescription_:
		sd := cmd.SessionDescription
		if sd == nil || sd.GetSdpType() != pb.WebRtc_SessionDescription_ANSWER {
			return
		}
		c.webrtcMu.Lock()
		sess := c.webrtcSessions[sd.GetSessionId()]
		c.webrtcMu.Unlock()
		if sess == nil {
			return
		}
		sess.mu.Lock()
		if sess.answered {
			sess.mu.Unlock()
			return
		}
		sess.answered = true
		sess.mu.Unlock()
		sess.markSettled()
		select {
		case sess.answerCh <- sd.GetDescription():
		default:
		}

	case *pb.WebRtc_IceCandidate_:
		ic := cmd.IceCandidate
		if ic == nil {
			return
		}
		c.webrtcMu.Lock()
		sess := c.webrtcSessions[ic.GetSessionId()]
		c.webrtcMu.Unlock()
		if sess == nil || sess.onCandidate == nil {
			return
		}
		ev := WebRTCCandidate{
			SessionID: ic.GetSessionId(),
			Candidate: ic.GetSdp(),
		}
		if ic.SdpMid != nil {
			m := ic.GetSdpMid()
			ev.SDPMid = &m
		}
		if ic.SdpMLineIndex != nil {
			idx := ic.GetSdpMLineIndex()
			ev.SDPMLineIndex = &idx
		}
		sess.onCandidate(ev)

	case *pb.WebRtc_Close_:
		cl := cmd.Close
		if cl == nil {
			return
		}
		sess := c.forgetWebRTCSession(cl.GetSessionId())
		if sess == nil {
			return
		}
		sess.markSettled()
		ev := WebRTCClosed{
			SessionID: cl.GetSessionId(),
			Error:     cl.GetError(),
			Message:   cl.GetErrorMessage(),
		}
		sess.mu.Lock()
		answered := sess.answered
		if sess.closed {
			sess.mu.Unlock()
			return
		}
		sess.closed = true
		sess.mu.Unlock()

		if !answered {
			select {
			case sess.closeCh <- ev:
			default:
			}
			return
		}
		if sess.onClose != nil {
			sess.onClose(ev)
		}
	}
}

// handleWebRTCResponse records the camera's response to an outbound
// PUT_WEBRTC_SERVICE_MESSAGE request. Because Nanit's relay copies other apps'
// responses onto every socket, a non-200 status only causes a refusal if no
// SDP answer arrives before the offer timeout.
func (c *CameraClient) handleWebRTCResponse(resp *pb.Response) {
	if resp == nil {
		return
	}
	c.webrtcMu.Lock()
	sess := c.webrtcOffers[resp.GetRequestId()]
	c.webrtcMu.Unlock()
	if sess == nil {
		return
	}
	code := resp.GetStatusCode()
	if code == 200 {
		sess.markSettled()
		return
	}
	sess.mu.Lock()
	sess.refusedStatus = code
	sess.refusedMsg = resp.GetStatusMessage()
	sess.mu.Unlock()
	log.Printf("[camera:%s] PUT_WEBRTC_SERVICE_MESSAGE ack status=%d %s (waiting for answer)",
		c.cameraUID, code, resp.GetStatusMessage())
}
