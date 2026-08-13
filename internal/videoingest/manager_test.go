package videoingest

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"net"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	srt "github.com/datarhei/gosrt"
	"github.com/example/autostream-encoder-recorder/internal/observability"
)

type recordingReporter struct {
	mu      sync.Mutex
	signals []observability.Signal
}

func (r *recordingReporter) Report(_ context.Context, signal observability.Signal) error {
	r.mu.Lock()
	r.signals = append(r.signals, signal)
	r.mu.Unlock()
	return nil
}

func (r *recordingReporter) has(name string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, signal := range r.signals {
		if signal.Name == name {
			return true
		}
	}
	return false
}

func (r *recordingReporter) find(name string) (observability.Signal, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, signal := range r.signals {
		if signal.Name == name {
			return signal, true
		}
	}
	return observability.Signal{}, false
}

func (r *recordingReporter) snapshot() []observability.Signal {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]observability.Signal(nil), r.signals...)
}

func waitForSignal(t *testing.T, reporter *recordingReporter, name string) observability.Signal {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		if signal, ok := reporter.find(name); ok {
			return signal
		}
		select {
		case <-deadline:
			t.Fatalf("missing diagnostic signal %q", name)
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}
}

func TestConfigFromEnvFailsClosedInProductionWithoutExplicitRoute(t *testing.T) {
	t.Setenv("AUTOSTREAM_ENV", "production")
	t.Setenv("AUTOSTREAM_WORKER_VIDEO_BIND_ADDR", "")
	t.Setenv("AUTOSTREAM_WORKER_VIDEO_ADVERTISE_HOST", "")

	manager := NewManagerFromEnv()
	if _, err := manager.StartBridge("stream-01", "signed-worker-token"); err == nil {
		t.Fatal("production worker video ingest must require an explicit bind address and advertise host")
	}
}

func TestConfigValidationAcceptsFixedOrDynamicBindPortAndOrdinaryHostname(t *testing.T) {
	if err := (Config{BindAddr: "0.0.0.0:0", AdvertiseHost: "studio-encoder.example.com"}).validate(); err != nil {
		t.Fatalf("valid route rejected: %v", err)
	}
	if err := (Config{BindAddr: "0.0.0.0:10080", AdvertiseHost: "studio-encoder.example.com"}).validate(); err != nil {
		t.Fatalf("fixed SRT port must be accepted for host/Docker UDP publication: %v", err)
	}
}

func TestConfigValidationRejectsHostnameBindThatGoSRTCannotListenOn(t *testing.T) {
	err := (Config{BindAddr: "localhost:10080", AdvertiseHost: "encoder.example.com"}).validate()
	if err == nil || !strings.Contains(err.Error(), "literal IP address") {
		t.Fatalf("hostname bind validation error = %v", err)
	}
}

func TestStartBridgeReturnsSecretFreeRouteAndMemoryOnlyCredential(t *testing.T) {
	manager := &Manager{Config: Config{BindAddr: "127.0.0.1:0", AdvertiseHost: "encoder.example.com"}}
	t.Cleanup(func() { manager.StopBridge("stream-01") })

	const token = "ast_ingest_v1.sensitive-payload.sensitive-signature"
	bridge, err := manager.StartBridge("stream-01", token)
	if err != nil {
		t.Fatalf("StartBridge: %v", err)
	}
	if bridge.Passphrase == "" || len(bridge.Passphrase) < 32 || len(bridge.Passphrase) > 79 {
		t.Fatalf("invalid derived passphrase length: %d", len(bridge.Passphrase))
	}
	if bridge.PBKeylen != 32 {
		t.Fatalf("PBKeylen=%d, want 32", bridge.PBKeylen)
	}
	if strings.Contains(bridge.URL, token) || strings.Contains(bridge.URL, bridge.Passphrase) {
		t.Fatalf("advertised URL contains a credential: %q", bridge.URL)
	}
	parsed, err := url.Parse(bridge.URL)
	if err != nil {
		t.Fatalf("parse advertised URL: %v", err)
	}
	if parsed.Scheme != "srt" || parsed.Hostname() != "encoder.example.com" || parsed.Port() == "" || parsed.User != nil || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		t.Fatalf("unexpected advertised URL: %q", bridge.URL)
	}
	if !strings.HasPrefix(bridge.InputURL, "internal_worker_video:tcp://127.0.0.1:") {
		t.Fatalf("unexpected FFmpeg input URL: %q", bridge.InputURL)
	}
	if strings.Contains(bridge.InputURL, token) || strings.Contains(bridge.InputURL, bridge.Passphrase) {
		t.Fatalf("FFmpeg input URL contains a credential: %q", bridge.InputURL)
	}
}

