package webrtc

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bluenviron/gortsplib/v5/pkg/format"
	"github.com/bluenviron/gortsplib/v5/pkg/format/rtph264"
	"github.com/notedit/rtmp/av"
	"github.com/pion/interceptor"
	"github.com/pion/rtcp"
	"github.com/pion/sdp/v3"
	"github.com/pion/webrtc/v4"

	"nanit-bridge/internal/nanit"
	"nanit-bridge/internal/rtmp"
	"nanit-bridge/internal/rtsp"
)

var (
	iceConnectTimeout  = 15 * time.Second
	iceDisconnectGrace = 5 * time.Second
	videoStallTimeout  = 15 * time.Second
	watchdogTick       = 1 * time.Second
	pliMinInterval     = 2 * time.Second
	healthySessionAge  = 30 * time.Second
	retryBackoffs      = []time.Duration{
		2 * time.Second,
		5 * time.Second,
		10 * time.Second,
		30 * time.Second,
	}
)

// Signaler is the subset of nanit.CameraClient used by WebRTC ingest.
type Signaler interface {
	CameraUID() string
	IsConnected() bool
	SendWebRTCOffer(
		ctx context.Context,
		sessionID int64,
		offerSDP string,
		onCandidate func(nanit.WebRTCCandidate),
		onClose func(nanit.WebRTCClosed),
	) (string, error)
	SendWebRTCCandidate(
		ctx context.Context,
		sessionID int64,
		candidate string,
		sdpMid *string,
		sdpMLineIndex *int32,
	) error
	SendWebRTCClose(sessionID int64) error
}

// Config configures a per-camera WebRTC media ingester.
type Config struct {
	BabyUID       string
	Signaler      Signaler
	RTSPServer    *rtsp.Server
	RTMPServer    *rtmp.Server
	OnStarting    func()
	OnActive      func()
	OnStopped     func()
	OnUnsupported func(err error)
}

// Ingest maintains a camera's native WebRTC session (H.264 + Opus over LAN UDP)
// and republishes it to the RTSP server (full H.264 + Opus) and internal RTMP
// broadcaster (H.264 video for BMM frame capture and dashboard HTTP-FLV).
type Ingest struct {
	cfg Config

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	wakeCh    chan struct{}
	sleepMode atomic.Bool

	sessionMu     sync.Mutex
	sessionCancel context.CancelFunc
}

// NewIngest creates a WebRTC ingest controller for one camera.
func NewIngest(cfg Config) *Ingest {
	ctx, cancel := context.WithCancel(context.Background())
	return &Ingest{
		cfg:    cfg,
		ctx:    ctx,
		cancel: cancel,
		wakeCh: make(chan struct{}, 1),
	}
}

// Start begins the background WebRTC session lifecycle loop.
func (in *Ingest) Start() {
	in.wg.Add(1)
	go in.runLoop()
}

// Stop terminates any active WebRTC session (sending WebRtc.Close over the
// still-open control socket) and waits for the ingest goroutines to exit.
func (in *Ingest) Stop() {
	in.cancel()
	in.cancelActiveSession()
	in.wg.Wait()
}

// NotifyConnected wakes the ingest loop when the camera control socket connects.
func (in *Ingest) NotifyConnected() {
	select {
	case in.wakeCh <- struct{}{}:
	default:
	}
}

// SetSleepMode pauses streaming while the camera is in sleep mode and resumes
// immediately when sleep mode turns off.
func (in *Ingest) SetSleepMode(sleeping bool) {
	prev := in.sleepMode.Swap(sleeping)
	if sleeping && !prev {
		log.Printf("[webrtc:%s] camera entered sleep mode, pausing stream", in.cfg.BabyUID)
		in.cancelActiveSession()
	} else if !sleeping && prev {
		log.Printf("[webrtc:%s] camera exited sleep mode, resuming stream", in.cfg.BabyUID)
		in.NotifyConnected()
	}
}

