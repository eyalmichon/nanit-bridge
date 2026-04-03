package api

import (
	"context"
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
	"nanit-bridge/internal/config"
	"nanit-bridge/internal/nanit"
	rtmpserver "nanit-bridge/internal/rtmp"
)

//go:embed static/*
var staticFiles embed.FS

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

const (
	logRingSize           = 200
	httpReadHeaderTimeout = 10 * time.Second
)

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
	version    string
	manager    *baby.Manager
	rtmpServer *rtmpserver.Server
	logBcast   *LogBroadcaster
	auth       *authManager
	nanitAuth  *nanitAuthManager

	rtmpAddr      string
	rtmpTokenFile string

	mu      sync.Mutex
	clients map[*wsClient]struct{}
	httpSrv *http.Server
}

type wsClient struct {
	conn   *websocket.Conn
	send   chan []byte
	closed bool
}

type babiesResponse struct {
	Babies []babyJSON `json:"babies"`
}

type wsInitialMessage struct {
	Type   string     `json:"type"`
	Babies []babyJSON `json:"babies"`
}

type babyJSON struct {
	UID           string         `json:"uid"`
	CameraUID     string         `json:"camera_uid"`
	Name          string         `json:"name"`
	WSAlive       bool           `json:"ws_alive"`
	Stream        string         `json:"stream"`
	RTMPActive    bool           `json:"rtmp_active"`
	SensorPollSec int            `json:"sensor_poll_sec"`
	PushActive    bool           `json:"push_active"`
	Sensors       sensorJSON     `json:"sensors"`
	Controls      controlJSON    `json:"controls"`
	Camera        cameraInfoJSON `json:"camera"`
}

type sensorJSON struct {
	Temperature   float64 `json:"temperature"`
	Humidity      float64 `json:"humidity"`
	Light         float64 `json:"light"`
	IsNight       bool    `json:"is_night"`
	CryDetected   bool    `json:"cry_detected"`
	CryDetectedAt string  `json:"cry_detected_at"`
	SoundAlert    bool    `json:"sound_alert"`
	SoundAlertAt  string  `json:"sound_alert_at"`
	MotionAlert   bool    `json:"motion_alert"`
	MotionAlertAt string  `json:"motion_alert_at"`
	LastUpdate    string  `json:"last_update"`
}

type controlJSON struct {
	NightLight           bool                  `json:"night_light"`
	NightLightBrightness int                   `json:"night_light_brightness"`
	NightLightTimeout    int                   `json:"night_light_timeout"`
	Volume               int                   `json:"volume"`
	Playback             bool                  `json:"playback"`
	CurrentTrack         string                `json:"current_track"`
	Soundtracks          []baby.SoundtrackInfo `json:"soundtracks"`
	SoundSensitivity     int                   `json:"sound_sensitivity"`
	MotionSensitivity    int                   `json:"motion_sensitivity"`
	SleepMode            bool                  `json:"sleep_mode"`
	NightVision          int                   `json:"night_vision"`
	StatusLight          bool                  `json:"status_light"`
	MicMute              bool                  `json:"mic_mute"`
	Breathing            breathingControlJSON  `json:"breathing"`
}

type breathingControlJSON struct {
	Active        bool `json:"active"`
	Calibrating   bool `json:"calibrating"`
	BreathsPerMin int  `json:"breaths_per_min"`
}

type cameraInfoJSON struct {
	FirmwareVersion string `json:"firmware_version"`
	HardwareVersion string `json:"hardware_version"`
	MountingMode    string `json:"mounting_mode"`
}