func TestStartBridgeAllowsOnlyOneActiveEncoderPipeline(t *testing.T) {
	manager := &Manager{Config: Config{BindAddr: "127.0.0.1:0", AdvertiseHost: "127.0.0.1"}}
	if _, err := manager.StartBridge("stream-01", "signed-worker-token-01"); err != nil {
		t.Fatalf("start first bridge: %v", err)
	}
	t.Cleanup(func() { manager.StopBridge("stream-01") })

	if _, err := manager.StartBridge("stream-02", "signed-worker-token-02"); !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("second active bridge error = %v, want ErrAlreadyRunning", err)
	}
}

func TestDerivePassphraseIsDomainSeparatedAndDeterministic(t *testing.T) {
	const token = "signed-worker-token"
	first, err := derivePassphrase(token)
	if err != nil {
		t.Fatalf("derivePassphrase: %v", err)
	}
	second, err := derivePassphrase(token)
	if err != nil {
		t.Fatalf("derivePassphrase again: %v", err)
	}
	if first != second {
		t.Fatalf("passphrase is not deterministic: %q != %q", first, second)
	}
	if first == token || strings.Contains(first, token) {
		t.Fatal("derived passphrase exposes the source token")
	}
}

func TestEncryptedSRTBridgePacesWorkerJPEGFramesToCredentialFreeLoopback(t *testing.T) {
	manager := &Manager{Config: Config{BindAddr: "127.0.0.1:0", AdvertiseHost: "127.0.0.1"}}
	bridge, err := manager.StartBridge("stream-01", "signed-worker-token")
	if err != nil {
		t.Fatalf("StartBridge: %v", err)
	}
	t.Cleanup(func() { manager.StopBridge("stream-01") })

	srtURL, err := url.Parse(bridge.URL)
	if err != nil {
		t.Fatal(err)
	}
	config := srt.DefaultConfig()
	config.StreamId = "stream-01"
	config.Passphrase = bridge.Passphrase
	config.PBKeylen = bridge.PBKeylen
	config.Logger = srt.NewLogger(nil)
	srtConn, err := srt.Dial("srt", srtURL.Host, config)
	if err != nil {
		t.Fatalf("dial encrypted SRT bridge: %v", err)
	}
	defer srtConn.Close()

	loopbackURL, ok := ResolveInputTarget(bridge.InputURL)
	if !ok {
		t.Fatalf("invalid loopback input URL: %q", bridge.InputURL)
	}
	loopback, err := net.DialTimeout("tcp", strings.TrimPrefix(loopbackURL, "tcp://"), time.Second)
	if err != nil {
		t.Fatalf("dial loopback bridge: %v", err)
	}
	defer loopback.Close()
	if err := loopback.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}

	input := image.NewRGBA(image.Rect(0, 0, 4, 2))
	input.Set(1, 1, color.RGBA{R: 255, A: 255})
	var payload bytes.Buffer
	if err := jpeg.Encode(&payload, input, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatal(err)
	}
	if _, err := srtConn.Write(payload.Bytes()); err != nil {
		t.Fatalf("write SRT payload: %v", err)
	}
	got, err := jpeg.Decode(bufio.NewReader(loopback))
	if err != nil {
		t.Fatalf("decode loopback JPEG: %v", err)
	}
	if got.Bounds().Dx() != 4 || got.Bounds().Dy() != 2 {
		t.Fatalf("forwarded frame bounds = %v", got.Bounds())
	}
}

