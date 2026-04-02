package baby

import (
	"sync"
	"time"
)

type SensorState struct {
	Temperature    float64
	Humidity       float64
	Light          float64
	IsNight        bool
	CryDetected    bool
	CryDetectedAt  time.Time
	SoundAlert     bool
	SoundAlertAt   time.Time
	MotionAlert    bool
	MotionAlertAt  time.Time
	LastUpdate     time.Time
}

type SoundtrackInfo struct {
	Name     string `json:"name"`
	Category int    `json:"category"`
}

type BreathingState struct {
	Active         bool
	Calibrating    bool
	BreathsPerMin  int
	BreathingScore float32
}

type CameraInfo struct {
	FirmwareVersion   string
	HardwareVersion   string
	MountingMode      string
}

type ControlState struct {
	NightLight           bool
	NightLightBrightness int
	NightLightTimeout    int
	Volume               int
	PlaybackActive       bool
	CurrentTrack         string
	Soundtracks          []SoundtrackInfo
	SoundSensitivity     int
	MotionSensitivity    int
	Breathing            BreathingState
	SleepMode            bool
	NightVision          int32
	StatusLight          bool
	MicMute              bool
}

type StreamState int

const (
	StreamStopped StreamState = iota
	StreamStarting
	StreamActive
	StreamUnhealthy
)

const AlertTTL = 30 * time.Second

func (s StreamState) String() string {
	switch s {
	case StreamStopped:
		return "stopped"
	case StreamStarting:
		return "starting"
	case StreamActive:
		return "active"
	case StreamUnhealthy:
		return "unhealthy"
	default:
		return "unknown"
	}
}

type State struct {
	mu sync.RWMutex

	BabyUID    string
	CameraUID  string
	Name       string
	Sensors    SensorState
	Controls   ControlState
	Camera     CameraInfo
	Stream     StreamState
	WSAlive    bool

	subscribers []func()
	alertTimer  *time.Timer
}

func NewState(babyUID, cameraUID, name string) *State {
	return &State{
		BabyUID:   babyUID,
		CameraUID: cameraUID,
		Name:      name,
	}
}

func (s *State) Subscribe(fn func()) {
	s.mu.Lock()
	s.subscribers = append(s.subscribers, fn)
	s.mu.Unlock()
}

func (s *State) UpdateSensors(fn func(*SensorState)) {
	s.mu.Lock()
	fn(&s.Sensors)
	s.Sensors.LastUpdate = time.Now()
	hasAlert := s.Sensors.CryDetected || s.Sensors.SoundAlert || s.Sensors.MotionAlert
	subs := append([]func(){}, s.subscribers...)
	s.mu.Unlock()

	for _, sub := range subs {
		sub()
	}

	if hasAlert {
		s.scheduleAlertClear()
	}
}

func (s *State) SetStreamState(st StreamState) {
	s.mu.Lock()
	s.Stream = st
	subs := append([]func(){}, s.subscribers...)
	s.mu.Unlock()

	for _, sub := range subs {
		sub()
	}
}

func (s *State) IsWSAlive() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.WSAlive
}

func (s *State) SetWSAlive(alive bool) {
	s.mu.Lock()
	s.WSAlive = alive
	subs := append([]func(){}, s.subscribers...)
	s.mu.Unlock()

	for _, sub := range subs {
		sub()
	}
}

func (s *State) UpdateControls(fn func(*ControlState)) {
	s.mu.Lock()
	fn(&s.Controls)
	subs := append([]func(){}, s.subscribers...)
	s.mu.Unlock()

	for _, sub := range subs {
		sub()
	}
}

func (s *State) UpdateCameraInfo(fn func(*CameraInfo)) {
	s.mu.Lock()
	fn(&s.Camera)
	subs := append([]func(){}, s.subscribers...)
	s.mu.Unlock()

	for _, sub := range subs {
		sub()
	}
}

func (s *State) scheduleAlertClear() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.alertTimer != nil {
		s.alertTimer.Reset(AlertTTL + time.Second)
		return
	}
	s.alertTimer = time.AfterFunc(AlertTTL+time.Second, func() {
		s.mu.Lock()
		s.alertTimer = nil
		subs := append([]func(){}, s.subscribers...)
		s.mu.Unlock()
		for _, sub := range subs {
			sub()
		}
	})
}

func (s *State) Snapshot() (SensorState, ControlState, CameraInfo, StreamState, bool) {
	s.mu.RLock()
	now := time.Now()
	shouldClear := (s.Sensors.CryDetected && !s.Sensors.CryDetectedAt.IsZero() && now.Sub(s.Sensors.CryDetectedAt) > AlertTTL) ||
		(s.Sensors.SoundAlert && !s.Sensors.SoundAlertAt.IsZero() && now.Sub(s.Sensors.SoundAlertAt) > AlertTTL) ||
		(s.Sensors.MotionAlert && !s.Sensors.MotionAlertAt.IsZero() && now.Sub(s.Sensors.MotionAlertAt) > AlertTTL)
	if !shouldClear {
		defer s.mu.RUnlock()
		return s.Sensors, s.Controls, s.Camera, s.Stream, s.WSAlive
	}
	s.mu.RUnlock()

	s.mu.Lock()
	now = time.Now()
	if s.Sensors.CryDetected && !s.Sensors.CryDetectedAt.IsZero() && now.Sub(s.Sensors.CryDetectedAt) > AlertTTL {
		s.Sensors.CryDetected = false
	}
	if s.Sensors.SoundAlert && !s.Sensors.SoundAlertAt.IsZero() && now.Sub(s.Sensors.SoundAlertAt) > AlertTTL {
		s.Sensors.SoundAlert = false
	}
	if s.Sensors.MotionAlert && !s.Sensors.MotionAlertAt.IsZero() && now.Sub(s.Sensors.MotionAlertAt) > AlertTTL {
		s.Sensors.MotionAlert = false
	}
	sensors, controls, camera, stream, wsAlive := s.Sensors, s.Controls, s.Camera, s.Stream, s.WSAlive
	s.mu.Unlock()
	return sensors, controls, camera, stream, wsAlive
}
