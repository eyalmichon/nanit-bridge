package nanit

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"google.golang.org/protobuf/proto"

	pb "nanit-bridge/internal/nanit/nanitpb"
)

func startTestWSServer(t *testing.T, handler func(conn *websocket.Conn)) (*TokenManager, func()) {
	t.Helper()
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		handler(conn)
	}))
	restore := SetAPIBaseForTest(srv.URL)
	tm := NewTokenManager("a@b.com", "pw", t.TempDir()+"/session.json")
	tm.session = Session{
		AccessToken:  "test-token",
		RefreshToken: "test-refresh",
		ExpiresAt:    time.Now().Add(time.Hour),
	}
	return tm, func() {
		restore()
		srv.Close()
	}
}

func writePB(conn *websocket.Conn, mu *sync.Mutex, msg *pb.Message) error {
	b, err := proto.Marshal(msg)
	if err != nil {
		return err
	}
	mu.Lock()
	defer mu.Unlock()
	return conn.WriteMessage(websocket.BinaryMessage, b)
}

func TestWebRTCOfferAnswerCandidateAndClose(t *testing.T) {
	var (
		wsMu         sync.Mutex
		gotCandCh    = make(chan *pb.WebRtc_IceCandidate, 2)
		gotCloseCh   = make(chan int64, 2)
		gotStreamReq = make(chan *pb.Request, 4)
	)

	tm, cleanup := startTestWSServer(t, func(conn *websocket.Conn) {
		for {
			_, data, err := conn.ReadMessage()
			if err != nil {
				return
			}
			var m pb.Message
			if proto.Unmarshal(data, &m) != nil || m.GetRequest() == nil {
				continue
			}
			req := m.GetRequest()
			if req.GetType() == pb.RequestType_PUT_STREAMING {
				gotStreamReq <- req
				continue
			}
			if req.GetType() != pb.RequestType_PUT_WEBRTC_SERVICE_MESSAGE || req.GetWebrtc() == nil {
				continue
			}
			w := req.GetWebrtc()
			switch cmd := w.Command.(type) {
			case *pb.WebRtc_SessionDescription_:
				sid := cmd.SessionDescription.GetSessionId()
				// Send 200 ACK for offer
				_ = writePB(conn, &wsMu, &pb.Message{
					Type: pb.Message_RESPONSE.Enum(),
					Response: &pb.Response{
						RequestId:   proto.Int32(req.GetId()),
						RequestType: pb.RequestType_PUT_WEBRTC_SERVICE_MESSAGE.Enum(),
						StatusCode:  proto.Int32(200),
					},
				})
				// Send unrelated session message (relayed from another app) - must be ignored
				otherSID := sid + 999
				otherCand := "candidate:other"
				_ = writePB(conn, &wsMu, &pb.Message{
					Type: pb.Message_REQUEST.Enum(),
					Request: &pb.Request{
						Id:   proto.Int32(800),
						Type: pb.RequestType_PUT_WEBRTC_SERVICE_MESSAGE.Enum(),
						Webrtc: &pb.WebRtc{
							Command: &pb.WebRtc_IceCandidate_{
								IceCandidate: &pb.WebRtc_IceCandidate{
									SessionId: &otherSID,
									Sdp:       &otherCand,
								},
							},
						},
					},
				})
				// Send camera ANSWER
				ansType := pb.WebRtc_SessionDescription_ANSWER
				ansSDP := "v=0\r\nm=video 9 UDP/TLS/RTP/SAVPF 96\r\n"
				_ = writePB(conn, &wsMu, &pb.Message{
					Type: pb.Message_REQUEST.Enum(),
					Request: &pb.Request{
						Id:   proto.Int32(801),
						Type: pb.RequestType_PUT_WEBRTC_SERVICE_MESSAGE.Enum(),
						Webrtc: &pb.WebRtc{
							Command: &pb.WebRtc_SessionDescription_{
								SessionDescription: &pb.WebRtc_SessionDescription{
									SessionId:   &sid,
									SdpType:     &ansType,
									Description: &ansSDP,
								},
							},
						},
					},
				})
				// Send camera ICE candidate
				cand := "candidate:1 1 UDP 2122252543 192.168.50.86 1935 typ host"
				mid := "0"
				idx := int32(0)
				_ = writePB(conn, &wsMu, &pb.Message{
					Type: pb.Message_REQUEST.Enum(),
					Request: &pb.Request{
						Id:   proto.Int32(802),
						Type: pb.RequestType_PUT_WEBRTC_SERVICE_MESSAGE.Enum(),
						Webrtc: &pb.WebRtc{
							Command: &pb.WebRtc_IceCandidate_{
								IceCandidate: &pb.WebRtc_IceCandidate{
									SessionId:     &sid,
									SdpMid:        &mid,
									SdpMLineIndex: &idx,
									Sdp:           &cand,
								},
							},
						},
					},
				})

			case *pb.WebRtc_IceCandidate_:
				gotCandCh <- cmd.IceCandidate

			case *pb.WebRtc_Close_:
				gotCloseCh <- cmd.Close.GetSessionId()
			}
		}
	})
	defer cleanup()

	client := NewCameraClient("cam-1", "baby-1", tm, "rtmp://127.0.0.1:1935/tok/baby-1", 60)
	client.SetRTMPStreaming(false)
	connected := make(chan struct{}, 1)
	client.OnConnect(func() {
		select {
		case connected <- struct{}{}:
		default:
		}
	})
	client.Start()
	defer client.Stop()

	select {
	case <-connected:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for connect")
	}

	// Verify no PUT_STREAMING was sent when RTMP streaming is disabled
	select {
	case req := <-gotStreamReq:
		t.Fatalf("unexpected PUT_STREAMING when RTMP disabled: %v", req)
	case <-time.After(100 * time.Millisecond):
	}

	candCh := make(chan WebRTCCandidate, 4)
	const sid int64 = 123456789
	ans, err := client.SendWebRTCOffer(context.Background(), sid, "v=0\r\n", func(c WebRTCCandidate) {
		candCh <- c
	}, nil)
	if err != nil {
		t.Fatalf("SendWebRTCOffer error: %v", err)
	}
	if ans == "" {
		t.Fatal("expected non-empty SDP answer")
	}

	select {
	case c := <-candCh:
		if c.SessionID != sid || c.Candidate != "candidate:1 1 UDP 2122252543 192.168.50.86 1935 typ host" {
			t.Fatalf("unexpected camera candidate: %+v", c)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for camera ICE candidate")
	}

	mid := "0"
	idx := int32(0)
	if err := client.SendWebRTCCandidate(context.Background(), sid, "candidate:local", &mid, &idx); err != nil {
		t.Fatalf("SendWebRTCCandidate error: %v", err)
	}
	select {
	case ic := <-gotCandCh:
		if ic.GetSessionId() != sid || ic.GetSdp() != "candidate:local" {
			t.Fatalf("unexpected outbound candidate: %+v", ic)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("server did not receive local ICE candidate")
	}

	if err := client.SendWebRTCClose(sid); err != nil {
		t.Fatalf("SendWebRTCClose error: %v", err)
	}
	select {
	case closedSID := <-gotCloseCh:
		if closedSID != sid {
			t.Fatalf("closed session = %d, want %d", closedSID, sid)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("server did not receive WebRtc.Close")
	}
}

func TestWebRTCRefusedUnsupported(t *testing.T) {
	oldTimeout := webrtcOfferTimeout
	webrtcOfferTimeout = 250 * time.Millisecond
	defer func() { webrtcOfferTimeout = oldTimeout }()

	var wsMu sync.Mutex
	tm, cleanup := startTestWSServer(t, func(conn *websocket.Conn) {
		for {
			_, data, err := conn.ReadMessage()
			if err != nil {
				return
			}
			var m pb.Message
			if proto.Unmarshal(data, &m) != nil || m.GetRequest() == nil {
				continue
			}
			req := m.GetRequest()
			if req.GetType() == pb.RequestType_PUT_WEBRTC_SERVICE_MESSAGE {
				_ = writePB(conn, &wsMu, &pb.Message{
					Type: pb.Message_RESPONSE.Enum(),
					Response: &pb.Response{
						RequestId:     proto.Int32(req.GetId()),
						RequestType:   pb.RequestType_PUT_WEBRTC_SERVICE_MESSAGE.Enum(),
						StatusCode:    proto.Int32(501),
						StatusMessage: proto.String("Not Implemented"),
					},
				})
			}
		}
	})
	defer cleanup()

	client := NewCameraClient("cam-1", "baby-1", tm, "rtmp://127.0.0.1:1935/tok/baby-1", 60)
	client.SetRTMPStreaming(false)
	connected := make(chan struct{}, 1)
	client.OnConnect(func() {
		select {
		case connected <- struct{}{}:
		default:
		}
	})
	client.Start()
	defer client.Stop()
	<-connected

	_, err := client.SendWebRTCOffer(context.Background(), 42, "v=0\r\n", nil, nil)
	var refused *WebRTCRefusedError
	if !errors.As(err, &refused) {
		t.Fatalf("expected WebRTCRefusedError, got %v", err)
	}
	if !refused.IsUnsupported() {
		t.Fatalf("expected IsUnsupported()=true for 501, got false")
	}
}

func TestRTMPStopSendsURLOnShutdown(t *testing.T) {
	const wantURL = "rtmp://192.168.50.209:1935/testtoken/baby-1"
	gotStarted := make(chan struct{}, 1)
	gotStopped := make(chan string, 1)

	tm, cleanup := startTestWSServer(t, func(conn *websocket.Conn) {
		for {
			_, data, err := conn.ReadMessage()
			if err != nil {
				return
			}
			var m pb.Message
			if proto.Unmarshal(data, &m) != nil || m.GetRequest() == nil {
				continue
			}
			req := m.GetRequest()
			if req.GetType() == pb.RequestType_PUT_STREAMING && req.GetStreaming() != nil {
				switch req.GetStreaming().GetStatus() {
				case pb.Streaming_STARTED:
					select {
					case gotStarted <- struct{}{}:
					default:
					}
				case pb.Streaming_STOPPED:
					select {
					case gotStopped <- req.GetStreaming().GetRtmpUrl():
					default:
					}
				}
			}
		}
	})
	defer cleanup()

	client := NewCameraClient("cam-1", "baby-1", tm, wantURL, 60)
	client.Start()

	select {
	case <-gotStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for PUT_STREAMING STARTED")
	}

	client.Stop()

	select {
	case url := <-gotStopped:
		if url != wantURL {
			t.Fatalf("PUT_STREAMING STOPPED url = %q, want %q", url, wantURL)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for PUT_STREAMING STOPPED on Stop()")
	}
}
