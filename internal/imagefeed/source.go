package imagefeed

import (
	"fmt"
	"net"
	"strings"
	"sync"
	"time"
)

const frameInterval = 500 * time.Millisecond

// Source presents a continuously available image input on loopback. Replacing
// the current frame does not require the consuming FFmpeg process to restart.
type Source struct {
	listener net.Listener
	kind     string

	mu      sync.RWMutex
	frame   []byte
	current net.Conn
	closed  bool

	done    chan struct{}
	updated chan struct{}
	wg      sync.WaitGroup
}

// New starts a named loopback image feed with a copy of initialFrame. The name
// keeps adapter error contracts stable while the transport remains generic.
func New(kind string, initialFrame []byte) (*Source, error) {
	kind = strings.TrimSpace(kind)
	if kind == "" {
		kind = "image feed"
	}
	if len(initialFrame) == 0 {
		return nil, fmt.Errorf("%s frame is required", kind)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	source := &Source{
		listener: listener,
		kind:     kind,
		frame:    append([]byte(nil), initialFrame...),
		done:     make(chan struct{}),
		updated:  make(chan struct{}, 1),
	}
	source.wg.Add(1)
	go source.serve()
	return source, nil
}

func (s *Source) InputURL() string {
	if s == nil || s.listener == nil {
		return ""
	}
	return "tcp://" + s.listener.Addr().String()
}

func (s *Source) Update(frame []byte) error {
	if s == nil || len(frame) == 0 {
		return fmt.Errorf("%s frame is required", s.name())
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return fmt.Errorf("%s source is closed", s.kind)
	}
	s.frame = append(s.frame[:0], frame...)
	select {
	case s.updated <- struct{}{}:
	default:
	}
	return nil
}

func (s *Source) name() string {
	if s == nil || s.kind == "" {
		return "image feed"
	}
	return s.kind
}

func (s *Source) Close() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	close(s.done)
	listener := s.listener
	current := s.current
	s.mu.Unlock()
	if current != nil {
		_ = current.Close()
	}
	var err error
	if listener != nil {
		err = listener.Close()
	}
	s.wg.Wait()
	return err
}

func (s *Source) serve() {
	defer s.wg.Done()
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			select {
			case <-s.done:
				return
			default:
				continue
			}
		}
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			_ = conn.Close()
			return
		}
		s.current = conn
		s.mu.Unlock()
		s.writeFrames(conn)
		s.mu.Lock()
		if s.current == conn {
			s.current = nil
		}
		s.mu.Unlock()
		_ = conn.Close()
	}
}

func (s *Source) writeFrames(conn net.Conn) {
	ticker := time.NewTicker(frameInterval)
	defer ticker.Stop()
	for {
		s.mu.RLock()
		frame := append([]byte(nil), s.frame...)
		closed := s.closed
		s.mu.RUnlock()
		if closed {
			return
		}
		_ = conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
		if _, err := conn.Write(frame); err != nil {
			return
		}
		select {
		case <-s.done:
			return
		case <-s.updated:
		case <-ticker.C:
		}
	}
}