func (in *Ingest) cancelActiveSession() {
	in.sessionMu.Lock()
	cancel := in.sessionCancel
	in.sessionMu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (in *Ingest) runLoop() {
	defer in.wg.Done()

	backoffIdx := 0
	for {
		if in.ctx.Err() != nil {
			return
		}

		if in.sleepMode.Load() || !in.cfg.Signaler.IsConnected() {
			select {
			case <-in.ctx.Done():
				return
			case <-in.wakeCh:
			case <-time.After(500 * time.Millisecond):
			}
			continue
		}

		started := time.Now()
		err := in.runSession()
		if in.ctx.Err() != nil {
			return
		}

		if in.sleepMode.Load() {
			backoffIdx = 0
			continue
		}

		var refused *nanit.WebRTCRefusedError
		if errors.As(err, &refused) && refused.IsUnsupported() {
			log.Printf("[webrtc:%s] camera does not support native WebRTC (%v)", in.cfg.BabyUID, refused)
			if in.cfg.OnUnsupported != nil {
				in.cfg.OnUnsupported(refused)
			}
			return
		}

		if time.Since(started) >= healthySessionAge {
			backoffIdx = 0
		}
		wait := retryBackoffs[backoffIdx]
		if backoffIdx+1 < len(retryBackoffs) {
			backoffIdx++
		}

		if err != nil {
			log.Printf("[webrtc:%s] session ended (%v); retrying in %v", in.cfg.BabyUID, err, wait)
		} else {
			log.Printf("[webrtc:%s] session ended; retrying in %v", in.cfg.BabyUID, wait)
		}

		select {
		case <-in.ctx.Done():
			return
		case <-in.wakeCh:
			// Re-check immediately if woken by reconnect/unsleep after a brief pause.
			select {
			case <-in.ctx.Done():
				return
			case <-time.After(500 * time.Millisecond):
			}
		case <-time.After(wait):
		}
	}
}

func (in *Ingest) runSession() error {
	sessCtx, sessCancel := context.WithCancel(in.ctx)
	in.sessionMu.Lock()
	in.sessionCancel = sessCancel
	in.sessionMu.Unlock()
	defer func() {
		sessCancel()
		in.sessionMu.Lock()
		in.sessionCancel = nil
		in.sessionMu.Unlock()
	}()

	if in.cfg.OnStarting != nil {
		in.cfg.OnStarting()
	}

	pc, err := newPeerConnection()
	if err != nil {
		return fmt.Errorf("create peer connection: %w", err)
	}
	defer pc.Close()

	if _, err := pc.AddTransceiverFromKind(webrtc.RTPCodecTypeAudio, webrtc.RTPTransceiverInit{
		Direction: webrtc.RTPTransceiverDirectionRecvonly,
	}); err != nil {
		return fmt.Errorf("add audio transceiver: %w", err)
	}
	if _, err := pc.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo, webrtc.RTPTransceiverInit{
		Direction: webrtc.RTPTransceiverDirectionRecvonly,
	}); err != nil {
		return fmt.Errorf("add video transceiver: %w", err)
	}

	sessionID := randomSessionID()
	offerSent := false
	defer func() {
		if offerSent {
			if err := in.cfg.Signaler.SendWebRTCClose(sessionID); err != nil {
				log.Printf("[webrtc:%s] WebRtc.Close failed (session=%d): %v", in.cfg.BabyUID, sessionID, err)
			}
		}
		if in.cfg.OnStopped != nil {
			in.cfg.OnStopped()
		}
	}()

	var (
		candMu          sync.Mutex
		remoteDescSet   bool
		earlyCandidates []nanit.WebRTCCandidate
	)

	addRemoteCandidate := func(c nanit.WebRTCCandidate) {
		if c.Candidate == "" {
			return
		}
		init := webrtc.ICECandidateInit{Candidate: c.Candidate}
		if c.SDPMid != nil {
			m := *c.SDPMid
			init.SDPMid = &m
		}
		if c.SDPMLineIndex != nil {
			idx := uint16(*c.SDPMLineIndex)
			init.SDPMLineIndex = &idx
		}
		if err := pc.AddICECandidate(init); err != nil {
			log.Printf("[webrtc:%s] add remote ICE candidate failed: %v", in.cfg.BabyUID, err)
		}
	}

	onCameraCandidate := func(c nanit.WebRTCCandidate) {
		candMu.Lock()
		if !remoteDescSet {
			earlyCandidates = append(earlyCandidates, c)
			candMu.Unlock()
			return
		}
		candMu.Unlock()
		addRemoteCandidate(c)
	}

	errCh := make(chan error, 4)
	signalErr := func(err error) {
		select {
		case errCh <- err:
		default:
		}
	}

	onCameraClose := func(ev nanit.WebRTCClosed) {
		signalErr(fmt.Errorf("camera closed WebRTC session (error=%v %s)", ev.Error, ev.Message))
	}

	var (
		iceConnected atomic.Bool
		disconnMu    sync.Mutex
		disconnAt    time.Time
	)

	pc.OnICEConnectionStateChange(func(st webrtc.ICEConnectionState) {
		log.Printf("[webrtc:%s] ICE state: %s", in.cfg.BabyUID, st.String())
		switch st {
		case webrtc.ICEConnectionStateConnected, webrtc.ICEConnectionStateCompleted:
			iceConnected.Store(true)
			disconnMu.Lock()
			disconnAt = time.Time{}
			disconnMu.Unlock()
		case webrtc.ICEConnectionStateDisconnected:
			disconnMu.Lock()
			if disconnAt.IsZero() {
				disconnAt = time.Now()
			}
			disconnMu.Unlock()
		case webrtc.ICEConnectionStateFailed:
			signalErr(fmt.Errorf("ICE connection failed"))
		case webrtc.ICEConnectionStateClosed:
			signalErr(fmt.Errorf("ICE connection closed"))
		}
	})

	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
		init := c.ToJSON()
		if init.Candidate == "" {
			return
		}
		var mid *string
		var idx *int32
		if init.SDPMid != nil {
			m := *init.SDPMid
			mid = &m
		}
		if init.SDPMLineIndex != nil {
			v := int32(*init.SDPMLineIndex)
			idx = &v
		}
		go func() {
			if err := in.cfg.Signaler.SendWebRTCCandidate(sessCtx, sessionID, init.Candidate, mid, idx); err != nil && sessCtx.Err() == nil {
				log.Printf("[webrtc:%s] send local ICE candidate failed: %v", in.cfg.BabyUID, err)
			}
		}()
	})

	media := &sessionMedia{
		babyUID:    in.cfg.BabyUID,
		rtspServer: in.cfg.RTSPServer,
		rtmpServer: in.cfg.RTMPServer,
		pc:         pc,
		onActive:   in.cfg.OnActive,
		epoch:      time.Now(),
	}
	defer media.close()

	var trackWG sync.WaitGroup
	pc.OnTrack(func(track *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		codec := track.Codec()
		log.Printf("[webrtc:%s] track started: kind=%s mime=%s clock=%d ch=%d",
			in.cfg.BabyUID, track.Kind(), codec.MimeType, codec.ClockRate, codec.Channels)

		trackWG.Add(1)
		go func() {
			defer trackWG.Done()
			switch track.Kind() {
			case webrtc.RTPCodecTypeVideo:
				media.readVideoTrack(sessCtx, track)
			case webrtc.RTPCodecTypeAudio:
				media.readAudioTrack(sessCtx, track)
			}
		}()
	})

	offer, err := pc.CreateOffer(nil)
	if err != nil {
		return fmt.Errorf("create SDP offer: %w", err)
	}
	if err := pc.SetLocalDescription(offer); err != nil {
		return fmt.Errorf("set local description: %w", err)
	}

	offerSent = true
	log.Printf("[webrtc:%s] sending offer (session=%d)", in.cfg.BabyUID, sessionID)
	answerSDP, err := in.cfg.Signaler.SendWebRTCOffer(sessCtx, sessionID, offer.SDP, onCameraCandidate, onCameraClose)
	if err != nil {
		return err
	}

	media.setNegotiatedAudio(parseAnswerOpusChannels(answerSDP))

	if err := pc.SetRemoteDescription(webrtc.SessionDescription{
		Type: webrtc.SDPTypeAnswer,
		SDP:  answerSDP,
	}); err != nil {
		return fmt.Errorf("set remote description: %w", err)
	}

	candMu.Lock()
	remoteDescSet = true
	pending := earlyCandidates
	earlyCandidates = nil
	candMu.Unlock()
	for _, c := range pending {
		addRemoteCandidate(c)
	}

	startedAt := time.Now()
	ticker := time.NewTicker(watchdogTick)
	defer ticker.Stop()

	var runErr error
