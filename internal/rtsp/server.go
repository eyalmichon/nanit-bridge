package rtsp

import (
	"crypto/subtle"
	"fmt"
	"log"
	"net"
	"regexp"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bluenviron/gortsplib/v5"
	"github.com/bluenviron/gortsplib/v5/pkg/base"
	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/bluenviron/gortsplib/v5/pkg/format"
	"github.com/bluenviron/gortsplib/v5/pkg/format/rtph264"
	"github.com/bluenviron/gortsplib/v5/pkg/format/rtpsimpleaudio"
)

var pathPattern = regexp.MustCompile(`^/?([a-zA-Z0-9_-]+)/([a-zA-Z0-9_-]+)$`)

// Server is a pure-Go TCP RTSP server that serves H.264 + Opus camera streams
// ingested over WebRTC to subscribers such as go2rtc / Frigate.
//
// All RTSP URLs require the configured token in the path:
//
//	rtsp://host:port/{token}/{uid}
type Server struct {
	port     int
	token    atomic.Value
	srv      *gortsplib.Server
	mu       sync.RWMutex
	streams  map[string]*Stream
	running  atomic.Bool
	stopOnce sync.Once
}

// Stream represents one published camera stream on the RTSP server.
type Stream struct {
	server       *Server
	key          string
	ss           *gortsplib.ServerStream
	videoMedia   *description.Media
	videoFormat  *format.H264
	videoEnc     *rtph264.Encoder
	audioMedia   *description.Media
	audioFormat  *format.Opus
	audioEnc     *rtpsimpleaudio.Encoder
	onReaderPlay func()

	writeMu   sync.Mutex
	closed    bool
	closeOnce sync.Once
}

// NewServer creates a new RTSP server bound to port and authenticated by token.
func NewServer(port int, token string) *Server {
	s := &Server{
		port:    port,
		streams: make(map[string]*Stream),
	}
	s.token.Store(token)
	return s
}

// SetToken updates the bearer path token for subsequent RTSP connections.
func (s *Server) SetToken(token string) {
	s.token.Store(token)
	log.Printf("[rtsp] token updated")
}

// GetToken returns the current path token.
func (s *Server) GetToken() string {
	return s.token.Load().(string)
}

// IsRunning reports whether the RTSP listener is active.
func (s *Server) IsRunning() bool {
	return s.running.Load()
}

// Port returns the actual bound TCP port (useful when started with port 0).
func (s *Server) Port() int {
	if s.srv != nil && s.srv.NetListener() != nil {
		if addr, ok := s.srv.NetListener().Addr().(*net.TCPAddr); ok {
			return addr.Port
		}
	}
	return s.port
}

// HasStream reports whether a stream is currently published for key.
func (s *Server) HasStream(key string) bool {
	s.mu.RLock()
	_, ok := s.streams[key]
	s.mu.RUnlock()
	return ok
}

// Start begins listening for RTSP over TCP connections.
func (s *Server) Start() error {
	addr := fmt.Sprintf(":%d", s.port)
	srv := &gortsplib.Server{
		Handler:        s,
		RTSPAddress:    addr,
		WriteQueueSize: 512,
	}
	if err := srv.Start(); err != nil {
		return fmt.Errorf("rtsp listen: %w", err)
	}
	s.srv = srv
	s.running.Store(true)
	log.Printf("[rtsp] listening on :%d", s.Port())
	return nil
}

// Stop closes all published streams and shuts down the RTSP server.
func (s *Server) Stop() {
	s.stopOnce.Do(func() {
		s.running.Store(false)

		s.mu.Lock()
		streams := make([]*Stream, 0, len(s.streams))
		for k, st := range s.streams {
			streams = append(streams, st)
			delete(s.streams, k)
		}
		s.mu.Unlock()

		for _, st := range streams {
			st.Close()
		}

		if s.srv != nil {
			s.srv.Close()
		}
	})
}

// Publish registers a live H.264 (and optional Opus) stream under key.
// If opusChannels <= 0, the stream is published as video-only.
func (s *Server) Publish(
	key string,
	sps, pps []byte,
	opusChannels int,
	onReaderPlay func(),
) (*Stream, error) {
	if !s.running.Load() || s.srv == nil {
		return nil, fmt.Errorf("rtsp server not running")
	}

	spsCopy := append([]byte(nil), sps...)
	ppsCopy := append([]byte(nil), pps...)

	h264Fmt := &format.H264{
		PayloadTyp:        96,
		SPS:               spsCopy,
		PPS:               ppsCopy,
		PacketizationMode: 1,
	}
	videoEnc, err := h264Fmt.CreateEncoder()
	if err != nil {
		return nil, fmt.Errorf("create H264 RTP encoder: %w", err)
	}
	videoMedia := &description.Media{
		Type:    description.MediaTypeVideo,
		Formats: []format.Format{h264Fmt},
	}

	medias := []*description.Media{videoMedia}

	var audioMedia *description.Media
	var audioFmt *format.Opus
	var audioEnc *rtpsimpleaudio.Encoder
	if opusChannels > 0 {
		audioFmt = &format.Opus{
			PayloadTyp:   111,
			ChannelCount: opusChannels,
		}
		audioEnc, err = audioFmt.CreateEncoder()
		if err != nil {
			return nil, fmt.Errorf("create Opus RTP encoder: %w", err)
		}
		audioMedia = &description.Media{
			Type:    description.MediaTypeAudio,
			Formats: []format.Format{audioFmt},
		}
		medias = append(medias, audioMedia)
	}

	desc := &description.Session{
		Medias: medias,
	}
	ss := &gortsplib.ServerStream{
		Server: s.srv,
		Desc:   desc,
	}
	if err := ss.Initialize(); err != nil {
		return nil, fmt.Errorf("initialize RTSP stream: %w", err)
	}

	st := &Stream{
		server:       s,
		key:          key,
		ss:           ss,
		videoMedia:   videoMedia,
		videoFormat:  h264Fmt,
		videoEnc:     videoEnc,
		audioMedia:   audioMedia,
		audioFormat:  audioFmt,
		audioEnc:     audioEnc,
		onReaderPlay: onReaderPlay,
	}

	s.mu.Lock()
	old := s.streams[key]
	s.streams[key] = st
	s.mu.Unlock()

	if old != nil {
		old.Close()
	}

	log.Printf("[rtsp] stream published: key=%s video=H264 audio_opus_ch=%d", key, opusChannels)
	return st, nil
}

