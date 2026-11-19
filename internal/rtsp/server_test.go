package rtsp

import (
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bluenviron/gortsplib/v5"
	"github.com/bluenviron/gortsplib/v5/pkg/base"
	"github.com/bluenviron/gortsplib/v5/pkg/format"
	"github.com/pion/rtp"
)

// Minimal valid H.264 Baseline SPS/PPS/IDR bytes for testing RTP framing.
var (
	testSPS = []byte{0x67, 0x42, 0xc0, 0x1f, 0xd9, 0x00, 0x78, 0x02, 0x27, 0xe5, 0x84, 0x00, 0x00, 0x03, 0x00, 0x04, 0x00, 0x00, 0x03, 0x00, 0xf0, 0x3c, 0x60, 0xc6, 0x58}
	testPPS = []byte{0x68, 0xce, 0x3c, 0x80}
	testIDR = []byte{0x65, 0x88, 0x84, 0x21, 0xa0}
)

func TestServerPublishDescribePlayAndAuth(t *testing.T) {
	srv := NewServer(0, "secret-token-123")
	if err := srv.Start(); err != nil {
		t.Fatalf("srv.Start(): %v", err)
	}
	defer srv.Stop()

	var playCalled atomic.Int32
	st, err := srv.Publish("baby1", testSPS, testPPS, 1, func() {
		playCalled.Add(1)
	})
	if err != nil {
		t.Fatalf("Publish(): %v", err)
	}
	defer st.Close()

	if !srv.HasStream("baby1") {
		t.Fatal("HasStream(baby1) = false, want true")
	}

	// Wrong token should fail with 401.
	badURL, _ := base.ParseURL(fmt.Sprintf("rtsp://127.0.0.1:%d/wrong-token/baby1", srv.Port()))
	badClient := &gortsplib.Client{Scheme: badURL.Scheme, Host: badURL.Host}
	if err := badClient.Start(); err != nil {
		t.Fatalf("badClient.Start(): %v", err)
	}
	defer badClient.Close()
	if _, _, err := badClient.Describe(badURL); err == nil {
		t.Fatal("expected error with wrong token")
	}

	// Valid token over TCP interleaved should receive both H264 and Opus RTP packets.
	tcpProto := gortsplib.ProtocolTCP
	goodURL, _ := base.ParseURL(fmt.Sprintf("rtsp://127.0.0.1:%d/secret-token-123/baby1", srv.Port()))
	client := &gortsplib.Client{
		Scheme:   goodURL.Scheme,
		Host:     goodURL.Host,
		Protocol: &tcpProto,
	}
	if err := client.Start(); err != nil {
		t.Fatalf("client.Start(): %v", err)
	}
	defer client.Close()

	desc, _, err := client.Describe(goodURL)
	if err != nil {
		t.Fatalf("Describe(): %v", err)
	}

	var h264Fmt *format.H264
	videoMedia := desc.FindFormat(&h264Fmt)
	if videoMedia == nil {
		t.Fatal("expected H264 video track in SDP")
	}
	var opusFmt *format.Opus
	audioMedia := desc.FindFormat(&opusFmt)
	if audioMedia == nil {
		t.Fatal("expected Opus audio track in SDP")
	}

	if err := client.SetupAll(desc.BaseURL, desc.Medias); err != nil {
		t.Fatalf("SetupAll(): %v", err)
	}

	gotVideo := make(chan struct{}, 1)
	gotAudio := make(chan struct{}, 1)
	client.OnPacketRTP(videoMedia, h264Fmt, func(_ *rtp.Packet) {
		select {
		case gotVideo <- struct{}{}:
		default:
		}
	})
	client.OnPacketRTP(audioMedia, opusFmt, func(_ *rtp.Packet) {
		select {
		case gotAudio <- struct{}{}:
		default:
		}
	})

	if _, err := client.Play(nil); err != nil {
		t.Fatalf("Play(): %v", err)
	}

	// Wait briefly for OnPlay callback and write video + audio frames.
	deadline := time.Now().Add(2 * time.Second)
	for playCalled.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if playCalled.Load() == 0 {
		t.Fatal("expected onReaderPlay callback to be invoked")
	}

	now := time.Now()
	if err := st.WriteH264([][]byte{testSPS, testPPS, testIDR}, 100*time.Millisecond, now); err != nil {
		t.Fatalf("WriteH264(): %v", err)
	}
	if err := st.WriteOpus([]byte{0xf8, 0xff, 0xfe}, 100*time.Millisecond, now); err != nil {
		t.Fatalf("WriteOpus(): %v", err)
	}

	select {
	case <-gotVideo:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for H264 RTP packet")
	}
	select {
	case <-gotAudio:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for Opus RTP packet")
	}

	// Verify token update works.
	srv.SetToken("rotated-token-456")
	if srv.GetToken() != "rotated-token-456" {
		t.Fatalf("GetToken() = %q", srv.GetToken())
	}
}
