package control

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/example/autostream-encoder-recorder/internal/observability"
	"github.com/example/autostream-encoder-recorder/internal/version"
)

const testStaticRelayBindingID = "relay-11111111-1111-1111-1111-111111111111"

func TestRegisterPostsServiceRegistration(t *testing.T) {
	var gotAuth string
	var got Registration
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/services/register" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		gotAuth = r.Header.Get("Authorization")
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()

	client := Client{Config: Config{ControlPanelURL: server.URL, Token: "secret-token", ServiceID: "enc-01", ServiceName: "Encoder 01", ServicePublicURL: "https://encoder.example.com", Version: "0.1.0"}}
	if err := client.Register(t.Context()); err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer secret-token" {
		t.Fatalf("unexpected auth header: %s", gotAuth)
	}
	if got.ServiceType != ServiceType || got.ServiceID != "enc-01" || got.Capabilities["remux_final_mp4"] != true {
		t.Fatalf("unexpected registration: %#v", got)
	}
	if got.OS != runtime.GOOS || got.Arch != runtime.GOARCH {
		t.Fatalf("registration did not include runtime platform: %#v", got)
	}
	if got.Commit != version.Commit || got.BuildDate != version.BuildDate {
		t.Fatalf("registration did not include build metadata: %#v", got)
	}
}

func TestHeartbeatPostsStatus(t *testing.T) {
	var got Heartbeat
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/services/heartbeat" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()

	client := Client{Config: Config{ControlPanelURL: server.URL, Token: "secret-token", ServiceID: "enc-01", ServiceName: "Encoder 01", ServicePublicURL: "https://encoder.example.com"}}
	if err := client.HeartbeatWithMetrics(t.Context(), "", "stream-01", map[string]float64{"encoder.process_alive": 1}); err != nil {
		t.Fatal(err)
	}
	if got.Status != "online" || got.CurrentStreamID != "stream-01" || got.Metrics["encoder.process_alive"] != 1 {
		t.Fatalf("unexpected heartbeat: %#v", got)
	}
	if got.Metrics["node.cpu_count"] <= 0 || got.Metrics["process.heap_alloc_bytes"] <= 0 || got.Metrics["process.uptime_seconds"] < 0 {
		t.Fatalf("heartbeat did not include host/process metrics: %#v", got.Metrics)
	}
	if got.OS != runtime.GOOS || got.Arch != runtime.GOARCH || got.Capabilities["package_endpoint"] != true {
		t.Fatalf("heartbeat did not include platform/capabilities: %#v", got)
	}
	if got.Commit != version.Commit || got.BuildDate != version.BuildDate {
		t.Fatalf("heartbeat did not include build metadata: %#v", got)
	}
}