// UpdateH264Params updates the SPS/PPS advertised in SDP for future DESCRIBE requests.
func (st *Stream) UpdateH264Params(sps, pps []byte) {
	if st == nil {
		return
	}
	st.videoFormat.SafeSetParams(append([]byte(nil), sps...), append([]byte(nil), pps...))
}

// WriteH264 packetizes an H.264 access unit into RTP and sends it to all subscribers.
func (st *Stream) WriteH264(au [][]byte, pts time.Duration, ntp time.Time) error {
	if st == nil || len(au) == 0 {
		return nil
	}

	st.writeMu.Lock()
	defer st.writeMu.Unlock()
	if st.closed {
		return nil
	}

	pkts, err := st.videoEnc.Encode(au)
	if err != nil {
		return err
	}
	ts := durationToRTP(pts, 90000)
	for _, pkt := range pkts {
		pkt.Timestamp = ts
		if err := st.ss.WritePacketRTPWithNTP(st.videoMedia, pkt, ntp); err != nil {
			return err
		}
	}
	return nil
}

// WriteOpus packetizes a single Opus frame into RTP and sends it to all subscribers.
func (st *Stream) WriteOpus(payload []byte, pts time.Duration, ntp time.Time) error {
	if st == nil || st.audioMedia == nil || len(payload) == 0 {
		return nil
	}

	st.writeMu.Lock()
	defer st.writeMu.Unlock()
	if st.closed {
		return nil
	}

	pkt, err := st.audioEnc.Encode(payload)
	if err != nil {
		return err
	}
	pkt.Timestamp = durationToRTP(pts, 48000)
	return st.ss.WritePacketRTPWithNTP(st.audioMedia, pkt, ntp)
}

// Close removes this stream from the server and disconnects its readers.
func (st *Stream) Close() {
	if st == nil {
		return
	}
	st.closeOnce.Do(func() {
		st.writeMu.Lock()
		st.closed = true
		st.writeMu.Unlock()

		st.server.mu.Lock()
		if cur, ok := st.server.streams[st.key]; ok && cur == st {
			delete(st.server.streams, st.key)
		}
		st.server.mu.Unlock()

		st.ss.Close()
		log.Printf("[rtsp] stream closed: key=%s", st.key)
	})
}

func durationToRTP(d time.Duration, clockRate int64) uint32 {
	if d < 0 {
		return 0
	}
	sec := int64(d / time.Second)
	rem := int64(d % time.Second)
	return uint32(sec*clockRate + (rem*clockRate)/int64(time.Second))
}

func parseStreamPath(path string) (pathToken, key string, err error) {
	matches := pathPattern.FindStringSubmatch(path)
	if len(matches) != 3 {
		return "", "", fmt.Errorf("invalid stream path")
	}
	return matches[1], matches[2], nil
}

func (s *Server) authorizeStream(path string) (*Stream, base.StatusCode) {
	pathToken, key, err := parseStreamPath(path)
	if err != nil {
		return nil, base.StatusNotFound
	}
	expected := s.token.Load().(string)
	if subtle.ConstantTimeCompare([]byte(pathToken), []byte(expected)) != 1 {
		return nil, base.StatusUnauthorized
	}

	s.mu.RLock()
	st := s.streams[key]
	s.mu.RUnlock()
	if st == nil {
		return nil, base.StatusNotFound
	}
	return st, base.StatusOK
}

// OnDescribe handles RTSP DESCRIBE requests.
func (s *Server) OnDescribe(ctx *gortsplib.ServerHandlerOnDescribeCtx) (*base.Response, *gortsplib.ServerStream, error) {
	st, code := s.authorizeStream(ctx.Path)
	if code != base.StatusOK {
		return &base.Response{StatusCode: code}, nil, nil
	}
	return &base.Response{StatusCode: base.StatusOK}, st.ss, nil
}

// OnSetup handles RTSP SETUP requests for readers.
func (s *Server) OnSetup(ctx *gortsplib.ServerHandlerOnSetupCtx) (*base.Response, *gortsplib.ServerStream, error) {
	st, code := s.authorizeStream(ctx.Path)
	if code != base.StatusOK {
		return &base.Response{StatusCode: code}, nil, nil
	}
	return &base.Response{StatusCode: base.StatusOK}, st.ss, nil
}

// OnPlay handles RTSP PLAY requests and notifies the ingest to request a fresh keyframe.
func (s *Server) OnPlay(ctx *gortsplib.ServerHandlerOnPlayCtx) (*base.Response, error) {
	st, code := s.authorizeStream(ctx.Path)
	if code != base.StatusOK {
		return &base.Response{StatusCode: code}, nil
	}
	if st.onReaderPlay != nil {
		go st.onReaderPlay()
	}
	return &base.Response{StatusCode: base.StatusOK}, nil
}
