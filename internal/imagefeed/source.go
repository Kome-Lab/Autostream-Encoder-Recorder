package imagefeed

import (
	"context"
	"fmt"
	"io"
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

	mu          sync.RWMutex
	frame       []byte
	current     net.Conn
	closed      bool
	version     uint64
	delivered   uint64
	deliveredCh chan struct{}

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
		listener:    listener,
		kind:        kind,
		frame:       append([]byte(nil), initialFrame...),
		version:     1,
		done:        make(chan struct{}),
		updated:     make(chan struct{}, 1),
		deliveredCh: make(chan struct{}),
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
	_, err := s.update(frame)
	return err
}

// UpdateAndWait replaces the frame and waits until the connected consumer has
// accepted a complete write of that exact feed version. It does not imply a
// decoded or graph-applied frame; callers must add their own downstream
// witness before reporting an applied state.
func (s *Source) UpdateAndWait(ctx context.Context, frame []byte) error {
	version, err := s.update(frame)
	if err != nil {
		return err
	}
	return s.waitDelivered(ctx, version)
}

// WaitInitialDelivery waits for the initial frame to be written completely to
// the first connected consumer.
func (s *Source) WaitInitialDelivery(ctx context.Context) error {
	if s == nil {
		return fmt.Errorf("%s source is closed", s.name())
	}
	return s.waitDelivered(ctx, 1)
}

func (s *Source) update(frame []byte) (uint64, error) {
	if s == nil || len(frame) == 0 {
		return 0, fmt.Errorf("%s frame is required", s.name())
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return 0, fmt.Errorf("%s source is closed", s.kind)
	}
	s.frame = append(s.frame[:0], frame...)
	s.version++
	version := s.version
	select {
	case s.updated <- struct{}{}:
	default:
	}
	return version, nil
}

func (s *Source) waitDelivered(ctx context.Context, version uint64) error {
	for {
		s.mu.RLock()
		if s.delivered >= version {
			s.mu.RUnlock()
			return nil
		}
		closed := s.closed
		ch := s.deliveredCh
		s.mu.RUnlock()
		if closed {
			return fmt.Errorf("%s source is closed", s.name())
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ch:
		}
	}
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
	close(s.deliveredCh)
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
		version := s.version
		closed := s.closed
		s.mu.RUnlock()
		if closed {
			return
		}
		_ = conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
		if err := writeFull(conn, frame); err != nil {
			return
		}
		s.markDelivered(version)
		select {
		case <-s.done:
			return
		case <-s.updated:
		case <-ticker.C:
		}
	}
}

func (s *Source) markDelivered(version uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || version <= s.delivered {
		return
	}
	s.delivered = version
	close(s.deliveredCh)
	s.deliveredCh = make(chan struct{})
}

func writeFull(conn net.Conn, frame []byte) error {
	for len(frame) > 0 {
		written, err := conn.Write(frame)
		if err != nil {
			return err
		}
		if written <= 0 {
			return io.ErrShortWrite
		}
		frame = frame[written:]
	}
	return nil
}
