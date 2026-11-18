package nanit

// Command confirmation (local fix, not upstream).
//
// Problem: a control command (e.g. night light ON) could be written to the
// cloud WebSocket and then silently have no effect, with nothing in the logs.
//
// What this does:
//   - every PUT_CONTROL / GET_CONTROL request is tracked until the camera replies;
//   - a night-light command is only "confirmed" once the camera itself reports
//     the requested light state (a reply alone is not enough);
//   - if that doesn't happen in time, the connection is re-established with a
//     fresh session and the command is re-sent ONCE, and only while it is still
//     recent (maxCommandAge). A newer light command always replaces an older one;
//   - a periodic GET_CONTROL probe notices a control channel that stopped answering;
//   - every outcome is logged.
//
// Only a COMMAND (something the user asked for: night light, light timer) may
// force a reconnect. Status questions and connection-setup requests that get no
// answer are only logged: a reconnect stops and restarts the video stream, and
// on a slow or lossy network those requests time out all the time, so
// reconnecting for them kept interrupting a video stream that was working.
// A connection that is really dead is still caught by the stale-connection
// timeout in client.go.

import (
	"fmt"
	"log"
	"time"

	"github.com/gorilla/websocket"
	"google.golang.org/protobuf/proto"

	pb "nanit-bridge/internal/nanit/nanitpb"
)

var (
	commandAckTimeout  = 12 * time.Second // reply (and, for the light, the new state) must arrive within this
	ackCheckInterval   = 1 * time.Second
	stateProbeAfter    = 5 * time.Second  // light not reported as changed by then: ask the camera for its state
	controlProbeEvery  = 5 * time.Minute  // idle health check of the control channel
	minAckReconnectGap = 2 * time.Minute  // never force reconnects more often than this
	maxCommandAge      = 45 * time.Second // a command older than this is never re-sent
	maxResends         = 1
)

type trackedRequest struct {
	reqType   pb.RequestType
	sent      time.Time              // when this attempt was written
	first     time.Time              // when the command was first sent
	attempts  int                    // re-sends so far
	command   bool                   // asked for by the user (may force a reconnect); false = status/setup request
	populate  func(*pb.Request)      // non-nil: may be re-sent after a reconnect
	wantLight *pb.Control_NightLight // non-nil: the camera must report this light state
	replied   bool
	status    int32
	statusMsg string
	probed    bool
}

func tracked(t pb.RequestType) bool {
	return t == pb.RequestType_PUT_CONTROL || t == pb.RequestType_GET_CONTROL
}

func (tr *trackedRequest) describe() string {
	if tr.wantLight != nil {
		if *tr.wantLight == pb.Control_LIGHT_ON {
			return "night light ON"
		}
		return "night light OFF"
	}
	return tr.reqType.String()
}

// sendAttempt marshals and writes a request. Tracked request types are
// registered BEFORE the write so a fast reply can never be missed.
// command=true marks something the user asked for: only those may force a
// reconnect and be re-sent (see the note at the top of this file).
func (c *CameraClient) sendAttempt(reqType pb.RequestType, populate func(*pb.Request), command bool, first time.Time, attempts int) error {
	id := c.requestID.Add(1)
	req := &pb.Request{
		Id:   &id,
		Type: reqType.Enum(),
	}
	if populate != nil {
		populate(req)
	}

	msg := &pb.Message{
		Type:    pb.Message_REQUEST.Enum(),
		Request: req,
	}

	data, err := proto.Marshal(msg)
	if err != nil {
		return fmt.Errorf("marshal request: %w", err)
	}

	if tracked(reqType) {
		now := time.Now()
		tr := &trackedRequest{reqType: reqType, sent: now, first: first, attempts: attempts, command: command}
		if tr.first.IsZero() {
			tr.first = now
		}
		if command {
			tr.populate = populate
		}
		if ctrl := req.GetControl(); reqType == pb.RequestType_PUT_CONTROL && ctrl != nil && ctrl.NightLight != nil {
			v := ctrl.GetNightLight()
			tr.wantLight = &v
		}

		c.pendingMu.Lock()
		if c.pending == nil {
			c.pending = map[int32]*trackedRequest{}
		}
		if tr.wantLight != nil {
			// Only the latest desired light state matters: drop older ones.
			for oid, o := range c.pending {
				if o.wantLight != nil {
					delete(c.pending, oid)
				}
			}
			kept := c.replay[:0]
			for _, o := range c.replay {
				if o.wantLight == nil {
					kept = append(kept, o)
				}
			}
			c.replay = kept
		}
		c.pending[id] = tr
		c.pendingMu.Unlock()
	}

	c.connMu.Lock()
	if c.conn == nil {
		c.connMu.Unlock()
		c.forgetPending(id)
		return fmt.Errorf("not connected")
	}
	_ = c.conn.SetWriteDeadline(time.Now().Add(writeTimeout))
	err = c.conn.WriteMessage(websocket.BinaryMessage, data)
	c.connMu.Unlock()
	if err != nil {
		c.forgetPending(id)
	}
	return err
}

