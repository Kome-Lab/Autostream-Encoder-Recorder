package httpapi

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/example/autostream-encoder-recorder/internal/control"
	"github.com/example/autostream-encoder-recorder/internal/outputrelay"
	"github.com/example/autostream-encoder-recorder/internal/streamproc"
	"github.com/example/autostream-encoder-recorder/internal/workerevents"
)

func TestDryRunEndpointRejectsMissingRequiredRelayWithoutRuntimeProvider(t *testing.T) {
	t.Setenv("AUTOSTREAM_ENV", "development")
	t.Setenv("AUTOSTREAM_REQUIRE_OUTPUT_RELAY", "true")
	t.Setenv("AUTOSTREAM_OUTPUT_RELAY_URL", "")
	t.Setenv("AUTOSTREAM_OUTPUT_RELAY_MODE", "direct")
	root := t.TempDir()
	t.Setenv("AUTOSTREAM_ARCHIVE_DIR", root)

	handler := newV2TestServerWithManagers(t, nil, workerevents.NewManager(root), TokenVerifier{PlainToken: "service-token"}, "stream-required-relay")
	req := httptest.NewRequest(http.MethodPost, "/streams/dry-run", bytes.NewBufferString(`{"stream_id":"stream-required-relay","name":"Required Relay","input_url":"srt://input.example.com:9000","dry_run":true}`))
	req.Header.Set("Authorization", "Bearer service-token")
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)

	if res.Code != http.StatusBadRequest || !strings.Contains(res.Body.String(), `"code":"dry_run_failed"`) {
		t.Fatalf("missing required relay must reject before runtime fallback, status=%d body=%s", res.Code, res.Body.String())
	}
	if _, err := os.Stat(filepath.Join(root, "tmp", "stream-required-relay")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing required relay must not create a dry-run archive, stat error=%v", err)
	}
}

func TestStartEndpointAcceptsRuntimeStreamKeyReferenceWithoutStaticRelay(t *testing.T) {
	t.Setenv("SERVICE_CONTROL_TOKEN", "service-token")
	root := t.TempDir()
	starter := &httpFakeStarter{}
	processManager := &streamproc.Manager{ArchiveRoot: root, FFmpegBin: "ffmpeg", Starter: starter, InputResolver: testInputResolver, AllowHostnameInputs: true, OutputRelayMode: outputrelay.ModeDirect}
	handler := newV2TestServer(t, processManager, "stream-01")

	body := `{"stream_id":"stream-01","name":"Morning Stream","input_url":"srt://input.example.com:9000"}`
	req := httptest.NewRequest(http.MethodPost, "/streams/start", bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer service-token")
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusAccepted {
		t.Fatalf("start status = %d body = %s", res.Code, res.Body.String())
	}
	if strings.Contains(res.Body.String(), "test-runtime-stream-key") {
		t.Fatal("runtime stream key leaked in start response")
	}
	args := strings.Join(starter.args, " ")
	if !strings.Contains(args, "rtmps://youtube.example.com/live2/test-runtime-stream-key") {
		t.Fatalf("expected direct target without a static relay, got %#v", starter.args)
	}
}

func TestStartEndpointRejectsMissingRequiredRelayWithoutRuntimeProvider(t *testing.T) {
	t.Setenv("AUTOSTREAM_ENV", "development")
	t.Setenv("AUTOSTREAM_REQUIRE_OUTPUT_RELAY", "true")
	t.Setenv("AUTOSTREAM_OUTPUT_RELAY_URL", "")
	t.Setenv("AUTOSTREAM_OUTPUT_RELAY_MODE", "direct")
	root := t.TempDir()
	starter := &httpFakeStarter{}
	inputResolverCalls := 0
	processManager := &streamproc.Manager{
		ArchiveRoot: root,
		FFmpegBin:   "ffmpeg",
		Starter:     starter,
		InputResolver: func(context.Context, string) ([]net.IP, error) {
			inputResolverCalls++
			return nil, errors.New("input must not be resolved when the required relay is missing")
		},
		AllowHostnameInputs: true,
		RequireOutputRelay:  true,
		OutputRelayMode:     outputrelay.ModeDirect,
	}
	handler := NewServerWithManagersAndRuntimeConfig("encoder_recorder", processManager, workerevents.NewManager(root), TokenVerifier{PlainToken: "service-token"}, nil, testYouTubeRuntimeProvider("stream-required-relay"))
	req := httptest.NewRequest(http.MethodPost, "/streams/start", bytes.NewBufferString(`{"stream_id":"stream-required-relay","name":"Required Relay","input_url":"srt://input.example.com:9000"}`))
	req.Header.Set("Authorization", "Bearer service-token")
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)

	if res.Code != http.StatusBadRequest || !strings.Contains(res.Body.String(), `"code":"start_stream_failed"`) {
		t.Fatalf("missing required relay must reject before runtime fallback, status=%d body=%s", res.Code, res.Body.String())
	}
	if inputResolverCalls != 0 || starter.process != nil || len(starter.args) != 0 {
		t.Fatalf("missing required relay must reject before input resolution or FFmpeg: inputCalls=%d process=%#v args=%#v", inputResolverCalls, starter.process, starter.args)
	}
}

