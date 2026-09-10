package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/example/autostream-encoder-recorder/internal/control"
	"github.com/example/autostream-encoder-recorder/internal/ingesttoken"
	"github.com/example/autostream-encoder-recorder/internal/lifecycle"
	"github.com/example/autostream-encoder-recorder/internal/outputrelay"
	"github.com/example/autostream-encoder-recorder/internal/streamproc"
	"github.com/example/autostream-encoder-recorder/internal/videocover"
)

// Identical to the actual Client.Start capture oracle in Control Panel's
// internal/servicecall/archive_wire_test.go; do not omit producer fields here.
const canonicalArchiveStartWire = `{
  "stream_id":"stream-01","name":"Morning",
  "input_url":"srt://input.example.com:9000","rtmp_url":"rtmps://youtube.example.com/live2",
  "stream_key_secret_name":"youtube_stream_key:stream-01",
  "encoder_profile_id":"enc-profile-01","overlay_profile_id":"overlay-profile-01",
  "encoder_audio_gain_db":-3.5,"archive_profile_id":"archive-profile-01",
  "archive_run_id":"run-01","started_at":"2026-08-18T05:06:29.123456789Z",
  "youtube_runtime":{"mode":"live_api","output_id":"output-01","oauth_account_id":"account-01",
    "broadcast_id":"broadcast01","live_stream_id":"live01","rtmp_url":"rtmps://youtube.example.com/live2",
    "stream_key_secret_name":"youtube_stream_key:stream-01","watch_url":"https://www.youtube.com/watch?v=broadcast01",
    "dry_run":false,"complete_on_stop":true},
  "video_cover_start":{"job_generation":17,"revision":3,"active":false,"idempotency_key":"cover-start-17"}
}`

const canonicalArchivePackageWire = `{
  "stream_id":"stream-01","name":"Morning","archive_run_id":"run-01",
  "started_at":"2026-08-18T05:06:29.123456789Z","dry_run":false
}`

func TestArchiveWireRejectsLegacyBeforeSideEffects(t *testing.T) {
	values := []struct{ name, value string }{
		{"null", `null`}, {"empty", `{}`},
		{"settings", `{"archive_file_name":"caller.mp4","retention_days":999}`},
		{"raw_secret", `{"refresh_token":"wire-secret-sentinel"}`},
		{"boolean", `false`}, {"number", `0`}, {"string", `"legacy"`}, {"array", `[]`},
	}
	for _, path := range []string{"/streams/start", "/streams/dry-run", "/streams/package"} {
		for _, value := range values {
			for _, mixed := range []bool{false, true} {
				name := path + "/" + value.name
				if mixed {
					name += "/canonical_fields"
				}
				t.Run(name, func(t *testing.T) {
					body := `{"stream_id":"stream-01","name":"Morning","archive_config":` + value.value
					if mixed {
						body += `,"archive_run_id":"run-01","started_at":"2026-08-18T05:06:29Z","dry_run":true`
						if path != "/streams/package" {
							body += `,"rtmp_url":"","archive_profile_id":"archive-profile-01","youtube_runtime":{"mode":"stream_key"},"worker_video_ingest":true,"worker_video_ingest_token":"wire-secret-sentinel"`
						}
					}
					body += `}`
					assertArchiveWireRejected(t, path, body, http.StatusBadRequest, "bad_request")
				})
			}
		}
	}
}

