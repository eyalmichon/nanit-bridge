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
	NightVision          bool
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
	subs := append([]func(){}, s.subscribers...)
	s.mu.Unlock()

	for _, sub := range subs {
		sub()
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

func (s *State) Snapshot() (SensorState, ControlState, CameraInfo, StreamState, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.Sensors, s.Controls, s.Camera, s.Stream, s.WSAlive
}
