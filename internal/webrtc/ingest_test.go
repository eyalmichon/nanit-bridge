package webrtc

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bluenviron/gortsplib/v5"
	"github.com/bluenviron/gortsplib/v5/pkg/base"
	"github.com/bluenviron/gortsplib/v5/pkg/format"
	"github.com/notedit/rtmp/av"
	"github.com/notedit/rtmp/codec/h264"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"

	"nanit-bridge/internal/nanit"
	"nanit-bridge/internal/rtmp"
	"nanit-bridge/internal/rtsp"
)

var (
	sampleSPS = []byte{0x67, 0x42, 0xc0, 0x1f, 0xd9, 0x00, 0x78, 0x02, 0x27, 0xe5, 0x84, 0x00, 0x00, 0x03, 0x00, 0x04, 0x00, 0x00, 0x03, 0x00, 0xf0, 0x3c, 0x60, 0xc6, 0x58}
	samplePPS = []byte{0x68, 0xce, 0x3c, 0x80}
	sampleIDR = []byte{0x65, 0x88, 0x84, 0x21, 0xa0}
)

// fakeCameraSignaler runs a real Pion PeerConnection acting as a Nanit camera,
// sending H.264 and Opus media over local loopback UDP.
type fakeCameraSignaler struct {
	mu           sync.Mutex
	pc           *webrtc.PeerConnection
	videoTrack   *webrtc.TrackLocalStaticSample
	audioTrack   *webrtc.TrackLocalStaticSample
	stopMedia    chan struct{}
	closedSIDs   []int64
	refuseWith   *nanit.WebRTCRefusedError
	connected    atomic.Bool
	offerCount   atomic.Int32
}

func newFakeCameraSignaler() *fakeCameraSignaler {
	f := &fakeCameraSignaler{}
	f.connected.Store(true)
	return f
}

func (f *fakeCameraSignaler) CameraUID() string { return "cam-test" }
func (f *fakeCameraSignaler) IsConnected() bool { return f.connected.Load() }

func (f *fakeCameraSignaler) SendWebRTCOffer(
	_ context.Context,
	sessionID int64,
	offerSDP string,
	onCandidate func(nanit.WebRTCCandidate),
	_ func(nanit.WebRTCClosed),
) (string, error) {
	f.offerCount.Add(1)
	if f.refuseWith != nil {
		return "", f.refuseWith
	}

	me := &webrtc.MediaEngine{}
	if err := me.RegisterDefaultCodecs(); err != nil {
		return "", err
	}
	api := webrtc.NewAPI(webrtc.WithMediaEngine(me))
	pc, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		return "", err
	}

	videoTrack, err := webrtc.NewTrackLocalStaticSample(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264, ClockRate: 90000, SDPFmtpLine: "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42001f"},
		"video", "nanit-cam",
	)
	if err != nil {
		pc.Close()
		return "", err
	}
	audioTrack, err := webrtc.NewTrackLocalStaticSample(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2},
		"audio", "nanit-cam",
	)
	if err != nil {
		pc.Close()
		return "", err
	}
	if _, err := pc.AddTrack(audioTrack); err != nil {
		pc.Close()
		return "", err
	}
	if _, err := pc.AddTrack(videoTrack); err != nil {
		pc.Close()
		return "", err
	}

	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil || onCandidate == nil {
			return
		}
		init := c.ToJSON()
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
		onCandidate(nanit.WebRTCCandidate{
			SessionID:     sessionID,
			Candidate:     init.Candidate,
			SDPMid:        mid,
			SDPMLineIndex: idx,
		})
	})

	if err := pc.SetRemoteDescription(webrtc.SessionDescription{
		Type: webrtc.SDPTypeOffer,
		SDP:  offerSDP,
	}); err != nil {
		pc.Close()
		return "", err
	}

	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		pc.Close()
		return "", err
	}
	if err := pc.SetLocalDescription(answer); err != nil {
		pc.Close()
		return "", err
	}

	stopMedia := make(chan struct{})
	f.mu.Lock()
	f.pc = pc
	f.videoTrack = videoTrack
	f.audioTrack = audioTrack
	f.stopMedia = stopMedia
	f.mu.Unlock()

	// Pump H.264 (SPS+PPS+IDR in Annex-B) + Opus frames once ICE connects.
	go func() {
		annexB := make([]byte, 0, len(sampleSPS)+len(samplePPS)+len(sampleIDR)+12)
		annexB = append(annexB, 0, 0, 0, 1)
		annexB = append(annexB, sampleSPS...)
		annexB = append(annexB, 0, 0, 0, 1)
		annexB = append(annexB, samplePPS...)
		annexB = append(annexB, 0, 0, 0, 1)
		annexB = append(annexB, sampleIDR...)

		vTick := time.NewTicker(50 * time.Millisecond)
		aTick := time.NewTicker(20 * time.Millisecond)
		defer vTick.Stop()
		defer aTick.Stop()
		for {
			select {
			case <-stopMedia:
				return
			case <-vTick.C:
				_ = videoTrack.WriteSample(media.Sample{Data: annexB, Duration: 50 * time.Millisecond})
			case <-aTick.C:
				_ = audioTrack.WriteSample(media.Sample{Data: []byte{0xf8, 0xff, 0xfe}, Duration: 20 * time.Millisecond})
			}
		}
	}()

	return answer.SDP, nil
}