func assertArchiveWireRejected(t *testing.T, path, body string, status int, code string) {
	t.Helper()
	root := t.TempDir()
	t.Setenv("AUTOSTREAM_ARCHIVE_DIR", root)
	starter := &httpFakeStarter{}
	manager := &streamproc.Manager{ArchiveRoot: root, Starter: starter, OutputRelayMode: outputrelay.ModeDirect}
	providerCalls, resolverCalls := 0, 0
	handler := archiveWireTestHandler(path, manager,
		func(context.Context, string, string, string) (string, error) {
			resolverCalls++
			return "", errors.New("unexpected secret resolution")
		},
		func(context.Context) (control.RuntimeConfig, error) {
			providerCalls++
			return control.RuntimeConfig{}, errors.New("unexpected runtime fetch")
		})
	before, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer service-token")
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	var response map[string]string
	if err := json.Unmarshal(res.Body.Bytes(), &response); err != nil {
		t.Fatal("response must be a safe JSON error")
	}
	if res.Code != status || !reflect.DeepEqual(response, map[string]string{"code": code}) {
		t.Errorf("status=%d code=%q, want status=%d code=%q", res.Code, response["code"], status, code)
	}
	if providerCalls != 0 || resolverCalls != 0 || starter.process != nil || len(starter.args) != 0 {
		t.Errorf("side effects: provider=%d resolver=%d process=%t", providerCalls, resolverCalls, starter.process != nil)
	}
	if _, err := manager.Status("stream-01"); err == nil {
		t.Error("rejected request created a tracked process")
	}
	after, err := os.ReadDir(root)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Error("rejected request changed archive storage")
	}
	if strings.Contains(res.Body.String(), "wire-secret-sentinel") {
		t.Error("rejected input leaked into response")
	}
}

func archiveWireTestHandler(path string, manager *streamproc.Manager, resolver RuntimeSecretResolver, provider RuntimeConfigProvider) http.Handler {
	verifier := TokenVerifier{PlainToken: "service-token"}
	switch path {
	case "/streams/start":
		return startStream(manager, nil, nil, verifier, resolver, provider)
	case "/streams/dry-run":
		return dryRunStream(verifier, provider)
	case "/streams/package":
		return packageStream(verifier, resolver, provider)
	default:
		panic("unknown archive wire test route")
	}
}

func archiveWireRuntimeProvider(archiveConfig map[string]any) RuntimeConfigProvider {
	return func(ctx context.Context) (control.RuntimeConfig, error) {
		cfg, err := testYouTubeRuntimeProvider("stream-01")(ctx)
		cfg.Profiles = map[string][]control.RuntimeProfile{
			"encoder": {{ID: "enc-profile-01", Config: map[string]any{"width": 160, "height": 90, "fps": 15}}},
			"overlay": {{ID: "overlay-profile-01", Config: map[string]any{"watermark_enabled": false}}},
		}
		cfg.StreamArchiveConfigs = []control.RuntimeArchiveStreamConfig{
			{StreamID: "another-stream", AssignmentRole: "primary", Ready: true, ArchiveProfileID: "wrong-stream-profile"},
			{StreamID: "stream-01", AssignmentRole: "standby", Ready: true, ArchiveProfileID: "standby-profile"},
			{StreamID: "stream-01", AssignmentRole: "primary", Ready: false, ArchiveProfileID: "unready-profile"},
			{StreamID: "stream-01", AssignmentRole: "primary", Ready: true, ArchiveProfileID: "archive-profile-01", ArchiveConfig: archiveConfig},
		}
		return cfg, err
	}
}

func archiveWireConfig() map[string]any {
	return map[string]any{
		"drive_destination_id": "dest-01", "auth_mode": "oauth2", "oauth_account_id": "account-01", "oauth_provider_id": "provider-01",
		"folder_id_secret_name": "drive_destination:dest-01:folder_id", "client_id": "client-01",
		"client_secret_secret_name": "oauth_provider:provider-01:client_secret",
		"refresh_token_secret_name": "oauth_account:account-01:refresh_token",
		"archive_file_name":         "Recording.mp4", "retention_days": 45, "shared_drive": true, "shared_drive_id": "shared-01",
	}
}

func archiveWireResolver(t *testing.T, calls map[string]int) RuntimeSecretResolver {
	t.Helper()
	return func(_ context.Context, streamID, profileID, name string) (string, error) {
		if streamID != "stream-01" {
			t.Error("secret resolution lost stream scope")
		}
		if name == "youtube_stream_key:stream-01" {
			if profileID != "" {
				t.Error("YouTube secret unexpectedly acquired archive profile scope")
			}
		} else if profileID != "archive-profile-01" {
			t.Error("archive secret resolution lost the assigned profile scope")
		}
		calls[name]++
		switch name {
		case "drive_destination:dest-01:folder_id":
			return "resolved-folder", nil
		case "oauth_provider:provider-01:client_secret":
			return "resolved-client-secret", nil
		case "oauth_account:account-01:refresh_token":
			return "resolved-refresh-token", nil
		case "youtube_stream_key:stream-01":
			return "resolved-stream-key", nil
		default:
			t.Error("unexpected secret reference")
			return "", errors.New("unexpected reference")
		}
	}
}

