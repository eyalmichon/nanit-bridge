package nanit

import (
	"sync"
	"testing"
	"time"

	pb "nanit-bridge/internal/nanit/nanitpb"
)

// fakeCamera sits behind the test WebSocket server and behaves like the
// Nanit cloud + camera for control requests. Behaviour is chosen per
// connection number (1 = first connection, 2 = after the first reconnect...).
type fakeCamera struct {
	cloud *wsCloud

	mu          sync.Mutex
	light       pb.Control_NightLight
	lightReqs   []bool // every night-light command received (true = ON)
	getControls int
	setupPuts   int // PUT_CONTROL requests that are not commands (e.g. "enable sensor push")
	timerPuts   int // night-light timer commands received
	pushID      int32

	replyToLight     func(conn int) bool  // send a reply to a light command
	lightStatus      func(conn int) int32 // status code of that reply
	applyLight       func(conn int) bool  // the light really changes
	pushOnChange     bool                 // camera announces the change by itself
	answerGetControl func(conn int) bool
	answerSetupPut   func(conn int) bool // reply to connection-setup PUT_CONTROL
	replyToTimer     func(conn int) bool // reply to a night-light timer command

	stop chan struct{}
}

func newFakeCamera(t *testing.T) *fakeCamera {
	t.Helper()
	f := &fakeCamera{
		cloud:            newWSCloud(t),
		replyToLight:     func(int) bool { return true },
		lightStatus:      func(int) int32 { return 200 },
		applyLight:       func(int) bool { return true },
		pushOnChange:     true,
		answerGetControl: func(int) bool { return true },
		answerSetupPut:   func(int) bool { return true },
		replyToTimer:     func(int) bool { return true },
		stop:             make(chan struct{}),
	}
	return f
}

func (f *fakeCamera) conns() int {
	f.cloud.mu.Lock()
	defer f.cloud.mu.Unlock()
	return len(f.cloud.connections)
}

func (f *fakeCamera) run() {
	for {
		select {
		case <-f.stop:
			return
		case msg := <-f.cloud.recvCh:
			if msg.GetType() != pb.Message_REQUEST {
				continue
			}
			f.handle(msg.GetRequest())
		}
	}
}

func (f *fakeCamera) handle(req *pb.Request) {
	conn := f.conns()
	id := req.GetId()
	switch req.GetType() {
	case pb.RequestType_GET_CONTROL:
		f.mu.Lock()
		f.getControls++
		state := f.light
		f.mu.Unlock()
		if f.answerGetControl(conn) {
			_ = f.cloud.send(&pb.Message{Type: pb.Message_RESPONSE.Enum(), Response: &pb.Response{
				RequestId: &id, RequestType: pb.RequestType_GET_CONTROL.Enum(), StatusCode: int32Ptr(200),
				Control: &pb.Control{NightLight: &state},
			}})
		}

	case pb.RequestType_PUT_CONTROL:
		ctrl := req.GetControl()
		if ctrl != nil && ctrl.NightLight == nil && ctrl.NightLightTimeout != nil {
			// night-light timer: a command
			f.mu.Lock()
			f.timerPuts++
			f.mu.Unlock()
			if f.replyToTimer(conn) {
				_ = f.cloud.send(&pb.Message{Type: pb.Message_RESPONSE.Enum(), Response: &pb.Response{
					RequestId: &id, RequestType: pb.RequestType_PUT_CONTROL.Enum(), StatusCode: int32Ptr(200),
				}})
			}
			return
		}
		if ctrl == nil || ctrl.NightLight == nil {
			// e.g. "enable sensor push" during connection setup
			f.mu.Lock()
			f.setupPuts++
			f.mu.Unlock()
			if f.answerSetupPut(conn) {
				_ = f.cloud.send(&pb.Message{Type: pb.Message_RESPONSE.Enum(), Response: &pb.Response{
					RequestId: &id, RequestType: pb.RequestType_PUT_CONTROL.Enum(), StatusCode: int32Ptr(200),
				}})
			}
			return
		}
		want := ctrl.GetNightLight()
		applied := f.applyLight(conn)
		f.mu.Lock()
		f.lightReqs = append(f.lightReqs, want == pb.Control_LIGHT_ON)
		if applied {
			f.light = want
		}
		f.pushID++
		pushID := 9000 + f.pushID
		f.mu.Unlock()

		if f.replyToLight(conn) {
			_ = f.cloud.send(&pb.Message{Type: pb.Message_RESPONSE.Enum(), Response: &pb.Response{
				RequestId: &id, RequestType: pb.RequestType_PUT_CONTROL.Enum(), StatusCode: int32Ptr(f.lightStatus(conn)),
			}})
		}
		if applied && f.pushOnChange {
			_ = f.cloud.send(&pb.Message{Type: pb.Message_REQUEST.Enum(), Request: &pb.Request{
				Id: &pushID, Type: pb.RequestType_PUT_CONTROL.Enum(), Control: &pb.Control{NightLight: &want},
			}})
		}
	}
}

