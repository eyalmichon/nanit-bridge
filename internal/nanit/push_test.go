package nanit

import (
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"
)

func TestPushParseAndDispatch(t *testing.T) {
	p := NewPushReceiver(NewTokenManager("", "", filepath.Join(t.TempDir(), "session.json")), filepath.Join(t.TempDir(), "creds.json"))

	var mu sync.Mutex
	var got []PushNotification
	p.OnMessage(func(n PushNotification) {
		mu.Lock()
		got = append(got, n)
		mu.Unlock()
	})

	now := time.Now().Unix()
	p.parseAndDispatch([]byte(`{"type":"SOUND","baby_uid":"baby-1","time":` + itoa(now) + `,"id":1}`))

	mu.Lock()
	if len(got) != 1 {
		mu.Unlock()
		t.Fatalf("expected 1 notification, got %d", len(got))
	}
	if got[0].Type != "SOUND" || got[0].BabyUID != "baby-1" {
		mu.Unlock()
		t.Fatalf("unexpected notification: %+v", got[0])
	}
	mu.Unlock()
}

func TestPushParseAndDispatchWrapperPayload(t *testing.T) {
	p := NewPushReceiver(NewTokenManager("", "", filepath.Join(t.TempDir(), "session.json")), filepath.Join(t.TempDir(), "creds.json"))
	var called bool
	p.OnMessage(func(PushNotification) { called = true })

	now := time.Now().Unix()
	p.parseAndDispatch([]byte(`{"notification":{"type":"MOTION","baby_uid":"baby-2","time":` + itoa(now) + `,"id":22}}`))
	if !called {
		t.Fatalf("expected wrapped notification to dispatch")
	}
}

func TestPushParseAndDispatchStaleAndMalformed(t *testing.T) {
	p := NewPushReceiver(NewTokenManager("", "", filepath.Join(t.TempDir(), "session.json")), filepath.Join(t.TempDir(), "creds.json"))
	var called bool
	p.OnMessage(func(PushNotification) { called = true })

	old := time.Now().Add(-2 * time.Minute).Unix()
	p.parseAndDispatch([]byte(`{"type":"SOUND","baby_uid":"baby-1","time":` + itoa(old) + `,"id":1}`))
	if called {
		t.Fatalf("stale notification should not dispatch")
	}
	if p.staleCount != 1 {
		t.Fatalf("staleCount = %d, want 1", p.staleCount)
	}

	p.parseAndDispatch([]byte(`{"type":`))
	if called {
		t.Fatalf("malformed payload should not dispatch")
	}
}

func TestPushCredentialsSaveLoadRoundTrip(t *testing.T) {
	credsFile := filepath.Join(t.TempDir(), "push", "creds.json")
	p := NewPushReceiver(NewTokenManager("", "", filepath.Join(t.TempDir(), "session.json")), credsFile)

	in := &PushCredentials{
		FcmToken:      "fcm-token",
		GcmToken:      "gcm-token",
		AndroidId:     123,
		SecurityToken: 456,
		PrivateKey:    "pk",
		AuthSecret:    "as",
		NanitDeviceID: 789,
	}
	if err := p.saveCredentials(in); err != nil {
		t.Fatalf("saveCredentials error: %v", err)
	}

	out, err := p.loadCredentials()
	if err != nil {
		t.Fatalf("loadCredentials error: %v", err)
	}
	if out.FcmToken != in.FcmToken || out.AndroidId != in.AndroidId || out.NanitDeviceID != in.NanitDeviceID {
		t.Fatalf("roundtrip mismatch: in=%+v out=%+v", in, out)
	}
}

func TestPushReceiverStopBeforeListenLoopStarts(t *testing.T) {
	p := NewPushReceiver(NewTokenManager("", "", filepath.Join(t.TempDir(), "session.json")), filepath.Join(t.TempDir(), "creds.json"))
	p.mu.Lock()
	p.running = true
	p.stopCh = make(chan struct{})
	p.mu.Unlock()

	p.Stop()

	select {
	case <-p.stopCh:
	default:
		t.Fatalf("stopCh should be closed by Stop")
	}
}

func TestPushReceiverDoubleStopDoesNotPanic(t *testing.T) {
	p := NewPushReceiver(NewTokenManager("", "", filepath.Join(t.TempDir(), "session.json")), filepath.Join(t.TempDir(), "creds.json"))
	p.mu.Lock()
	p.running = true
	p.stopCh = make(chan struct{})
	p.mu.Unlock()

	p.Stop()
	p.Stop()
}

func itoa(v int64) string {
	return strconv.FormatInt(v, 10)
}