type archiveWireCapturePackager struct{ jobs chan lifecycle.PackageJob }

func (p archiveWireCapturePackager) Package(_ context.Context, job lifecycle.PackageJob) (lifecycle.Result, error) {
	p.jobs <- job
	return lifecycle.Result{}, nil
}

func TestArchiveWireCanonicalStartRetainsRuntimeAndRun(t *testing.T) {
	root := t.TempDir()
	starter := &httpFakeStarter{}
	packaged := make(chan lifecycle.PackageJob, 1)
	manager := &streamproc.Manager{
		ArchiveRoot: root, FFmpegBin: "ffmpeg", Starter: starter, InputResolver: testInputResolver,
		AllowHostnameInputs: true, OutputRelayMode: outputrelay.ModeDirect,
		CoverAssets: videocover.NewLoader(nil, 1, 1<<20), CoverWitness: &httpCoverWitness{},
		Packager: archiveWireCapturePackager{jobs: packaged},
	}
	resolved := map[string]int{}
	handler := archiveWireTestHandler("/streams/start", manager, archiveWireResolver(t, resolved), archiveWireRuntimeProvider(archiveWireConfig()))
	req := httptest.NewRequest(http.MethodPost, "/streams/start", strings.NewReader(canonicalArchiveStartWire))
	req.Header.Set("Authorization", "Bearer service-token")
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusAccepted || starter.process == nil {
		t.Fatalf("canonical producer start status=%d", res.Code)
	}
	t.Cleanup(func() { _, _ = manager.Stop("stream-01") })
	state, err := manager.VideoCoverState("stream-01")
	if err != nil || state.JobGeneration != 17 || state.Desired.Revision != 3 {
		t.Error("canonical start lost the requested visual epoch")
	}
	if _, err := os.Stat(filepath.Join(root, "tmp", "stream-01", "final.mkv")); err != nil {
		t.Error("start did not retain the managed recording destination")
	}
	if _, err := manager.Stop("stream-01"); err != nil {
		t.Fatal(err)
	}
	select {
	case job := <-packaged:
		assertArchiveWireRun(t, job.StreamID, job.ArchiveRunID, job.Name, job.StartedAt)
		want := lifecycle.ArchiveConfig{
			ArchiveProfileID: "archive-profile-01", DriveDestinationID: "dest-01", AuthMode: "oauth2",
			OAuthAccountID: "account-01", OAuthProviderID: "provider-01", ClientID: "client-01",
			FolderID: "resolved-folder", FolderIDSecretName: "drive_destination:dest-01:folder_id",
			ClientSecret: "resolved-client-secret", ClientSecretSecretName: "oauth_provider:provider-01:client_secret",
			RefreshToken: "resolved-refresh-token", RefreshTokenSecretName: "oauth_account:account-01:refresh_token",
			ArchiveFileName: "Recording.mp4", RetentionDays: 45, SharedDrive: true, SharedDriveID: "shared-01",
		}
		if !reflect.DeepEqual(job.ArchiveConfig, want) {
			t.Error("start/stop lost resolved archive configuration before packaging")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("start/stop did not reach the archive packager")
	}
	assertArchiveWireSecrets(t, resolved, true)
	assertArchiveWireNoSecrets(t, res.Body.String())
}

func TestArchiveWireCanonicalPackageRetainsRuntimeAndRun(t *testing.T) {
	var request packageStreamRequest
	req := httptest.NewRequest(http.MethodPost, "/streams/package", strings.NewReader(canonicalArchivePackageWire))
	if _, err := decodeLimitedStrictJSON(httptest.NewRecorder(), req, maxControlBodyBytes, &request); err != nil {
		t.Fatal("real CP package body did not decode")
	}
	job := request.packageJob()
	assertArchiveWireRun(t, job.StreamID, job.ArchiveRunID, job.Name, job.StartedAt)
	if job.ArchiveConfig != (lifecycle.ArchiveConfig{}) {
		t.Fatal("HTTP package input supplied internal archive settings")
	}
	root := t.TempDir()
	t.Setenv("AUTOSTREAM_ARCHIVE_DIR", root)
	// Keep optional completion reporting local to this handler test.
	t.Setenv("AUTOSTREAM_NODE_CONFIG", filepath.Join(root, "no-report-config.yml"))
	t.Setenv("OBSERVABILITY_URL", "")
	dir := filepath.Join(root, "tmp", "stream-01")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "final.mkv"), []byte("synthetic recording"), 0600); err != nil {
		t.Fatal(err)
	}
	// Only the execution mode changes: the real handler uses its DryRunRunner
	// and DryRunUploader instead of launching FFmpeg or contacting Drive.
	request.DryRun = true
	body, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	resolved := map[string]int{}
	handler := archiveWireTestHandler("/streams/package", nil, archiveWireResolver(t, resolved), archiveWireRuntimeProvider(archiveWireConfig()))
	req = httptest.NewRequest(http.MethodPost, "/streams/package", strings.NewReader(string(body)))
	req.Header.Set("Authorization", "Bearer service-token")
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusAccepted {
		t.Fatalf("canonical package status=%d", res.Code)
	}
	assertArchiveWireSecrets(t, resolved, false)
	assertArchiveWireNoSecrets(t, res.Body.String())
	for _, want := range []string{`"archive_profile_id":"archive-profile-01"`, `"drive_destination_id":"dest-01"`,
		`"retention_days":45`, `"archive_file_name":"Recording.mp4"`, `"shared_drive":true`,
		`"folder_id_configured":true`, `"client_secret_configured":true`, `"refresh_token_configured":true`, `"archive_run_id":"run-01"`} {
		if !strings.Contains(res.Body.String(), want) {
			t.Errorf("package metadata missing %s", want)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "final", "stream-01", "run-01", "final.mp4")); err != nil {
		t.Error("package did not preserve the original run destination")
	}
}

