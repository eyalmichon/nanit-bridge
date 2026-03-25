package api

import (
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/notedit/rtmp/format/flv"

	"nanit-bridge/internal/baby"
	rtmpserver "nanit-bridge/internal/rtmp"
)

//go:embed static/*
var staticFiles embed.FS

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

type Server struct {
	port       int
	manager    *baby.Manager
	rtmpServer *rtmpserver.Server

	mu      sync.Mutex
	clients map[*wsClient]struct{}
}

type wsClient struct {
	conn *websocket.Conn
	send chan []byte
}

func NewServer(port int, manager *baby.Manager, rtmpServer *rtmpserver.Server) *Server {
	return &Server{
		port:       port,
		manager:    manager,
		rtmpServer: rtmpServer,
		clients:    make(map[*wsClient]struct{}),
	}
}

func (s *Server) Start() error {
	mux := http.NewServeMux()

	mux.HandleFunc("/api/babies", s.handleBabies)
	mux.HandleFunc("/api/babies/", s.handleBabyOrControl)
	mux.HandleFunc("/api/stream/", s.handleStreamFLV)
	mux.HandleFunc("/ws", s.handleWS)

	staticFS, err := fs.Sub(staticFiles, "static")
	if err != nil {
		return fmt.Errorf("embed static: %w", err)
	}
	mux.Handle("/", http.FileServer(http.FS(staticFS)))

	addr := fmt.Sprintf(":%d", s.port)
	log.Printf("[api] dashboard at http://0.0.0.0%s", addr)

	go func() {
		srv := &http.Server{
			Addr:        addr,
			Handler:     mux,
			ReadTimeout: 10 * time.Second,
		}
		if err := srv.ListenAndServe(); err != nil {
			log.Printf("[api] http server error: %v", err)
		}
	}()

	return nil
}

// BroadcastState sends a state update to all connected WebSocket clients.
func (s *Server) BroadcastState(babyUID string, state *baby.State) {
	msg := s.buildBabyJSON(babyUID, state)
	data, err := json.Marshal(map[string]interface{}{
		"type": "state_update",
		"baby": msg,
	})
	if err != nil {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	for c := range s.clients {
		select {
		case c.send <- data:
		default:
			close(c.send)
			delete(s.clients, c)
		}
	}
}

func (s *Server) handleBabies(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	states := s.manager.AllStates()
	babies := make([]interface{}, 0, len(states))
	for uid, state := range states {
		babies = append(babies, s.buildBabyJSON(uid, state))
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"babies": babies,
	})
}

func (s *Server) handleBabyOrControl(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/babies/")

	if strings.Contains(path, "/control") {
		uid := strings.TrimSuffix(path, "/control")
		s.handleControl(w, r, uid)
		return
	}

	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	state := s.manager.GetState(path)
	if state == nil {
		http.Error(w, "baby not found", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(s.buildBabyJSON(path, state))
}

func (s *Server) handleControl(w http.ResponseWriter, r *http.Request, uid string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var body struct {
		Action string      `json:"action"`
		Value  interface{} `json:"value"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}

	var err error
	switch body.Action {
	case "night_light":
		on, ok := body.Value.(bool)
		if !ok {
			http.Error(w, "value must be boolean", http.StatusBadRequest)
			return
		}
		err = s.manager.SetNightLight(uid, on)

	case "night_light_timeout":
		v, ok := body.Value.(float64)
		if !ok {
			http.Error(w, "value must be number", http.StatusBadRequest)
			return
		}
		err = s.manager.SetNightLightTimeout(uid, int(v))

	case "playback":
		on, ok := body.Value.(bool)
		if !ok {
			http.Error(w, "value must be boolean", http.StatusBadRequest)
			return
		}
		err = s.manager.SetPlayback(uid, on)

	case "volume":
		v, ok := body.Value.(float64)
		if !ok {
			http.Error(w, "value must be number", http.StatusBadRequest)
			return
		}
		err = s.manager.SetVolume(uid, int(v))

	default:
		http.Error(w, "unknown action: "+body.Action, http.StatusBadRequest)
		return
	}

	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("[api] websocket upgrade: %v", err)
		return
	}

	client := &wsClient{
		conn: conn,
		send: make(chan []byte, 32),
	}

	s.mu.Lock()
	s.clients[client] = struct{}{}
	s.mu.Unlock()

	// Send initial state snapshot.
	states := s.manager.AllStates()
	babies := make([]interface{}, 0, len(states))
	for uid, state := range states {
		babies = append(babies, s.buildBabyJSON(uid, state))
	}
	initial, _ := json.Marshal(map[string]interface{}{
		"type":   "initial",
		"babies": babies,
	})
	client.send <- initial

	go s.wsWriter(client)
	s.wsReader(client)
}

func (s *Server) wsWriter(c *wsClient) {
	defer c.conn.Close()
	for msg := range c.send {
		if err := c.conn.WriteMessage(websocket.TextMessage, msg); err != nil {
			return
		}
	}
}

func (s *Server) wsReader(c *wsClient) {
	defer func() {
		s.mu.Lock()
		delete(s.clients, c)
		close(c.send)
		s.mu.Unlock()
		c.conn.Close()
	}()

	for {
		_, _, err := c.conn.ReadMessage()
		if err != nil {
			return
		}
	}
}

func (s *Server) handleStreamFLV(w http.ResponseWriter, r *http.Request) {
	uid := strings.TrimPrefix(r.URL.Path, "/api/stream/")
	if uid == "" {
		http.Error(w, "missing stream uid", http.StatusBadRequest)
		return
	}

	packets, unsub, ok := s.rtmpServer.Subscribe(uid)
	if !ok {
		http.Error(w, "stream not available", http.StatusNotFound)
		return
	}
	defer unsub()

	flusher, canFlush := w.(http.Flusher)

	w.Header().Set("Content-Type", "video/x-flv")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.WriteHeader(http.StatusOK)

	muxer := flv.NewMuxer(w)
	muxer.HasVideo = true
	muxer.HasAudio = true
	if err := muxer.WriteFileHeader(); err != nil {
		return
	}
	if canFlush {
		flusher.Flush()
	}

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case pkt, open := <-packets:
			if !open {
				return
			}
			if err := muxer.WritePacket(pkt); err != nil {
				return
			}
			if canFlush {
				flusher.Flush()
			}
		}
	}
}

func (s *Server) buildBabyJSON(uid string, state *baby.State) map[string]interface{} {
	sensors, controls, stream, wsAlive := state.Snapshot()
	return map[string]interface{}{
		"uid":         uid,
		"camera_uid":  state.CameraUID,
		"name":        state.Name,
		"ws_alive":    wsAlive,
		"stream":      stream.String(),
		"rtmp_active": s.rtmpServer.HasStream(uid),
		"sensors": map[string]interface{}{
			"temperature":  sensors.Temperature,
			"humidity":     sensors.Humidity,
			"light":        sensors.Light,
			"is_night":     sensors.IsNight,
			"sound_alert":  sensors.SoundAlert,
			"motion_alert": sensors.MotionAlert,
			"last_update":  sensors.LastUpdate.Format(time.RFC3339),
		},
		"controls": map[string]interface{}{
			"night_light":         controls.NightLight,
			"night_light_timeout": controls.NightLightTimeout,
			"volume":              controls.Volume,
			"playback":            controls.PlaybackActive,
		},
	}
}
