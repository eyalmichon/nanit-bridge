package baby

import (
	"fmt"
	"log"
	"sync"
	"time"

	"nanit-bridge/internal/nanit"
	pb "nanit-bridge/internal/nanit/nanitpb"
)

type Manager struct {
	mu            sync.Mutex
	babies        map[string]*ManagedBaby
	tokenMgr      *nanit.TokenManager
	rtmpAddr      string
	sensorPollSec int
	stopCh        chan struct{}
	pushReceiver  *nanit.PushReceiver

	onStateChange func(babyUID string, state *State)
}

type ManagedBaby struct {
	State  *State
	client *nanit.CameraClient
}

func NewManager(tokenMgr *nanit.TokenManager, rtmpAddr string, sensorPollSec int, pushCredsFile string) *Manager {
	m := &Manager{
		babies:        make(map[string]*ManagedBaby),
		tokenMgr:      tokenMgr,
		rtmpAddr:      rtmpAddr,
		sensorPollSec: sensorPollSec,
		stopCh:        make(chan struct{}),
	}

	if pushCredsFile != "" {
		m.pushReceiver = nanit.NewPushReceiver(tokenMgr, pushCredsFile)
		m.pushReceiver.OnMessage(m.handlePushNotification)
	}

	return m
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

	if m.pushReceiver != nil {
		if err := m.pushReceiver.Start(); err != nil {
			log.Printf("[manager] FCM push receiver failed, falling back to REST polling: %v", err)
			go m.messagePollLoop()
		} else {
			log.Printf("[manager] using FCM push notifications for instant alerts")
		}
	} else {
		go m.messagePollLoop()
	}

	return nil
}

func (m *Manager) Stop() {
	close(m.stopCh)

	if m.pushReceiver != nil {
		m.pushReceiver.Stop()
	}

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

func (m *Manager) SetPlaybackTrack(babyUID string, trackName string) error {
	m.mu.Lock()
	mb, ok := m.babies[babyUID]
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("baby %q not found", babyUID)
	}
	return mb.client.SetPlaybackTrack(true, trackName)
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

func (m *Manager) SetSensorPollInterval(babyUID string, seconds int) error {
	m.mu.Lock()
	mb, ok := m.babies[babyUID]
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("baby %q not found", babyUID)
	}
	mb.client.SetSensorPollInterval(seconds)
	return nil
}

func (m *Manager) GetSensorPollInterval(babyUID string) int {
	m.mu.Lock()
	mb, ok := m.babies[babyUID]
	m.mu.Unlock()
	if !ok {
		return 30
	}
	return mb.client.GetSensorPollInterval()
}

func (m *Manager) IsPushActive() bool {
	return m.pushReceiver != nil
}

func (m *Manager) GetNotificationSettings(babyUID string) (nanit.NotificationSettings, error) {
	return m.tokenMgr.GetNotificationSettings(babyUID)
}

func (m *Manager) SetNotificationSetting(babyUID, key string, enabled bool) (nanit.NotificationSettings, error) {
	updates := nanit.NotificationSettings{key: enabled}
	return m.tokenMgr.PutNotificationSettings(babyUID, updates)
}

func (m *Manager) SetSoundSensitivity(babyUID string, value int) error {
	m.mu.Lock()
	mb, ok := m.babies[babyUID]
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("baby %q not found", babyUID)
	}
	return mb.client.SetSoundSensitivity(int32(value))
}