func (f *fakeCameraSignaler) SendWebRTCCandidate(
	_ context.Context,
	_ int64,
	candidate string,
	sdpMid *string,
	sdpMLineIndex *int32,
) error {
	f.mu.Lock()
	pc := f.pc
	f.mu.Unlock()
	if pc == nil {
		return nil
	}
	init := webrtc.ICECandidateInit{Candidate: candidate}
	if sdpMid != nil {
		m := *sdpMid
		init.SDPMid = &m
	}
	if sdpMLineIndex != nil {
		idx := uint16(*sdpMLineIndex)
		init.SDPMLineIndex = &idx
	}
	return pc.AddICECandidate(init)
}

func (f *fakeCameraSignaler) SendWebRTCClose(sessionID int64) error {
	f.mu.Lock()
	f.closedSIDs = append(f.closedSIDs, sessionID)
	stopMedia := f.stopMedia
	pc := f.pc
	f.stopMedia = nil
	f.pc = nil
	f.mu.Unlock()

	if stopMedia != nil {
		close(stopMedia)
	}
	if pc != nil {
		_ = pc.Close()
	}
	return nil
}

func TestIngestEndToEndRTSPAndRTMP(t *testing.T) {
	rtspSrv := rtsp.NewServer(0, "tok12345")
	if err := rtspSrv.Start(); err != nil {
		t.Fatalf("rtspSrv.Start(): %v", err)
	}
	defer rtspSrv.Stop()

	rtmpSrv := rtmp.NewServer(0, "tok12345")
	sig := newFakeCameraSignaler()

	activeCh := make(chan struct{}, 1)
	ingest := NewIngest(Config{
		BabyUID:    "baby1",
		Signaler:   sig,
		RTSPServer: rtspSrv,
		RTMPServer: rtmpSrv,
		OnActive: func() {
			select {
			case activeCh <- struct{}{}:
			default:
			}
		},
	})
	ingest.Start()
	defer ingest.Stop()

	select {
	case <-activeCh:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for WebRTC ingest to become active")
	}

	// 1. Verify internal RTMP publisher delivers valid AVCC H264DecoderConfig + H264 IDR (used by BMM & FLV).
	if !rtmpSrv.HasStream("baby1") {
		t.Fatal("expected rtmpSrv.HasStream(baby1) = true")
	}
	if rtmpSrv.HasAudio("baby1") {
		t.Fatal("expected rtmpSrv.HasAudio(baby1) = false for WebRTC video-only FLV bridge")
	}
	pkts, unsub, ok := rtmpSrv.Subscribe("baby1")
	if !ok {
		t.Fatal("rtmpSrv.Subscribe failed")
	}
	defer unsub()

	var gotCfg, gotKey bool
	deadline := time.After(5 * time.Second)
	for !gotCfg || !gotKey {
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for RTMP packets (gotCfg=%v gotKey=%v)", gotCfg, gotKey)
		case pkt := <-pkts:
			switch pkt.Type {
			case av.H264DecoderConfig:
				codec, err := h264.FromDecoderConfig(pkt.Data)
				if err != nil {
					t.Fatalf("h264.FromDecoderConfig failed: %v", err)
				}
				if len(codec.SPS) == 0 || len(codec.PPS) == 0 {
					t.Fatalf("missing SPS/PPS in decoder config")
				}
				gotCfg = true
			case av.H264:
				if pkt.IsKeyFrame {
					nalus, _ := h264.SplitNALUs(pkt.Data)
					if len(nalus) == 0 {
						t.Fatal("SplitNALUs returned 0 NALUs for keyframe")
					}
					gotKey = true
				}
			}
		}
	}

	// 2. Verify RTSP subscriber receives BOTH H.264 video and Opus audio over TCP.
	tcpProto := gortsplib.ProtocolTCP
	u, _ := base.ParseURL(fmt.Sprintf("rtsp://127.0.0.1:%d/tok12345/baby1", rtspSrv.Port()))
	c := &gortsplib.Client{Scheme: u.Scheme, Host: u.Host, Protocol: &tcpProto}
	if err := c.Start(); err != nil {
		t.Fatalf("rtsp client.Start(): %v", err)
	}
	defer c.Close()

	desc, _, err := c.Describe(u)
	if err != nil {
		t.Fatalf("rtsp Describe(): %v", err)
	}
	var h264Fmt *format.H264
	videoMedia := desc.FindFormat(&h264Fmt)
	var opusFmt *format.Opus
	audioMedia := desc.FindFormat(&opusFmt)
	if videoMedia == nil || audioMedia == nil {
		t.Fatalf("expected both H264 and Opus in RTSP SDP (video=%v audio=%v)", videoMedia != nil, audioMedia != nil)
	}
	if err := c.SetupAll(desc.BaseURL, desc.Medias); err != nil {
		t.Fatalf("rtsp SetupAll(): %v", err)
	}

	gotRTSPVideo := make(chan struct{}, 1)
	gotRTSPAudio := make(chan struct{}, 1)
	c.OnPacketRTP(videoMedia, h264Fmt, func(_ *rtp.Packet) {
		select {
		case gotRTSPVideo <- struct{}{}:
		default:
		}
	})
	c.OnPacketRTP(audioMedia, opusFmt, func(_ *rtp.Packet) {
		select {
		case gotRTSPAudio <- struct{}{}:
		default:
		}
	})
	if _, err := c.Play(nil); err != nil {
		t.Fatalf("rtsp Play(): %v", err)
	}

	select {
	case <-gotRTSPVideo:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for RTSP H264 RTP")
	}
	select {
	case <-gotRTSPAudio:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for RTSP Opus RTP")
	}

	// 3. Stop must send WebRtc.Close and tear down published streams.
	ingest.Stop()
	sig.mu.Lock()
	closedCount := len(sig.closedSIDs)
	sig.mu.Unlock()
	if closedCount == 0 {
		t.Fatal("expected SendWebRTCClose to be called on Stop()")
	}
	if rtspSrv.HasStream("baby1") || rtmpSrv.HasStream("baby1") {
		t.Fatal("expected streams to be cleaned up after Stop()")
	}
}

