package baby

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestStreamStateString(t *testing.T) {
	tests := []struct {
		in   StreamState
		want string
	}{
		{StreamStopped, "stopped"},
		{StreamStarting, "starting"},
		{StreamActive, "active"},
		{StreamUnhealthy, "unhealthy"},
		{StreamState(999), "unknown"},
	}

	for _, tc := range tests {
		if got := tc.in.String(); got != tc.want {
			t.Fatalf("String(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestNewState(t *testing.T) {
	s := NewState("baby-1", "cam-1", "Ava")
	if s.BabyUID != "baby-1" {
		t.Fatalf("BabyUID = %q", s.BabyUID)
	}
	if s.CameraUID != "cam-1" {
		t.Fatalf("CameraUID = %q", s.CameraUID)
	}
	if s.Name != "Ava" {
		t.Fatalf("Name = %q", s.Name)
	}
}

func TestStateUpdatesAndSubscribers(t *testing.T) {
	s := NewState("baby-1", "cam-1", "Ava")
	var calls atomic.Int32
	s.Subscribe(func() { calls.Add(1) })

	before := time.Now()
	s.UpdateSensors(func(ss *SensorState) {
		ss.Temperature = 22.5
		ss.Humidity = 55.1
	})
	if calls.Load() != 1 {
		t.Fatalf("subscriber calls after UpdateSensors = %d, want 1", calls.Load())
	}

	snap := s.Snapshot()
	if snap.Sensors.Temperature != 22.5 || snap.Sensors.Humidity != 55.1 {
		t.Fatalf("unexpected sensor snapshot: %+v", snap.Sensors)
	}
	if snap.Sensors.LastUpdate.Before(before) {
		t.Fatalf("LastUpdate %v before test start %v", snap.Sensors.LastUpdate, before)
	}

	s.UpdateControls(func(c *ControlState) {
		c.NightLight = true
		c.Volume = 20
	})
	s.SetStreamState(StreamActive)
	s.SetWSAlive(true)
	s.UpdateCameraInfo(func(ci *CameraInfo) {
		ci.FirmwareVersion = "1.2.3"
	})

	if calls.Load() != 5 {
		t.Fatalf("subscriber calls total = %d, want 5", calls.Load())
	}

	snap2 := s.Snapshot()
	if !snap2.Controls.NightLight || snap2.Controls.Volume != 20 {
		t.Fatalf("unexpected controls snapshot: %+v", snap2.Controls)
	}
	if snap2.Camera.FirmwareVersion != "1.2.3" {
		t.Fatalf("unexpected camera snapshot: %+v", snap2.Camera)
	}
	if snap2.Stream != StreamActive {
		t.Fatalf("stream = %v, want %v", snap2.Stream, StreamActive)
	}
	if !snap2.WSAlive {
		t.Fatalf("ws alive = false, want true")
	}
}

func TestStateSnapshotConcurrentAccess(t *testing.T) {
	s := NewState("baby-1", "cam-1", "Ava")
	s.Subscribe(func() {})

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			s.UpdateSensors(func(ss *SensorState) {
				ss.Temperature = float64(i)
			})
			s.UpdateControls(func(c *ControlState) {
				c.Volume = i
			})
		}(i)
	}

	for i := 0; i < 100; i++ {
		s.Snapshot()
	}
	wg.Wait()

	snap := s.Snapshot()
	if snap.Controls.Volume < 0 || snap.Controls.Volume > 9 {
		t.Fatalf("unexpected final volume: %d", snap.Controls.Volume)
	}
}

func TestAlertAutoClearAfterTTL(t *testing.T) {
	s := NewState("baby-1", "cam-1", "Ava")
	old := time.Now().Add(-AlertTTL - time.Second)
	s.UpdateSensors(func(ss *SensorState) {
		ss.CryDetected = true
		ss.CryDetectedAt = old
		ss.SoundAlert = true
		ss.SoundAlertAt = old
		ss.MotionAlert = true
		ss.MotionAlertAt = old
	})

	snap := s.Snapshot()
	if snap.Sensors.CryDetected {
		t.Fatalf("CryDetected should auto-clear after TTL")
	}
	if snap.Sensors.SoundAlert {
		t.Fatalf("SoundAlert should auto-clear after TTL")
	}
	if snap.Sensors.MotionAlert {
		t.Fatalf("MotionAlert should auto-clear after TTL")
	}
}