watchdog:
	for {
		select {
		case <-sessCtx.Done():
			runErr = sessCtx.Err()
			break watchdog
		case runErr = <-errCh:
			break watchdog
		case now := <-ticker.C:
			if !iceConnected.Load() && now.Sub(startedAt) > iceConnectTimeout {
				runErr = fmt.Errorf("ICE did not connect within %v", iceConnectTimeout)
				break watchdog
			}
			disconnMu.Lock()
			dAt := disconnAt
			disconnMu.Unlock()
			if !dAt.IsZero() && now.Sub(dAt) > iceDisconnectGrace {
				runErr = fmt.Errorf("ICE disconnected for more than %v", iceDisconnectGrace)
				break watchdog
			}
			lastVideo := media.lastVideoTime()
			if lastVideo.IsZero() {
				if now.Sub(startedAt) > videoStallTimeout {
					runErr = fmt.Errorf("no video RTP received within %v", videoStallTimeout)
					break watchdog
				}
			} else if now.Sub(lastVideo) > videoStallTimeout {
				runErr = fmt.Errorf("video RTP stalled for %v", now.Sub(lastVideo).Round(time.Second))
				break watchdog
			}
		}
	}

	_ = pc.Close()
	trackWG.Wait()
	return runErr
}

func newPeerConnection() (*webrtc.PeerConnection, error) {
	me := &webrtc.MediaEngine{}

	videoRTCPFeedback := []webrtc.RTCPFeedback{
		{Type: "nack"},
		{Type: "nack", Parameter: "pli"},
		{Type: "ccm", Parameter: "fir"},
	}

	h264Fmtps := []string{
		"level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42001f",
		"level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f",
		"level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=4d001f",
		"level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=4d0028",
		"level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=64001f",
		"level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=640028",
	}
	for i, fmtp := range h264Fmtps {
		if err := me.RegisterCodec(webrtc.RTPCodecParameters{
			RTPCodecCapability: webrtc.RTPCodecCapability{
				MimeType:     webrtc.MimeTypeH264,
				ClockRate:    90000,
				SDPFmtpLine:  fmtp,
				RTCPFeedback: videoRTCPFeedback,
			},
			PayloadType: webrtc.PayloadType(96 + i),
		}, webrtc.RTPCodecTypeVideo); err != nil {
			return nil, err
		}
	}

	if err := me.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:    webrtc.MimeTypeOpus,
			ClockRate:   48000,
			Channels:    2,
			SDPFmtpLine: "minptime=10;useinbandfec=1",
		},
		PayloadType: 111,
	}, webrtc.RTPCodecTypeAudio); err != nil {
		return nil, err
	}

	ir := &interceptor.Registry{}
	if err := webrtc.RegisterDefaultInterceptors(me, ir); err != nil {
		return nil, err
	}

	api := webrtc.NewAPI(
		webrtc.WithMediaEngine(me),
		webrtc.WithInterceptorRegistry(ir),
	)
	return api.NewPeerConnection(webrtc.Configuration{})
}

