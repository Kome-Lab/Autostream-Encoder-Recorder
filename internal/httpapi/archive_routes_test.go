package httpapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/example/autostream-encoder-recorder/internal/archive"
	"github.com/example/autostream-encoder-recorder/internal/control"
	"github.com/example/autostream-encoder-recorder/internal/lifecycle"
	"github.com/example/autostream-encoder-recorder/internal/outputrelay"
	"github.com/example/autostream-encoder-recorder/internal/streamproc"
	"github.com/example/autostream-encoder-recorder/internal/workerevents"
)

func TestRunlessArchiveArtifactRoutesAreRemoved(t *testing.T) {
	handler := NewServerWithManagers("encoder_recorder", nil, workerevents.NewManager(t.TempDir()), TokenVerifier{PlainToken: "service-token"})
	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		req := httptest.NewRequest(method, "/streams/stream-01/artifacts/final.mp4", nil)
		req.Header.Set("Authorization", "Bearer service-token")
		res := httptest.NewRecorder()
		handler.ServeHTTP(res, req)
		if res.Code != http.StatusNotFound {
			t.Fatalf("removed runless route method=%s status=%d body=%s", method, res.Code, res.Body.String())
		}
	}
}

func TestArchiveArtifactEndpointUsesRunScopedDirectory(t *testing.T) {
	root := t.TempDir()
	layout, err := archive.NewRunLayout(root, "stream-01", "run-01")
	if err != nil {
		t.Fatal(err)
	}
	if err := archive.EnsureDirNoSymlinks(layout.RootDir, layout.FinalDir()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(layout.FinalMP4(), []byte("run-archive"), 0o640); err != nil {
		t.Fatal(err)
	}
	handler := newV2TestServerWithManagers(t, nil, workerevents.NewManager(root), TokenVerifier{PlainToken: "service-token"}, "stream-required-relay")
	req := httptest.NewRequest(http.MethodGet, "/streams/stream-01/archive-runs/run-01/artifacts/final.mp4", nil)
	req.Header.Set("Authorization", "Bearer service-token")
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusOK || res.Body.String() != "run-archive" {
		t.Fatalf("run-scoped download status = %d body = %q", res.Code, res.Body.String())
	}

	unsafeReq := httptest.NewRequest(http.MethodGet, "/streams/stream-01/archive-runs/bad..run/artifacts/final.mp4", nil)
	unsafeReq.Header.Set("Authorization", "Bearer service-token")
	unsafeRes := httptest.NewRecorder()
	handler.ServeHTTP(unsafeRes, unsafeReq)
	if unsafeRes.Code != http.StatusBadRequest {
		t.Fatalf("unsafe run status = %d body = %s", unsafeRes.Code, unsafeRes.Body.String())
	}
}

func TestPreviewEndpointRequiresTokenAndServesHLSWithSafeHeaders(t *testing.T) {
	root := t.TempDir()
	layout, err := archive.NewLayout(root, "stream-01")
	if err != nil {
		t.Fatal(err)
	}
	if err := archive.PreparePreviewDir(layout); err != nil {
		t.Fatal(err)
	}
	playlist := []byte("#EXTM3U\n#EXTINF:2.0,\nsegment-000001.ts\n")
	segment := []byte{0, 1, 2, 3, 4, 5}
	if err := os.WriteFile(layout.PreviewPlaylist(), playlist, 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(layout.PreviewDir(), "segment-000001.ts"), segment, 0o640); err != nil {
		t.Fatal(err)
	}
	server := NewServerWithManagers(
		"encoder_recorder",
		&streamproc.Manager{ArchiveRoot: root, OutputRelayMode: outputrelay.ModeDirect},
		nil,
		TokenVerifier{PlainToken: "preview-token"},
	)

	unauthorized := httptest.NewRecorder()
	server.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodGet, "/streams/stream-01/preview/index.m3u8", nil))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("expected preview auth failure, got %d: %s", unauthorized.Code, unauthorized.Body.String())
	}
	invalidTokenRequest := httptest.NewRequest(http.MethodGet, "/streams/stream-01/preview/index.m3u8", nil)
	invalidTokenRequest.Header.Set("Authorization", "Bearer wrong-token")
	invalidTokenResponse := httptest.NewRecorder()
	server.ServeHTTP(invalidTokenResponse, invalidTokenRequest)
	if invalidTokenResponse.Code != http.StatusUnauthorized {
		t.Fatalf("expected invalid preview token rejection, got %d: %s", invalidTokenResponse.Code, invalidTokenResponse.Body.String())
	}

	playlistRequest := httptest.NewRequest(http.MethodGet, "/streams/stream-01/preview/index.m3u8", nil)
	playlistRequest.Header.Set("Authorization", "Bearer preview-token")
	playlistResponse := httptest.NewRecorder()
	server.ServeHTTP(playlistResponse, playlistRequest)
	if playlistResponse.Code != http.StatusOK || playlistResponse.Body.String() != string(playlist) {
		t.Fatalf("unexpected playlist response: status=%d body=%q", playlistResponse.Code, playlistResponse.Body.String())
	}
	if got := playlistResponse.Header().Get("Content-Type"); got != "application/vnd.apple.mpegurl" {
		t.Fatalf("unexpected playlist Content-Type: %q", got)
	}
	if got := playlistResponse.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("unexpected playlist Cache-Control: %q", got)
	}
	if got := playlistResponse.Header().Get("Vary"); got != "Authorization" {
		t.Fatalf("unexpected playlist Vary: %q", got)
	}

	segmentRequest := httptest.NewRequest(http.MethodGet, "/streams/stream-01/preview/segment-000001.ts", nil)
	segmentRequest.Header.Set("Authorization", "Bearer preview-token")
	segmentRequest.Header.Set("Range", "bytes=1-3")
	segmentResponse := httptest.NewRecorder()
	server.ServeHTTP(segmentResponse, segmentRequest)
	if segmentResponse.Code != http.StatusPartialContent || !bytes.Equal(segmentResponse.Body.Bytes(), segment[1:4]) {
		t.Fatalf("unexpected ranged segment response: status=%d body=%v", segmentResponse.Code, segmentResponse.Body.Bytes())
	}
	if got := segmentResponse.Header().Get("Content-Type"); got != "video/mp2t" {
		t.Fatalf("unexpected segment Content-Type: %q", got)
	}
	if got := segmentResponse.Header().Get("Cache-Control"); got != "private, max-age=30" {
		t.Fatalf("unexpected segment Cache-Control: %q", got)
	}
	if got := segmentResponse.Header().Get("Content-Range"); got != "bytes 1-3/6" {
		t.Fatalf("unexpected segment Content-Range: %q", got)
	}
}