func TestIngestUnsupportedCallback(t *testing.T) {
	sig := newFakeCameraSignaler()
	sig.refuseWith = &nanit.WebRTCRefusedError{StatusCode: 501, StatusMessage: "Not Implemented"}

	unsupported := make(chan error, 1)
	ingest := NewIngest(Config{
		BabyUID:  "baby1",
		Signaler: sig,
		OnUnsupported: func(err error) {
			unsupported <- err
		},
	})
	ingest.Start()
	defer ingest.Stop()

	select {
	case err := <-unsupported:
		if err == nil {
			t.Fatal("expected non-nil error")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for OnUnsupported callback")
	}
}

func TestHelpersAndRTPClockWraparound(t *testing.T) {
	// AVCC config round-trip with notedit/rtmp H264 parser.
	cfg := buildAVCCConfig(sampleSPS, samplePPS)
	parsed, err := h264.FromDecoderConfig(cfg)
	if err != nil {
		t.Fatalf("FromDecoderConfig: %v", err)
	}
	if len(parsed.SPS) != 1 || len(parsed.PPS) != 1 {
		t.Fatalf("unexpected SPS/PPS counts: %d/%d", len(parsed.SPS), len(parsed.PPS))
	}

	// auToAVCC strips SPS/PPS/AUD and flags IDR keyframe.
	data, key := auToAVCC([][]byte{sampleSPS, samplePPS, {0x09, 0x10}, sampleIDR})
	if !key {
		t.Fatal("expected keyframe=true")
	}
	nalus, _ := h264.SplitNALUs(data)
	if len(nalus) != 1 || nalus[0][0]&0x1F != 5 {
		t.Fatalf("unexpected AVCC NALUs: %v", nalus)
	}

	// RTP 32-bit wraparound test (90kHz).
	epoch := time.Now()
	clk := &rtpClock{clockRate: 90000}
	p1 := clk.next(0xFFFFFF00, epoch, epoch)
	p2 := clk.next(0x00000100, epoch, epoch) // +512 ticks across 32-bit boundary
	if p2 <= p1 {
		t.Fatalf("expected monotonic PTS across 32-bit wrap: p1=%v p2=%v", p1, p2)
	}

	// SDP Opus channel parsing.
	monoSDP := "v=0\r\no=- 1 1 IN IP4 0.0.0.0\r\ns=-\r\nt=0 0\r\nm=audio 9 UDP/TLS/RTP/SAVPF 111\r\na=rtpmap:111 opus/48000/2\r\na=fmtp:111 minptime=10;useinbandfec=1\r\n"
	if ch := parseAnswerOpusChannels(monoSDP); ch != 1 {
		t.Fatalf("mono SDP channels = %d, want 1", ch)
	}
	stereoSDP := "v=0\r\no=- 1 1 IN IP4 0.0.0.0\r\ns=-\r\nt=0 0\r\nm=audio 9 UDP/TLS/RTP/SAVPF 111\r\na=rtpmap:111 opus/48000/2\r\na=fmtp:111 sprop-stereo=1\r\n"
	if ch := parseAnswerOpusChannels(stereoSDP); ch != 2 {
		t.Fatalf("stereo SDP channels = %d, want 2", ch)
	}
	videoOnlySDP := "v=0\r\no=- 1 1 IN IP4 0.0.0.0\r\ns=-\r\nt=0 0\r\nm=video 9 UDP/TLS/RTP/SAVPF 96\r\n"
	if ch := parseAnswerOpusChannels(videoOnlySDP); ch != 0 {
		t.Fatalf("video-only SDP channels = %d, want 0", ch)
	}
}
