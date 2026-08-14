package watermarkfeed

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"net"
	"strings"
	"sync"
	"time"

	_ "golang.org/x/image/webp"
	_ "image/jpeg"
)

const (
	maxImageBytes  = 5 << 20
	maxImagePixels = 4096 * 4096
	frameInterval  = 500 * time.Millisecond
)

// Source presents a continuously available piped-PNG input on loopback.
// Replacing the current frame changes the Encoder watermark without changing
// the FFmpeg process or any of its output clocks.
type Source struct {
	listener net.Listener

	mu      sync.RWMutex
	frame   []byte
	current net.Conn
	closed  bool

	done chan struct{}
	wg   sync.WaitGroup
}

func New(config map[string]any) (*Source, error) {
	frame, err := Frame(config)
	if err != nil {
		return nil, err
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	source := &Source{listener: listener, frame: frame, done: make(chan struct{})}
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
		return errors.New("watermark frame is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("watermark source is closed")
	}
	s.frame = append(s.frame[:0], frame...)
	return nil
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
		case <-ticker.C:
		}
	}
}

// Frame validates and normalizes a Control Panel watermark profile to PNG.
// A disabled or unselected watermark becomes a transparent frame, which keeps
// the FFmpeg graph stable while allowing later live enablement.
func Frame(config map[string]any) ([]byte, error) {
	if config == nil {
		return transparentPNG()
	}
	if enabled, ok := config["watermark_enabled"].(bool); ok && !enabled {
		return transparentPNG()
	}
	dataURL, _ := config["watermark_image_data_url"].(string)
	dataURL = strings.TrimSpace(dataURL)
	if dataURL == "" {
		return transparentPNG()
	}
	header, encoded, ok := strings.Cut(dataURL, ",")
	lowerHeader := strings.ToLower(strings.TrimSpace(header))
	if !ok || !strings.HasPrefix(lowerHeader, "data:image/") || !strings.Contains(lowerHeader, ";base64") {
		return nil, errors.New("watermark image must be a base64 data URL")
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encoded))
	if err != nil {
		return nil, errors.New("watermark image data is not valid base64")
	}
	if len(decoded) == 0 || len(decoded) > maxImageBytes {
		return nil, fmt.Errorf("watermark image must be between 1 and %d bytes", maxImageBytes)
	}
	configOnly, _, err := image.DecodeConfig(bytes.NewReader(decoded))
	if err != nil || configOnly.Width <= 0 || configOnly.Height <= 0 || configOnly.Width > maxImagePixels/configOnly.Height {
		return nil, errors.New("watermark image dimensions are invalid")
	}
	img, _, err := image.Decode(bytes.NewReader(decoded))
	if err != nil {
		return nil, errors.New("watermark image could not be decoded")
	}
	var out bytes.Buffer
	if err := png.Encode(&out, img); err != nil {
		return nil, errors.New("watermark image could not be normalized")
	}
	return out.Bytes(), nil
}

func transparentPNG() ([]byte, error) {
	img := image.NewNRGBA(image.Rect(0, 0, 1, 1))
	img.SetNRGBA(0, 0, color.NRGBA{})
	var out bytes.Buffer
	if err := png.Encode(&out, img); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}