func NewServer(
	port int,
	manager *baby.Manager,
	rtmpServer *rtmpserver.Server,
	logBcast *LogBroadcaster,
	authFile string,
	tokenMgr *nanit.TokenManager,
	onNanitAuth func() error,
	rtmpAddr string,
	rtmpTokenFile string,
	version string,
) *Server {
	s := &Server{
		port:          port,
		version:       version,
		manager:       manager,
		rtmpServer:    rtmpServer,
		logBcast:      logBcast,
		auth:          newAuthManager(authFile),
		nanitAuth:     newNanitAuthManager(tokenMgr, manager, onNanitAuth),
		clients:       make(map[*wsClient]struct{}),
		rtmpAddr:      rtmpAddr,
		rtmpTokenFile: rtmpTokenFile,
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

	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/api/version", s.handleVersion)
	mux.HandleFunc("/api/auth/setup", s.auth.handleSetup)
	mux.HandleFunc("/api/auth/login", s.auth.handleLogin)
	mux.HandleFunc("/api/auth/change-password", s.auth.handleChangePassword)
	mux.HandleFunc("/api/auth/logout", s.auth.handleLogout)
	mux.HandleFunc("/api/nanit/status", s.nanitAuth.handleStatus)
	mux.HandleFunc("/api/nanit/login", s.nanitAuth.handleLogin)
	mux.HandleFunc("/api/nanit/mfa", s.nanitAuth.handleMFA)
	mux.HandleFunc("/api/babies", s.handleBabies)
	mux.HandleFunc("/api/babies/", s.handleBabyOrControl)
	mux.HandleFunc("/api/stream/", s.handleStreamFLV)
	mux.HandleFunc("/api/rtmp/token", s.handleRTMPToken)
	mux.HandleFunc("/api/rtmp/token/regenerate", s.handleRTMPTokenRegenerate)
	mux.HandleFunc("/ws", s.handleWS)

	staticFS, err := fs.Sub(staticFiles, "static")
	if err != nil {
		return fmt.Errorf("embed static: %w", err)
	}
	fileServer := http.FileServer(http.FS(staticFS))
	mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache, must-revalidate")
		switch r.URL.Path {
		case "/", "/index.html":
			http.ServeFileFS(w, r, staticFS, "dashboard/index.html")
		case "/login", "/login/", "/login/index.html":
			http.ServeFileFS(w, r, staticFS, "login/index.html")
		case "/setup", "/setup/", "/setup/index.html":
			http.ServeFileFS(w, r, staticFS, "setup/index.html")
		case "/settings", "/settings/", "/settings/index.html":
			http.ServeFileFS(w, r, staticFS, "settings/index.html")
		default:
			fileServer.ServeHTTP(w, r)
		}
	}))

	handler := s.auth.authMiddleware(mux)

	addr := fmt.Sprintf(":%d", s.port)
	log.Printf("[api] dashboard at http://0.0.0.0%s", addr)

	srv := &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: httpReadHeaderTimeout,
	}
	s.mu.Lock()
	s.httpSrv = srv
	s.mu.Unlock()

	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("[api] http server error: %v", err)
		}
	}()

	return nil
}

func (s *Server) Stop(ctx context.Context) error {
	s.mu.Lock()
	srv := s.httpSrv
	s.httpSrv = nil
	s.mu.Unlock()
	if srv == nil {
		return nil
	}
	return srv.Shutdown(ctx)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"status":  "ok",
		"version": s.version,
	})
}

func (s *Server) handleVersion(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"version": s.version})
}