func TestServiceCapabilitiesAdvertiseNonSecretOutputRelayMode(t *testing.T) {
	t.Setenv("AUTOSTREAM_ENV", "development")
	t.Setenv("AUTOSTREAM_REQUIRE_OUTPUT_RELAY", "false")
	t.Setenv("AUTOSTREAM_COMPOSE_OUTPUT_RELAY", "")
	t.Setenv("AUTOSTREAM_OUTPUT_RELAY_URL", "rtmp://127.0.0.1/autostream/{stream_id}")
	t.Setenv("AUTOSTREAM_OUTPUT_RELAY_MODE", "")
	t.Setenv("AUTOSTREAM_OUTPUT_RELAY_BINDING_ID", testStaticRelayBindingID)
	capabilities := serviceCapabilities()
	if got := capabilities["output_relay_mode"]; got != "legacy_stream_key" {
		t.Fatalf("legacy output relay capability=%#v", capabilities)
	}
	if _, ok := capabilities["output_relay_binding_id"]; ok {
		t.Fatalf("legacy output relay must not advertise a static binding: %#v", capabilities)
	}

	t.Setenv("AUTOSTREAM_OUTPUT_RELAY_MODE", "live_api_static")
	capabilities = serviceCapabilities()
	if got := capabilities["output_relay_mode"]; got != "live_api_static" {
		t.Fatalf("static output relay capability=%#v", capabilities)
	}
	if got := capabilities["output_relay_binding_id"]; got != testStaticRelayBindingID {
		t.Fatalf("static output relay binding capability=%#v", capabilities)
	}
	encoded, err := json.Marshal(capabilities)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "127.0.0.1") || strings.Contains(string(encoded), "autostream/{stream_id}") {
		t.Fatalf("output relay capability leaked relay address: %s", encoded)
	}

	t.Setenv("AUTOSTREAM_OUTPUT_RELAY_URL", "")
	t.Setenv("AUTOSTREAM_OUTPUT_RELAY_MODE", "direct")
	// A stale environment binding without a local relay must not make a
	// direct-output service look statically bound.
	if got := serviceCapabilities()["output_relay_mode"]; got != "direct" {
		t.Fatalf("direct output capability=%#v", serviceCapabilities())
	}
	if _, ok := serviceCapabilities()["output_relay_binding_id"]; ok {
		t.Fatalf("direct output capability must not advertise an empty relay binding: %#v", serviceCapabilities())
	}

	t.Setenv("AUTOSTREAM_REQUIRE_OUTPUT_RELAY", "true")
	if _, ok := serviceCapabilities()["output_relay_mode"]; ok {
		t.Fatalf("missing required relay must omit output mode: %#v", serviceCapabilities())
	}
	if _, ok := serviceCapabilities()["output_relay_binding_id"]; ok {
		t.Fatalf("missing required relay must omit output binding: %#v", serviceCapabilities())
	}

	t.Setenv("AUTOSTREAM_REQUIRE_OUTPUT_RELAY", "false")
	t.Setenv("AUTOSTREAM_OUTPUT_RELAY_URL", "rtmp://127.0.0.1/autostream/{stream_id}")
	t.Setenv("AUTOSTREAM_OUTPUT_RELAY_MODE", "managed")
	if _, ok := serviceCapabilities()["output_relay_mode"]; ok {
		t.Fatalf("invalid relay configuration must omit output mode: %#v", serviceCapabilities())
	}
	if _, ok := serviceCapabilities()["output_relay_binding_id"]; ok {
		t.Fatalf("invalid relay configuration must not advertise a binding: %#v", serviceCapabilities())
	}

	t.Setenv("AUTOSTREAM_OUTPUT_RELAY_MODE", "")
	t.Setenv("AUTOSTREAM_OUTPUT_RELAY_BINDING_ID", "")
	t.Setenv("AUTOSTREAM_OUTPUT_RELAY_URL", "rtmp://relay.example.com/autostream/{stream_id}")
	if _, ok := serviceCapabilities()["output_relay_mode"]; ok {
		t.Fatalf("non-loopback relay must omit output mode: %#v", serviceCapabilities())
	}
	t.Setenv("AUTOSTREAM_OUTPUT_RELAY_URL", "rtmp://output-relay:1935/autostream/{stream_id}")
	if _, ok := serviceCapabilities()["output_relay_mode"]; ok {
		t.Fatalf("compose relay without explicit identity must omit output mode: %#v", serviceCapabilities())
	}
	t.Setenv("AUTOSTREAM_COMPOSE_OUTPUT_RELAY", "1")
	if got := serviceCapabilities()["output_relay_mode"]; got != "legacy_stream_key" {
		t.Fatalf("explicit Compose relay capability=%#v", serviceCapabilities())
	}

	t.Setenv("AUTOSTREAM_COMPOSE_OUTPUT_RELAY", "")
	t.Setenv("AUTOSTREAM_OUTPUT_RELAY_MODE", "live_api_static")
	t.Setenv("AUTOSTREAM_OUTPUT_RELAY_BINDING_ID", "relay-binding-static")
	if _, ok := serviceCapabilities()["output_relay_mode"]; ok {
		t.Fatalf("invalid static binding must omit output mode: %#v", serviceCapabilities())
	}
	if _, ok := serviceCapabilities()["output_relay_binding_id"]; ok {
		t.Fatalf("invalid static binding must not advertise a binding: %#v", serviceCapabilities())
	}

	t.Setenv("AUTOSTREAM_OUTPUT_RELAY_URL", "")
	t.Setenv("AUTOSTREAM_OUTPUT_RELAY_BINDING_ID", testStaticRelayBindingID)
	if _, ok := serviceCapabilities()["output_relay_mode"]; ok {
		t.Fatalf("URL-free static configuration must omit output mode: %#v", serviceCapabilities())
	}
	if _, ok := serviceCapabilities()["output_relay_binding_id"]; ok {
		t.Fatalf("URL-free static configuration must omit a binding: %#v", serviceCapabilities())
	}
}