func TestStartEndpointResolvesRuntimeStreamKeySecretNameWithoutStaticRelay(t *testing.T) {
	t.Setenv("SERVICE_CONTROL_TOKEN", "service-token")
	root := t.TempDir()
	starter := &httpFakeStarter{}
	processManager := &streamproc.Manager{ArchiveRoot: root, FFmpegBin: "ffmpeg", Starter: starter, InputResolver: testInputResolver, AllowHostnameInputs: true, OutputRelayMode: outputrelay.ModeDirect}
	resolverCalls := 0
	handler := NewServerWithManagersAndRuntimeConfig("encoder_recorder", processManager, workerevents.NewManager(root), TokenVerifier{PlainToken: "service-token"}, func(ctx context.Context, streamID, archiveProfileID, secretName string) (string, error) {
		resolverCalls++
		if streamID != "stream-01" || archiveProfileID != "" || secretName != "youtube_stream_key_main" {
			t.Fatalf("unexpected resolve context stream=%q profile=%q secret=%q", streamID, archiveProfileID, secretName)
		}
		return "resolved-runtime-stream-key", nil
	}, testYouTubeRuntimeProvider("stream-01"))

	body := `{"stream_id":"stream-01","name":"Morning Stream","input_url":"srt://input.example.com:9000","rtmp_url":"rtmps://youtube.example.com/live2","stream_key_secret_name":"youtube_stream_key_main"}`
	req := httptest.NewRequest(http.MethodPost, "/streams/start", bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer service-token")
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusAccepted {
		t.Fatalf("start status = %d body = %s", res.Code, res.Body.String())
	}
	if strings.Contains(res.Body.String(), "resolved-runtime-stream-key") || strings.Contains(res.Body.String(), "youtube_stream_key_main") {
		t.Fatalf("runtime stream key details leaked in start response: %s", res.Body.String())
	}
	if resolverCalls != 1 {
		t.Fatalf("runtime stream key resolver calls = %d, want 1", resolverCalls)
	}
	args := strings.Join(starter.args, " ")
	if !strings.Contains(args, "rtmps://youtube.example.com/live2/resolved-runtime-stream-key") {
		t.Fatalf("expected direct target without a static relay, got %#v", starter.args)
	}
}

func TestStartEndpointAppliesControlPanelYouTubeRuntimeConfigWithoutStaticRelay(t *testing.T) {
	t.Setenv("SERVICE_CONTROL_TOKEN", "service-token")
	t.Setenv("AUTOSTREAM_REQUIRE_CONTROL_PANEL_RUNTIME_CONFIG", "true")
	root := t.TempDir()
	starter := &httpFakeStarter{}
	processManager := &streamproc.Manager{ArchiveRoot: root, FFmpegBin: "ffmpeg", Starter: starter, InputResolver: testInputResolver, AllowHostnameInputs: true, OutputRelayMode: outputrelay.ModeDirect}
	resolverCalls := 0
	handler := NewServerWithManagersAndRuntimeConfig(
		"encoder_recorder",
		processManager,
		workerevents.NewManager(root),
		TokenVerifier{PlainToken: "service-token"},
		func(ctx context.Context, streamID, archiveProfileID, secretName string) (string, error) {
			resolverCalls++
			if streamID != "stream-01" || archiveProfileID != "" || secretName != "youtube_stream_key_runtime_stream-01" {
				t.Fatalf("unexpected resolve context stream=%q profile=%q secret=%q", streamID, archiveProfileID, secretName)
			}
			return "resolved-control-panel-stream-key", nil
		},
		func(ctx context.Context) (control.RuntimeConfig, error) {
			return control.RuntimeConfig{
				StreamYouTubeConfigs: []control.RuntimeYouTubeStreamConfig{{
					StreamID:        "stream-01",
					AssignmentRole:  "primary",
					YouTubeOutputID: "youtube-output-01",
					Ready:           true,
					YouTubeConfig: map[string]any{
						"mode":                   "stream_key",
						"rtmp_url":               "rtmps://control.example.com/live2",
						"stream_key_secret_name": "youtube_stream_key_profile",
					},
					ActiveRuntime: map[string]any{
						"mode":                   "stream_key",
						"stream_key_secret_name": "youtube_stream_key_runtime_stream-01",
					},
				}},
			}, nil
		},
	)

	body := `{"stream_id":"stream-01","name":"Morning Stream","input_url":"srt://input.example.com:9000"}`
	req := httptest.NewRequest(http.MethodPost, "/streams/start", bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer service-token")
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusAccepted {
		t.Fatalf("start status = %d body = %s", res.Code, res.Body.String())
	}
	if strings.Contains(res.Body.String(), "resolved-control-panel-stream-key") || strings.Contains(res.Body.String(), "env-secret-stream-key") {
		t.Fatalf("start response leaked stream key material: %s", res.Body.String())
	}
	if resolverCalls != 1 {
		t.Fatalf("Control Panel YouTube resolver calls = %d, want 1", resolverCalls)
	}
	args := strings.Join(starter.args, " ")
	if !strings.Contains(args, "rtmps://control.example.com/live2/resolved-control-panel-stream-key") {
		t.Fatalf("expected Control Panel direct target without a static relay, got %#v", starter.args)
	}
	if strings.Contains(args, "env-secret-stream-key") || strings.Contains(args, "rtmps://env.example.com/live2") {
		t.Fatalf("env youtube fallback should not be used when control panel runtime config is available: %#v", starter.args)
	}
}

func TestStartEndpointRejectsTrustedLiveAPIWithStaticRelayBeforeSecretResolution(t *testing.T) {
	t.Setenv("SERVICE_CONTROL_TOKEN", "service-token")
	root := t.TempDir()
	starter := &httpFakeStarter{}
	processManager := &streamproc.Manager{ArchiveRoot: root, FFmpegBin: "ffmpeg", Starter: starter, InputResolver: testInputResolver, AllowHostnameInputs: true, OutputRelayURL: "rtmp://127.0.0.1/autostream/{stream_id}", OutputRelayMode: outputrelay.ModeManagedLiveAPI, OutputRelayBindingID: testStaticRelayBindingID}
	resolverCalls := 0
	runtimeConfigCalls := 0
	handler := NewServerWithManagersAndRuntimeConfig(
		"encoder_recorder",
		processManager,
		workerevents.NewManager(root),
		TokenVerifier{PlainToken: "service-token"},
		func(ctx context.Context, streamID, archiveProfileID, secretName string) (string, error) {
			resolverCalls++
			return "resolved-runtime-stream-key", nil
		},
		func(ctx context.Context) (control.RuntimeConfig, error) {
			runtimeConfigCalls++
			return control.RuntimeConfig{StreamYouTubeConfigs: []control.RuntimeYouTubeStreamConfig{{
				StreamID:       "stream-01",
				AssignmentRole: "primary",
				Ready:          false,
				YouTubeConfig:  map[string]any{"mode": "live_api"},
			}}}, nil
		},
	)

	body := `{"stream_id":"stream-01","name":"Morning Stream","input_url":"srt://input.example.com:9000","rtmp_url":"rtmps://youtube.example.com/live2","stream_key_secret_name":"youtube_stream_key_runtime_stream-01"}`
	req := httptest.NewRequest(http.MethodPost, "/streams/start", bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer service-token")
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusConflict || !strings.Contains(res.Body.String(), "live_api_requires_managed_output_relay") {
		t.Fatalf("expected trusted live api static relay rejection, status=%d body=%s", res.Code, res.Body.String())
	}
	if runtimeConfigCalls == 0 {
		t.Fatal("trusted Control Panel runtime config was not fetched")
	}
	if resolverCalls != 0 {
		t.Fatalf("runtime secret must not be resolved for static relay live api, calls=%d", resolverCalls)
	}
	if starter.process != nil || len(starter.args) != 0 {
		t.Fatalf("ffmpeg must not start for static relay live api: process=%#v args=%#v", starter.process, starter.args)
	}
}

func TestStartEndpointStaticOutputRelayRejectsNonStaticYouTubeOutputModesBeforeSecretResolution(t *testing.T) {
	for _, outputMode := range []string{"stream_key", "live_api", "live_api_dry_run"} {
		t.Run(outputMode, func(t *testing.T) {
			t.Setenv("SERVICE_CONTROL_TOKEN", "service-token")
			root := t.TempDir()
			starter := &httpFakeStarter{}
			processManager := &streamproc.Manager{
				ArchiveRoot:          root,
				FFmpegBin:            "ffmpeg",
				Starter:              starter,
				InputResolver:        testInputResolver,
				AllowHostnameInputs:  true,
				OutputRelayURL:       "rtmp://127.0.0.1/autostream/{stream_id}",
				OutputRelayMode:      outputrelay.ModeManagedLiveAPI,
				OutputRelayBindingID: testStaticRelayBindingID,
			}
			resolverCalls := 0
			handler := NewServerWithManagersAndRuntimeConfig(
				"encoder_recorder",
				processManager,
				workerevents.NewManager(root),
				TokenVerifier{PlainToken: "service-token"},
				func(ctx context.Context, streamID, archiveProfileID, secretName string) (string, error) {
					resolverCalls++
					return "resolved-runtime-stream-key", nil
				},
				func(ctx context.Context) (control.RuntimeConfig, error) {
					return control.RuntimeConfig{StreamYouTubeConfigs: []control.RuntimeYouTubeStreamConfig{{
						StreamID:       "stream-01",
						AssignmentRole: "primary",
						Ready:          true,
						YouTubeConfig: map[string]any{
							"mode":                   outputMode,
							"rtmp_url":               "rtmps://youtube.example.com/live2",
							"stream_key_secret_name": "youtube_stream_key_runtime_stream-01",
						},
					}}}, nil
				},
			)

			req := httptest.NewRequest(http.MethodPost, "/streams/start", bytes.NewBufferString(`{"stream_id":"stream-01","name":"Morning Stream","input_url":"srt://input.example.com:9000"}`))
			req.Header.Set("Authorization", "Bearer service-token")
			res := httptest.NewRecorder()
			handler.ServeHTTP(res, req)
			if res.Code != http.StatusConflict || !strings.Contains(res.Body.String(), "live_api_requires_managed_output_relay") {
				t.Fatalf("output mode %q status=%d body=%s", outputMode, res.Code, res.Body.String())
			}
			if resolverCalls != 0 {
				t.Fatalf("output mode %q resolved a runtime secret before static relay rejection, calls=%d", outputMode, resolverCalls)
			}
			if starter.process != nil || len(starter.args) != 0 {
				t.Fatalf("output mode %q started ffmpeg: process=%#v args=%#v", outputMode, starter.process, starter.args)
			}
		})
	}
}

func TestStartEndpointRejectsRemovedLegacyRelayModeBeforeSecretResolution(t *testing.T) {
	for _, tt := range []struct {
		name       string
		outputMode string
		wantStatus int
		wantStart  bool
	}{
		{name: "removed legacy relay mode is rejected", outputMode: "stream_key", wantStatus: http.StatusBadRequest},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("SERVICE_CONTROL_TOKEN", "service-token")
			root := t.TempDir()
			starter := &httpFakeStarter{}
			processManager := &streamproc.Manager{
				ArchiveRoot:          root,
				FFmpegBin:            "ffmpeg",
				Starter:              starter,
				InputResolver:        testInputResolver,
				AllowHostnameInputs:  true,
				OutputRelayURL:       "rtmp://127.0.0.1/autostream/{stream_id}",
				OutputRelayBindingID: "stale-binding-must-not-matter",
				OutputRelayMode:      "legacy_stream_key",
			}
			resolverCalls := 0
			handler := NewServerWithManagersAndRuntimeConfig(
				"encoder_recorder",
				processManager,
				workerevents.NewManager(root),
				TokenVerifier{PlainToken: "service-token"},
				func(context.Context, string, string, string) (string, error) {
					resolverCalls++
					return "resolved-runtime-stream-key", nil
				},
				func(context.Context) (control.RuntimeConfig, error) {
					return control.RuntimeConfig{StreamYouTubeConfigs: []control.RuntimeYouTubeStreamConfig{{
						StreamID:       "stream-legacy-01",
						AssignmentRole: "primary",
						Ready:          true,
						YouTubeConfig: map[string]any{
							"mode":                   tt.outputMode,
							"rtmp_url":               "rtmps://youtube.example.com/live2",
							"stream_key_secret_name": "youtube_stream_key_runtime_stream-legacy-01",
						},
					}}}, nil
				},
			)

			req := httptest.NewRequest(http.MethodPost, "/streams/start", bytes.NewBufferString(`{"stream_id":"stream-legacy-01","name":"Legacy Stream","input_url":"srt://input.example.com:9000"}`))
			req.Header.Set("Authorization", "Bearer service-token")
			res := httptest.NewRecorder()
			handler.ServeHTTP(res, req)
			if res.Code != tt.wantStatus {
				t.Fatalf("mode %q status=%d body=%s", tt.outputMode, res.Code, res.Body.String())
			}
			if resolverCalls != 0 {
				t.Fatalf("mode %q resolved a YouTube key despite local/failed Relay policy, calls=%d", tt.outputMode, resolverCalls)
			}
			if tt.wantStart {
				args := strings.Join(starter.args, " ")
				if starter.process == nil || !strings.Contains(args, "rtmp://127.0.0.1/autostream/stream-legacy-01") {
					t.Fatalf("legacy stream-key output did not start local Relay: process=%#v args=%#v", starter.process, starter.args)
				}
				for _, forbidden := range []string{"youtube.example.com", "resolved-runtime-stream-key"} {
					if strings.Contains(args, forbidden) {
						t.Fatalf("legacy Relay FFmpeg args leaked unused output material %q: %#v", forbidden, starter.args)
					}
				}
			} else if starter.process != nil || len(starter.args) != 0 {
				t.Fatalf("mode %q started FFmpeg: process=%#v args=%#v", tt.outputMode, starter.process, starter.args)
			}
		})
	}
}

func TestStartEndpointStaticLiveAPIRelayRequiresTrustedBindingAndSkipsYouTubeSecrets(t *testing.T) {
	tests := []struct {
		name               string
		environmentBinding string
		profileBinding     string
		runtimeReady       bool
		wantStatus         int
		wantCode           string
		wantStart          bool
	}{
		{name: "matching binding starts local relay", environmentBinding: testStaticRelayBindingID, profileBinding: testStaticRelayBindingID, runtimeReady: true, wantStatus: http.StatusAccepted, wantStart: true},
		{name: "mismatched binding rejects before secret resolution", environmentBinding: testStaticRelayBindingID, profileBinding: testOtherStaticRelayBindingID, runtimeReady: true, wantStatus: http.StatusConflict, wantCode: "live_api_relay_binding_mismatch"},
		{name: "unready runtime config rejects before secret resolution", environmentBinding: testStaticRelayBindingID, profileBinding: testStaticRelayBindingID, runtimeReady: false, wantStatus: http.StatusConflict, wantCode: "live_api_relay_static_not_ready"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("SERVICE_CONTROL_TOKEN", "service-token")
			root := t.TempDir()
			starter := &httpFakeStarter{}
			processManager := &streamproc.Manager{
				ArchiveRoot:          root,
				FFmpegBin:            "ffmpeg",
				Starter:              starter,
				InputResolver:        testInputResolver,
				AllowHostnameInputs:  true,
				OutputRelayURL:       "rtmp://127.0.0.1/autostream/{stream_id}",
				OutputRelayMode:      outputrelay.ModeManagedLiveAPI,
				OutputRelayBindingID: tt.environmentBinding,
			}
			resolverCalls := 0
			handler := NewServerWithManagersAndRuntimeConfig(
				"encoder_recorder",
				processManager,
				workerevents.NewManager(root),
				TokenVerifier{PlainToken: "service-token"},
				func(ctx context.Context, streamID, archiveProfileID, secretName string) (string, error) {
					resolverCalls++
					return "resolved-runtime-stream-key", nil
				},
				func(ctx context.Context) (control.RuntimeConfig, error) {
					return control.RuntimeConfig{
						StreamYouTubeConfigs: []control.RuntimeYouTubeStreamConfig{{
							StreamID:        "stream-01",
							AssignmentRole:  "primary",
							YouTubeOutputID: "youtube-output-01",
							Ready:           tt.runtimeReady,
							YouTubeConfig: map[string]any{
								"mode":                   "stream_key",
								"rtmp_url":               "rtmps://youtube.example.com/live2",
								"stream_key_secret_name": "youtube_stream_key_runtime_stream-01",
							},
						}},
						Profiles: map[string][]control.RuntimeProfile{
							"youtube_output": {{
								ID: "youtube-output-01",
								Config: map[string]any{
									"mode":             "live_api_relay_static",
									"relay_binding_id": tt.profileBinding,
								},
							}},
						},
					}, nil
				},
			)

			body := `{"stream_id":"stream-01","name":"Morning Stream","input_url":"srt://input.example.com:9000"}`
			req := httptest.NewRequest(http.MethodPost, "/streams/start", bytes.NewBufferString(body))
			req.Header.Set("Authorization", "Bearer service-token")
			res := httptest.NewRecorder()
			handler.ServeHTTP(res, req)
			if res.Code != tt.wantStatus || (tt.wantCode != "" && !strings.Contains(res.Body.String(), tt.wantCode)) {
				t.Fatalf("start status=%d body=%s", res.Code, res.Body.String())
			}
			if resolverCalls != 0 {
				t.Fatalf("static live api relay must not resolve a YouTube key, calls=%d", resolverCalls)
			}
			if tt.wantStart {
				args := strings.Join(starter.args, " ")
				if starter.process == nil || !strings.Contains(args, "rtmp://127.0.0.1/autostream/stream-01") {
					t.Fatalf("matching static relay binding did not start local relay: process=%#v args=%#v", starter.process, starter.args)
				}
				for _, forbidden := range []string{"resolved-runtime-stream-key", "rtmps://youtube.example.com/live2"} {
					if strings.Contains(args, forbidden) {
						t.Fatalf("static relay process args leaked unused YouTube output %q: %#v", forbidden, starter.args)
					}
				}
			} else if starter.process != nil || len(starter.args) != 0 {
				t.Fatalf("mismatched static relay binding must not start ffmpeg: process=%#v args=%#v", starter.process, starter.args)
			}
		})
	}
}