func (c *CameraClient) forgetPending(id int32) {
	c.pendingMu.Lock()
	delete(c.pending, id)
	c.pendingMu.Unlock()
}

// observeControl records the light state reported by the camera and confirms
// any light command that was waiting for exactly that state.
func (c *CameraClient) observeControl(ctrl *pb.Control) {
	if ctrl == nil || ctrl.NightLight == nil {
		return
	}
	v := ctrl.GetNightLight()
	c.lightState.Store(int32(v) + 1) // 0 = unknown

	c.pendingMu.Lock()
	for id, tr := range c.pending {
		if tr.wantLight != nil && *tr.wantLight == v {
			delete(c.pending, id)
			log.Printf("[camera:%s] %s confirmed: camera reports the new state after %v",
				c.cameraUID, tr.describe(), time.Since(tr.sent).Round(10*time.Millisecond))
		}
	}
	c.pendingMu.Unlock()
}

// ackResponse notes the camera's reply to a tracked request.
func (c *CameraClient) ackResponse(resp *pb.Response) {
	if ctrl := resp.GetControl(); ctrl != nil {
		c.observeControl(ctrl)
	}

	done := false
	var what string
	var took time.Duration

	c.pendingMu.Lock()
	if tr, ok := c.pending[resp.GetRequestId()]; ok {
		tr.replied = true
		tr.status = resp.GetStatusCode()
		tr.statusMsg = resp.GetStatusMessage()
		if tr.status == 200 && tr.wantLight == nil {
			delete(c.pending, resp.GetRequestId())
			done = tr.reqType == pb.RequestType_PUT_CONTROL
			what, took = tr.describe(), time.Since(tr.sent)
		}
	}
	c.pendingMu.Unlock()

	if done {
		log.Printf("[camera:%s] %s acknowledged by camera in %v", c.cameraUID, what, took.Round(10*time.Millisecond))
	}
}

// carryOverPending is called when a connection ends: commands that were still
// waiting move to the re-send list (they are age-checked before re-sending).
func (c *CameraClient) carryOverPending() {
	c.pendingMu.Lock()
	for id, tr := range c.pending {
		if tr.populate != nil && tr.attempts < maxResends {
			c.replay = append(c.replay, tr)
		}
		delete(c.pending, id)
	}
	c.pendingMu.Unlock()
}

