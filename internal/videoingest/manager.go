package videoingest

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"image/jpeg"
	"io"
	"log"
	"net"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	srt "github.com/datarhei/gosrt"
	"github.com/example/autostream-encoder-recorder/internal/observability"
)

const (
	inputPrefix             = "internal_worker_video:"
	pbKeylen                = 32
	encoderFrameInterval    = time.Second / 60
	maxSceneFrameDimension  = 3840
	diagnosticReportTimeout = 2 * time.Second
)

const (
	videoIngestSRTAccepted   = "encoder.video_ingest.srt_accepted"
	videoIngestLocalAccepted = "encoder.video_ingest.local_accepted"
	videoIngestFirstFrame    = "encoder.video_ingest.first_frame"
	videoIngestClosed        = "encoder.video_ingest.closed"
)

var (
	ErrAlreadyRunning = errors.New("worker video ingest is already running")
	errNotConfigured  = errors.New("worker video ingest is not configured")
	streamIDPattern   = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)
)

// Config contains only non-secret listener routing configuration. The
// per-stream SRT credential is derived and retained by Manager in memory.
type Config struct {
	BindAddr      string
	AdvertiseHost string
}

// Bridge contains the one-time start response for the Control Panel plus the
// loopback-only FFmpeg input. InputURL is deliberately never serialized.
type Bridge struct {
	URL        string `json:"url"`
	Passphrase string `json:"passphrase"`
	PBKeylen   int    `json:"pbkeylen"`
	InputURL   string `json:"-"`
}

type Manager struct {
	Config      Config
	ConfigError error
	Reporter    Reporter

	mu      sync.Mutex
	bridges map[string]*bridgeRecord
}

type Reporter interface {
	Report(context.Context, observability.Signal) error
}

type bridgeRecord struct {
	streamID   string
	passphrase string
	srt        srt.Listener
	local      net.Listener
	startedAt  time.Time

	mu              sync.Mutex
	srtConn         srt.Conn
	localConn       net.Conn
	closed          bool
	closeOnce       sync.Once
	finishOnce      sync.Once
	stopped         atomic.Bool
	srtRejections   atomic.Uint64
	framesReceived  atomic.Uint64
	framesForwarded atomic.Uint64
}

type acceptResult struct {
	conn       net.Conn
	errorClass string
}

type srtAcceptResult struct {
	conn       srt.Conn
	errorClass string
}

func NewManagerFromEnv() *Manager {
	config, err := configFromEnv()
	return &Manager{Config: config, ConfigError: err}
}

// Available reports whether an opt-in start can allocate a listener. It does
// not allocate a UDP port and never exposes a credential.
func (m *Manager) Available() bool {
	return m != nil && m.ConfigError == nil && m.Config.validate() == nil
}

func (m *Manager) StartBridge(streamID, jobCredential string) (Bridge, error) {
	if m == nil {
		return Bridge{}, errNotConfigured
	}
	if m.ConfigError != nil {
		return Bridge{}, m.ConfigError
	}
	if err := m.Config.validate(); err != nil {
		return Bridge{}, err
	}
	streamID = strings.TrimSpace(streamID)
	if !streamIDPattern.MatchString(streamID) {
		return Bridge{}, errors.New("invalid worker video stream id")
	}
	passphrase, err := derivePassphrase(jobCredential)
	if err != nil {
		return Bridge{}, err
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.bridges == nil {
		m.bridges = make(map[string]*bridgeRecord)
	}
	// An Encoder runs one media pipeline at a time. This global guard also
	// makes a fixed production UDP port deterministic instead of leaking an
	// EADDRINUSE failure for a second stream as a generic availability error.
	if len(m.bridges) != 0 {
		return Bridge{}, ErrAlreadyRunning
	}

	config := srt.DefaultConfig()
	config.PBKeylen = pbKeylen
	config.EnforcedEncryption = true
	config.Logger = srt.NewLogger(nil)
	listener, err := srt.Listen("srt", m.Config.BindAddr, config)
	if err != nil {
		return Bridge{}, fmt.Errorf("start worker video SRT listener: %w", err)
	}
	local, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		listener.Close()
		return Bridge{}, fmt.Errorf("start worker video loopback bridge: %w", err)
	}

	udpAddr, ok := listener.Addr().(*net.UDPAddr)
	if !ok || udpAddr.Port <= 0 {
		local.Close()
		listener.Close()
		return Bridge{}, errors.New("worker video SRT listener did not allocate a UDP port")
	}
	tcpAddr, ok := local.Addr().(*net.TCPAddr)
	if !ok || tcpAddr.Port <= 0 {
		local.Close()
		listener.Close()
		return Bridge{}, errors.New("worker video loopback bridge did not allocate a TCP port")
	}

	record := &bridgeRecord{streamID: streamID, passphrase: passphrase, srt: listener, local: local, startedAt: time.Now().UTC()}
	m.bridges[streamID] = record
	go m.run(record)

	return Bridge{
		URL:        "srt://" + net.JoinHostPort(normalizeAdvertiseHost(m.Config.AdvertiseHost), fmt.Sprintf("%d", udpAddr.Port)),
		Passphrase: passphrase,
		PBKeylen:   pbKeylen,
		InputURL:   inputPrefix + "tcp://" + net.JoinHostPort("127.0.0.1", fmt.Sprintf("%d", tcpAddr.Port)),
	}, nil
}