func TestPreviewEndpointUsesProcessManagerArchiveRoot(t *testing.T) {
	processRoot := t.TempDir()
	eventRoot := t.TempDir()
	layout, err := archive.NewLayout(processRoot, "stream-01")
	if err != nil {
		t.Fatal(err)
	}
	if err := archive.PreparePreviewDir(layout); err != nil {
		t.Fatal(err)
	}
	playlist := []byte("#EXTM3U\n#EXTINF:2.0,\nsegment-000001.ts\n")
	if err := os.WriteFile(layout.PreviewPlaylist(), playlist, 0o640); err != nil {
		t.Fatal(err)
	}
	handler := NewServerWithManagersAndRuntimeConfig(
		"encoder_recorder",
		&streamproc.Manager{ArchiveRoot: processRoot, OutputRelayMode: outputrelay.ModeDirect},
		workerevents.NewManager(eventRoot),
		TokenVerifier{PlainToken: "preview-token"},
		nil,
		nil,
	)
	req := httptest.NewRequest(http.MethodGet, "/streams/stream-01/preview/index.m3u8", nil)
	req.Header.Set("Authorization", "Bearer preview-token")
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusOK || !bytes.Equal(res.Body.Bytes(), playlist) {
		t.Fatalf("preview did not use process manager archive root: status=%d body=%q", res.Code, res.Body.String())
	}
}

