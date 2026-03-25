package baby

import (
	"fmt"
	"log"
	"sync"

	"nanit-bridge/internal/nanit"
	pb "nanit-bridge/internal/nanit/nanitpb"
)

type Manager struct {
	mu       sync.Mutex
	babies   map[string]*ManagedBaby
	tokenMgr *nanit.TokenManager
	rtmpAddr string

	onStateChange func(babyUID string, state *State)
}

type ManagedBaby struct {
	State  *State
	client *nanit.CameraClient
}

func NewManager(tokenMgr *nanit.TokenManager, rtmpAddr string) *Manager {
	return &Manager{
		babies:   make(map[string]*ManagedBaby),
		tokenMgr: tokenMgr,
		rtmpAddr: rtmpAddr,
	}
}

func (m *Manager) OnStateChange(fn func(string, *State)) {
	m.onStateChange = fn
}

func (m *Manager) Start() error {
	babies, err := m.tokenMgr.FetchBabies()
	if err != nil {
		session := m.tokenMgr.GetSession()
		if len(session.Babies) > 0 {
			log.Printf("[manager] using cached baby list (%d babies)", len(session.Babies))
			babies = session.Babies
		} else {
			return fmt.Errorf("fetch babies: %w", err)
		}
	}

	log.Printf("[manager] found %d baby/camera(s)", len(babies))

	for _, b := range babies {
		m.startBaby(b)
	}

	return nil
}

func (m *Manager) Stop() {
	m.mu.Lock()
	defer m.mu.Unlock()

	for uid, mb := range m.babies {
		log.Printf("[manager] stopping baby %s", uid)
		mb.client.Stop()
	}
}

func (m *Manager) GetState(babyUID string) *State {
	m.mu.Lock()
	defer m.mu.Unlock()
	if mb, ok := m.babies[babyUID]; ok {
		return mb.State
	}
	return nil
}

func (m *Manager) SetNightLight(babyUID string, on bool) error {
	m.mu.Lock()
	mb, ok := m.babies[babyUID]
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("baby %q not found", babyUID)
	}
	return mb.client.SetNightLight(on)
}

func (m *Manager) SetNightLightTimeout(babyUID string, seconds int) error {
	m.mu.Lock()
	mb, ok := m.babies[babyUID]
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("baby %q not found", babyUID)
	}
	return mb.client.SetNightLightTimeout(seconds)
}

func (m *Manager) SetPlayback(babyUID string, on bool) error {
	m.mu.Lock()
	mb, ok := m.babies[babyUID]
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("baby %q not found", babyUID)
	}
	return mb.client.SetPlayback(on)
}

func (m *Manager) SetVolume(babyUID string, level int) error {
	m.mu.Lock()
	mb, ok := m.babies[babyUID]
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("baby %q not found", babyUID)
	}
	return mb.client.SetVolume(level)
}

func (m *Manager) AllStates() map[string]*State {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := make(map[string]*State, len(m.babies))
	for uid, mb := range m.babies {
		result[uid] = mb.State
	}
	return result
}

func (m *Manager) startBaby(b nanit.Baby) {
	rtmpURL := fmt.Sprintf("rtmp://%s/local/%s", m.rtmpAddr, b.UID)

	state := NewState(b.UID, b.CameraUID, b.Name)
	client := nanit.NewCameraClient(b.CameraUID, b.UID, m.tokenMgr, rtmpURL)

	client.OnSensor(func(update nanit.SensorUpdate) {
		state.UpdateSensors(func(s *SensorState) {
			for _, sd := range update.Data {
				applySensorData(s, sd)
			}
		})
	})

	client.OnStreaming(func(update nanit.StreamingUpdate) {
		switch update.Streaming.GetStatus() {
		case pb.Streaming_STARTED:
			state.SetStreamState(StreamActive)
		case pb.Streaming_STOPPED:
			state.SetStreamState(StreamStopped)
		case pb.Streaming_PAUSED:
			state.SetStreamState(StreamUnhealthy)
		}
	})

	client.OnPlayback(func(active bool) {
		state.UpdateControls(func(c *ControlState) {
			c.PlaybackActive = active
		})
	})

	client.OnControl(func(ctrl *pb.Control) {
		state.UpdateControls(func(c *ControlState) {
			if ctrl.NightLight != nil {
				c.NightLight = ctrl.GetNightLight() == pb.Control_LIGHT_ON
			}
			if ctrl.NightLightTimeout != nil {
				c.NightLightTimeout = int(ctrl.GetNightLightTimeout())
			}
		})
	})

	client.OnSettings(func(settings *pb.Settings) {
		state.UpdateControls(func(c *ControlState) {
			if settings.Volume != nil {
				c.Volume = int(settings.GetVolume())
			}
		})
	})

	if m.onStateChange != nil {
		state.Subscribe(func() {
			m.onStateChange(b.UID, state)
		})
	}

	m.mu.Lock()
	m.babies[b.UID] = &ManagedBaby{
		State:  state,
		client: client,
	}
	m.mu.Unlock()

	log.Printf("[manager] starting baby %s (camera: %s, name: %s)", b.UID, b.CameraUID, b.Name)
	client.Start()
	state.SetWSAlive(true)
}

func applySensorData(s *SensorState, sd *pb.SensorData) {
	if sd == nil {
		return
	}

	switch sd.GetSensorType() {
	case pb.SensorType_TEMPERATURE:
		if sd.ValueMilli != nil {
			s.Temperature = float64(sd.GetValueMilli()) / 1000.0
		} else if sd.Value != nil {
			s.Temperature = float64(sd.GetValue())
		}

	case pb.SensorType_HUMIDITY:
		if sd.ValueMilli != nil {
			s.Humidity = float64(sd.GetValueMilli()) / 1000.0
		} else if sd.Value != nil {
			s.Humidity = float64(sd.GetValue())
		}

	case pb.SensorType_LIGHT:
		if sd.ValueMilli != nil {
			s.Light = float64(sd.GetValueMilli()) / 1000.0
		} else if sd.Value != nil {
			s.Light = float64(sd.GetValue())
		}

	case pb.SensorType_NIGHT:
		s.IsNight = sd.GetValue() == 1

	case pb.SensorType_SOUND:
		s.SoundAlert = sd.GetIsAlert()

	case pb.SensorType_MOTION:
		s.MotionAlert = sd.GetIsAlert()
	}
}