func (f *fakeCamera) lightCommands() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.lightReqs)
}

func (f *fakeCamera) lightIsOn() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.light == pb.Control_LIGHT_ON
}

func (f *fakeCamera) getControlCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.getControls
}

func (f *fakeCamera) setupPutCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.setupPuts
}

func (f *fakeCamera) timerPutCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.timerPuts
}

type confirmTiming struct {
	ack, check, probeAfter, probeEvery, gap, maxAge time.Duration
}

var fastConfirm = confirmTiming{
	ack: 500 * time.Millisecond, check: 25 * time.Millisecond, probeAfter: 150 * time.Millisecond,
	probeEvery: time.Hour, gap: 10 * time.Second, maxAge: 5 * time.Second,
}

// startWithFakeCamera connects a real CameraClient to the fake camera.
func startWithFakeCamera(t *testing.T, f *fakeCamera, ct confirmTiming) *CameraClient {
	t.Helper()
	withTestAPIBase(t, f.cloud.srv.URL)
	setClientTimingForTest(t, 200*time.Millisecond, 100*time.Millisecond, 5*time.Second)

	old := confirmTiming{commandAckTimeout, ackCheckInterval, stateProbeAfter, controlProbeEvery, minAckReconnectGap, maxCommandAge}
	commandAckTimeout, ackCheckInterval, stateProbeAfter = ct.ack, ct.check, ct.probeAfter
	controlProbeEvery, minAckReconnectGap, maxCommandAge = ct.probeEvery, ct.gap, ct.maxAge
	t.Cleanup(func() {
		commandAckTimeout, ackCheckInterval, stateProbeAfter = old.ack, old.check, old.probeAfter
		controlProbeEvery, minAckReconnectGap, maxCommandAge = old.probeEvery, old.gap, old.maxAge
	})

	tm := NewTokenManager("u@example.com", "pw", t.TempDir()+"/session.json")
	tm.session.AccessToken = "token-1"
	tm.session.ExpiresAt = time.Now().Add(30 * time.Minute)

	client := NewCameraClient("cam-confirm", "baby-confirm", tm, "rtmp://127.0.0.1:1935/local/baby-confirm", 3600)
	connected := make(chan struct{}, 16)
	client.OnConnect(func() {
		select {
		case connected <- struct{}{}:
		default:
		}
	})

	go f.run()
	client.Start()
	// cleanups run last-in-first-out: stop the client first, then the fake camera, then restore timings
	t.Cleanup(func() { close(f.stop) })
	t.Cleanup(client.Stop)

	_ = await(t, connected, 3*time.Second, "first connection")
	// wait until the connection-setup requests were sent AND answered (or timed out).
	// Right after OnConnect nothing has been sent yet, so "nothing pending" alone is true too early.
	eventually(t, 2*time.Second, func() bool { return f.setupPutCount() >= 1 && pendingCount(client) == 0 }, "setup requests sent and settled")
	return client
}