func TestPreviewEndpointRejectsNamesTraversalAndMissingFiles(t *testing.T) {
	root := t.TempDir()
	verifier := TokenVerifier{PlainToken: "preview-token"}
	handler := streamPreview(root, verifier)

	request := func(streamID, name, token string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/preview", nil)
		req.SetPathValue("id", streamID)
		req.SetPathValue("name", name)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, req)
		return response
	}

	if response := request("../outside", "index.m3u8", ""); response.Code != http.StatusUnauthorized {
		t.Fatalf("authorization must run before path validation, got %d", response.Code)
	}
	for _, test := range []struct {
		streamID string
		name     string
	}{
		{streamID: "../outside", name: "index.m3u8"},
		{streamID: "stream-01", name: "../index.m3u8"},
		{streamID: "stream-01", name: "other.m3u8"},
		{streamID: "stream-01", name: "segment-00001.ts"},
		{streamID: "stream-01", name: "segment-000001.ts.tmp"},
	} {
		if response := request(test.streamID, test.name, "preview-token"); response.Code != http.StatusBadRequest {
			t.Errorf("expected invalid preview path rejection for stream=%q name=%q, got %d", test.streamID, test.name, response.Code)
		}
	}
	if response := request("stream-01", "segment-000001.ts", "preview-token"); response.Code != http.StatusNotFound {
		t.Fatalf("expected missing preview file response, got %d: %s", response.Code, response.Body.String())
	}
}

func TestPreviewEndpointRejectsSymlinkFile(t *testing.T) {
	root := t.TempDir()
	layout, err := archive.NewLayout(root, "stream-01")
	if err != nil {
		t.Fatal(err)
	}
	if err := archive.PreparePreviewDir(layout); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(root, "outside.ts")
	if err := os.WriteFile(outside, []byte("outside"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(layout.PreviewDir(), "segment-000001.ts")); err != nil {
		t.Skipf("symlink creation is not available in this environment: %v", err)
	}
	server := NewServerWithManagers("encoder_recorder", &streamproc.Manager{ArchiveRoot: root, OutputRelayMode: outputrelay.ModeDirect}, nil, TokenVerifier{PlainToken: "preview-token"})
	req := httptest.NewRequest(http.MethodGet, "/streams/stream-01/preview/segment-000001.ts", nil)
	req.Header.Set("Authorization", "Bearer preview-token")
	response := httptest.NewRecorder()
	server.ServeHTTP(response, req)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("expected preview symlink rejection, got %d: %s", response.Code, response.Body.String())
	}
	if body, readErr := os.ReadFile(outside); readErr != nil || string(body) != "outside" {
		t.Fatalf("symlink target must not be served or modified: body=%q err=%v", body, readErr)
	}
}

func TestPackageStreamReturnsConflictWhenRuntimeSecretLeaseActive(t *testing.T) {
	t.Setenv("SERVICE_CONTROL_TOKEN", "service-token")
	resolved := 0
	handler := archiveWireTestHandler("/streams/package", nil, func(ctx context.Context, streamID, archiveProfileID, secretName string) (string, error) {
		resolved++
		if streamID != "stream-01" || archiveProfileID != "archive-profile-01" || secretName != "oauth_account:account-01:refresh_token" {
			t.Error("lease resolve context differs from assigned archive runtime")
		}
		return "", control.ErrRuntimeSecretLeaseActive
	}, archiveWireRuntimeProvider(map[string]any{"auth_mode": "oauth2", "refresh_token_secret_name": "oauth_account:account-01:refresh_token"}))

	body := `{"stream_id":"stream-01","archive_run_id":"run-01","name":"Morning Stream","started_at":"2026-05-31T01:02:03Z","dry_run":true}`
	req := httptest.NewRequest(http.MethodPost, "/streams/package", bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer service-token")
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if resolved != 1 {
		t.Fatalf("lease resolver calls=%d, want 1", resolved)
	}
	if res.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d body = %s", res.Code, res.Body.String())
	}
	if !strings.Contains(res.Body.String(), `"code":"runtime_secret_lease_active"`) {
		t.Fatalf("expected runtime secret lease code, got %s", res.Body.String())
	}
	if strings.Contains(res.Body.String(), "refresh_token") || strings.Contains(res.Body.String(), "account-01") {
		t.Fatalf("runtime secret resolve response leaked context details: %s", res.Body.String())
	}
}