type rtpClock struct {
	clockRate   int64
	initialized bool
	lastTS      uint32
	totalTicks  int64
	basePTS     time.Duration
}

func (c *rtpClock) next(ts uint32, arrival time.Time, epoch time.Time) time.Duration {
	if !c.initialized {
		c.initialized = true
		c.lastTS = ts
		c.basePTS = arrival.Sub(epoch)
		if c.basePTS < 0 {
			c.basePTS = 0
		}
		return c.basePTS
	}
	delta := int64(int32(ts - c.lastTS))
	if delta > 0 {
		c.totalTicks += delta
		c.lastTS = ts
	}
	sec := c.totalTicks / c.clockRate
	rem := c.totalTicks % c.clockRate
	return c.basePTS + time.Duration(sec)*time.Second + time.Duration(rem*int64(time.Second)/c.clockRate)
}

type sessionMedia struct {
	babyUID    string
	rtspServer *rtsp.Server
	rtmpServer *rtmp.Server
	pc         *webrtc.PeerConnection
	onActive   func()
	epoch      time.Time

	mu            sync.Mutex
	opusChannels  int
	sps           []byte
	pps           []byte
	rtspStream    *rtsp.Stream
	rtmpPub       *rtmp.InternalPublisher
	activeNotif   bool
	gotKeyframe   atomic.Bool
	videoSSRC     atomic.Uint32
	lastPLINanos  atomic.Int64
	lastVideoUnix atomic.Int64
	closed        bool
}