// resendUnconfirmed runs once per new connection, after the init requests.
func (c *CameraClient) resendUnconfirmed() {
	c.pendingMu.Lock()
	replay := c.replay
	c.replay = nil
	c.pendingMu.Unlock()

	for _, tr := range replay {
		if age := time.Since(tr.first); age > maxCommandAge {
			log.Printf("[camera:%s] not re-sending %s: the command is %v old", c.cameraUID, tr.describe(), age.Round(time.Second))
			continue
		}
		if tr.wantLight != nil {
			c.pendingMu.Lock()
			newer := false
			for _, o := range c.pending {
				if o.wantLight != nil {
					newer = true
				}
			}
			c.pendingMu.Unlock()
			if newer {
				log.Printf("[camera:%s] not re-sending %s: a newer light command was sent", c.cameraUID, tr.describe())
				continue
			}
		}
		log.Printf("[camera:%s] re-sending %s after reconnect (attempt %d)", c.cameraUID, tr.describe(), tr.attempts+2)
		if err := c.sendAttempt(tr.reqType, tr.populate, true, tr.first, tr.attempts+1); err != nil {
			log.Printf("[camera:%s] re-send of %s failed: %v", c.cameraUID, tr.describe(), err)
		}
	}
}

// checkPending evaluates all waiting requests. It returns true when the
// connection should be re-established.
func (c *CameraClient) checkPending() bool {
	now := time.Now()
	type failure struct {
		tr     *trackedRequest
		reason string
	}
	var failed []failure
	needProbe := false

	c.pendingMu.Lock()
	for id, tr := range c.pending {
		age := now.Sub(tr.sent)
		switch {
		case tr.replied && tr.status != 200:
			failed = append(failed, failure{tr, fmt.Sprintf("rejected by camera (status=%d %s)", tr.status, tr.statusMsg)})
			delete(c.pending, id)
		case age > commandAckTimeout:
			reason := "no reply from camera"
			if tr.replied {
				reason = "camera replied OK but never reported the new state"
			}
			failed = append(failed, failure{tr, reason})
			delete(c.pending, id)
		case tr.wantLight != nil && !tr.probed && age > stateProbeAfter:
			tr.probed = true
			needProbe = true
		}
	}

	// Only a failed COMMAND may force a reconnect; unanswered status/setup
	// requests never do (a reconnect interrupts the video stream).
	commandFailed := false
	for _, f := range failed {
		if f.tr.command {
			commandFailed = true
		}
	}
	reconnect := false
	if commandFailed {
		if now.Sub(c.lastAckReconnect) >= minAckReconnectGap {
			c.lastAckReconnect = now
			reconnect = true
		}
		for _, f := range failed {
			if reconnect && f.tr.command && f.tr.populate != nil && f.tr.attempts < maxResends {
				c.replay = append(c.replay, f.tr)
			}
		}
	}
	c.pendingMu.Unlock()

	if needProbe {
		if err := c.RequestControl(); err != nil {
			log.Printf("[camera:%s] state probe failed: %v", c.cameraUID, err)
		}
	}

	for _, f := range failed {
		var next string
		switch {
		case !f.tr.command:
			next = "status/setup request, not a command: keeping the connection (and the video stream) as is"
		case reconnect && f.tr.populate != nil && f.tr.attempts < maxResends:
			next = "reconnecting with a fresh session and re-sending it once"
		case reconnect:
			next = "reconnecting with a fresh session"
		default:
			next = "not reconnecting again (already forced a reconnect in the last " + minAckReconnectGap.String() + "); giving up on it"
		}
		log.Printf("[camera:%s] %s NOT confirmed after %v: %s; %s",
			c.cameraUID, f.tr.describe(), now.Sub(f.tr.sent).Round(100*time.Millisecond), f.reason, next)
	}
	return reconnect
}

// ackWatchLoop runs for the lifetime of one connection.
func (c *CameraClient) ackWatchLoop(connDone <-chan struct{}, errCh chan<- error) {
	defer c.wg.Done()

	check := time.NewTicker(ackCheckInterval)
	defer check.Stop()
	probe := time.NewTicker(controlProbeEvery)
	defer probe.Stop()

	for {
		select {
		case <-connDone:
			return
		case <-c.done:
			return
		case <-probe.C:
			if err := c.RequestControl(); err != nil {
				log.Printf("[camera:%s] control probe send failed: %v", c.cameraUID, err)
			}
		case <-check.C:
			if c.checkPending() {
				select {
				case errCh <- fmt.Errorf("camera did not confirm a request; reconnecting with a fresh session"):
				default:
				}
				return
			}
		}
	}
}