func TestPackageStreamRejectsRawArchiveSecretFields(t *testing.T) {
	t.Setenv("SERVICE_CONTROL_TOKEN", "service-token")
	handler := archiveWireTestHandler("/streams/package", nil, func(ctx context.Context, streamID, archiveProfileID, secretName string) (string, error) {
		t.Fatalf("runtime secret resolver should not be called for raw archive secret fields")
		return "", nil
	}, nil)

	body := `{"stream_id":"stream-01","archive_run_id":"run-01","name":"Morning Stream","started_at":"2026-05-31T01:02:03Z","dry_run":true,"archive_config":{"archive_profile_id":"archive-profile-01","auth_mode":"oauth2","refresh_token":"raw-refresh-token","folder_id_secret_name":"drive_destination:dest-01:folder_id"}}`
	req := httptest.NewRequest(http.MethodPost, "/streams/package", bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer service-token")
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d body = %s", res.Code, res.Body.String())
	}
	if !strings.Contains(res.Body.String(), `"code":"bad_request"`) {
		t.Fatalf("expected raw archive secret error, got %s", res.Body.String())
	}
	if strings.Contains(res.Body.String(), "raw-refresh-token") || strings.Contains(res.Body.String(), "refresh_token") || strings.Contains(res.Body.String(), "folder_id") {
		t.Fatalf("raw archive secret rejection leaked field context: %s", res.Body.String())
	}
}

