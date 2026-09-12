package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/example/autostream-encoder-recorder/internal/control"
	"github.com/example/autostream-encoder-recorder/internal/imagefeed"
	"github.com/example/autostream-encoder-recorder/internal/outputrelay"
	"github.com/example/autostream-encoder-recorder/internal/streamproc"
	"github.com/example/autostream-encoder-recorder/internal/version"
	"github.com/example/autostream-encoder-recorder/internal/workerevents"
)

const (
	testStaticRelayBindingID      = "relay-11111111-1111-1111-1111-111111111111"
	testOtherStaticRelayBindingID = "relay-22222222-2222-2222-2222-222222222222"
)

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "autostream-encoder-httpapi-")
	if err != nil {
		panic(err)
	}
	configPath := filepath.Join(dir, "config.yml")
	credentialDir := filepath.Join(dir, "credentials")
	if err := os.Mkdir(credentialDir, 0700); err != nil {
		panic(err)
	}
	if err := os.WriteFile(filepath.Join(credentialDir, "node-listener.json"), []byte(`{"schema_version":2,"service_type":"encoder_recorder","bind_address":"127.0.0.1:18081","config_revision":1}`), 0600); err != nil {
		panic(err)
	}
	body := `panel:
  url: "https://panel.example.jp"
node:
  id: "encoder-recorder-01"
  name: "Encoder Recorder 01"
  type: "encoder_recorder"
listener:
  credential: "node-listener.json"
api:
  host: "encoder.example.jp"
  port: 8443
  ssl_enabled: true
auth:
  token_id: "token-id"
  token: "service-token"
stream_ingest:
  signing_key: "node-config-signing-key"
`
	if err := os.WriteFile(configPath, []byte(body), 0o600); err != nil {
		panic(err)
	}
	if err := os.Setenv("AUTOSTREAM_NODE_CONFIG", configPath); err != nil {
		panic(err)
	}
	if err := os.Setenv("CREDENTIALS_DIRECTORY", credentialDir); err != nil {
		panic(err)
	}
	if err := os.Setenv("AUTOSTREAM_OUTPUT_RELAY_MODE", outputrelay.ModeDirect); err != nil {
		panic(err)
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

func testInputResolver(ctx context.Context, host string) ([]net.IP, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return []net.IP{net.ParseIP("93.184.216.34")}, nil
}

func testYouTubeRuntimeProvider(streamIDs ...string) RuntimeConfigProvider {
	return func(context.Context) (control.RuntimeConfig, error) {
		items := make([]control.RuntimeYouTubeStreamConfig, 0, len(streamIDs))
		for _, streamID := range streamIDs {
			items = append(items, control.RuntimeYouTubeStreamConfig{
				StreamID:       streamID,
				AssignmentRole: "primary",
				Ready:          true,
				YouTubeConfig: map[string]any{
					"mode":                   "stream_key",
					"rtmp_url":               "rtmps://youtube.example.com/live2",
					"stream_key_secret_name": "youtube_stream_key:" + streamID,
				},
			})
		}
		return control.RuntimeConfig{StreamYouTubeConfigs: items}, nil
	}
}

func testYouTubeSecretResolver(context.Context, string, string, string) (string, error) {
	return "test-runtime-stream-key", nil
}

func newV2TestServer(t *testing.T, processManager *streamproc.Manager, streamIDs ...string) http.Handler {
	t.Helper()
	root := t.TempDir()
	if processManager != nil && strings.TrimSpace(processManager.ArchiveRoot) != "" {
		root = processManager.ArchiveRoot
	}
	return newV2TestServerWithManagers(t, processManager, workerevents.NewManager(root), TokenVerifier{PlainToken: "service-token"}, streamIDs...)
}

func newV2TestServerWithManagers(t *testing.T, processManager *streamproc.Manager, eventManager *workerevents.Manager, verifier TokenVerifier, streamIDs ...string) http.Handler {
	t.Helper()
	return NewServerWithManagersAndRuntimeConfig(
		"encoder_recorder",
		processManager,
		eventManager,
		verifier,
		testYouTubeSecretResolver,
		testYouTubeRuntimeProvider(streamIDs...),
	)
}

type httpCoverWitness struct {
	calls    int
	failNext bool
}

func (w *httpCoverWitness) Apply(_ context.Context, source *imagefeed.Source, frame []byte, initial bool, _ string) error {
	w.calls++
	if !initial {
		if err := source.Update(frame); err != nil {
			return err
		}
	}
	if w.failNext {
		w.failNext = false
		return errors.New("injected cover witness failure")
	}
	return nil
}

func TestUpdaterVersionEndpointIsUnauthenticated(t *testing.T) {
	originalVersion := version.Version
	version.Version = "v1.2.3"
	t.Cleanup(func() {
		version.Version = originalVersion
	})
	configPath := filepath.Join(t.TempDir(), "config.yml")
	writeNodeConfigForVerifierTest(t, configPath, control.ServiceType)
	t.Setenv("AUTOSTREAM_NODE_CONFIG", configPath)
	t.Setenv("SERVICE_ID", "legacy-env-service-id")
	t.Setenv("SERVICE_VERSION", "v9.9.9")

	handler := NewServerWithManagers(
		"encoder_recorder",
		nil,
		workerevents.NewManager(t.TempDir()),
		TokenVerifier{PlainToken: "service-token"},
	)
	req := httptest.NewRequest(http.MethodGet, "/updater/version", nil)
	res := httptest.NewRecorder()

	handler.ServeHTTP(res, req)

	if res.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", res.Code, res.Body.String())
	}
	if got := res.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q", got)
	}
	if got := res.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q", got)
	}
	var body map[string]any
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(body) != 4 ||
		body["version"] != version.Current() ||
		body["service_id"] != "encoder-recorder-01" ||
		body["service_type"] != control.ServiceType ||
		body["config_revision"] != float64(7) {
		t.Fatalf("body = %#v, want updater identity for configured service", body)
	}

	methodReq := httptest.NewRequest(http.MethodPost, "/updater/version", nil)
	methodRes := httptest.NewRecorder()
	handler.ServeHTTP(methodRes, methodReq)
	if methodRes.Code != http.StatusMethodNotAllowed {
		t.Fatalf("updater version POST status = %d body = %s", methodRes.Code, methodRes.Body.String())
	}
	if got := methodRes.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("updater version POST cache control = %q", got)
	}
	if got := methodRes.Header().Get("Allow"); !strings.Contains(got, http.MethodGet) {
		t.Fatalf("updater version POST Allow = %q", got)
	}
}

