package baby

import (
	"testing"
	"time"

	"nanit-bridge/internal/nanit"
)

func TestManagerHandlePushNotificationUpdatesState(t *testing.T) {
	st := NewState("baby-1", "cam-1", "Ava")
	m := &Manager{
		babies: map[string]*ManagedBaby{
			"baby-1": {State: st},
		},
	}

	m.handlePushNotification(nanit.PushNotification{BabyUID: "baby-1", Type: "SOUND"})
	m.handlePushNotification(nanit.PushNotification{BabyUID: "baby-1", Type: "MOTION"})
	m.handlePushNotification(nanit.PushNotification{BabyUID: "baby-1", Type: "CAMERA_CRY_DETECTION"})
	m.handlePushNotification(nanit.PushNotification{BabyUID: "baby-1", Type: "TEMPERATURE"})
	m.handlePushNotification(nanit.PushNotification{BabyUID: "baby-1", Type: "HUMIDITY"})

	snap := st.Snapshot()
	if !snap.Sensors.SoundAlert {
		t.Fatalf("SoundAlert = false, want true")
	}
	if !snap.Sensors.MotionAlert {
		t.Fatalf("MotionAlert = false, want true")
	}
	if !snap.Sensors.CryDetected {
		t.Fatalf("CryDetected = false, want true")
	}
	if time.Since(snap.Sensors.SoundAlertAt) > 5*time.Second {
		t.Fatalf("SoundAlertAt is too old: %v", snap.Sensors.SoundAlertAt)
	}
	if time.Since(snap.Sensors.MotionAlertAt) > 5*time.Second {
		t.Fatalf("MotionAlertAt is too old: %v", snap.Sensors.MotionAlertAt)
	}
	if time.Since(snap.Sensors.CryDetectedAt) > 5*time.Second {
		t.Fatalf("CryDetectedAt is too old: %v", snap.Sensors.CryDetectedAt)
	}
}

func TestManagerHandlePushNotificationUnknownBabyNoPanic(t *testing.T) {
	m := &Manager{
		babies: map[string]*ManagedBaby{},
	}
	m.handlePushNotification(nanit.PushNotification{BabyUID: "missing", Type: "SOUND"})
}