func TestReportArtifactsPostsLogicalPathsOnly(t *testing.T) {
	var got ArtifactReport
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/services/stream-artifacts" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()

	client := Client{Config: Config{ControlPanelURL: server.URL, Token: "secret-token", ServiceID: "enc-01", ServiceName: "Encoder 01", ServicePublicURL: "https://encoder.example.com"}}
	artifacts := []Artifact{{Kind: "archive", Name: "final.mp4", RelativePath: "final/stream-01/final.mp4", SizeBytes: 123}}
	if err := client.ReportArtifacts(t.Context(), "stream-01", artifacts); err != nil {
		t.Fatal(err)
	}
	if got.ServiceID != "enc-01" || got.StreamID != "stream-01" || len(got.Artifacts) != 1 {
		t.Fatalf("unexpected artifact report: %#v", got)
	}
	body, _ := json.Marshal(got)
	if strings.Contains(string(body), `C:\`) || strings.Contains(string(body), "/var/lib/") {
		t.Fatalf("local path leaked in artifact report: %s", body)
	}
}

func TestReportSignalPostsViaControlPanel(t *testing.T) {
	var gotAuth string
	var got observability.Signal
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/services/observability/signals" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		gotAuth = r.Header.Get("Authorization")
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()

	client := Client{Config: Config{ControlPanelURL: server.URL, Token: "node-token", ServiceID: "enc-01", ServiceName: "Encoder 01", ServicePublicURL: server.URL}}
	value := 60.0
	if err := client.Report(t.Context(), observability.Signal{Type: "metric", Name: "encoder.output_fps", StreamID: "stream-01", Value: &value}); err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer node-token" {
		t.Fatalf("unexpected auth header: %q", gotAuth)
	}
	if got.Type != "metric" || got.Name != "encoder.output_fps" || got.Value == nil || *got.Value != 60 {
		t.Fatalf("unexpected signal: %#v", got)
	}
}

func TestRuntimeConfigFetchesScopedServiceConfig(t *testing.T) {
	var gotAuth string
	var gotQuery string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/services/runtime-config" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		gotAuth = r.Header.Get("Authorization")
		gotQuery = r.URL.Query().Get("service_id")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"service":{"service_id":"enc-01","service_type":"encoder_recorder","service_name":"Encoder 01","status":"assigned"},
			"assignments":[{"stream_id":"stream-01","service_id":"enc-01","service_type":"encoder_recorder","assignment_role":"primary","assigned_at":"2026-06-10T00:00:00Z"}],
			"profiles":{"encoder":[{"id":"profile-01","kind":"encoder","name":"1080p60","config":{"service_id":"enc-01","video_bitrate_kbps":8000,"audio_bitrate_kbps":160},"created_at":"2026-06-10T00:00:00Z","updated_at":"2026-06-10T00:00:00Z"}]},
			"stream_youtube_configs":[{"stream_id":"stream-01","assignment_role":"primary","youtube_output_id":"youtube-output-01","ready":true,"youtube_config":{"mode":"stream_key","rtmp_url":"rtmps://a.rtmps.youtube.com/live2","stream_key_secret_name":"youtube_stream_key_youtube-output-01","complete_on_stop":true},"active_runtime":{"mode":"stream_key","stream_key_secret_name":"youtube_stream_key_runtime_stream-01","complete_on_stop":true}}],
			"stream_archive_configs":[
				{"stream_id":"stream-01","assignment_role":"standby","archive_profile_id":"archive-profile-standby","ready":true,"archive_config":{"auth_mode":"service_account","folder_id_secret_name":"drive_destination:standby:folder_id"}},
				{"stream_id":"stream-01","assignment_role":"primary","archive_profile_id":"archive-profile-01","ready":true,"archive_config":{"drive_destination_id":"drive-destination-01","archive_profile_id":"archive-profile-01","auth_mode":"oauth2","oauth_account_id":"account-01","oauth_provider_id":"provider-01","folder_id_secret_name":"drive_destination:drive-destination-01:folder_id","base_path":"AutoStream/Archives","shared_drive":true,"shared_drive_id":"shared-drive-01","archive_file_name":"Council Meeting.mp4","retention_days":45,"client_id":"google-client-id","client_secret_secret_name":"oauth_provider:provider-01:client_secret","refresh_token_secret_name":"oauth_account:account-01:refresh_token"}}
			]
		}`))
	}))
	defer server.Close()

	client := Client{Config: Config{ControlPanelURL: server.URL, Token: "secret-token", ServiceID: "enc-01", ServiceName: "Encoder 01", ServicePublicURL: "https://encoder.example.com"}}
	cfg, err := client.RuntimeConfig(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer secret-token" || gotQuery != "enc-01" {
		t.Fatalf("unexpected request auth=%q query=%q", gotAuth, gotQuery)
	}
	if cfg.Service.ServiceID != "enc-01" || len(cfg.Assignments) != 1 {
		t.Fatalf("unexpected runtime config: %#v", cfg)
	}
	profiles := cfg.Profiles["encoder"]
	if len(profiles) != 1 || profiles[0].Config["video_bitrate_kbps"] != float64(8000) {
		t.Fatalf("unexpected runtime profiles: %#v", profiles)
	}
	youtubeConfig, ok := cfg.YouTubeConfigForStream("stream-01")
	if !ok {
		t.Fatalf("expected youtube runtime config: %#v", cfg.StreamYouTubeConfigs)
	}
	if youtubeConfig.Mode() != "stream_key" || youtubeConfig.RTMPURL() != "rtmps://a.rtmps.youtube.com/live2" {
		t.Fatalf("unexpected youtube runtime config fields: %#v", youtubeConfig)
	}
	if mode, ok := cfg.YouTubeOutputModeForStream("stream-01"); !ok || mode != "stream_key" {
		t.Fatalf("unexpected nonsecret YouTube output mode: mode=%q ok=%v", mode, ok)
	}
	if youtubeConfig.StreamKeySecretName() != "youtube_stream_key_runtime_stream-01" || !youtubeConfig.CompleteOnStop() {
		t.Fatalf("unexpected youtube runtime secret reference: %#v", youtubeConfig)
	}
	archiveConfig, ok := cfg.ArchiveConfigForStream("stream-01")
	if !ok {
		t.Fatalf("expected archive runtime config: %#v", cfg.StreamArchiveConfigs)
	}
	if archiveConfig.ArchiveProfileIDValue() != "archive-profile-01" || archiveConfig.AuthMode() != "oauth2" || archiveConfig.DriveDestinationID() != "drive-destination-01" {
		t.Fatalf("unexpected archive runtime config fields: %#v", archiveConfig)
	}
	if archiveConfig.FolderIDSecretName() != "drive_destination:drive-destination-01:folder_id" ||
		archiveConfig.ClientSecretSecretName() != "oauth_provider:provider-01:client_secret" ||
		archiveConfig.RefreshTokenSecretName() != "oauth_account:account-01:refresh_token" {
		t.Fatalf("unexpected archive runtime secret references: %#v", archiveConfig)
	}
	if archiveConfig.OAuthAccountID() != "account-01" || archiveConfig.OAuthProviderID() != "provider-01" || archiveConfig.ClientID() != "google-client-id" {
		t.Fatalf("unexpected archive runtime OAuth references: %#v", archiveConfig)
	}
	if sharedDrive, ok := archiveConfig.SharedDrive(); !ok || !sharedDrive {
		t.Fatalf("expected shared drive archive runtime config: %#v", archiveConfig)
	}
	if archiveConfig.SharedDriveID() != "shared-drive-01" || archiveConfig.ArchiveFileName() != "Council Meeting.mp4" {
		t.Fatalf("unexpected archive runtime file/shared drive id config: %#v", archiveConfig)
	}
	if archiveConfig.RetentionDays() != 45 {
		t.Fatalf("unexpected archive runtime retention days: %#v", archiveConfig)
	}
	body, _ := json.Marshal(cfg)
	for _, rawSecret := range []string{`"stream_key":`, "raw-youtube-stream-key", "raw-drive-folder-id", "raw-google-client-secret", "raw-google-refresh-token"} {
		if strings.Contains(string(body), rawSecret) {
			t.Fatalf("runtime config must not include raw secret material %q: %s", rawSecret, body)
		}
	}
}