func (m *Manager) StopBridge(streamID string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	record := m.bridges[strings.TrimSpace(streamID)]
	if record != nil {
		delete(m.bridges, strings.TrimSpace(streamID))
	}
	m.mu.Unlock()
	if record != nil {
		record.stopped.Store(true)
		record.close()
	}
}

// MarkStopRequested records the lifecycle intent before the process manager
// asks FFmpeg to exit. The bridge remains open until the process manager has
// completed its graceful stop, so an input close during that window is
// reported as a normal stop instead of a truncated SRT failure.
func (m *Manager) MarkStopRequested(streamID string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	record := m.bridges[strings.TrimSpace(streamID)]
	m.mu.Unlock()
	if record != nil {
		record.stopped.Store(true)
	}
}

func (m *Manager) run(record *bridgeRecord) {
	srtReady := make(chan srtAcceptResult, 1)
	localReady := make(chan acceptResult, 1)
	go func() { srtReady <- acceptPublisher(record) }()
	go func() {
		conn, err := record.local.Accept()
		if err != nil {
			localReady <- acceptResult{errorClass: "local_accept"}
			return
		}
		localReady <- acceptResult{conn: conn}
	}()

	srtResult := <-srtReady
	if srtResult.conn != nil {
		m.reportDiagnostic(record.streamID, videoIngestSRTAccepted, "accepted", nil)
	}
	localResult := <-localReady
	if localResult.conn != nil {
		m.reportDiagnostic(record.streamID, videoIngestLocalAccepted, "accepted", map[string]any{"transport": "tcp_loopback"})
	}
	if srtResult.conn == nil || localResult.conn == nil {
		if srtResult.conn != nil {
			_ = srtResult.conn.Close()
		}
		if localResult.conn != nil {
			_ = localResult.conn.Close()
		}
		if srtResult.conn == nil {
			m.finish(record, "srt_accept_failed", srtResult.errorClass)
		} else {
			m.finish(record, "local_accept_failed", localResult.errorClass)
		}
		return
	}
	if !record.attach(srtResult.conn, localResult.conn) {
		_ = srtResult.conn.Close()
		_ = localResult.conn.Close()
		m.finish(record, "bridge_attach_failed", "bridge_attach")
		return
	}

	frames := make(chan []byte, 1)
	readResult := make(chan string, 1)
	go func() {
		readResult <- readSceneFrames(srtResult.conn, frames, func(width, height, size int) {
			count := record.framesReceived.Add(1)
			if count == 1 {
				m.reportDiagnostic(record.streamID, videoIngestFirstFrame, "received", map[string]any{
					"frame_width":  width,
					"frame_height": height,
					"frame_bytes":  size,
				})
			}
		})
	}()
	ticker := time.NewTicker(encoderFrameInterval)
	defer ticker.Stop()
	var latest []byte
	for {
		select {
		case frame := <-frames:
			latest = frame
		case <-ticker.C:
			if len(latest) > 0 {
				if _, err := localResult.conn.Write(latest); err != nil {
					m.finish(record, "local_write_failed", "local_write")
					return
				}
				record.framesForwarded.Add(1)
			}
		case errorClass := <-readResult:
			m.finish(record, "srt_input_closed", errorClass)
			return
		}
	}
}