func TestBridgeReportsSafeLifecycleDiagnostics(t *testing.T) {
	reporter := &recordingReporter{}
	manager := &Manager{
		Config:   Config{BindAddr: "127.0.0.1:0", AdvertiseHost: "127.0.0.1"},
		Reporter: reporter,
	}
	bridge, err := manager.StartBridge("stream-01", "signed-worker-token")
	if err != nil {
		t.Fatalf("StartBridge: %v", err)
	}
	t.Cleanup(func() { manager.StopBridge("stream-01") })

	srtURL, err := url.Parse(bridge.URL)
	if err != nil {
		t.Fatal(err)
	}
	config := srt.DefaultConfig()
	config.StreamId = "stream-01"
	config.Passphrase = bridge.Passphrase
	config.PBKeylen = bridge.PBKeylen
	config.Logger = srt.NewLogger(nil)
	srtConn, err := srt.Dial("srt", srtURL.Host, config)
	if err != nil {
		t.Fatalf("dial encrypted SRT bridge: %v", err)
	}
	t.Cleanup(func() { _ = srtConn.Close() })

	loopbackURL, ok := ResolveInputTarget(bridge.InputURL)
	if !ok {
		t.Fatalf("invalid loopback input URL: %q", bridge.InputURL)
	}
	loopback, err := net.DialTimeout("tcp", strings.TrimPrefix(loopbackURL, "tcp://"), time.Second)
	if err != nil {
		t.Fatalf("dial loopback bridge: %v", err)
	}
	t.Cleanup(func() { _ = loopback.Close() })
	if err := loopback.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}

	input := image.NewRGBA(image.Rect(0, 0, 4, 2))
	input.Set(1, 1, color.RGBA{R: 255, A: 255})
	var payload bytes.Buffer
	if err := jpeg.Encode(&payload, input, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatal(err)
	}
	if _, err := srtConn.Write(payload.Bytes()); err != nil {
		t.Fatalf("write SRT payload: %v", err)
	}
	if _, err := jpeg.Decode(bufio.NewReader(loopback)); err != nil {
		t.Fatalf("decode loopback JPEG: %v", err)
	}

	waitForSignal(t, reporter, videoIngestSRTAccepted)
	waitForSignal(t, reporter, videoIngestLocalAccepted)
	firstFrame := waitForSignal(t, reporter, videoIngestFirstFrame)
	if firstFrame.Attributes["frame_width"] != 4 || firstFrame.Attributes["frame_height"] != 2 {
		t.Fatalf("unexpected first frame attributes: %#v", firstFrame.Attributes)
	}

	manager.StopBridge("stream-01")
	closed := waitForSignal(t, reporter, videoIngestClosed)
	if closed.Status != "stopped" || closed.Attributes["error_class"] != "bridge_stopped" {
		t.Fatalf("unexpected closed signal: %#v", closed)
	}
	if got, ok := closed.Attributes["frames_received"].(uint64); !ok || got < 1 {
		t.Fatalf("frames_received=%#v, want at least one", closed.Attributes["frames_received"])
	}
	if got, ok := closed.Attributes["frames_forwarded"].(uint64); !ok || got < 1 {
		t.Fatalf("frames_forwarded=%#v, want at least one", closed.Attributes["frames_forwarded"])
	}

	body, err := json.Marshal(reporter.snapshot())
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"signed-worker-token", bridge.Passphrase, "Authorization", "passphrase"} {
		if strings.Contains(string(body), forbidden) {
			t.Fatalf("diagnostic signals leaked %q: %s", forbidden, body)
		}
	}
}
