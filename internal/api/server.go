package api

import (
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
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

const logRingSize = 200

// LogBroadcaster is an io.Writer that buffers log lines in a ring buffer and
// broadcasts them to registered listener functions. It can be created before the
// Server exists and wired in later.
type LogBroadcaster struct {
	mu        sync.Mutex
	ring      []string
	buf       []byte
	listeners []func([]byte)
}

func NewLogBroadcaster() *LogBroadcaster {
	return &LogBroadcaster{}
}

func (lb *LogBroadcaster) Write(p []byte) (int, error) {
	n, err := os.Stderr.Write(p)

	lb.mu.Lock()
	defer lb.mu.Unlock()

	lb.buf = append(lb.buf, p...)
	for {
		idx := -1
		for i, b := range lb.buf {
			if b == '\n' {
				idx = i
				break
			}
		}
		if idx < 0 {
			break
		}
		line := string(lb.buf[:idx])
		lb.buf = lb.buf[idx+1:]
		if line == "" {
			continue
		}

		lb.ring = append(lb.ring, line)
		if len(lb.ring) > logRingSize {
			lb.ring = lb.ring[len(lb.ring)-logRingSize:]
		}

		data, _ := json.Marshal(map[string]string{"type": "log", "line": line})
		for _, fn := range lb.listeners {
			fn(data)
		}
	}

	return n, err
}

func (lb *LogBroadcaster) addListener(fn func([]byte)) {
	lb.mu.Lock()
	defer lb.mu.Unlock()
	lb.listeners = append(lb.listeners, fn)
}

func (lb *LogBroadcaster) snapshot() []string {
	lb.mu.Lock()
	defer lb.mu.Unlock()
	out := make([]string, len(lb.ring))
	copy(out, lb.ring)
	return out
}

type Server struct {
	port       int
	manager    *baby.Manager
	rtmpServer *rtmpserver.Server
	logBcast   *LogBroadcaster

	mu      sync.Mutex
	clients map[*wsClient]struct{}
}

type wsClient struct {
	conn   *websocket.Conn
	send   chan []byte
	closed bool
}

func NewServer(port int, manager *baby.Manager, rtmpServer *rtmpserver.Server, logBcast *LogBroadcaster) *Server {
	s := &Server{
		port:       port,
		manager:    manager,
		rtmpServer: rtmpServer,
		logBcast:   logBcast,
		clients:    make(map[*wsClient]struct{}),
	}

	logBcast.addListener(func(data []byte) {
		s.mu.Lock()
		defer s.mu.Unlock()
		for c := range s.clients {
			if c.closed {
				continue
			}
			select {
			case c.send <- data:
			default:
			}
		}
	})

	return s
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
	fileServer := http.FileServer(http.FS(staticFS))
	mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache, must-revalidate")
		fileServer.ServeHTTP(w, r)
	}))

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
		if c.closed {
			continue
		}
		select {
		case c.send <- data:
		default:
			c.closed = true
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

	if strings.Contains(path, "/notification_settings") {
		uid := strings.TrimSuffix(path, "/notification_settings")
		s.handleNotificationSettings(w, r, uid)
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

	case "night_light_brightness":
		v, ok := body.Value.(float64)
		if !ok {
			http.Error(w, "value must be number (0-100)", http.StatusBadRequest)
			return
		}
		err = s.manager.SetNightLightBrightness(uid, int(v))

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

	case "select_track":
		trackName, ok := body.Value.(string)
		if !ok || trackName == "" {
			http.Error(w, "value must be a track name string", http.StatusBadRequest)
			return
		}
		err = s.manager.SetPlaybackTrack(uid, trackName)

	case "sensor_poll":
		v, ok := body.Value.(float64)
		if !ok {
			http.Error(w, "value must be number (seconds)", http.StatusBadRequest)
			return
		}
		err = s.manager.SetSensorPollInterval(uid, int(v))

	case "sound_sensitivity":
		v, ok := body.Value.(float64)
		if !ok {
			http.Error(w, "value must be number (2-9)", http.StatusBadRequest)
			return
		}
		err = s.manager.SetSoundSensitivity(uid, int(v))

	case "motion_sensitivity":
		v, ok := body.Value.(float64)
		if !ok {
			http.Error(w, "value must be number (10000-250000)", http.StatusBadRequest)
			return
		}
		err = s.manager.SetMotionSensitivity(uid, int(v))

	case "breathing_monitoring":
		on, ok := body.Value.(bool)
		if !ok {
			http.Error(w, "value must be boolean", http.StatusBadRequest)
			return
		}
		if on {
			err = s.manager.StartBreathingMonitoring(uid)
		} else {
			err = s.manager.StopBreathingMonitoring(uid)
		}

	case "sleep_mode":
		on, ok := body.Value.(bool)
		if !ok {
			http.Error(w, "value must be boolean", http.StatusBadRequest)
			return
		}
		err = s.manager.SetSleepMode(uid, on)

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

func (s *Server) handleNotificationSettings(w http.ResponseWriter, r *http.Request, uid string) {
	switch r.Method {
	case http.MethodGet:
		settings, err := s.manager.GetNotificationSettings(uid)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{"settings": settings})

	case http.MethodPut:
		var body struct {
			Key     string `json:"key"`
			Enabled bool   `json:"enabled"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Key == "" {
			http.Error(w, "need {\"key\":\"SOUND\",\"enabled\":true}", http.StatusBadRequest)
			return
		}
		settings, err := s.manager.SetNotificationSetting(uid, body.Key, body.Enabled)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{"settings": settings})

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
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

	// Send buffered log lines.
	if lines := s.logBcast.snapshot(); len(lines) > 0 {
		logInit, _ := json.Marshal(map[string]interface{}{
			"type":  "log_init",
			"lines": lines,
		})
		client.send <- logInit
	}

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
		if !c.closed {
			c.closed = true
			close(c.send)
		}
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
	sensors, controls, camera, stream, wsAlive := state.Snapshot()
	return map[string]interface{}{
		"uid":         uid,
		"camera_uid":  state.CameraUID,
		"name":        state.Name,
		"ws_alive":    wsAlive,
		"stream":      stream.String(),
		"rtmp_active":    s.rtmpServer.HasStream(uid),
		"sensor_poll_sec": s.manager.GetSensorPollInterval(uid),
		"push_active":    s.manager.IsPushActive(),
		"sensors": map[string]interface{}{
			"temperature":      sensors.Temperature,
			"humidity":         sensors.Humidity,
			"light":            sensors.Light,
			"is_night":         sensors.IsNight,
			"cry_detected":     sensors.CryDetected,
			"cry_detected_at":  sensors.CryDetectedAt.Format(time.RFC3339),
			"sound_alert":      sensors.SoundAlert,
			"sound_alert_at":   sensors.SoundAlertAt.Format(time.RFC3339),
			"motion_alert":     sensors.MotionAlert,
			"motion_alert_at":  sensors.MotionAlertAt.Format(time.RFC3339),
			"last_update":      sensors.LastUpdate.Format(time.RFC3339),
		},
		"controls": map[string]interface{}{
			"night_light":            controls.NightLight,
			"night_light_brightness": controls.NightLightBrightness,
			"night_light_timeout":    controls.NightLightTimeout,
			"volume":                 controls.Volume,
			"playback":            controls.PlaybackActive,
			"current_track":       controls.CurrentTrack,
			"soundtracks":         controls.Soundtracks,
			"sound_sensitivity":   controls.SoundSensitivity,
			"motion_sensitivity":  controls.MotionSensitivity,
			"sleep_mode":          controls.SleepMode,
			"breathing": map[string]interface{}{
				"active":          controls.Breathing.Active,
				"calibrating":     controls.Breathing.Calibrating,
				"breaths_per_min": controls.Breathing.BreathsPerMin,
			},
		},
		"camera": map[string]interface{}{
			"firmware_version": camera.FirmwareVersion,
			"hardware_version": camera.HardwareVersion,
			"mounting_mode":    camera.MountingMode,
		},
	}
}