func TestRuntimeYouTubeOutputPolicyUsesTrustedProfileModeAndRelayBinding(t *testing.T) {
	for _, tt := range []struct {
		name        string
		youtube     map[string]any
		profile     map[string]any
		ready       bool
		wantMode    string
		wantBinding string
		wantReady   bool
	}{
		{
			name:        "specialized runtime config",
			youtube:     map[string]any{"mode": "live_api_relay_static", "relay_binding_id": testStaticRelayBindingID},
			ready:       true,
			wantMode:    "live_api_relay_static",
			wantBinding: testStaticRelayBindingID,
			wantReady:   true,
		},
		{
			name:        "surrounding binding whitespace is preserved for policy rejection",
			youtube:     map[string]any{"mode": "live_api_relay_static", "relay_binding_id": " " + testStaticRelayBindingID},
			ready:       true,
			wantMode:    "live_api_relay_static",
			wantBinding: " " + testStaticRelayBindingID,
			wantReady:   true,
		},
		{
			name:    "trusted profile overrides specialized config",
			youtube: map[string]any{"mode": "stream_key"},
			profile: map[string]any{
				"mode":             "live_api_relay_static",
				"relay_binding_id": testStaticRelayBindingID,
			},
			wantMode:    "live_api_relay_static",
			wantBinding: testStaticRelayBindingID,
			wantReady:   false,
		},
		{
			name:        "explicitly cleared profile binding fails closed",
			youtube:     map[string]any{"mode": "live_api_relay_static", "relay_binding_id": testStaticRelayBindingID},
			profile:     map[string]any{"relay_binding_id": ""},
			wantMode:    "live_api_relay_static",
			wantBinding: "",
			wantReady:   false,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := RuntimeConfig{
				StreamYouTubeConfigs: []RuntimeYouTubeStreamConfig{{
					StreamID:        "stream-01",
					AssignmentRole:  "primary",
					YouTubeOutputID: "youtube-output-01",
					Ready:           tt.ready,
					YouTubeConfig:   tt.youtube,
				}},
				Profiles: map[string][]RuntimeProfile{
					"youtube_output": {{ID: "youtube-output-01", Config: tt.profile}},
				},
			}
			policy, ok := cfg.YouTubeOutputPolicyForStream("stream-01")
			if !ok || policy.Mode != tt.wantMode || policy.RelayBindingID != tt.wantBinding || policy.Ready != tt.wantReady {
				t.Fatalf("unexpected trusted YouTube output policy: %#v ok=%v", policy, ok)
			}
		})
	}
}