func TestNewServerFailsClosedForInvalidListenerConfigRevision(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yml")
	writeNodeConfigForVerifierTest(t, configPath, control.ServiceType)
	writeNodeListenerCredentialForVerifierTest(t, configPath, control.ServiceType, "0")
	t.Setenv("AUTOSTREAM_NODE_CONFIG", configPath)
	defer func() {
		if recover() == nil {
			t.Fatal("NewServer must reject an invalid listener config_revision")
		}
	}()
	_ = NewServerWithManagers(
		control.ServiceType,
		nil,
		workerevents.NewManager(t.TempDir()),
		TokenVerifier{},
	)
}

func TestNewServerFailsClosedForInvalidNodeIdentity(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yml")
	writeNodeConfigForVerifierTest(t, configPath, "worker")
	t.Setenv("AUTOSTREAM_NODE_CONFIG", configPath)
	defer func() {
		if recover() == nil {
			t.Fatal("NewServer must reject a node config for a different service type")
		}
	}()
	_ = NewServer(control.ServiceType)
}

func TestUpdaterVersionFailsClosedWhenIdentityDriftsAfterConstruction(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yml")
	writeNodeConfigForVerifierTest(t, configPath, control.ServiceType)
	t.Setenv("AUTOSTREAM_NODE_CONFIG", configPath)
	handler := NewServerWithManagers(
		control.ServiceType,
		nil,
		workerevents.NewManager(t.TempDir()),
		TokenVerifier{},
	)
	writeNodeListenerCredentialForVerifierTest(t, configPath, control.ServiceType, "8")

	req := httptest.NewRequest(http.MethodGet, "/updater/version", nil)
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)

	if res.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d body = %s, want 503", res.Code, res.Body.String())
	}
	if got := res.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("drift cache control = %q", got)
	}
	if strings.Contains(res.Body.String(), "service_id") {
		t.Fatalf("drift response leaked service identity: %s", res.Body.String())
	}
}

