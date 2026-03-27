package rtmp

import (
	"context"
	"fmt"
	"log"
	"net"
	"regexp"
	"sync"
	"syscall"
	"time"

	"github.com/notedit/rtmp/av"
	"github.com/notedit/rtmp/format/rtmp"
)

var pathPattern = regexp.MustCompile(`^/?(?:local/)?([a-zA-Z0-9_-]+)$`)

// broadcaster manages a single published stream and its subscribers.
type broadcaster struct {
	mu          sync.RWMutex
	subscribers map[string]*subscriber
	metadata    *av.Packet
	aacConfig   *av.Packet
	h264Config  *av.Packet
}

type subscriber struct {
	pktC          chan av.Packet
	needsKeyframe bool // when true, non-keyframe H264 packets are skipped
}

func newBroadcaster() *broadcaster {
	return &broadcaster{
		subscribers: make(map[string]*subscriber),
	}
}

func (b *broadcaster) addSubscriber(id string) *subscriber {
	sub := &subscriber{
		pktC:          make(chan av.Packet, 256),
		needsKeyframe: true, // wait for keyframe before sending video
	}

	b.mu.Lock()
	b.subscribers[id] = sub

	if b.metadata != nil {
		sub.pktC <- *b.metadata
	}
	if b.aacConfig != nil {
		sub.pktC <- *b.aacConfig
	}
	if b.h264Config != nil {
		sub.pktC <- *b.h264Config
	}
	b.mu.Unlock()

	return sub
}

func (b *broadcaster) removeSubscriber(id string) {
	b.mu.Lock()
	if sub, ok := b.subscribers[id]; ok {
		close(sub.pktC)
		delete(b.subscribers, id)
	}
	b.mu.Unlock()
}

func (b *broadcaster) closeSubscribers() {
	b.mu.Lock()
	for id, sub := range b.subscribers {
		close(sub.pktC)
		delete(b.subscribers, id)
	}
	b.mu.Unlock()
}

func (b *broadcaster) broadcast(pkt av.Packet) {
	isVideo := pkt.Type == av.H264
	isKeyframe := isVideo && pkt.IsKeyFrame

	b.mu.RLock()
	defer b.mu.RUnlock()
	for _, sub := range b.subscribers {
		if sub.needsKeyframe {
			if isVideo && !isKeyframe {
				continue // skip P/B-frames until a keyframe arrives
			}
			if isKeyframe {
				sub.needsKeyframe = false
			}
		}

		select {
		case sub.pktC <- pkt:
		default:
			// channel full — drain it and wait for the next keyframe to resync
			sub.needsKeyframe = true
			n := len(sub.pktC)
			for i := 0; i < n; i++ {
				<-sub.pktC
			}
		}
	}
}

// Server is an RTMP relay that accepts publishes from Nanit cameras
// and serves streams to consumers (go2rtc/Frigate).
//
// SECURITY: This server has no authentication. Any client that can reach the
// listen port can publish or subscribe to streams. Restrict access via firewall
// rules, Docker network isolation, or a reverse proxy.
type Server struct {
	port         int
	broadcasters map[string]*broadcaster
	mu           sync.RWMutex
}

func NewServer(port int) *Server {
	return &Server{
		port:         port,
		broadcasters: make(map[string]*broadcaster),
	}
}

// HasStream returns true if a publisher is currently broadcasting for the given key.
func (s *Server) HasStream(key string) bool {
	s.mu.RLock()
	_, ok := s.broadcasters[key]
	s.mu.RUnlock()
	return ok
}

// Subscribe returns a packet channel for the given stream key, or nil if the
// stream is not currently being published. Call unsubscribe when done.
func (s *Server) Subscribe(key string) (packets <-chan av.Packet, unsubscribe func(), ok bool) {
	s.mu.RLock()
	b, found := s.broadcasters[key]
	s.mu.RUnlock()
	if !found {
		return nil, nil, false
	}

	subID := fmt.Sprintf("http-flv-%d", time.Now().UnixNano())
	sub := b.addSubscriber(subID)
	return sub.pktC, func() { b.removeSubscriber(subID) }, true
}