func TestResolveRuntimeSecretPostsScopedServiceRequest(t *testing.T) {
	var gotAuth string
	var gotBody RuntimeSecretResolveRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/services/runtime-secrets/resolve" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		gotAuth = r.Header.Get("Authorization")
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatal(err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"secret_name":"oauth_account:account-01:refresh_token","value":"<RAW_REFRESH_TOKEN>","expires_in_sec":60}`))
	}))
	defer server.Close()

	client := Client{Config: Config{ControlPanelURL: server.URL, Token: "secret-token", ServiceID: "enc-01", ServiceName: "Encoder 01", ServicePublicURL: "https://encoder.example.com"}}
	value, err := client.ResolveRuntimeSecret(t.Context(), "stream-01", "archive-01", "oauth_account:account-01:refresh_token")
	if err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer secret-token" {
		t.Fatalf("unexpected auth header: %q", gotAuth)
	}
	if gotBody.ServiceID != "enc-01" || gotBody.StreamID != "stream-01" || gotBody.ArchiveProfileID != "archive-01" || gotBody.SecretName != "oauth_account:account-01:refresh_token" {
		t.Fatalf("unexpected runtime secret request: %#v", gotBody)
	}
	if value != "<RAW_REFRESH_TOKEN>" {
		t.Fatalf("unexpected runtime secret value: %q", value)
	}
}

func TestResolveRuntimeSecretLeaseActiveIsTypedAndRedacted(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/services/runtime-secrets/resolve" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"code":"runtime_secret_lease_active","value":"<RAW_REFRESH_TOKEN>","detail":"secret-token"}`))
	}))
	defer server.Close()

	client := Client{Config: Config{ControlPanelURL: server.URL, Token: "secret-token", ServiceID: "enc-01", ServiceName: "Encoder 01", ServicePublicURL: "https://encoder.example.com"}}
	_, err := client.ResolveRuntimeSecret(t.Context(), "stream-01", "archive-01", "oauth_account:account-01:refresh_token")
	if err == nil {
		t.Fatal("expected lease-active error")
	}
	if !errors.Is(err, ErrRuntimeSecretLeaseActive) {
		t.Fatalf("expected ErrRuntimeSecretLeaseActive, got %v", err)
	}
	for _, forbidden := range []string{"<RAW_REFRESH_TOKEN>", "secret-token", "detail"} {
		if strings.Contains(err.Error(), forbidden) {
			t.Fatalf("runtime secret error leaked response body: %v", err)
		}
	}
}