func (m *Manager) SetMotionSensitivity(babyUID string, value int) error {
	m.mu.Lock()
	mb, ok := m.babies[babyUID]
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("baby %q not found", babyUID)
	}
	return mb.client.SetMotionSensitivity(int32(value))
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
	client := nanit.NewCameraClient(b.CameraUID, b.UID, m.tokenMgr, rtmpURL, m.sensorPollSec)

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

	client.OnPlaybackState(func(playback *pb.Playback) {
		state.UpdateControls(func(c *ControlState) {
			c.PlaybackActive = playback.GetStatus() == pb.Playback_STARTED
			if playback.GetCurrentTrack() != nil {
				c.CurrentTrack = playback.GetCurrentTrack().GetName()
			} else if !c.PlaybackActive {
				c.CurrentTrack = ""
			}
			if len(playback.GetSoundtracks()) > 0 {
				c.Soundtracks = make([]SoundtrackInfo, len(playback.GetSoundtracks()))
				for i, t := range playback.GetSoundtracks() {
					c.Soundtracks[i] = SoundtrackInfo{
						Name:     t.GetName(),
						Category: int(t.GetCategory()),
					}
				}
			}
		})
	})

	client.OnSoundtracks(func(tracks []*pb.Soundtrack) {
		state.UpdateControls(func(c *ControlState) {
			c.Soundtracks = make([]SoundtrackInfo, len(tracks))
			for i, t := range tracks {
				c.Soundtracks[i] = SoundtrackInfo{
					Name:     t.GetName(),
					Category: int(t.GetCategory()),
				}
			}
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
			for _, s := range settings.GetSensors() {
				switch s.GetSensorType() {
				case pb.SensorType_SOUND:
					if s.HighThreshold != nil {
						c.SoundSensitivity = int(s.GetHighThreshold())
					}
				case pb.SensorType_MOTION:
					if s.HighThreshold != nil {
						c.MotionSensitivity = int(s.GetHighThreshold())
					}
				}
			}
		})
	})

	client.OnConnect(func() {
		state.SetWSAlive(true)
	})
	client.OnDisconnect(func() {
		state.SetWSAlive(false)
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
}

func (m *Manager) handlePushNotification(notif nanit.PushNotification) {
	m.mu.Lock()
	mb, ok := m.babies[notif.BabyUID]
	m.mu.Unlock()
	if !ok {
		log.Printf("[manager] push notification for unknown baby %s (type: %s)", notif.BabyUID, notif.Type)
		return
	}

	now := time.Now()

	switch notif.Type {
	case "SOUND":
		mb.State.UpdateSensors(func(s *SensorState) {
			s.SoundAlert = true
			s.SoundAlertAt = now
		})
		log.Printf("[manager] PUSH: SOUND alert for %s", notif.BabyUID)
	case "MOTION":
		mb.State.UpdateSensors(func(s *SensorState) {
			s.MotionAlert = true
			s.MotionAlertAt = now
		})
		log.Printf("[manager] PUSH: MOTION alert for %s", notif.BabyUID)
	case "CAMERA_CRY_DETECTION":
		mb.State.UpdateSensors(func(s *SensorState) {
			s.CryDetected = true
			s.CryDetectedAt = now
		})
		log.Printf("[manager] PUSH: CRY alert for %s", notif.BabyUID)
	case "TEMPERATURE":
		log.Printf("[manager] PUSH: temperature alert for %s", notif.BabyUID)
	case "HUMIDITY":
		log.Printf("[manager] PUSH: humidity alert for %s", notif.BabyUID)
	default:
		log.Printf("[manager] PUSH: %s notification for %s", notif.Type, notif.BabyUID)
	}
}

const messagePollInterval = 15 * time.Second

func (m *Manager) messagePollLoop() {
	lastSeenID := make(map[string]int64)

	// Initialize with current latest message IDs to avoid alerting on old messages.
	m.mu.Lock()
	for uid := range m.babies {
		msgs, err := m.tokenMgr.FetchMessages(uid, 1)
		if err == nil && len(msgs) > 0 {
			lastSeenID[uid] = msgs[0].ID
		}
	}
	m.mu.Unlock()

	ticker := time.NewTicker(messagePollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-m.stopCh:
			return
		case <-ticker.C:
			m.mu.Lock()
			babyUIDs := make([]string, 0, len(m.babies))
			for uid := range m.babies {
				babyUIDs = append(babyUIDs, uid)
			}
			m.mu.Unlock()

			for _, uid := range babyUIDs {
				msgs, err := m.tokenMgr.FetchMessages(uid, 5)
				if err != nil {
					log.Printf("[manager] message poll error for %s: %v", uid, err)
					continue
				}

				prevID := lastSeenID[uid]
				var newMsgs []nanit.AlertMessage
				for _, msg := range msgs {
					if msg.ID > prevID {
						newMsgs = append(newMsgs, msg)
					}
				}

				if len(msgs) > 0 {
					lastSeenID[uid] = msgs[0].ID
				}

				if len(newMsgs) == 0 {
					continue
				}

				m.mu.Lock()
				mb, ok := m.babies[uid]
				m.mu.Unlock()
				if !ok {
					continue
				}

				mb.State.UpdateSensors(func(s *SensorState) {
					for _, msg := range newMsgs {
						switch msg.Type {
						case "SOUND":
							s.SoundAlert = true
							s.SoundAlertAt = time.Unix(msg.Time, 0)
							log.Printf("[manager] cloud SOUND alert for %s at %v", uid, s.SoundAlertAt.Format(time.Kitchen))
						case "MOTION":
							s.MotionAlert = true
							s.MotionAlertAt = time.Unix(msg.Time, 0)
							log.Printf("[manager] cloud MOTION alert for %s at %v", uid, s.MotionAlertAt.Format(time.Kitchen))
						}
					}
				})
			}
		}
	}
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

	case pb.SensorType_CRY:
		if sd.GetIsAlert() {
			s.CryDetected = true
			s.CryDetectedAt = time.Now()
		}
	}
}
