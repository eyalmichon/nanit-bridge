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

	sensors, _, _, _, _ := s.Snapshot()
	if sensors.Temperature != 22.5 || sensors.Humidity != 55.1 {
		t.Fatalf("unexpected sensor snapshot: %+v", sensors)
	}
	if sensors.LastUpdate.Before(before) {
		t.Fatalf("LastUpdate %v before test start %v", sensors.LastUpdate, before)
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

	_, controls, camera, stream, ws := s.Snapshot()
	if !controls.NightLight || controls.Volume != 20 {
		t.Fatalf("unexpected controls snapshot: %+v", controls)
	}
	if camera.FirmwareVersion != "1.2.3" {
		t.Fatalf("unexpected camera snapshot: %+v", camera)
	}
	if stream != StreamActive {
		t.Fatalf("stream = %v, want %v", stream, StreamActive)
	}
	if !ws {
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
		_, _, _, _, _ = s.Snapshot()
	}
	wg.Wait()

	_, controls, _, _, _ := s.Snapshot()
	if controls.Volume < 0 || controls.Volume > 9 {
		t.Fatalf("unexpected final volume: %d", controls.Volume)
	}
}