func TestDryRunEndpointRejectsRemovedLegacyRelayMode(t *testing.T) {
	t.Setenv("SERVICE_CONTROL_TOKEN", "service-token")
	t.Setenv("AUTOSTREAM_REQUIRE_CONTROL_PANEL_RUNTIME_CONFIG", "true")
	t.Setenv("AUTOSTREAM_ARCHIVE_DIR", t.TempDir())
	t.Setenv("AUTOSTREAM_OUTPUT_RELAY_URL", "rtmp://127.0.0.1/autostream/{stream_id}")
	t.Setenv("AUTOSTREAM_OUTPUT_RELAY_MODE", "legacy_stream_key")

	handler := NewServerWithManagersAndRuntimeConfig(
		"encoder_recorder",
		nil,
		workerevents.NewManager(t.TempDir()),
		TokenVerifier{PlainToken: "service-token"},
		nil,
		func(context.Context) (control.RuntimeConfig, error) {
			return control.RuntimeConfig{StreamYouTubeConfigs: []control.RuntimeYouTubeStreamConfig{{
				StreamID:       "stream-legacy-01",
				AssignmentRole: "primary",
				Ready:          true,
				YouTubeConfig: map[string]any{
					"mode":                   "stream_key",
					"rtmp_url":               "rtmps://control.example.com/live2",
					"stream_key_secret_name": "youtube_stream_key_runtime_stream-legacy-01",
				},
			}}}, nil
		},
	)

	req := httptest.NewRequest(http.MethodPost, "/streams/dry-run", bytes.NewBufferString(`{"stream_id":"stream-legacy-01","name":"Legacy Stream","input_url":"srt://input.example.com:9000"}`))
	req.Header.Set("Authorization", "Bearer service-token")
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusBadRequest || !strings.Contains(res.Body.String(), `"code":"dry_run_failed"`) {
		t.Fatalf("dry-run status = %d body = %s", res.Code, res.Body.String())
	}
	if strings.Contains(res.Body.String(), "commands") {
		t.Fatalf("removed relay mode must not run dry-run FFmpeg: %s", res.Body.String())
	}
}