func assertArchiveWireRun(t *testing.T, stream, run, name string, started time.Time) {
	t.Helper()
	if stream != "stream-01" || run != "run-01" || name != "Morning" || started.Format(time.RFC3339Nano) != "2026-08-18T05:06:29.123456789Z" {
		t.Error("canonical wire lost archive run identity")
	}
}

func assertArchiveWireSecrets(t *testing.T, calls map[string]int, youtube bool) {
	t.Helper()
	want := map[string]int{"drive_destination:dest-01:folder_id": 1, "oauth_provider:provider-01:client_secret": 1, "oauth_account:account-01:refresh_token": 1}
	if youtube {
		want["youtube_stream_key:stream-01"] = 1
	}
	if !reflect.DeepEqual(calls, want) {
		t.Error("secret resolution did not match the assigned runtime references exactly once")
	}
}

func assertArchiveWireNoSecrets(t *testing.T, body string) {
	t.Helper()
	for _, value := range []string{"resolved-folder", "resolved-client-secret", "resolved-refresh-token", "resolved-stream-key", "drive_destination:", "oauth_provider:", "oauth_account:"} {
		if strings.Contains(body, value) {
			t.Error("archive wire response disclosed secret material or reference context")
		}
	}
}

func TestArchiveWireStrictFraming(t *testing.T) {
	for _, path := range []string{"/streams/start", "/streams/dry-run", "/streams/package"} {
		for _, tc := range []struct{ name, body string }{
			{"unknown", `{"stream_id":"stream-01","unexpected":null}`},
			{"base_path", `{"stream_id":"stream-01","base_path":"legacy"}`},
			{"trailing_object", `{"stream_id":"stream-01"} {}`},
			{"trailing_null", `{"stream_id":"stream-01"} null`},
			{"trailing_array", `{"stream_id":"stream-01"} []`},
			{"raw_stream_key", `{"stream_id":"stream-01","stream_key":"wire-secret-sentinel"}`},
		} {
			t.Run(path+"/"+tc.name, func(t *testing.T) {
				assertArchiveWireRejected(t, path, tc.body, http.StatusBadRequest, "bad_request")
			})
		}
		t.Run(path+"/body_limit", func(t *testing.T) {
			body := `{"stream_id":"stream-01","name":"` + strings.Repeat("a", maxControlBodyBytes) + `"}`
			assertArchiveWireRejected(t, path, body, http.StatusRequestEntityTooLarge, "request_body_too_large")
		})
		if path != "/streams/package" {
			for _, nested := range []string{`"refresh_token":"wire-secret-sentinel"`, `"arbitrary":null`, `"complete_retry_count":"wrong-type"`} {
				body := `{"stream_id":"stream-01","youtube_runtime":{"mode":"live_api",` + nested + `}}`
				assertArchiveWireRejected(t, path, body, http.StatusBadRequest, "bad_request")
			}
		}
	}
}