func pendingCount(c *CameraClient) int {
	c.pendingMu.Lock()
	defer c.pendingMu.Unlock()
	return len(c.pending)
}

func eventually(t *testing.T, timeout time.Duration, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(15 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for: %s", what)
}

// stays checks that a condition keeps holding for a while (no late surprises).
func stays(t *testing.T, d time.Duration, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if !cond() {
			t.Fatalf("no longer true: %s", what)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// Normal case: the camera does what it's told. Nothing extra may happen.
func TestConfirm_LightCommandWorks(t *testing.T) {
	f := newFakeCamera(t)
	c := startWithFakeCamera(t, f, fastConfirm)

	if err := c.SetNightLight(true); err != nil {
		t.Fatal(err)
	}
	eventually(t, time.Second, func() bool { return f.lightIsOn() && pendingCount(c) == 0 }, "light on and confirmed")
	stays(t, 900*time.Millisecond, func() bool { return f.conns() == 1 && f.lightCommands() == 1 }, "one connection, one command")
}

// The camera sends no reply at all on the first connection (the suspected
// "silent" failure). Expect one reconnect, one re-send, and then it works.
func TestConfirm_NoReply_ReconnectsAndResendsOnce(t *testing.T) {
	f := newFakeCamera(t)
	f.replyToLight = func(conn int) bool { return conn >= 2 }
	f.applyLight = func(conn int) bool { return conn >= 2 }
	c := startWithFakeCamera(t, f, fastConfirm)

	if err := c.SetNightLight(true); err != nil {
		t.Fatal(err)
	}
	eventually(t, 3*time.Second, func() bool { return f.conns() == 2 }, "reconnect")
	eventually(t, 2*time.Second, func() bool { return f.lightIsOn() && pendingCount(c) == 0 }, "light on after re-send")
	stays(t, time.Second, func() bool { return f.conns() == 2 && f.lightCommands() == 2 }, "exactly one reconnect and one re-send")
}

// The camera replies "OK" but the light never changes (the case a plain
// reply check would miss). Expect a state probe, one reconnect, one re-send,
// and then no endless loop.
func TestConfirm_RepliedOKButNoEffect(t *testing.T) {
	f := newFakeCamera(t)
	f.applyLight = func(int) bool { return false }
	c := startWithFakeCamera(t, f, fastConfirm)
	probesBefore := f.getControlCount()

	if err := c.SetNightLight(true); err != nil {
		t.Fatal(err)
	}
	eventually(t, time.Second, func() bool { return f.getControlCount() > probesBefore }, "state probe sent")
	eventually(t, 3*time.Second, func() bool { return f.conns() == 2 && f.lightCommands() == 2 }, "reconnect and one re-send")
	stays(t, 1500*time.Millisecond, func() bool { return f.conns() == 2 && f.lightCommands() == 2 }, "no further reconnects or re-sends")
	if f.lightIsOn() {
		t.Fatal("light should still be off in this scenario")
	}
}

// The camera rejects the command (e.g. an expired session). Expect a
// reconnect and a successful re-send.
func TestConfirm_Rejected_ReconnectsAndResends(t *testing.T) {
	f := newFakeCamera(t)
	f.lightStatus = func(conn int) int32 {
		if conn == 1 {
			return 403
		}
		return 200
	}
	f.applyLight = func(conn int) bool { return conn >= 2 }
	c := startWithFakeCamera(t, f, fastConfirm)

	if err := c.SetNightLight(true); err != nil {
		t.Fatal(err)
	}
	eventually(t, 3*time.Second, func() bool { return f.conns() == 2 && f.lightIsOn() && pendingCount(c) == 0 }, "reconnect and light on")
	stays(t, 800*time.Millisecond, func() bool { return f.conns() == 2 && f.lightCommands() == 2 }, "one re-send only")
}

// Safety: a command that is no longer recent must never be re-sent, even
// though the connection is re-established.
func TestConfirm_OldCommandIsNeverResent(t *testing.T) {
	f := newFakeCamera(t)
	f.replyToLight = func(int) bool { return false }
	f.applyLight = func(int) bool { return false }
	ct := fastConfirm
	ct.maxAge = 100 * time.Millisecond // shorter than the confirmation timeout
	c := startWithFakeCamera(t, f, ct)

	if err := c.SetNightLight(true); err != nil {
		t.Fatal(err)
	}
	eventually(t, 3*time.Second, func() bool { return f.conns() == 2 }, "reconnect")
	stays(t, 1200*time.Millisecond, func() bool { return f.lightCommands() == 1 && !f.lightIsOn() }, "old command not re-sent")
}

// Safety: ON immediately followed by OFF. Only the latest wish counts; the
// earlier ON must not come back later.
func TestConfirm_NewerCommandReplacesOlder(t *testing.T) {
	f := newFakeCamera(t)
	f.pushOnChange = false // camera doesn't announce changes; state is only visible when asked
	c := startWithFakeCamera(t, f, fastConfirm)

	if err := c.SetNightLight(true); err != nil {
		t.Fatal(err)
	}
	if err := c.SetNightLight(false); err != nil {
		t.Fatal(err)
	}
	eventually(t, 2*time.Second, func() bool { return pendingCount(c) == 0 }, "OFF confirmed via state probe")
	stays(t, 1200*time.Millisecond, func() bool { return f.conns() == 1 && f.lightCommands() == 2 && !f.lightIsOn() }, "light stays off, no reconnect, no re-send")
}

// The camera stops answering status questions (as it does on a slow or lossy
// network). That must NOT cause a reconnect: a reconnect stops the video stream.
// Commands must keep working on the same connection.
func TestConfirm_UnansweredStatusRequestsNeverReconnect(t *testing.T) {
	f := newFakeCamera(t)
	f.answerGetControl = func(int) bool { return false }
	ct := fastConfirm
	ct.probeEvery = 150 * time.Millisecond
	ct.gap = 0 // even with no rate limit at all, status requests must not reconnect

	withTestAPIBase(t, f.cloud.srv.URL)
	setClientTimingForTest(t, 200*time.Millisecond, 100*time.Millisecond, 5*time.Second)
	old := confirmTiming{commandAckTimeout, ackCheckInterval, stateProbeAfter, controlProbeEvery, minAckReconnectGap, maxCommandAge}
	commandAckTimeout, ackCheckInterval, stateProbeAfter = ct.ack, ct.check, ct.probeAfter
	controlProbeEvery, minAckReconnectGap, maxCommandAge = ct.probeEvery, ct.gap, ct.maxAge
	t.Cleanup(func() {
		commandAckTimeout, ackCheckInterval, stateProbeAfter = old.ack, old.check, old.probeAfter
		controlProbeEvery, minAckReconnectGap, maxCommandAge = old.probeEvery, old.gap, old.maxAge
	})
	tm := NewTokenManager("u@example.com", "pw", t.TempDir()+"/session.json")
	tm.session.AccessToken = "token-1"
	tm.session.ExpiresAt = time.Now().Add(30 * time.Minute)
	c := NewCameraClient("cam-confirm", "baby-confirm", tm, "rtmp://127.0.0.1:1935/local/baby-confirm", 3600)
	go f.run()
	c.Start()
	t.Cleanup(func() { close(f.stop) })
	t.Cleanup(c.Stop)

	// the setup request and several periodic probes all go unanswered and time out
	eventually(t, 4*time.Second, func() bool { return f.getControlCount() >= 6 }, "several unanswered status requests")
	stays(t, 1200*time.Millisecond, func() bool { return f.conns() == 1 }, "no reconnect for unanswered status requests")
	if n := pendingCount(c); n > 5 {
		t.Fatalf("timed-out status requests are not being cleaned up: %d still waiting", n)
	}

	// a command still works, on the same connection
	if err := c.SetNightLight(true); err != nil {
		t.Fatal(err)
	}
	eventually(t, 2*time.Second, func() bool { return f.lightIsOn() }, "light on")
	stays(t, 800*time.Millisecond, func() bool { return f.conns() == 1 && f.lightCommands() == 1 }, "command confirmed without any reconnect or re-send")
}

// Connection setup (status question + "enable sensor push") gets no answer at
// all. Expect no reconnect and no re-send of the setup request.
func TestConfirm_UnansweredSetupNeverReconnects(t *testing.T) {
	f := newFakeCamera(t)
	f.answerGetControl = func(int) bool { return false }
	f.answerSetupPut = func(int) bool { return false }
	ct := fastConfirm
	ct.gap = 0
	c := startWithFakeCamera(t, f, ct) // returns once the unanswered setup requests have timed out

	stays(t, 1200*time.Millisecond, func() bool { return f.conns() == 1 && f.setupPutCount() == 1 }, "one connection, setup request sent once")
	if n := pendingCount(c); n != 0 {
		t.Fatalf("%d requests still waiting", n)
	}
}

// Status requests go unanswered AND a command goes unanswered. The command
// (and only the command) must still trigger one reconnect and one re-send.
func TestConfirm_CommandStillReconnectsWhenStatusRequestsFail(t *testing.T) {
	f := newFakeCamera(t)
	f.answerGetControl = func(int) bool { return false }
	f.answerSetupPut = func(int) bool { return false }
	f.replyToLight = func(conn int) bool { return conn >= 2 }
	f.applyLight = func(conn int) bool { return conn >= 2 }
	c := startWithFakeCamera(t, f, fastConfirm)

	if err := c.SetNightLight(true); err != nil {
		t.Fatal(err)
	}
	eventually(t, 3*time.Second, func() bool { return f.conns() == 2 }, "reconnect for the unconfirmed command")
	eventually(t, 2*time.Second, func() bool { return f.lightIsOn() }, "light on after re-send")
	stays(t, 1200*time.Millisecond, func() bool {
		// setup request: once per connection, never re-sent as if it were a command
		return f.conns() == 2 && f.lightCommands() == 2 && f.setupPutCount() == 2
	}, "exactly one reconnect, one re-send of the command, setup requests not re-sent")
}

// The night-light timer is something the user asked for too: unanswered, it
// gets one reconnect and one re-send.
func TestConfirm_LightTimerIsACommand(t *testing.T) {
	f := newFakeCamera(t)
	f.replyToTimer = func(conn int) bool { return conn >= 2 }
	c := startWithFakeCamera(t, f, fastConfirm)

	if err := c.SetNightLightTimeout(600); err != nil {
		t.Fatal(err)
	}
	eventually(t, 3*time.Second, func() bool { return f.conns() == 2 && f.timerPutCount() == 2 }, "reconnect and re-send of the timer command")
	eventually(t, 2*time.Second, func() bool { return pendingCount(c) == 0 }, "timer command confirmed")
	stays(t, 800*time.Millisecond, func() bool { return f.conns() == 2 && f.timerPutCount() == 2 }, "one reconnect and one re-send only")
}

// The connection drops while a command is still unconfirmed. Expect it to be
// re-sent on the new connection.
func TestConfirm_ConnectionDropWhileWaiting(t *testing.T) {
	f := newFakeCamera(t)
	f.replyToLight = func(conn int) bool { return conn >= 2 }
	f.applyLight = func(conn int) bool { return conn >= 2 }
	c := startWithFakeCamera(t, f, fastConfirm)

	if err := c.SetNightLight(true); err != nil {
		t.Fatal(err)
	}
	eventually(t, time.Second, func() bool { return f.lightCommands() == 1 }, "command reached the camera")
	f.cloud.closeLatest()

	eventually(t, 3*time.Second, func() bool { return f.conns() == 2 && f.lightIsOn() && pendingCount(c) == 0 }, "re-sent and confirmed on the new connection")
	stays(t, 800*time.Millisecond, func() bool { return f.conns() == 2 && f.lightCommands() == 2 }, "one re-send only")
}