func TestDryRunEndpointRemovedLegacyRelayRejectsAllYouTubeModes(t *testing.T) {
	for _, outputMode := range []string{"live_api", "live_api_dry_run"} {
		t.Run(outputMode, func(t *testing.T) {
			t.Setenv("SERVICE_CONTROL_TOKEN", "service-token")
			t.Setenv("AUTOSTREAM_REQUIRE_CONTROL_PANEL_RUNTIME_CONFIG", "true")
			t.Setenv("AUTOSTREAM_ARCHIVE_DIR", t.TempDir())
			t.Setenv("AUTOSTREAM_OUTPUT_RELAY_URL", "rtmp://127.0.0.1/autostream/{stream_id}")
			t.Setenv("AUTOSTREAM_OUTPUT_RELAY_MODE", "legacy_stream_key")
			handler := NewServerWithManagersAndRuntimeConfig(
				"encoder_recorder",
				nil,
				workerevents.NewManager(t.TempDir()),
				TokenVerifier{PlainToken: "service-token"},
				nil,
				func(context.Context) (control.RuntimeConfig, error) {
					return control.RuntimeConfig{StreamYouTubeConfigs: []control.RuntimeYouTubeStreamConfig{{
						StreamID:       "stream-legacy-01",
						AssignmentRole: "primary",
						Ready:          true,
						YouTubeConfig: map[string]any{
							"mode":                   outputMode,
							"rtmp_url":               "rtmps://youtube.example.com/live2",
							"stream_key_secret_name": "youtube_stream_key_runtime_stream-legacy-01",
						},
					}}}, nil
				},
			)

			req := httptest.NewRequest(http.MethodPost, "/streams/dry-run", bytes.NewBufferString(`{"stream_id":"stream-legacy-01","name":"Legacy Stream","input_url":"srt://input.example.com:9000"}`))
			req.Header.Set("Authorization", "Bearer service-token")
			res := httptest.NewRecorder()
			handler.ServeHTTP(res, req)
			if res.Code != http.StatusBadRequest || !strings.Contains(res.Body.String(), `"code":"dry_run_failed"`) {
				t.Fatalf("mode %q status=%d body=%s", outputMode, res.Code, res.Body.String())
			}
			if strings.Contains(res.Body.String(), "commands") {
				t.Fatalf("mode %q must not run dry-run FFmpeg: %s", outputMode, res.Body.String())
			}
		})
	}
}