func TestPackageEndpoint(t *testing.T) {
	t.Setenv("SERVICE_CONTROL_TOKEN", "service-token")
	root := t.TempDir()
	t.Setenv("AUTOSTREAM_ARCHIVE_DIR", root)
	tmpDir := filepath.Join(root, "tmp", "stream-01")
	finalDir := filepath.Join(root, "final", "stream-01", "run-01")
	if err := os.MkdirAll(tmpDir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(finalDir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmpDir, "final.mkv"), []byte("mkv"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(finalDir, "final.mp4"), []byte("mp4"), 0o640); err != nil {
		t.Fatal(err)
	}
	handler := NewServer("encoder_recorder")
	body := `{"stream_id":"stream-01","archive_run_id":"run-01","name":"Morning Stream","started_at":"2026-05-31T01:02:03Z","dry_run":true}`
	req := httptest.NewRequest(http.MethodPost, "/streams/package", bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer service-token")
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusAccepted {
		t.Fatalf("status = %d body = %s", res.Code, res.Body.String())
	}
	if !strings.Contains(res.Body.String(), `"attempts":1`) {
		t.Fatalf("expected upload attempts in response: %s", res.Body.String())
	}
}

func TestPackageEndpointAppliesControlPanelArchiveRuntimeConfig(t *testing.T) {
	t.Setenv("SERVICE_CONTROL_TOKEN", "service-token")
	root := t.TempDir()
	t.Setenv("AUTOSTREAM_ARCHIVE_DIR", root)
	tmpDir := filepath.Join(root, "tmp", "stream-01")
	if err := os.MkdirAll(tmpDir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmpDir, "final.mkv"), []byte("mkv"), 0o640); err != nil {
		t.Fatal(err)
	}
	resolvedSecrets := map[string]string{}
	handler := NewServerWithManagersAndRuntimeConfig(
		"encoder_recorder",
		nil,
		workerevents.NewManager(root),
		TokenVerifier{PlainToken: "service-token"},
		func(ctx context.Context, streamID, archiveProfileID, secretName string) (string, error) {
			if streamID != "stream-01" || archiveProfileID != "archive-profile-01" {
				t.Fatalf("unexpected package archive resolve context stream=%q profile=%q secret=%q", streamID, archiveProfileID, secretName)
			}
			resolvedSecrets[secretName] = archiveProfileID
			switch secretName {
			case "drive_destination:dest-01:folder_id":
				return "raw-drive-folder-id", nil
			case "oauth_provider:provider-01:client_secret":
				return "raw-client-secret", nil
			case "oauth_account:account-01:refresh_token":
				return "raw-refresh-token", nil
			default:
				return "", errors.New("unexpected package archive runtime secret")
			}
		},
		func(ctx context.Context) (control.RuntimeConfig, error) {
			return control.RuntimeConfig{
				StreamArchiveConfigs: []control.RuntimeArchiveStreamConfig{{
					StreamID:         "stream-01",
					AssignmentRole:   "primary",
					ArchiveProfileID: "archive-profile-01",
					Ready:            true,
					ArchiveConfig: map[string]any{
						"drive_destination_id":      "dest-01",
						"auth_mode":                 "oauth2",
						"oauth_account_id":          "account-01",
						"oauth_provider_id":         "provider-01",
						"folder_id_secret_name":     "drive_destination:dest-01:folder_id",
						"shared_drive":              true,
						"retention_days":            float64(45),
						"client_id":                 "google-client-id",
						"client_secret_secret_name": "oauth_provider:provider-01:client_secret",
						"refresh_token_secret_name": "oauth_account:account-01:refresh_token",
					},
				}},
			}, nil
		},
	)

	body := `{"stream_id":"stream-01","archive_run_id":"run-01","name":"Morning Stream","started_at":"2026-05-31T01:02:03Z","dry_run":true}`
	req := httptest.NewRequest(http.MethodPost, "/streams/package", bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer service-token")
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusAccepted {
		t.Fatalf("package status = %d body = %s", res.Code, res.Body.String())
	}
	for _, secretName := range []string{
		"drive_destination:dest-01:folder_id",
		"oauth_provider:provider-01:client_secret",
		"oauth_account:account-01:refresh_token",
	} {
		if resolvedSecrets[secretName] != "archive-profile-01" {
			t.Fatalf("expected package runtime archive secret %q to be resolved, got %#v", secretName, resolvedSecrets)
		}
	}
	for _, expected := range []string{`"auth_mode":"oauth2"`, `"shared_drive":true`, `"retention_days":45`, `"folder_id_configured":true`, `"client_secret_configured":true`, `"refresh_token_configured":true`} {
		if !strings.Contains(res.Body.String(), expected) {
			t.Fatalf("expected safe archive config summary %q in response: %s", expected, res.Body.String())
		}
	}
	for _, leaked := range []string{"raw-drive-folder-id", "raw-client-secret", "raw-refresh-token", "drive_destination:dest-01:folder_id", "oauth_provider:provider-01:client_secret", "oauth_account:account-01:refresh_token"} {
		if strings.Contains(res.Body.String(), leaked) {
			t.Fatalf("package response leaked archive runtime config detail %q: %s", leaked, res.Body.String())
		}
	}
}

func TestPackageEndpointReturnsSafeFailureClassification(t *testing.T) {
	t.Setenv("SERVICE_CONTROL_TOKEN", "service-token")
	root := t.TempDir()
	t.Setenv("AUTOSTREAM_ARCHIVE_DIR", root)
	handler := NewServer("encoder_recorder")
	body := `{"stream_id":"stream-01","archive_run_id":"run-01","name":"Morning Stream","started_at":"2026-05-31T01:02:03Z","dry_run":true}`
	req := httptest.NewRequest(http.MethodPost, "/streams/package", bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer service-token")
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body = %s", res.Code, res.Body.String())
	}
	if !strings.Contains(res.Body.String(), `"code":"package_failed"`) ||
		!strings.Contains(res.Body.String(), `"failure_phase":"input"`) ||
		!strings.Contains(res.Body.String(), `"error_class":"archive_input_unavailable"`) {
		t.Fatalf("expected failure classification, got %s", res.Body.String())
	}
	if strings.Contains(res.Body.String(), root) || strings.Contains(res.Body.String(), "final.mkv") {
		t.Fatalf("raw local path leaked in failure response: %s", res.Body.String())
	}
}

func TestUploaderFromEnvIgnoresLegacyGoogleDriveEnvFallback(t *testing.T) {
	t.Setenv("GOOGLE_DRIVE_AUTH_MODE", "service_account")
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", "/etc/autostream/google-service-account.json")
	t.Setenv("GOOGLE_DRIVE_FOLDER_ID", "folder-id")

	if _, ok := uploaderFromEnv(false).(archive.DryRunUploader); !ok {
		t.Fatal("expected DryRunUploader because env fallback is unsupported")
	}
	if _, ok := uploaderFromEnv(true).(archive.DryRunUploader); !ok {
		t.Fatal("expected DryRunUploader for dry-run packaging")
	}
}

func TestPackageFailureAttributesDoNotExposeRawError(t *testing.T) {
	attrs := packageFailureAttributes(lifecycle.PackageError{Phase: "upload", Err: errors.New("https://example.com/upload?token=secret")}, false)
	if attrs["failure_phase"] != "upload" || attrs["error_class"] != "archive_upload_failed" {
		t.Fatalf("unexpected attributes: %#v", attrs)
	}
	if _, ok := attrs["error"]; ok || strings.Contains(attrsString(attrs), "secret") {
		t.Fatalf("raw error leaked in attributes: %#v", attrs)
	}
}

func TestPackageFailureResponseDoesNotExposeRawError(t *testing.T) {
	response := packageFailureResponse(lifecycle.PackageError{Phase: "remux", Err: errors.New(`C:\archives\tmp\stream-01\final.mkv failed with token=secret-token`)}, false)
	body, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	if !strings.Contains(text, `"failure_phase":"remux"`) || !strings.Contains(text, `"error_class":"ffmpeg_remux_failed"`) {
		t.Fatalf("expected safe failure classification, got %s", text)
	}
	for _, forbidden := range []string{"secret-token", "final.mkv", `C:\archives`, "token="} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("raw error detail leaked in package failure response: %s", text)
		}
	}
}