// readSceneFrames converts the authenticated Worker byte stream into bounded,
// validated JPEG images. Only the latest complete frame is retained; scene
// updates cannot build an unbounded queue behind the final Encoder.
func readSceneFrames(reader io.Reader, output chan []byte, onFrame func(width, height, size int)) string {
	buffered := bufio.NewReaderSize(reader, 256<<10)
	for {
		frame, err := jpeg.Decode(buffered)
		if err != nil {
			return classifyFrameReadError(err)
		}
		bounds := frame.Bounds()
		if bounds.Dx() <= 0 || bounds.Dy() <= 0 || bounds.Dx() > maxSceneFrameDimension || bounds.Dy() > maxSceneFrameDimension {
			return "srt_frame_invalid"
		}
		var encoded bytes.Buffer
		if err := jpeg.Encode(&encoded, frame, &jpeg.Options{Quality: 90}); err != nil {
			return "srt_frame_encode"
		}
		data := append([]byte(nil), encoded.Bytes()...)
		if onFrame != nil {
			onFrame(bounds.Dx(), bounds.Dy(), len(data))
		}
		select {
		case output <- data:
		default:
			select {
			case <-output:
			default:
			}
			select {
			case output <- data:
			default:
			}
		}
	}
}

func classifyFrameReadError(err error) string {
	switch {
	case errors.Is(err, io.EOF):
		return "srt_read_eof"
	case errors.Is(err, io.ErrUnexpectedEOF):
		return "srt_frame_truncated"
	default:
		return "srt_frame_decode"
	}
}

func acceptPublisher(record *bridgeRecord) srtAcceptResult {
	for {
		request, err := record.srt.Accept2()
		if err != nil {
			return srtAcceptResult{errorClass: "srt_accept"}
		}
		if request.Version() != 5 || request.StreamId() != record.streamID {
			record.srtRejections.Add(1)
			request.Reject(srt.REJX_UNAUTHORIZED)
			continue
		}
		if !request.IsEncrypted() {
			record.srtRejections.Add(1)
			request.Reject(srt.REJ_UNSECURE)
			continue
		}
		if err := request.SetPassphrase(record.passphrase); err != nil {
			record.srtRejections.Add(1)
			request.Reject(srt.REJ_BADSECRET)
			continue
		}
		conn, err := request.Accept()
		if err != nil {
			record.srtRejections.Add(1)
			continue
		}
		return srtAcceptResult{conn: conn}
	}
}

func (m *Manager) finish(record *bridgeRecord, reason, errorClass string) {
	record.finishOnce.Do(func() {
		if record.stopped.Load() {
			reason = "bridge_stopped"
			errorClass = "bridge_stopped"
		}
		record.close()
		m.mu.Lock()
		if current := m.bridges[record.streamID]; current == record {
			delete(m.bridges, record.streamID)
		}
		m.mu.Unlock()
		status := "failed"
		if record.stopped.Load() {
			status = "stopped"
		} else if errorClass == "srt_read_eof" {
			status = "closed"
		}
		attributes := map[string]any{
			"reason":           reason,
			"error_class":      errorClass,
			"frames_received":  record.framesReceived.Load(),
			"frames_forwarded": record.framesForwarded.Load(),
			"srt_rejections":   record.srtRejections.Load(),
			"duration_ms":      maxDurationMilliseconds(time.Since(record.startedAt)),
		}
		m.reportDiagnostic(record.streamID, videoIngestClosed, status, attributes)
	})
}

func (m *Manager) reportDiagnostic(streamID, name, status string, attributes map[string]any) {
	logDiagnostic(name, streamID, status, attributes)
	if m.Reporter == nil {
		return
	}
	if attributes != nil {
		attributes = cloneAttributes(attributes)
	}
	signal := observability.Signal{
		Type:       "event",
		Name:       name,
		StreamID:   streamID,
		Status:     status,
		Attributes: attributes,
		Timestamp:  time.Now().UTC(),
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), diagnosticReportTimeout)
		defer cancel()
		if err := m.Reporter.Report(ctx, signal); err != nil {
			log.Printf("encoder diagnostic report failed: event=%s stream_id=%s error_class=observability_request_failed report_error_class=%s", name, streamID, diagnosticReportErrorClass(err))
		}
	}()
}

func diagnosticReportErrorClass(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	if errors.Is(err, context.Canceled) {
		return "canceled"
	}
	message := err.Error()
	if strings.HasPrefix(message, "observability signal failed with status ") {
		return "http_status"
	}
	if strings.Contains(message, "OBSERVABILITY_") {
		return "configuration"
	}
	return "transport"
}

func cloneAttributes(attributes map[string]any) map[string]any {
	cloned := make(map[string]any, len(attributes))
	for key, value := range attributes {
		cloned[key] = value
	}
	return cloned
}

func logDiagnostic(name, streamID, status string, attributes map[string]any) {
	reason, _ := attributes["reason"].(string)
	errorClass, _ := attributes["error_class"].(string)
	log.Printf("encoder diagnostic: event=%s stream_id=%s status=%s reason=%s error_class=%s frame_width=%v frame_height=%v frame_bytes=%v frames_received=%v frames_forwarded=%v srt_rejections=%v duration_ms=%v", name, streamID, status, reason, errorClass, attributes["frame_width"], attributes["frame_height"], attributes["frame_bytes"], attributes["frames_received"], attributes["frames_forwarded"], attributes["srt_rejections"], attributes["duration_ms"])
}

