package videoingest

import (
	"errors"
	"io"
	"net"
	"net/url"
	"strings"
	"testing"
	"time"

	srt "github.com/datarhei/gosrt"
)

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

func TestEncryptedSRTBridgeForwardsMPEGTSBytesToCredentialFreeLoopback(t *testing.T) {
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

	payload := []byte{0x47, 0x40, 0x00, 0x10, 0xde, 0xad, 0xbe, 0xef}
	if _, err := srtConn.Write(payload); err != nil {
		t.Fatalf("write SRT payload: %v", err)
	}
	got := make([]byte, len(payload))
	if _, err := io.ReadFull(loopback, got); err != nil {
		t.Fatalf("read loopback payload: %v", err)
	}
	if string(got) != string(payload) {
		t.Fatalf("forwarded payload = %x, want %x", got, payload)
	}
}
