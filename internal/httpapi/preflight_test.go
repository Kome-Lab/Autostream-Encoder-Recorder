package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/example/autostream-encoder-recorder/internal/outputrelay"
)

func TestPreflightEndpointReportsReadinessWithoutLeakingSecrets(t *testing.T) {
	t.Setenv("SERVICE_CONTROL_TOKEN", "service-token")
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	credentialPath := filepath.Join(root, "google-service-account.json")
	if err := os.WriteFile(credentialPath, []byte(`{"client_email":"service-account@example.com"}`), 0o640); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FFMPEG_BIN", exe)
	t.Setenv("AUTOSTREAM_ARCHIVE_DIR", filepath.Join(root, "archives"))
	t.Setenv("GOOGLE_DRIVE_AUTH_MODE", "service_account")
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", credentialPath)
	t.Setenv("GOOGLE_DRIVE_FOLDER_ID", "drive-folder-id")
	t.Setenv("OBSERVABILITY_URL", "https://observability.example.com")
	t.Setenv("OBSERVABILITY_TOKEN", "observability-secret-token")

	handler := NewServer("encoder_recorder")
	req := httptest.NewRequest(http.MethodGet, "/preflight", nil)
	req.Header.Set("Authorization", "Bearer service-token")
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", res.Code, res.Body.String())
	}
	var body struct {
		Ready  bool `json:"ready"`
		Checks []struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"checks"`
	}
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if !body.Ready {
		t.Fatalf("expected ready preflight, got %#v", body.Checks)
	}
	for _, secret := range []string{"super-secret-stream-key", "observability-secret-token", credentialPath, "drive-folder-id"} {
		if strings.Contains(res.Body.String(), secret) {
			t.Fatalf("preflight leaked secret/config value %q: %s", secret, res.Body.String())
		}
	}
	if !hasPreflightCheck(body.Checks, "ffmpeg_binary", "ok") || !hasPreflightCheck(body.Checks, "archive_root", "ok") || !hasPreflightCheck(body.Checks, "google_drive", "unsupported_env_fallback") {
		t.Fatalf("missing expected checks: %#v", body.Checks)
	}
}

func TestPreflightEndpointRequiresControlPanelYouTubeRuntimeConfig(t *testing.T) {
	t.Setenv("SERVICE_CONTROL_TOKEN", "service-token")
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("FFMPEG_BIN", exe)
	t.Setenv("AUTOSTREAM_ARCHIVE_DIR", t.TempDir())

	handler := NewServer("encoder_recorder")
	req := httptest.NewRequest(http.MethodGet, "/preflight", nil)
	req.Header.Set("Authorization", "Bearer service-token")
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", res.Code, res.Body.String())
	}
	var body struct {
		Ready  bool `json:"ready"`
		Checks []struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"checks"`
	}
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if !body.Ready {
		t.Fatalf("expected ready host preflight without YouTube env fallback because stream runtime config is Control Panel-managed: %#v", body.Checks)
	}
	if !hasPreflightCheck(body.Checks, "youtube_rtmp_url", "runtime_config_required") || !hasPreflightCheck(body.Checks, "youtube_stream_key", "runtime_config_required") {
		t.Fatalf("missing YouTube runtime config checks: %#v", body.Checks)
	}
	if !hasPreflightCheck(body.Checks, "output_relay", "ok") {
		t.Fatalf("missing canonical direct output check: %#v", body.Checks)
	}
}

func TestPreflightEndpointAcceptsExplicitDirectOutputInProduction(t *testing.T) {
	t.Setenv("SERVICE_CONTROL_TOKEN", "service-token")
	t.Setenv("AUTOSTREAM_ENV", "production")
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("FFMPEG_BIN", exe)
	t.Setenv("AUTOSTREAM_ARCHIVE_DIR", t.TempDir())

	handler := NewServer("encoder_recorder")
	req := httptest.NewRequest(http.MethodGet, "/preflight", nil)
	req.Header.Set("Authorization", "Bearer service-token")
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", res.Code, res.Body.String())
	}
	var body struct {
		Ready  bool `json:"ready"`
		Checks []struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"checks"`
	}
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if !body.Ready {
		t.Fatalf("expected production preflight to allow explicitly configured direct output: %#v", body.Checks)
	}
	if !hasPreflightCheck(body.Checks, "output_relay", "ok") {
		t.Fatalf("missing explicit direct output check: %#v", body.Checks)
	}
}

func TestPreflightEndpointRequiresOutputRelayWhenFlagSet(t *testing.T) {
	t.Setenv("SERVICE_CONTROL_TOKEN", "service-token")
	t.Setenv("AUTOSTREAM_REQUIRE_OUTPUT_RELAY", "true")
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("FFMPEG_BIN", exe)
	t.Setenv("AUTOSTREAM_ARCHIVE_DIR", t.TempDir())

	handler := NewServer("encoder_recorder")
	req := httptest.NewRequest(http.MethodGet, "/preflight", nil)
	req.Header.Set("Authorization", "Bearer service-token")
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", res.Code, res.Body.String())
	}
	var body struct {
		Ready  bool `json:"ready"`
		Checks []struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"checks"`
	}
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Ready {
		t.Fatalf("expected output relay flag to make preflight fail without relay: %#v", body.Checks)
	}
	if !hasPreflightCheck(body.Checks, "output_relay", "missing") {
		t.Fatalf("missing output relay requirement failure: %#v", body.Checks)
	}
}