func TestArchiveWirePreservesAuthenticationAndConfigurationPriority(t *testing.T) {
	for _, path := range []string{"/streams/start", "/streams/dry-run", "/streams/package"} {
		t.Run(path, func(t *testing.T) {
			handler := archiveWireTestHandler(path, nil, nil, nil)
			req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"archive_config":null}`))
			res := httptest.NewRecorder()
			handler.ServeHTTP(res, req)
			if res.Code != http.StatusUnauthorized {
				t.Fatalf("authentication must precede wire rejection, status=%d", res.Code)
			}
			if path == "/streams/start" {
				req = httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"archive_config":null}`))
				req.Header.Set("Authorization", "Bearer service-token")
				res = httptest.NewRecorder()
				handler.ServeHTTP(res, req)
				if res.Code != http.StatusInternalServerError || !strings.Contains(res.Body.String(), `"code":"process_manager_not_configured"`) {
					t.Error("server configuration priority changed")
				}
			}
		})
	}
}

func TestArchiveWireInternalRawSecretProtection(t *testing.T) {
	for _, field := range []string{"FolderID", "ServiceAccountJSON", "ClientSecret", "RefreshToken"} {
		for _, withResolver := range []bool{false, true} {
			t.Run(field+"/"+map[bool]string{false: "no_resolver", true: "resolver"}[withResolver], func(t *testing.T) {
				config := lifecycle.ArchiveConfig{ArchiveProfileID: "archive-profile-01", AuthMode: "oauth2", FolderIDSecretName: "drive_destination:dest-01:folder_id"}
				reflect.ValueOf(&config).Elem().FieldByName(field).SetString("wire-secret-sentinel")
				var resolver RuntimeSecretResolver
				calls := 0
				if withResolver {
					resolver = func(context.Context, string, string, string) (string, error) {
						calls++
						return "", nil
					}
				}
				start := lifecycle.StreamJob{StreamID: "stream-01", ArchiveConfig: config}
				pack := lifecycle.PackageJob{StreamID: "stream-01", ArchiveConfig: config}
				for _, err := range []error{resolveArchiveRuntimeSecrets(t.Context(), &start, resolver), resolvePackageArchiveRuntimeSecrets(t.Context(), &pack, resolver)} {
					if !errors.Is(err, errRawArchiveSecretFieldsNotAllowed) {
						t.Error("internal raw secret guard was bypassed")
					}
				}
				if calls != 0 || start.ArchiveConfig != config || pack.ArchiveConfig != config {
					t.Error("raw internal configuration caused resolution or mutation")
				}
			})
		}
	}
}