func (m *sessionMedia) setNegotiatedAudio(channels int) {
	m.mu.Lock()
	m.opusChannels = channels
	m.mu.Unlock()
}

func (m *sessionMedia) lastVideoTime() time.Time {
	ns := m.lastVideoUnix.Load()
	if ns == 0 {
		return time.Time{}
	}
	return time.Unix(0, ns)
}

func (m *sessionMedia) requestPLI(force bool) {
	ssrc := m.videoSSRC.Load()
	if ssrc == 0 || m.pc == nil {
		return
	}
	now := time.Now().UnixNano()
	if !force {
		prev := m.lastPLINanos.Load()
		if now-prev < int64(pliMinInterval) {
			return
		}
	}
	m.lastPLINanos.Store(now)
	_ = m.pc.WriteRTCP([]rtcp.Packet{
		&rtcp.PictureLossIndication{MediaSSRC: ssrc},
	})
}

func (m *sessionMedia) ensurePublishedLocked() {
	if m.closed || len(m.sps) < 4 || len(m.pps) == 0 {
		return
	}

	if m.rtspServer != nil && m.rtspServer.IsRunning() && m.rtspStream == nil {
		st, err := m.rtspServer.Publish(m.babyUID, m.sps, m.pps, m.opusChannels, func() {
			m.requestPLI(false)
		})
		if err != nil {
			log.Printf("[webrtc:%s] RTSP publish failed: %v", m.babyUID, err)
		} else {
			m.rtspStream = st
		}
	}

	if m.rtmpServer != nil && m.rtmpPub == nil {
		m.rtmpPub = m.rtmpServer.PublishInternal(m.babyUID)
	}
}

func (m *sessionMedia) updateParams(sps, pps []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return
	}

	changed := false
	if len(sps) >= 4 && !bytes.Equal(m.sps, sps) {
		m.sps = append([]byte(nil), sps...)
		changed = true
	}
	if len(pps) > 0 && !bytes.Equal(m.pps, pps) {
		m.pps = append([]byte(nil), pps...)
		changed = true
	}

	if len(m.sps) < 4 || len(m.pps) == 0 {
		return
	}

	wasPublished := m.rtspStream != nil || m.rtmpPub != nil
	m.ensurePublishedLocked()

	if changed || !wasPublished {
		if m.rtspStream != nil {
			m.rtspStream.UpdateH264Params(m.sps, m.pps)
		}
		if m.rtmpPub != nil {
			avccCfg := buildAVCCConfig(m.sps, m.pps)
			if len(avccCfg) > 0 {
				m.rtmpPub.WritePacket(av.Packet{
					Type: av.H264DecoderConfig,
					Data: avccCfg,
				})
			}
		}
	}
}

