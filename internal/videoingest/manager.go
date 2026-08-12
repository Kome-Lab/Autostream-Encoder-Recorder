package videoingest

import (
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"

	srt "github.com/datarhei/gosrt"
)

const (
	inputPrefix = "internal_worker_video:"
	pbKeylen    = 32
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

	mu      sync.Mutex
	bridges map[string]*bridgeRecord
}

type bridgeRecord struct {
	streamID   string
	passphrase string
	srt        srt.Listener
	local      net.Listener

	mu        sync.Mutex
	srtConn   srt.Conn
	localConn net.Conn
	closed    bool
	closeOnce sync.Once
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

	record := &bridgeRecord{streamID: streamID, passphrase: passphrase, srt: listener, local: local}
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
		record.close()
	}
}

func (m *Manager) run(record *bridgeRecord) {
	srtReady := make(chan srt.Conn, 1)
	localReady := make(chan net.Conn, 1)
	go func() { srtReady <- acceptPublisher(record) }()
	go func() {
		conn, err := record.local.Accept()
		if err != nil {
			localReady <- nil
			return
		}
		localReady <- conn
	}()

	srtConn := <-srtReady
	localConn := <-localReady
	if srtConn == nil || localConn == nil || !record.attach(srtConn, localConn) {
		if srtConn != nil {
			_ = srtConn.Close()
		}
		if localConn != nil {
			_ = localConn.Close()
		}
		m.finish(record)
		return
	}

	_, _ = io.Copy(localConn, srtConn)
	m.finish(record)
}

func acceptPublisher(record *bridgeRecord) srt.Conn {
	for {
		request, err := record.srt.Accept2()
		if err != nil {
			return nil
		}
		if request.Version() != 5 || request.StreamId() != record.streamID {
			request.Reject(srt.REJX_UNAUTHORIZED)
			continue
		}
		if !request.IsEncrypted() {
			request.Reject(srt.REJ_UNSECURE)
			continue
		}
		if err := request.SetPassphrase(record.passphrase); err != nil {
			request.Reject(srt.REJ_BADSECRET)
			continue
		}
		conn, err := request.Accept()
		if err != nil {
			continue
		}
		return conn
	}
}

func (m *Manager) finish(record *bridgeRecord) {
	record.close()
	m.mu.Lock()
	if current := m.bridges[record.streamID]; current == record {
		delete(m.bridges, record.streamID)
	}
	m.mu.Unlock()
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
// loopback-only MPEG-TS endpoint to FFmpeg.
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