func TestDryRunEndpointStaticLiveAPIRelayUsesLocalTargetWithoutYouTubeKey(t *testing.T) {
	t.Setenv("SERVICE_CONTROL_TOKEN", "service-token")
	t.Setenv("AUTOSTREAM_REQUIRE_CONTROL_PANEL_RUNTIME_CONFIG", "true")
	t.Setenv("AUTOSTREAM_ARCHIVE_DIR", t.TempDir())
	t.Setenv("AUTOSTREAM_OUTPUT_RELAY_URL", "rtmp://127.0.0.1/autostream/{stream_id}")
	t.Setenv("AUTOSTREAM_OUTPUT_RELAY_MODE", "live_api_relay_static")
	t.Setenv("AUTOSTREAM_OUTPUT_RELAY_BINDING_ID", testStaticRelayBindingID)
	handler := NewServerWithManagersAndRuntimeConfig(
		"encoder_recorder",
		nil,
		workerevents.NewManager(t.TempDir()),
		TokenVerifier{PlainToken: "service-token"},
		nil,
		func(context.Context) (control.RuntimeConfig, error) {
			return control.RuntimeConfig{StreamYouTubeConfigs: []control.RuntimeYouTubeStreamConfig{{
				StreamID:       "stream-static-01",
				AssignmentRole: "primary",
				Ready:          true,
				YouTubeConfig: map[string]any{
					"mode":             "live_api_relay_static",
					"relay_binding_id": testStaticRelayBindingID,
				},
			}}}, nil
		},
	)

	req := httptest.NewRequest(http.MethodPost, "/streams/dry-run", bytes.NewBufferString(`{"stream_id":"stream-static-01","name":"Static Stream","input_url":"srt://input.example.com:9000"}`))
	req.Header.Set("Authorization", "Bearer service-token")
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusAccepted {
		t.Fatalf("static dry-run status=%d body=%s", res.Code, res.Body.String())
	}
	for _, forbidden := range []string{"youtube.example.com", "stream_key"} {
		if strings.Contains(res.Body.String(), forbidden) {
			t.Fatalf("static dry-run retained unused YouTube output %q: %s", forbidden, res.Body.String())
		}
	}
	if !strings.Contains(res.Body.String(), "rtmp://127.0.0.1/autostream/stream-static-01") {
		t.Fatalf("static dry-run must use local Relay: %s", res.Body.String())
	}
}