func (m *sessionMedia) readVideoTrack(ctx context.Context, track *webrtc.TrackRemote) {
	m.videoSSRC.Store(uint32(track.SSRC()))

	if sps, pps := parseSpropParameterSets(track.Codec().SDPFmtpLine); len(sps) >= 4 && len(pps) > 0 {
		m.updateParams(sps, pps)
	}

	// Request an immediate keyframe and repeat briefly until the first IDR arrives.
	go func() {
		m.requestPLI(true)
		for i := 0; i < 4; i++ {
			select {
			case <-ctx.Done():
				return
			case <-time.After(1 * time.Second):
			}
			if m.gotKeyframe.Load() {
				return
			}
			m.requestPLI(true)
		}
	}()

	h264Fmt := &format.H264{
		PayloadTyp:        uint8(track.PayloadType()),
		PacketizationMode: 1,
	}
	dec, err := h264Fmt.CreateDecoder()
	if err != nil {
		log.Printf("[webrtc:%s] create H264 RTP decoder: %v", m.babyUID, err)
		return
	}

	clock := &rtpClock{clockRate: 90000}

	for {
		pkt, _, err := track.ReadRTP()
		if err != nil {
			return
		}
		now := time.Now()
		m.lastVideoUnix.Store(now.UnixNano())

		au, err := dec.Decode(pkt)
		if err != nil {
			if errors.Is(err, rtph264.ErrMorePacketsNeeded) || errors.Is(err, rtph264.ErrNonStartingPacketAndNoPrevious) {
				continue
			}
			continue
		}
		if len(au) == 0 {
			continue
		}

		var sps, pps []byte
		hasSPS, hasPPS := false, false
		for _, nalu := range au {
			if len(nalu) == 0 {
				continue
			}
			switch nalu[0] & 0x1F {
			case 7:
				sps = nalu
				hasSPS = true
			case 8:
				pps = nalu
				hasPPS = true
			}
		}
		if sps != nil || pps != nil {
			m.updateParams(sps, pps)
		}

		avccData, isKeyFrame := auToAVCC(au)
		if isKeyFrame {
			m.gotKeyframe.Store(true)
		}
		if len(avccData) == 0 {
			continue
		}

		pts := clock.next(pkt.Timestamp, now, m.epoch)

		m.mu.Lock()
		rtspSt := m.rtspStream
		rtmpPub := m.rtmpPub
		curSPS := m.sps
		curPPS := m.pps
		shouldNotify := false
		if (rtspSt != nil || rtmpPub != nil) && !m.activeNotif {
			m.activeNotif = true
			shouldNotify = true
		}
		m.mu.Unlock()

		if shouldNotify && m.onActive != nil {
			m.onActive()
		}

		if rtspSt != nil {
			rtspAU := au
			if isKeyFrame && (!hasSPS || !hasPPS) && len(curSPS) >= 4 && len(curPPS) > 0 {
				rtspAU = make([][]byte, 0, len(au)+2)
				if !hasSPS {
					rtspAU = append(rtspAU, curSPS)
				}
				if !hasPPS {
					rtspAU = append(rtspAU, curPPS)
				}
				rtspAU = append(rtspAU, au...)
			}
			if err := rtspSt.WriteH264(rtspAU, pts, now); err != nil {
				log.Printf("[webrtc:%s] RTSP write H264: %v", m.babyUID, err)
			}
		}

		if rtmpPub != nil {
			rtmpPub.WritePacket(av.Packet{
				Type:       av.H264,
				IsKeyFrame: isKeyFrame,
				Time:       pts,
				Data:       avccData,
			})
		}
	}
}

func (m *sessionMedia) readAudioTrack(_ context.Context, track *webrtc.TrackRemote) {
	if !strings.EqualFold(track.Codec().MimeType, webrtc.MimeTypeOpus) {
		log.Printf("[webrtc:%s] ignoring unsupported WebRTC audio codec: %s", m.babyUID, track.Codec().MimeType)
		return
	}

	clock := &rtpClock{clockRate: 48000}

	for {
		pkt, _, err := track.ReadRTP()
		if err != nil {
			return
		}
		if len(pkt.Payload) == 0 {
			continue
		}
		now := time.Now()
		pts := clock.next(pkt.Timestamp, now, m.epoch)

		m.mu.Lock()
		rtspSt := m.rtspStream
		m.mu.Unlock()

		if rtspSt != nil {
			if err := rtspSt.WriteOpus(pkt.Payload, pts, now); err != nil {
				log.Printf("[webrtc:%s] RTSP write Opus: %v", m.babyUID, err)
			}
		}
	}
}

func (m *sessionMedia) close() {
	m.mu.Lock()
	m.closed = true
	rtspSt := m.rtspStream
	rtmpPub := m.rtmpPub
	m.rtspStream = nil
	m.rtmpPub = nil
	m.mu.Unlock()

	if rtspSt != nil {
		rtspSt.Close()
	}
	if rtmpPub != nil {
		rtmpPub.Close()
	}
}

