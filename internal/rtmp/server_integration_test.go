package rtmp

import (
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/notedit/rtmp/format"
)

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatalf("listen :0: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func waitFor(t *testing.T, timeout time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", msg)
}

func TestServerPublishSubscribeIntegration(t *testing.T) {
	port := freePort(t)
	const token = "integrationtoken"
	s := NewServer(port, token)

	disconnectCh := make(chan string, 1)
	s.OnPublisherDisconnect(func(key string) {
		disconnectCh <- key
	})

	if err := s.Start(); err != nil {
		t.Fatalf("Start() error: %v", err)
	}

	const streamKey = "it-stream"
	if s.HasStream(streamKey) {
		t.Fatalf("HasStream(%q) = true before publish", streamKey)
	}
	if _, _, ok := s.Subscribe(streamKey); ok {
		t.Fatalf("Subscribe(%q) unexpectedly succeeded before publish", streamKey)
	}

	url := "rtmp://127.0.0.1:" + itoa(port) + "/" + token + "/" + streamKey
	opener := &format.URLOpener{}

	// Real RTMP publisher connection.
	writer, err := opener.Create(url)
	if err != nil {
		t.Fatalf("Create(%q): %v", url, err)
	}
	defer writer.Close()

	// The RTMP handshake alone registers the broadcaster. Wait for it.
	waitFor(t, 2*time.Second, func() bool {
		return s.HasStream(streamKey)
	}, "stream to become active after publisher connect")

	// Subscribe while publisher is connected -- verifies Subscribe returns ok.
	packets, unsubscribe, ok := s.Subscribe(streamKey)
	if !ok {
		t.Fatalf("Subscribe(%q) failed while publisher is connected", streamKey)
	}
	defer unsubscribe()
	if packets == nil {
		t.Fatalf("expected non-nil subscriber packet channel")
	}

	// NOTE: The notedit/rtmp library's WritePacket/ReadPacket encodes through FLV
	// framing, which requires valid codec data to round-trip correctly. Synthetic
	// test packets don't survive this pipeline. Packet-level fan-out and keyframe
	// gating are already covered by the broadcaster unit tests
	// (TestBroadcasterReplayConfigsToNewSubscriber, TestBroadcasterKeyframeGate, etc.).

	// Disconnect publisher and verify stream teardown behavior.
	if err := writer.Close(); err != nil {
		t.Fatalf("publisher close: %v", err)
	}

	select {
	case got := <-disconnectCh:
		if got != streamKey {
			t.Fatalf("disconnect key = %q, want %q", got, streamKey)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("expected OnPublisherDisconnect callback")
	}

	waitFor(t, 2*time.Second, func() bool {
		return !s.HasStream(streamKey)
	}, "stream teardown")

	select {
	case _, ok := <-packets:
		if ok {
			t.Fatalf("expected subscriber channel to close after publisher disconnect")
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("subscriber channel did not close after publisher disconnect")
	}
}

func TestServerRejectsWrongToken(t *testing.T) {
	port := freePort(t)
	s := NewServer(port, "correct-token")
	if err := s.Start(); err != nil {
		t.Fatalf("Start() error: %v", err)
	}
	defer s.Stop()

	url := "rtmp://127.0.0.1:" + itoa(port) + "/wrong-token/mystream"
	opener := &format.URLOpener{}
	_, err := opener.Create(url)
	if err == nil {
		t.Fatalf("expected connection with wrong token to fail, but it succeeded")
	}
}

func TestServerRejectsNoToken(t *testing.T) {
	port := freePort(t)
	s := NewServer(port, "correct-token")
	if err := s.Start(); err != nil {
		t.Fatalf("Start() error: %v", err)
	}
	defer s.Stop()

	url := "rtmp://127.0.0.1:" + itoa(port) + "/mystream"
	opener := &format.URLOpener{}
	_, err := opener.Create(url)
	if err == nil {
		t.Fatalf("expected connection without token to fail, but it succeeded")
	}
}

func TestServerStopTerminatesAcceptGoroutine(t *testing.T) {
	port := freePort(t)
	s := NewServer(port, "tok")
	if err := s.Start(); err != nil {
		t.Fatalf("Start() error: %v", err)
	}

	// Verify listener is accepting.
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), time.Second)
	if err != nil {
		t.Fatalf("dial before stop: %v", err)
	}
	conn.Close()

	s.Stop()

	// After Stop, the port should refuse connections promptly (no lingering goroutine).
	// Give a brief moment for the OS to release the socket.
	time.Sleep(50 * time.Millisecond)
	conn, err = net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 500*time.Millisecond)
	if err == nil {
		conn.Close()
		t.Fatalf("expected dial after stop to fail, but it connected")
	}
}

func TestServerDoubleStopDoesNotPanic(t *testing.T) {
	port := freePort(t)
	s := NewServer(port, "tok")
	if err := s.Start(); err != nil {
		t.Fatalf("Start() error: %v", err)
	}

	// Both calls must complete without panic.
	s.Stop()
	s.Stop()
}

func TestServerStopBeforeStart(t *testing.T) {
	s := NewServer(0, "tok")
	// Stop on a never-started server must not panic.
	s.Stop()
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + (v % 10))
		v /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