// SetPendingMFA stores an MFA token so the dashboard can complete the challenge
// without the user re-entering credentials.
func (s *Server) SetPendingMFA(token string) {
	s.nanitAuth.SetPendingMFA(token)
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
	babies := make([]babyJSON, 0, len(states))
	for uid, state := range states {
		babies = append(babies, s.buildBabyJSON(uid, state))
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(babiesResponse{Babies: babies})
}

func (s *Server) handleBabyOrControl(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/babies/")

	if strings.HasSuffix(path, "/control") {
		uid := strings.TrimSuffix(path, "/control")
		s.handleControl(w, r, uid)
		return
	}

	if strings.HasSuffix(path, "/notification_settings") {
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

	case "night_vision":
		f, ok := body.Value.(float64)
		if !ok {
			http.Error(w, "value must be number (0=off, 1=auto, 2=on)", http.StatusBadRequest)
			return
		}
		mode := int32(f)
		if mode < 0 || mode > 2 {
			http.Error(w, "value must be 0, 1, or 2", http.StatusBadRequest)
			return
		}
		err = s.manager.SetNightVision(uid, mode)

	case "status_light":
		on, ok := body.Value.(bool)
		if !ok {
			http.Error(w, "value must be boolean", http.StatusBadRequest)
			return
		}
		err = s.manager.SetStatusLight(uid, on)

	case "mic_mute":
		on, ok := body.Value.(bool)
		if !ok {
			http.Error(w, "value must be boolean", http.StatusBadRequest)
			return
		}
		err = s.manager.SetMicMute(uid, on)

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
	babies := make([]babyJSON, 0, len(states))
	for uid, state := range states {
		babies = append(babies, s.buildBabyJSON(uid, state))
	}
	initial, _ := json.Marshal(wsInitialMessage{
		Type:   "initial",
		Babies: babies,
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

func (s *Server) handleRTMPToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	token := s.rtmpServer.GetToken()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"token":        token,
		"url_template": fmt.Sprintf("rtmp://%s/{token}/{uid}", s.rtmpAddr),
	})
}

func (s *Server) handleRTMPTokenRegenerate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	newToken, err := config.GenerateRTMPToken()
	if err != nil {
		http.Error(w, "failed to generate token: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if err := config.WriteRTMPToken(s.rtmpTokenFile, newToken); err != nil {
		http.Error(w, "failed to write token: "+err.Error(), http.StatusInternalServerError)
		return
	}
	s.rtmpServer.SetToken(newToken)
	if err := s.manager.SetRTMPToken(newToken); err != nil {
		log.Printf("[api] manager restart after token regeneration failed: %v", err)
		http.Error(w, "token saved but manager restart failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	log.Printf("[api] RTMP token regenerated")
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"token":        newToken,
		"url_template": fmt.Sprintf("rtmp://%s/{token}/{uid}", s.rtmpAddr),
	})
}

func (s *Server) buildBabyJSON(uid string, state *baby.State) babyJSON {
	snap := state.Snapshot()
	return babyJSON{
		UID:           uid,
		CameraUID:     state.CameraUID,
		Name:          state.Name,
		WSAlive:       snap.WSAlive,
		Stream:        snap.Stream.String(),
		RTMPActive:    s.rtmpServer.HasStream(uid),
		SensorPollSec: s.manager.GetSensorPollInterval(uid),
		PushActive:    s.manager.IsPushActive(),
		Sensors: sensorJSON{
			Temperature:   snap.Sensors.Temperature,
			Humidity:      snap.Sensors.Humidity,
			Light:         snap.Sensors.Light,
			IsNight:       snap.Sensors.IsNight,
			CryDetected:   snap.Sensors.CryDetected,
			CryDetectedAt: snap.Sensors.CryDetectedAt.Format(time.RFC3339),
			SoundAlert:    snap.Sensors.SoundAlert,
			SoundAlertAt:  snap.Sensors.SoundAlertAt.Format(time.RFC3339),
			MotionAlert:   snap.Sensors.MotionAlert,
			MotionAlertAt: snap.Sensors.MotionAlertAt.Format(time.RFC3339),
			LastUpdate:    snap.Sensors.LastUpdate.Format(time.RFC3339),
		},
		Controls: controlJSON{
			NightLight:           snap.Controls.NightLight,
			NightLightBrightness: snap.Controls.NightLightBrightness,
			NightLightTimeout:    snap.Controls.NightLightTimeout,
			Volume:               snap.Controls.Volume,
			Playback:             snap.Controls.PlaybackActive,
			CurrentTrack:         snap.Controls.CurrentTrack,
			Soundtracks:          snap.Controls.Soundtracks,
			SoundSensitivity:     snap.Controls.SoundSensitivity,
			MotionSensitivity:    snap.Controls.MotionSensitivity,
			SleepMode:            snap.Controls.SleepMode,
			NightVision:          int(snap.Controls.NightVision),
			StatusLight:          snap.Controls.StatusLight,
			MicMute:              snap.Controls.MicMute,
			Breathing: breathingControlJSON{
				Active:        snap.Controls.Breathing.Active,
				Calibrating:   snap.Controls.Breathing.Calibrating,
				BreathsPerMin: snap.Controls.Breathing.BreathsPerMin,
			},
		},
		Camera: cameraInfoJSON{
			FirmwareVersion: snap.Camera.FirmwareVersion,
			HardwareVersion: snap.Camera.HardwareVersion,
			MountingMode:    snap.Camera.MountingMode,
		},
	}
}
