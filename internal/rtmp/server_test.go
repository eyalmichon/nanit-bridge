package rtmp

import (
	"testing"

	"github.com/notedit/rtmp/av"
)

func TestParseStreamKey(t *testing.T) {
	tests := []struct {
		path    string
		want    string
		wantErr bool
	}{
		{path: "/local/abc123", want: "abc123"},
		{path: "abc123", want: "abc123"},
		{path: "/abc123", want: "abc123"},
		{path: "/local/a_b-c", want: "a_b-c"},
		{path: "/local/abc/def", wantErr: true},
		{path: "/local/", wantErr: true},
		{path: "", wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.path, func(t *testing.T) {
			got, err := parseStreamKey(tc.path)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error for %q", tc.path)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error for %q: %v", tc.path, err)
			}
			if got != tc.want {
				t.Fatalf("parseStreamKey(%q) = %q, want %q", tc.path, got, tc.want)
			}
		})
	}
}

func TestBroadcasterReplayConfigsToNewSubscriber(t *testing.T) {
	b := newBroadcaster()
	meta := av.Packet{Type: av.Metadata}
	aac := av.Packet{Type: av.AACDecoderConfig}
	h264cfg := av.Packet{Type: av.H264DecoderConfig}
	b.metadata = &meta
	b.aacConfig = &aac
	b.h264Config = &h264cfg

	sub := b.addSubscriber("s1")

	got1 := <-sub.pktC
	got2 := <-sub.pktC
	got3 := <-sub.pktC
	if got1.Type != av.Metadata || got2.Type != av.AACDecoderConfig || got3.Type != av.H264DecoderConfig {
		t.Fatalf("unexpected replay order/types: %v, %v, %v", got1.Type, got2.Type, got3.Type)
	}
}

func TestBroadcasterKeyframeGate(t *testing.T) {
	b := newBroadcaster()
	sub := b.addSubscriber("s1")

	nonKey := av.Packet{Type: av.H264, IsKeyFrame: false}
	b.broadcast(nonKey)
	if len(sub.pktC) != 0 {
		t.Fatalf("expected no packet before keyframe, got len=%d", len(sub.pktC))
	}

	key := av.Packet{Type: av.H264, IsKeyFrame: true}
	b.broadcast(key)
	if len(sub.pktC) != 1 {
		t.Fatalf("expected keyframe to pass, got len=%d", len(sub.pktC))
	}
	got := <-sub.pktC
	if !got.IsKeyFrame {
		t.Fatalf("expected keyframe packet")
	}

	nonKey2 := av.Packet{Type: av.H264, IsKeyFrame: false}
	b.broadcast(nonKey2)
	if len(sub.pktC) != 1 {
		t.Fatalf("expected non-keyframe after keyframe, got len=%d", len(sub.pktC))
	}
}

func TestBroadcasterDropsWhenChannelFullAndResyncs(t *testing.T) {
	b := newBroadcaster()
	sub := b.addSubscriber("s1")
	sub.needsKeyframe = false

	for i := 0; i < cap(sub.pktC); i++ {
		sub.pktC <- av.Packet{Type: av.Metadata}
	}
	if len(sub.pktC) != cap(sub.pktC) {
		t.Fatalf("channel not full before test")
	}

	b.broadcast(av.Packet{Type: av.H264, IsKeyFrame: false})
	if !sub.needsKeyframe {
		t.Fatalf("expected needsKeyframe to be reset true")
	}
	if len(sub.pktC) != 0 {
		t.Fatalf("expected channel drained on overflow, got len=%d", len(sub.pktC))
	}
}

func TestBroadcasterRemoveSubscriber(t *testing.T) {
	b := newBroadcaster()
	sub := b.addSubscriber("s1")
	b.removeSubscriber("s1")

	_, ok := <-sub.pktC
	if ok {
		t.Fatalf("expected closed channel")
	}
}