func (s *Server) Start() error {
	addr := fmt.Sprintf(":%d", s.port)
	lc := net.ListenConfig{
		Control: setReuseAddr,
	}
	lis, err := lc.Listen(context.Background(), "tcp", addr)
	if err != nil {
		return fmt.Errorf("rtmp listen: %w", err)
	}

	log.Printf("[rtmp] listening on %s", addr)

	srv := rtmp.NewServer()
	srv.HandleConn = func(c *rtmp.Conn, nc net.Conn) {
		s.handleConnection(c, nc)
	}

	go func() {
		for {
			nc, err := lis.Accept()
			if err != nil {
				time.Sleep(time.Second)
				continue
			}
			go srv.HandleNetConn(nc)
		}
	}()

	return nil
}

func parseStreamKey(path string) (string, error) {
	matches := pathPattern.FindStringSubmatch(path)
	if len(matches) != 2 {
		return "", fmt.Errorf("invalid stream path: %q", path)
	}
	return matches[1], nil
}

func (s *Server) handleConnection(c *rtmp.Conn, nc net.Conn) {
	key, err := parseStreamKey(c.URL.Path)
	if err != nil {
		log.Printf("[rtmp] %v", err)
		nc.Close()
		return
	}

	if c.Publishing {
		s.handlePublisher(c, nc, key)
	} else {
		s.handleSubscriber(c, nc, key)
	}
}

func (s *Server) handlePublisher(c *rtmp.Conn, nc net.Conn, key string) {
	log.Printf("[rtmp] publisher connected: stream=%s", key)

	b := newBroadcaster()

	s.mu.Lock()
	if old, ok := s.broadcasters[key]; ok {
		go old.closeSubscribers()
	}
	s.broadcasters[key] = b
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		if cur, ok := s.broadcasters[key]; ok && cur == b {
			delete(s.broadcasters, key)
		}
		s.mu.Unlock()
		b.closeSubscribers()
		log.Printf("[rtmp] publisher disconnected: stream=%s", key)
	}()

	for {
		pkt, err := c.ReadPacket()
		if err != nil {
			log.Printf("[rtmp] publisher read error: stream=%s err=%v", key, err)
			return
		}

		switch pkt.Type {
		case av.Metadata:
			b.mu.Lock()
			b.metadata = &pkt
			b.mu.Unlock()
		case av.AACDecoderConfig:
			b.mu.Lock()
			b.aacConfig = &pkt
			b.mu.Unlock()
		case av.H264DecoderConfig:
			b.mu.Lock()
			b.h264Config = &pkt
			b.mu.Unlock()
		}

		b.broadcast(pkt)
	}
}

func (s *Server) handleSubscriber(c *rtmp.Conn, nc net.Conn, key string) {
	log.Printf("[rtmp] subscriber connected: stream=%s", key)

	s.mu.RLock()
	b, ok := s.broadcasters[key]
	s.mu.RUnlock()

	if !ok {
		log.Printf("[rtmp] stream %q not available for subscriber", key)
		nc.Close()
		return
	}

	subID := fmt.Sprintf("sub-%p", nc)
	sub := b.addSubscriber(subID)

	defer func() {
		b.removeSubscriber(subID)
		log.Printf("[rtmp] subscriber disconnected: stream=%s", key)
	}()

	closeC := c.CloseNotify()
	for {
		select {
		case pkt, open := <-sub.pktC:
			if !open {
				nc.Close()
				return
			}
			c.WritePacket(pkt)

		case <-closeC:
			return
		}
	}
}

func setReuseAddr(network, address string, c syscall.RawConn) error {
	return c.Control(func(fd uintptr) {
		syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1)
	})
}
