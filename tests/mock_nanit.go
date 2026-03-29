package tests

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"google.golang.org/protobuf/proto"

	pb "nanit-bridge/internal/nanit/nanitpb"
)

type mockNanitCloud struct {
	t testingT

	srv      *httptest.Server
	upgrader websocket.Upgrader

	mu       sync.Mutex
	conns    []*websocket.Conn
	recvCh   chan *pb.Message
	babyUID  string
	cameraUID string
}

// testingT is the subset used by this helper.
type testingT interface {
	Fatalf(format string, args ...interface{})
	Cleanup(func())
	Helper()
}

func newMockNanitCloud(t testingT) *mockNanitCloud {
	t.Helper()
	m := &mockNanitCloud{
		t:         t,
		recvCh:    make(chan *pb.Message, 1024),
		babyUID:   "baby-1",
		cameraUID: "cam-1",
		upgrader: websocket.Upgrader{
			CheckOrigin: func(r *http.Request) bool { return true },
		},
	}
	m.srv = httptest.NewServer(http.HandlerFunc(m.handle))
	t.Cleanup(m.srv.Close)
	return m
}

func (m *mockNanitCloud) URL() string {
	return m.srv.URL
}

func (m *mockNanitCloud) handle(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/login":
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"access_token":  "mock-access",
			"refresh_token": "mock-refresh",
			"expires_in":    3600,
		})
		return
	case r.Method == http.MethodPost && r.URL.Path == "/tokens/refresh":
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"access_token":  "mock-access-refreshed",
			"refresh_token": "mock-refresh",
			"expires_in":    3600,
		})
		return
	case r.Method == http.MethodGet && r.URL.Path == "/babies":
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"babies": []map[string]interface{}{
				{
					"uid":  m.babyUID,
					"name": "Test Baby",
					"camera": map[string]interface{}{
						"uid": m.cameraUID,
					},
				},
			},
		})
		return
	case r.Method == http.MethodGet && r.URL.Path == "/babies/"+m.babyUID+"/messages":
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"messages": []map[string]interface{}{},
		})
		return
	case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/user_connect"):
		m.handleWS(w, r)
		return
	default:
		http.NotFound(w, r)
		return
	}
}

func (m *mockNanitCloud) handleWS(w http.ResponseWriter, r *http.Request) {
	conn, err := m.upgrader.Upgrade(w, r, nil)
	if err != nil {
		m.t.Fatalf("ws upgrade: %v", err)
	}

	m.mu.Lock()
	m.conns = append(m.conns, conn)
	m.mu.Unlock()

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
			m.recvCh <- msg
		}
	}()
}

func (m *mockNanitCloud) send(msg *pb.Message) error {
	data, err := proto.Marshal(msg)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.conns) == 0 {
		return nil
	}
	return m.conns[len(m.conns)-1].WriteMessage(websocket.BinaryMessage, data)
}

func (m *mockNanitCloud) waitForOutbound(timeout time.Duration, match func(*pb.Message) bool) *pb.Message {
	deadline := time.After(timeout)
	for {
		select {
		case msg := <-m.recvCh:
			if match(msg) {
				return msg
			}
		case <-deadline:
			return nil
		}
	}
}

func writeJSON(w http.ResponseWriter, code int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