func TestStartEndpointRequiresOutputRelayBeforeFFmpegStart(t *testing.T) {
	t.Setenv("SERVICE_CONTROL_TOKEN", "service-token")
	root := t.TempDir()
	starter := &httpFakeStarter{}
	processManager := &streamproc.Manager{
		ArchiveRoot:         root,
		FFmpegBin:           "ffmpeg",
		Starter:             starter,
		InputResolver:       testInputResolver,
		AllowHostnameInputs: true,
		RequireOutputRelay:  true,
		OutputRelayMode:     outputrelay.ModeDirect,
	}
	handler := newV2TestServerWithManagers(t, processManager, workerevents.NewManager(root), TokenVerifier{PlainToken: "service-token", DiscordAudioPlainToken: "audio-token"}, "stream-01")

	body := `{"stream_id":"stream-01","name":"Morning Stream","input_url":"srt://input.example.com:9000","rtmp_url":"rtmps://youtube.example.com/live2","stream_key":"runtime-secret-stream-key"}`
	req := httptest.NewRequest(http.MethodPost, "/streams/start", bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer service-token")
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusBadRequest {
		t.Fatalf("expected relay requirement failure, got %d body = %s", res.Code, res.Body.String())
	}
	if strings.Contains(res.Body.String(), "runtime-secret-stream-key") || strings.Contains(res.Body.String(), "rtmps://youtube.example.com/live2") {
		t.Fatalf("start failure leaked YouTube secret material: %s", res.Body.String())
	}
	if len(starter.args) != 0 || starter.process != nil {
		t.Fatalf("ffmpeg must not start when output relay is required but missing: process=%#v args=%#v", starter.process, starter.args)
	}
}