func TestControlPanelErrorsDoNotLeakTokenOrBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "secret-token", http.StatusForbidden)
	}))
	defer server.Close()

	client := Client{Config: Config{ControlPanelURL: server.URL, Token: "secret-token", ServiceID: "enc-01", ServiceName: "Encoder 01", ServicePublicURL: "https://encoder.example.com"}}
	err := client.Register(t.Context())
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), "secret-token") {
		t.Fatalf("token leaked in error: %v", err)
	}
}

func TestControlPanelClientDoesNotFollowRedirectsWithBearerToken(t *testing.T) {
	var redirectedAuth string
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		redirectedAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/capture", http.StatusFound)
	}))
	defer server.Close()

	client := Client{Config: Config{ControlPanelURL: server.URL, Token: "secret-token", ServiceID: "enc-01", ServiceName: "Encoder 01", ServicePublicURL: "https://encoder.example.com"}}
	err := client.Register(t.Context())
	if err == nil {
		t.Fatal("expected redirect response to fail")
	}
	if redirectedAuth != "" {
		t.Fatalf("authorization header followed redirect: %q", redirectedAuth)
	}
}

func TestValidateRejectsNonHTTPControlPanelURL(t *testing.T) {
	cfg := Config{ControlPanelURL: "ftp://control.example.com/api", Token: "<SERVICE_TOKEN>", ServiceID: "enc-01", ServiceName: "Encoder 01", ServicePublicURL: "https://encoder.example.com"}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected validation error")
	}
	if !strings.Contains(err.Error(), "http or https") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateRejectsRemoteHTTPControlPanelURL(t *testing.T) {
	cfg := Config{ControlPanelURL: "http://control.example.com", Token: "<SERVICE_TOKEN>", ServiceID: "enc-01", ServiceName: "Encoder 01", ServicePublicURL: "https://encoder.example.com"}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected validation error")
	}
	if !strings.Contains(err.Error(), "CONTROL_PANEL_URL") || !strings.Contains(err.Error(), "https") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateAllowsLocalHTTPControlPanelURL(t *testing.T) {
	for _, raw := range []string{"http://127.0.0.1:8080", "http://localhost:8080", "http://host.docker.internal:8080"} {
		cfg := Config{ControlPanelURL: raw, Token: "<SERVICE_TOKEN>", ServiceID: "enc-01", ServiceName: "Encoder 01", ServicePublicURL: "http://host.docker.internal:8081"}
		if err := cfg.Validate(); err != nil {
			t.Fatalf("local URL %s rejected: %v", raw, err)
		}
	}
}