func TestArchiveWireProfileRequiresAssignedReadyRuntime(t *testing.T) {
	providers := map[string]RuntimeConfigProvider{
		"nil": nil,
		"failed": func(context.Context) (control.RuntimeConfig, error) {
			return control.RuntimeConfig{}, errors.New("unavailable")
		},
		"missing": testYouTubeRuntimeProvider("stream-01"),
	}
	for name, item := range map[string]control.RuntimeArchiveStreamConfig{
		"wrong_stream":  {StreamID: "other", Ready: true, AssignmentRole: "primary", ArchiveProfileID: "archive-profile-01"},
		"not_ready":     {StreamID: "stream-01", Ready: false, AssignmentRole: "primary", ArchiveProfileID: "archive-profile-01"},
		"wrong_profile": {StreamID: "stream-01", Ready: true, AssignmentRole: "primary", ArchiveProfileID: "other-profile"},
	} {
		providers[name] = func(ctx context.Context) (control.RuntimeConfig, error) {
			cfg, err := testYouTubeRuntimeProvider("stream-01")(ctx)
			cfg.StreamArchiveConfigs = []control.RuntimeArchiveStreamConfig{item}
			return cfg, err
		}
	}
	for _, path := range []string{"/streams/start", "/streams/dry-run"} {
		for name, provider := range providers {
			t.Run(path+"/"+name, func(t *testing.T) {
				starter := &httpFakeStarter{}
				root := t.TempDir()
				t.Setenv("AUTOSTREAM_ARCHIVE_DIR", root)
				handler := archiveWireTestHandler(path, &streamproc.Manager{ArchiveRoot: root, Starter: starter}, nil, provider)
				req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"stream_id":"stream-01","name":"Morning","rtmp_url":"","archive_profile_id":"archive-profile-01"}`))
				req.Header.Set("Authorization", "Bearer service-token")
				res := httptest.NewRecorder()
				handler.ServeHTTP(res, req)
				if res.Code != http.StatusBadGateway || !strings.Contains(res.Body.String(), `"code":"runtime_config_fetch_failed"`) {
					t.Fatalf("selected profile silently lost runtime authority, status=%d", res.Code)
				}
				files, err := os.ReadDir(root)
				if err != nil || len(files) != 0 || starter.process != nil {
					t.Error("unresolved selected profile caused side effects")
				}
			})
		}
	}
}

func TestArchiveWireEmptyProfileAndDryRunRemainAccepted(t *testing.T) {
	for _, path := range []string{"/streams/start", "/streams/dry-run"} {
		t.Run(path, func(t *testing.T) {
			root := t.TempDir()
			t.Setenv("AUTOSTREAM_ARCHIVE_DIR", root)
			starter := &httpFakeStarter{}
			manager := &streamproc.Manager{ArchiveRoot: root, Starter: starter, InputResolver: testInputResolver, AllowHostnameInputs: true, OutputRelayMode: outputrelay.ModeDirect}
			t.Cleanup(func() { _, _ = manager.Stop("stream-01") })
			handler := archiveWireTestHandler(path, manager, testYouTubeSecretResolver, testYouTubeRuntimeProvider("stream-01"))
			req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"stream_id":"stream-01","name":"Morning","input_url":"srt://input.example.com:9000","rtmp_url":"","archive_profile_id":""}`))
			req.Header.Set("Authorization", "Bearer service-token")
			res := httptest.NewRecorder()
			handler.ServeHTTP(res, req)
			if res.Code != http.StatusAccepted {
				t.Fatalf("empty archive profile rejected, status=%d", res.Code)
			}
			if path == "/streams/dry-run" && starter.process != nil {
				t.Error("dry-run launched a process")
			}
		})
	}
}