func hasPreflightCheck(checks []struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}, id, status string) bool {
	for _, check := range checks {
		if check.ID == id && check.Status == status {
			return true
		}
	}
	return false
}

func getAudioStatus(t *testing.T, handler http.Handler) struct {
	BridgeActive     bool    `json:"bridge_active"`
	PacketsTotal     int64   `json:"packets_total"`
	RTPForwarded     int64   `json:"rtp_forwarded"`
	LastPacketAgeSec float64 `json:"last_packet_age_sec"`
} {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/streams/stream-01/audio-status", nil)
	req.Header.Set("Authorization", "Bearer service-token")
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("audio status = %d body = %s", res.Code, res.Body.String())
	}
	var status struct {
		BridgeActive     bool    `json:"bridge_active"`
		PacketsTotal     int64   `json:"packets_total"`
		RTPForwarded     int64   `json:"rtp_forwarded"`
		LastPacketAgeSec float64 `json:"last_packet_age_sec"`
	}
	if err := json.NewDecoder(res.Body).Decode(&status); err != nil {
		t.Fatal(err)
	}
	return status
}

func writeNodeConfigForVerifierTest(t *testing.T, path, nodeType string) {
	t.Helper()
	writeNodeConfigForVerifierTestWithValues(t, path, nodeType, "runtime-secret", "node-config-signing-key")
}

func writeNodeConfigForVerifierTestWithValues(t *testing.T, path, nodeType, runtimeToken, signingKey string) {
	t.Helper()
	writeNodeListenerCredentialForVerifierTest(t, path, nodeType, "7")
	body := `panel:
  url: "https://panel.example.jp"
node:
  id: "encoder-recorder-01"
  name: "Encoder Recorder 01"
  type: "` + nodeType + `"
listener:
  credential: "node-listener.json"
api:
  host: "encoder.example.jp"
  port: 8443
  ssl_enabled: true
auth:
  token_id: "token-id"
  token: "` + runtimeToken + `"
stream_ingest:
  signing_key: "` + signingKey + `"
`
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
}

func writeNodeListenerCredentialForVerifierTest(t *testing.T, configPath, serviceType, revision string) {
	t.Helper()
	credentialDir := filepath.Join(filepath.Dir(configPath), "credentials")
	if err := os.MkdirAll(credentialDir, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CREDENTIALS_DIRECTORY", credentialDir)
	body := `{"schema_version":2,"service_type":"` + serviceType + `","bind_address":"127.0.0.1:18081","config_revision":` + revision + `}`
	if err := os.WriteFile(filepath.Join(credentialDir, "node-listener.json"), []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
}

func attrsString(attrs map[string]any) string {
	out := ""
	for key, value := range attrs {
		if text, ok := value.(string); ok {
			out += key + "=" + text + ";"
		}
	}
	return out
}

type httpFakeStarter struct {
	process *httpFakeProcess
	bin     string
	args    []string
}

type httpBlockingStarter struct {
	started chan struct{}
	release chan struct{}
}

func (s *httpBlockingStarter) Start(ctx context.Context, bin string, args []string) (streamproc.RunningProcess, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	close(s.started)
	<-s.release
	return &httpFakeProcess{done: make(chan error, 1)}, nil
}

func (s *httpFakeStarter) Start(ctx context.Context, bin string, args []string) (streamproc.RunningProcess, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.bin = bin
	s.args = append([]string(nil), args...)
	s.process = &httpFakeProcess{done: make(chan error, 1)}
	return s.process, nil
}

type httpFakeProcess struct {
	done chan error
}

func (p *httpFakeProcess) PID() int {
	return 4321
}

func (p *httpFakeProcess) Wait() error {
	return <-p.done
}

func (p *httpFakeProcess) Terminate() error {
	p.done <- nil
	return nil
}

func (p *httpFakeProcess) Kill() error {
	p.done <- nil
	return nil
}