func TestPreflightEndpointAcceptsLoopbackOutputRelayInProduction(t *testing.T) {
	t.Setenv("SERVICE_CONTROL_TOKEN", "service-token")
	t.Setenv("AUTOSTREAM_ENV", "production")
	t.Setenv("AUTOSTREAM_OUTPUT_RELAY_URL", "rtmp://127.0.0.1/autostream/{stream_id}")
	t.Setenv("AUTOSTREAM_OUTPUT_RELAY_MODE", outputrelay.ModeManagedLiveAPI)
	t.Setenv("AUTOSTREAM_OUTPUT_RELAY_BINDING_ID", testStaticRelayBindingID)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("FFMPEG_BIN", exe)
	t.Setenv("AUTOSTREAM_ARCHIVE_DIR", t.TempDir())

	handler := NewServer("encoder_recorder")
	req := httptest.NewRequest(http.MethodGet, "/preflight", nil)
	req.Header.Set("Authorization", "Bearer service-token")
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", res.Code, res.Body.String())
	}
	var body struct {
		Ready  bool `json:"ready"`
		Checks []struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"checks"`
	}
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if !body.Ready {
		t.Fatalf("expected ready production preflight with loopback relay: %#v", body.Checks)
	}
	if !hasPreflightCheck(body.Checks, "output_relay", "ok") {
		t.Fatalf("missing output relay ok check: %#v", body.Checks)
	}
}

func TestOutputRelayPreflightUsesExplicitModeContract(t *testing.T) {
	t.Setenv("AUTOSTREAM_ENV", "development")
	for _, tt := range []struct {
		name         string
		url          string
		mode         string
		binding      string
		requireRelay bool
		composeRelay bool
		wantStatus   string
	}{
		{name: "url without explicit mode is invalid", url: "rtmp://127.0.0.1/autostream/{stream_id}", wantStatus: "invalid"},
		{name: "required relay missing is not direct", mode: "direct", requireRelay: true, wantStatus: "missing"},
		{name: "static needs binding", url: "rtmp://127.0.0.1/autostream/{stream_id}", mode: "live_api_relay_static", wantStatus: "invalid"},
		{name: "static rejects generic binding", url: "rtmp://127.0.0.1/autostream/{stream_id}", mode: "live_api_relay_static", binding: "relay-binding-static", wantStatus: "invalid"},
		{name: "static rejects whitespace binding", url: "rtmp://127.0.0.1/autostream/{stream_id}", mode: "live_api_relay_static", binding: " " + testStaticRelayBindingID, wantStatus: "invalid"},
		{name: "static with canonical binding is ready", url: "rtmp://127.0.0.1/autostream/{stream_id}", mode: "live_api_relay_static", binding: testStaticRelayBindingID, wantStatus: "ok"},
		{name: "non-loopback relay is invalid", url: "rtmp://relay.example.com/autostream/{stream_id}", wantStatus: "invalid"},
		{name: "compose relay without explicit identity is invalid", url: "rtmp://output-relay:1935/autostream/{stream_id}", wantStatus: "invalid"},
		{name: "compose relay with explicit identity is ready", url: "rtmp://output-relay:1935/autostream/{stream_id}", mode: "live_api_relay_static", binding: testStaticRelayBindingID, composeRelay: true, wantStatus: "ok"},
		{name: "url with direct is invalid", url: "rtmp://127.0.0.1/autostream/{stream_id}", mode: "direct", wantStatus: "invalid"},
		{name: "url-free static is invalid", mode: "live_api_relay_static", binding: testStaticRelayBindingID, wantStatus: "invalid"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("AUTOSTREAM_OUTPUT_RELAY_URL", tt.url)
			t.Setenv("AUTOSTREAM_OUTPUT_RELAY_MODE", tt.mode)
			t.Setenv("AUTOSTREAM_OUTPUT_RELAY_BINDING_ID", tt.binding)
			if tt.requireRelay {
				t.Setenv("AUTOSTREAM_REQUIRE_OUTPUT_RELAY", "true")
			} else {
				t.Setenv("AUTOSTREAM_REQUIRE_OUTPUT_RELAY", "false")
			}
			if tt.composeRelay {
				t.Setenv("AUTOSTREAM_COMPOSE_OUTPUT_RELAY", "1")
			} else {
				t.Setenv("AUTOSTREAM_COMPOSE_OUTPUT_RELAY", "")
			}
			if got := outputRelayPreflight().Status; got != tt.wantStatus {
				t.Fatalf("preflight status=%q want %q", got, tt.wantStatus)
			}
		})
	}
}

func TestPathInsideRootRejectsSiblingPrefix(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "archives")
	inside := filepath.Join(root, "tmp", "stream-01", "final.mkv")
	sibling := filepath.Join(parent, "archives-sibling", "probe")
	parentPath := filepath.Join(root, "..", "outside")

	if !pathInsideRoot(root, inside) {
		t.Fatalf("expected inside path to be accepted: root=%s path=%s", root, inside)
	}
	if pathInsideRoot(root, sibling) {
		t.Fatalf("sibling prefix path must be rejected: root=%s path=%s", root, sibling)
	}
	if pathInsideRoot(root, parentPath) {
		t.Fatalf("parent traversal path must be rejected: root=%s path=%s", root, parentPath)
	}
}
