package baby

import (
	"sync"
	"time"
)

type SensorState struct {
	Temperature float64
	Humidity    float64
	Light       float64
	IsNight     bool
	SoundAlert  bool
	MotionAlert bool
	LastUpdate  time.Time
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

func (s *State) Snapshot() (SensorState, StreamState, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.Sensors, s.Stream, s.WSAlive
}