func TestWorkerEventsEndpointWritesArchiveSidecars(t *testing.T) {
	root := t.TempDir()
	eventManager := workerevents.NewManager(root)
	starter := &httpFakeStarter{}
	processManager := &streamproc.Manager{ArchiveRoot: root, FFmpegBin: "ffmpeg", Starter: starter, InputResolver: testInputResolver, AllowHostnameInputs: true, OutputRelayMode: outputrelay.ModeDirect}
	controlSum := sha256.Sum256([]byte("control-token"))
	workerSum := sha256.Sum256([]byte("worker-token"))
	handler := newV2TestServerWithManagers(t, processManager, eventManager, TokenVerifier{SHA256Hex: hex.EncodeToString(controlSum[:]), WorkerEventsSHA256Hex: hex.EncodeToString(workerSum[:])}, "stream-01")
	startReq := httptest.NewRequest(http.MethodPost, "/streams/start", bytes.NewBufferString(`{"stream_id":"stream-01","name":"Worker Event Stream","input_url":"srt://input.example.com:9000","rtmp_url":"rtmps://youtube.example.com/live2"}`))
	startReq.Header.Set("Authorization", "Bearer control-token")
	startRes := httptest.NewRecorder()
	handler.ServeHTTP(startRes, startReq)
	if startRes.Code != http.StatusAccepted {
		t.Fatalf("start status = %d body = %s", startRes.Code, startRes.Body.String())
	}

	body := `{"id":"event-01","stream_id":"stream-01","type":"caption.final","payload":{"text":"hello","speaker_user_id":"user-01"},"timestamp":"2026-05-28T00:00:00Z"}`
	req := httptest.NewRequest(http.MethodPost, "/worker-events", bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer worker-token")
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusAccepted {
		t.Fatalf("status = %d body = %s", res.Code, res.Body.String())
	}
	if strings.Contains(res.Body.String(), root) || strings.Contains(res.Body.String(), `\tmp\`) || strings.Contains(res.Body.String(), `/tmp/`) {
		t.Fatalf("local archive path leaked in worker event response: %s", res.Body.String())
	}
	if _, err := os.Stat(filepath.Join(root, "tmp", "stream-01", "logs.jsonl")); err != nil {
		t.Fatal(err)
	}
	captions, err := os.ReadFile(filepath.Join(root, "tmp", "stream-01", "captions.vtt"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(captions), "hello") {
		t.Fatalf("caption not written: %s", string(captions))
	}
	transcript, err := os.ReadFile(filepath.Join(root, "tmp", "stream-01", "transcript.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(transcript), "user-01") {
		t.Fatalf("transcript not written: %s", string(transcript))
	}
}