// buildAVCCConfig builds an ISO/IEC 14496-15 AVCDecoderConfigurationRecord from SPS and PPS.
func buildAVCCConfig(sps, pps []byte) []byte {
	if len(sps) < 4 || len(pps) == 0 {
		return nil
	}
	buf := make([]byte, 0, 11+len(sps)+len(pps))
	buf = append(buf,
		0x01,   // configurationVersion
		sps[1], // AVCProfileIndication
		sps[2], // profile_compatibility
		sps[3], // AVCLevelIndication
		0xFF,   // 6 bits reserved + 2 bits lengthSizeMinusOne (3 -> 4 bytes)
		0xE1,   // 3 bits reserved + 5 bits numOfSequenceParameterSets (1)
		byte(len(sps)>>8),
		byte(len(sps)),
	)
	buf = append(buf, sps...)
	buf = append(buf,
		0x01, // numOfPictureParameterSets (1)
		byte(len(pps)>>8),
		byte(len(pps)),
	)
	buf = append(buf, pps...)
	return buf
}

// auToAVCC packs non-parameter-set NALUs into 4-byte length-prefixed AVCC format.
func auToAVCC(au [][]byte) ([]byte, bool) {
	total := 0
	isKey := false
	for _, nalu := range au {
		if len(nalu) == 0 {
			continue
		}
		nalType := nalu[0] & 0x1F
		if nalType == 7 || nalType == 8 || nalType == 9 {
			continue
		}
		if nalType == 5 {
			isKey = true
		}
		total += 4 + len(nalu)
	}
	if total == 0 {
		return nil, false
	}

	out := make([]byte, 0, total)
	var lenBuf [4]byte
	for _, nalu := range au {
		if len(nalu) == 0 {
			continue
		}
		nalType := nalu[0] & 0x1F
		if nalType == 7 || nalType == 8 || nalType == 9 {
			continue
		}
		binary.BigEndian.PutUint32(lenBuf[:], uint32(len(nalu)))
		out = append(out, lenBuf[:]...)
		out = append(out, nalu...)
	}
	return out, isKey
}

func parseSpropParameterSets(fmtp string) (sps, pps []byte) {
	for _, part := range strings.Split(fmtp, ";") {
		part = strings.TrimSpace(part)
		if !strings.HasPrefix(strings.ToLower(part), "sprop-parameter-sets=") {
			continue
		}
		val := part[len("sprop-parameter-sets="):]
		sets := strings.Split(val, ",")
		if len(sets) >= 2 {
			s, err1 := base64.StdEncoding.DecodeString(strings.TrimSpace(sets[0]))
			p, err2 := base64.StdEncoding.DecodeString(strings.TrimSpace(sets[1]))
			if err1 == nil && err2 == nil {
				s = bytes.TrimPrefix(s, []byte{0, 0, 0, 1})
				p = bytes.TrimPrefix(p, []byte{0, 0, 0, 1})
				return s, p
			}
		}
	}
	return nil, nil
}

// parseAnswerOpusChannels inspects an SDP answer and returns the negotiated
// Opus channel count (1 for mono, 2 for stereo, 0 if Opus audio is absent).
func parseAnswerOpusChannels(answerSDP string) int {
	var parsed sdp.SessionDescription
	if err := parsed.UnmarshalString(answerSDP); err != nil {
		return 0
	}
	for _, md := range parsed.MediaDescriptions {
		if md.MediaName.Media != "audio" || md.MediaName.Port.Value == 0 {
			continue
		}
		opusPT := ""
		for _, attr := range md.Attributes {
			if attr.Key == "rtpmap" {
				fields := strings.Fields(attr.Value)
				if len(fields) >= 2 && strings.HasPrefix(strings.ToLower(fields[1]), "opus/") {
					opusPT = fields[0]
					break
				}
			}
		}
		if opusPT == "" {
			continue
		}
		// Default to mono unless sprop-stereo=1 is explicitly advertised.
		channels := 1
		for _, attr := range md.Attributes {
			if attr.Key == "fmtp" && strings.HasPrefix(attr.Value, opusPT+" ") {
				lower := strings.ToLower(attr.Value)
				if strings.Contains(lower, "sprop-stereo=1") {
					channels = 2
				}
			}
		}
		return channels
	}
	return 0
}

func randomSessionID() int64 {
	var b [8]byte
	_, _ = rand.Read(b[:])
	v := int64(binary.BigEndian.Uint64(b[:]) & 0x7fffffffffffffff)
	if v == 0 {
		v = 1
	}
	return v
}