func TestArchiveWireRejectsUnsafeOrIncompleteRunIdentity(t *testing.T) {
	for _, path := range []string{"/streams/start", "/streams/dry-run", "/streams/package"} {
		for _, tc := range []struct{ name, stream, run, started string }{
			{"unsafe_stream", "../outside", "run-01", "2026-08-18T05:06:29Z"},
			{"unsafe_run", "stream-01", "../outside", "2026-08-18T05:06:29Z"},
			{"missing_started_at", "stream-01", "run-01", ""},
		} {
			t.Run(path+"/"+tc.name, func(t *testing.T) {
				root := t.TempDir()
				t.Setenv("AUTOSTREAM_ARCHIVE_DIR", root)
				t.Setenv("AUTOSTREAM_NODE_CONFIG", filepath.Join(root, "no-report-config.yml"))
				t.Setenv("OBSERVABILITY_URL", "")
				starter := &httpFakeStarter{}
				manager := &streamproc.Manager{ArchiveRoot: root, Starter: starter, InputResolver: testInputResolver, AllowHostnameInputs: true, OutputRelayMode: outputrelay.ModeDirect}
				payload := map[string]any{"stream_id": tc.stream, "name": "Morning", "archive_run_id": tc.run, "dry_run": true}
				t.Cleanup(func() { _, _ = manager.Stop(tc.stream) })
				if tc.started != "" {
					payload["started_at"] = tc.started
				}
				if path != "/streams/package" {
					payload["input_url"] = "srt://input.example.com:9000"
					payload["rtmp_url"] = ""
				}
				body, err := json.Marshal(payload)
				if err != nil {
					t.Fatal(err)
				}
				handler := archiveWireTestHandler(path, manager, testYouTubeSecretResolver, testYouTubeRuntimeProvider(tc.stream))
				req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(body)))
				req.Header.Set("Authorization", "Bearer service-token")
				res := httptest.NewRecorder()
				handler.ServeHTTP(res, req)
				if res.Code != http.StatusBadRequest || starter.process != nil {
					t.Fatalf("invalid run identity crossed the process boundary, status=%d", res.Code)
				}
				files, err := os.ReadDir(root)
				if err != nil || len(files) != 0 {
					t.Error("invalid run identity changed archive storage")
				}
				if _, err := os.Stat(filepath.Join(filepath.Dir(root), "outside")); !errors.Is(err, os.ErrNotExist) {
					t.Error("invalid identity escaped the archive root")
				}
			})
		}
	}
}

func TestArchiveWireWorkerVideoTokenBoundary(t *testing.T) {
	const signingKey = "archive-wire-test-signing-key"
	for _, tc := range []struct {
		name, stream string
		optIn        bool
		status       int
		code         string
	}{
		{"valid_signed", "stream-01", true, http.StatusServiceUnavailable, "worker_video_ingest_unavailable"},
		{"wrong_stream", "other", true, http.StatusUnauthorized, "missing_or_invalid_worker_video_ingest_token"},
		{"missing_opt_in", "stream-01", false, http.StatusBadRequest, "worker_video_ingest_not_enabled"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			token, err := ingesttoken.Issue(signingKey, ingesttoken.Claims{StreamID: tc.stream, ServiceID: "worker-01", ServiceType: "worker", Purpose: "worker_video", Audience: "encoder_recorder", ExpiresAt: time.Now().Add(time.Minute).Unix()})
			if err != nil {
				t.Fatal("could not issue synthetic ingest token")
			}
			root := t.TempDir()
			starter := &httpFakeStarter{}
			manager := &streamproc.Manager{ArchiveRoot: root, Starter: starter, OutputRelayMode: outputrelay.ModeDirect}
			// A valid token reaches the unavailable bridge boundary; invalid tokens
			// must fail earlier. No media listener or FFmpeg is needed for this check.
			handler := startStream(manager, nil, nil, TokenVerifier{PlainToken: "service-token", IngestTokenSigningKey: signingKey, RequireSignedIngest: true}, testYouTubeSecretResolver, archiveWireRuntimeProvider(nil))
			body, err := json.Marshal(map[string]any{"stream_id": "stream-01", "name": "Morning", "rtmp_url": "", "encoder_profile_id": "enc-profile-01", "worker_video_ingest": tc.optIn, "worker_video_ingest_token": token})
			if err != nil {
				t.Fatal("could not encode signed ingest fixture")
			}
			req := httptest.NewRequest(http.MethodPost, "/streams/start", strings.NewReader(string(body)))
			req.Header.Set("Authorization", "Bearer service-token")
			res := httptest.NewRecorder()
			handler.ServeHTTP(res, req)
			var result map[string]string
			if err := json.Unmarshal(res.Body.Bytes(), &result); err != nil || res.Code != tc.status || result["code"] != tc.code {
				t.Errorf("signed ingest boundary status=%d, want %d", res.Code, tc.status)
			}
			files, err := os.ReadDir(root)
			if err != nil || len(files) != 0 || starter.process != nil || strings.Contains(res.Body.String(), token) {
				t.Error("token boundary caused side effects or disclosed the ingest token")
			}
		})
	}
}