func TestValidateRejectsNonHTTPServicePublicURL(t *testing.T) {
	cfg := Config{ControlPanelURL: "https://control.example.com", Token: "<SERVICE_TOKEN>", ServiceID: "enc-01", ServiceName: "Encoder 01", ServicePublicURL: "ftp://encoder.example.com"}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected validation error")
	}
	if !strings.Contains(err.Error(), "SERVICE_PUBLIC_URL") || !strings.Contains(err.Error(), "http or https") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateRejectsRemoteHTTPServicePublicURL(t *testing.T) {
	cfg := Config{ControlPanelURL: "https://control.example.com", Token: "<SERVICE_TOKEN>", ServiceID: "enc-01", ServiceName: "Encoder 01", ServicePublicURL: "http://encoder.example.com"}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected validation error")
	}
	if !strings.Contains(err.Error(), "SERVICE_PUBLIC_URL") || !strings.Contains(err.Error(), "https") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateAllowsComposeServiceHTTPPublicURL(t *testing.T) {
	cfg := Config{ControlPanelURL: "https://control.example.com", Token: "<SERVICE_TOKEN>", ServiceID: "enc-01", ServiceName: "Encoder 01", ServicePublicURL: "http://encoder-recorder:8080"}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("compose service URL rejected: %v", err)
	}
}

func TestValidateDoesNotAllowComposeServiceNameForControlPanelHTTP(t *testing.T) {
	cfg := Config{
		ControlPanelURL:  "http://encoder-recorder:8080",
		Token:            "<SERVICE_TOKEN>",
		ServiceID:        "enc-01",
		ServiceName:      "Encoder 01",
		ServicePublicURL: "http://encoder-recorder:8080",
	}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "CONTROL_PANEL_URL") {
		t.Fatalf("expected compose service exception to be limited to SERVICE_PUBLIC_URL, got %v", err)
	}
}

func TestValidateRejectsControlPanelURLQueryOrFragment(t *testing.T) {
	cfg := Config{ControlPanelURL: "https://control.example.com?token=bad", Token: "<SERVICE_TOKEN>", ServiceID: "enc-01", ServiceName: "Encoder 01", ServicePublicURL: "https://encoder.example.com"}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected validation error")
	}
	if !strings.Contains(err.Error(), "query or fragment") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestConfigFromEnv(t *testing.T) {
	t.Setenv("CONTROL_PANEL_URL", "https://control.example.com")
	t.Setenv("CONTROL_PANEL_TOKEN", "<SERVICE_TOKEN>")
	t.Setenv("SERVICE_ID", "enc-01")
	t.Setenv("SERVICE_NAME", "Encoder 01")
	t.Setenv("SERVICE_PUBLIC_URL", "https://encoder.example.com")
	t.Setenv("SERVICE_VERSION", "0.1.0")
	t.Setenv("CONTROL_PANEL_HEARTBEAT_INTERVAL_SEC", "5")

	cfg := ConfigFromEnv()
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if cfg.HeartbeatEvery != 5*time.Second || cfg.ServicePublicURL == "" {
		t.Fatalf("unexpected config: %#v", cfg)
	}
}