func maxDurationMilliseconds(value time.Duration) int64 {
	if value <= 0 {
		return 0
	}
	return value.Milliseconds()
}

func (r *bridgeRecord) attach(srtConn srt.Conn, localConn net.Conn) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return false
	}
	r.srtConn = srtConn
	r.localConn = localConn
	return true
}

func (r *bridgeRecord) close() {
	r.closeOnce.Do(func() {
		r.mu.Lock()
		r.closed = true
		srtConn := r.srtConn
		localConn := r.localConn
		r.mu.Unlock()
		r.srt.Close()
		_ = r.local.Close()
		if srtConn != nil {
			_ = srtConn.Close()
		}
		if localConn != nil {
			_ = localConn.Close()
		}
	})
}

func configFromEnv() (Config, error) {
	bindAddr := strings.TrimSpace(os.Getenv("AUTOSTREAM_WORKER_VIDEO_BIND_ADDR"))
	advertiseHost := strings.TrimSpace(os.Getenv("AUTOSTREAM_WORKER_VIDEO_ADVERTISE_HOST"))
	production := strings.EqualFold(strings.TrimSpace(os.Getenv("AUTOSTREAM_ENV")), "production")
	if bindAddr == "" && !production {
		bindAddr = "127.0.0.1:0"
	}
	if advertiseHost == "" && !production {
		advertiseHost = "127.0.0.1"
	}
	config := Config{BindAddr: bindAddr, AdvertiseHost: advertiseHost}
	if err := config.validate(); err != nil {
		return config, err
	}
	return config, nil
}

func (c Config) validate() error {
	if strings.TrimSpace(c.BindAddr) == "" || strings.TrimSpace(c.AdvertiseHost) == "" {
		return errNotConfigured
	}
	bindHost, portText, err := net.SplitHostPort(c.BindAddr)
	if err != nil {
		return errors.New("AUTOSTREAM_WORKER_VIDEO_BIND_ADDR must be host:port")
	}
	if bindHost != "" && net.ParseIP(strings.Trim(bindHost, "[]")) == nil {
		return errors.New("AUTOSTREAM_WORKER_VIDEO_BIND_ADDR host must be empty or a literal IP address")
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 0 || port > 65535 {
		return errors.New("AUTOSTREAM_WORKER_VIDEO_BIND_ADDR port must be between 0 and 65535")
	}
	host := normalizeAdvertiseHost(c.AdvertiseHost)
	if host == "" || strings.ContainsAny(host, "/?#@ \t\r\n\\") {
		return errors.New("AUTOSTREAM_WORKER_VIDEO_ADVERTISE_HOST must be a hostname or IP address")
	}
	if strings.Contains(host, ":") && net.ParseIP(host) == nil {
		return errors.New("AUTOSTREAM_WORKER_VIDEO_ADVERTISE_HOST is invalid")
	}
	if parsed, err := url.Parse("srt://" + net.JoinHostPort(host, "1")); err != nil || parsed.Hostname() == "" {
		return errors.New("AUTOSTREAM_WORKER_VIDEO_ADVERTISE_HOST is invalid")
	}
	return nil
}

func normalizeAdvertiseHost(host string) string {
	return strings.Trim(strings.TrimSpace(host), "[]")
}

func derivePassphrase(jobCredential string) (string, error) {
	jobCredential = strings.TrimSpace(jobCredential)
	if jobCredential == "" {
		return "", errors.New("worker video ingest credential is required")
	}
	sum := sha256.Sum256([]byte("autostream-worker-video-srt-v1\x00" + jobCredential))
	return base64.RawURLEncoding.EncodeToString(sum[:]), nil
}

// ResolveInputTarget removes the internal marker before passing the
// loopback-only paced MJPEG endpoint to FFmpeg.
func ResolveInputTarget(input string) (string, bool) {
	input = strings.TrimSpace(input)
	if !strings.HasPrefix(input, inputPrefix) {
		return "", false
	}
	target := strings.TrimPrefix(input, inputPrefix)
	parsed, err := url.Parse(target)
	if err != nil || parsed.Scheme != "tcp" || parsed.User != nil || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Port() == "" {
		return "", false
	}
	if parsed.Hostname() != "127.0.0.1" && parsed.Hostname() != "::1" {
		return "", false
	}
	return target, true
}
