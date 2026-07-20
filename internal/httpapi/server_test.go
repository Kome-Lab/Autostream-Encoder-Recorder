package httpapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/example/autostream-encoder-recorder/internal/archive"
	"github.com/example/autostream-encoder-recorder/internal/control"
	"github.com/example/autostream-encoder-recorder/internal/ingesttoken"
	"github.com/example/autostream-encoder-recorder/internal/lifecycle"
	"github.com/example/autostream-encoder-recorder/internal/streamproc"
	"github.com/example/autostream-encoder-recorder/internal/version"
	"github.com/example/autostream-encoder-recorder/internal/workerevents"
)

func testInputResolver(ctx context.Context, host string) ([]net.IP, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return []net.IP{net.ParseIP("93.184.216.34")}, nil
}

func TestUpdaterVersionEndpointIsUnauthenticated(t *testing.T) {
	originalVersion := version.Version
	version.Version = "v1.2.3"
	t.Cleanup(func() {
		version.Version = originalVersion
	})
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
	var body map[string]string
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(body) != 1 || body["version"] != version.Current() {
		t.Fatalf("body = %#v, want only version %q", body, version.Current())
	}
}

func TestDryRunEndpointRequiresAuthorization(t *testing.T) {
	t.Setenv("SERVICE_CONTROL_TOKEN", "service-token")
	handler := NewServer("encoder_recorder")
	req := httptest.NewRequest(http.MethodPost, "/streams/dry-run", bytes.NewBufferString(`{}`))
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d body = %s", res.Code, res.Body.String())
	}
}

func TestArchiveArtifactEndpointsRequireTokenAndSafeFinalFile(t *testing.T) {
	root := t.TempDir()
	layout, err := archive.NewLayout(root, "stream-01")
	if err != nil {
		t.Fatal(err)
	}
	if err := archive.EnsureDirNoSymlinks(layout.RootDir, layout.FinalDir()); err != nil {
		t.Fatal(err)
	}
	finalPath := filepath.Join(layout.FinalDir(), "final.mp4")
	if err := os.WriteFile(finalPath, []byte("archive-bytes"), 0o640); err != nil {
		t.Fatal(err)
	}
	handler := NewServerWithManagers("encoder_recorder", nil, workerevents.NewManager(root), TokenVerifier{PlainToken: "service-token"})

	unauthorizedReq := httptest.NewRequest(http.MethodGet, "/streams/stream-01/artifacts/final.mp4", nil)
	unauthorizedRes := httptest.NewRecorder()
	handler.ServeHTTP(unauthorizedRes, unauthorizedReq)
	if unauthorizedRes.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status = %d body = %s", unauthorizedRes.Code, unauthorizedRes.Body.String())
	}

	downloadReq := httptest.NewRequest(http.MethodGet, "/streams/stream-01/artifacts/final.mp4", nil)
	downloadReq.Header.Set("Authorization", "Bearer service-token")
	downloadRes := httptest.NewRecorder()
	handler.ServeHTTP(downloadRes, downloadReq)
	if downloadRes.Code != http.StatusOK || downloadRes.Body.String() != "archive-bytes" {
		t.Fatalf("download status = %d body = %s", downloadRes.Code, downloadRes.Body.String())
	}
	if downloadRes.Header().Get("Content-Disposition") != `attachment; filename="final.mp4"` {
		t.Fatalf("download content disposition = %q", downloadRes.Header().Get("Content-Disposition"))
	}

	unsafeReq := httptest.NewRequest(http.MethodGet, "/streams/stream-01/artifacts/bad..mp4", nil)
	unsafeReq.Header.Set("Authorization", "Bearer service-token")
	unsafeRes := httptest.NewRecorder()
	handler.ServeHTTP(unsafeRes, unsafeReq)
	if unsafeRes.Code != http.StatusBadRequest || !strings.Contains(unsafeRes.Body.String(), "invalid_archive_artifact") {
		t.Fatalf("unsafe status = %d body = %s", unsafeRes.Code, unsafeRes.Body.String())
	}

	renameReq := httptest.NewRequest(http.MethodPut, "/streams/stream-01/artifacts/final.mp4", strings.NewReader(`{"name":"renamed.mp4"}`))
	renameReq.Header.Set("Authorization", "Bearer service-token")
	renameRes := httptest.NewRecorder()
	handler.ServeHTTP(renameRes, renameReq)
	if renameRes.Code != http.StatusOK || !strings.Contains(renameRes.Body.String(), "renamed.mp4") {
		t.Fatalf("rename status = %d body = %s", renameRes.Code, renameRes.Body.String())
	}
	if _, err := os.Stat(filepath.Join(layout.FinalDir(), "renamed.mp4")); err != nil {
		t.Fatalf("renamed artifact missing: %v", err)
	}

	deleteReq := httptest.NewRequest(http.MethodDelete, "/streams/stream-01/artifacts/renamed.mp4", nil)
	deleteReq.Header.Set("Authorization", "Bearer service-token")
	deleteRes := httptest.NewRecorder()
	handler.ServeHTTP(deleteRes, deleteReq)
	if deleteRes.Code != http.StatusOK {
		t.Fatalf("delete status = %d body = %s", deleteRes.Code, deleteRes.Body.String())
	}
	if _, err := os.Stat(filepath.Join(layout.FinalDir(), "renamed.mp4")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("renamed artifact should be deleted, stat err=%v", err)
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
		&streamproc.Manager{ArchiveRoot: root},
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
		&streamproc.Manager{ArchiveRoot: processRoot},
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
	server := NewServerWithManagers("encoder_recorder", &streamproc.Manager{ArchiveRoot: root}, nil, TokenVerifier{PlainToken: "preview-token"})
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

func TestResolveArchiveRuntimeSecrets(t *testing.T) {
	job := lifecycle.StreamJob{
		StreamID: "stream-01",
		ArchiveConfig: lifecycle.ArchiveConfig{
			ArchiveProfileID:       "archive-profile-01",
			AuthMode:               "oauth2",
			FolderIDSecretName:     "drive_destination:dest-01:folder_id",
			ClientSecretSecretName: "oauth_provider:provider-01:client_secret",
			RefreshTokenSecretName: "oauth_account:account-01:refresh_token",
		},
	}
	resolved := map[string]string{
		"drive_destination:dest-01:folder_id":      "drive-folder-id",
		"oauth_provider:provider-01:client_secret": "google-client-secret",
		"oauth_account:account-01:refresh_token":   "google-refresh-token",
	}
	err := resolveArchiveRuntimeSecrets(context.Background(), &job, func(ctx context.Context, streamID, archiveProfileID, secretName string) (string, error) {
		if streamID != "stream-01" || archiveProfileID != "archive-profile-01" {
			t.Fatalf("unexpected resolve context stream=%q profile=%q", streamID, archiveProfileID)
		}
		value, ok := resolved[secretName]
		if !ok {
			return "", errors.New("unexpected secret name")
		}
		return value, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if job.ArchiveConfig.FolderID != "drive-folder-id" || job.ArchiveConfig.ServiceAccountJSON != "" || job.ArchiveConfig.ClientSecret != "google-client-secret" || job.ArchiveConfig.RefreshToken != "google-refresh-token" {
		t.Fatalf("archive secrets were not resolved: %#v", job.ArchiveConfig)
	}
}

func TestResolveYouTubeRuntimeSecrets(t *testing.T) {
	job := lifecycle.StreamJob{
		StreamID:            "stream-01",
		StreamKeySecretName: "youtube_stream_key_main",
	}
	err := resolveYouTubeRuntimeSecrets(context.Background(), &job, func(ctx context.Context, streamID, archiveProfileID, secretName string) (string, error) {
		if streamID != "stream-01" || archiveProfileID != "" || secretName != "youtube_stream_key_main" {
			t.Fatalf("unexpected resolve context stream=%q profile=%q secret=%q", streamID, archiveProfileID, secretName)
		}
		return "runtime-secret-stream-key", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if job.StreamKey != "runtime-secret-stream-key" {
		t.Fatalf("youtube stream key was not resolved: %#v", job)
	}
}

func TestResolveYouTubeRuntimeSecretsRejectsRawAndSecretNameTogether(t *testing.T) {
	job := lifecycle.StreamJob{
		StreamID:            "stream-01",
		StreamKey:           "raw-runtime-key",
		StreamKeySecretName: "youtube_stream_key_main",
	}
	err := resolveYouTubeRuntimeSecrets(context.Background(), &job, func(ctx context.Context, streamID, archiveProfileID, secretName string) (string, error) {
		t.Fatal("resolver should not be called when raw stream key is already present")
		return "", nil
	})
	if !errors.Is(err, errRawYouTubeSecretFieldNotAllowed) {
		t.Fatalf("expected raw youtube secret error, got %v", err)
	}
}

func TestResolvePackageArchiveRuntimeSecrets(t *testing.T) {
	job := lifecycle.PackageJob{
		StreamID: "stream-01",
		ArchiveConfig: lifecycle.ArchiveConfig{
			ArchiveProfileID:       "archive-profile-01",
			AuthMode:               "oauth2",
			FolderIDSecretName:     "drive_destination:dest-01:folder_id",
			ClientSecretSecretName: "oauth_provider:provider-01:client_secret",
			RefreshTokenSecretName: "oauth_account:account-01:refresh_token",
		},
	}
	resolved := map[string]string{
		"drive_destination:dest-01:folder_id":      "drive-folder-id",
		"oauth_provider:provider-01:client_secret": "google-client-secret",
		"oauth_account:account-01:refresh_token":   "google-refresh-token",
	}
	err := resolvePackageArchiveRuntimeSecrets(context.Background(), &job, func(ctx context.Context, streamID, archiveProfileID, secretName string) (string, error) {
		if streamID != "stream-01" || archiveProfileID != "archive-profile-01" {
			t.Fatalf("unexpected resolve context stream=%q profile=%q", streamID, archiveProfileID)
		}
		value, ok := resolved[secretName]
		if !ok {
			return "", errors.New("unexpected secret name")
		}
		return value, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if job.ArchiveConfig.FolderID != "drive-folder-id" || job.ArchiveConfig.ServiceAccountJSON != "" || job.ArchiveConfig.ClientSecret != "google-client-secret" || job.ArchiveConfig.RefreshToken != "google-refresh-token" {
		t.Fatalf("package archive secrets were not resolved: %#v", job.ArchiveConfig)
	}
}

func TestStartStreamReturnsConflictWhenRuntimeSecretLeaseActive(t *testing.T) {
	t.Setenv("SERVICE_CONTROL_TOKEN", "service-token")
	root := t.TempDir()
	processManager := &streamproc.Manager{ArchiveRoot: root, FFmpegBin: "ffmpeg", Starter: &httpFakeStarter{}, InputResolver: testInputResolver, AllowHostnameInputs: true}
	handler := NewServerWithManagersAndSecretResolver("encoder_recorder", processManager, workerevents.NewManager(root), TokenVerifier{PlainToken: "service-token"}, func(ctx context.Context, streamID, archiveProfileID, secretName string) (string, error) {
		return "", control.ErrRuntimeSecretLeaseActive
	})

	body := `{"stream_id":"stream-01","name":"Morning Stream","input_url":"srt://input.example.com:9000","rtmp_url":"rtmps://youtube.example.com/live2","stream_key":"runtime-stream-key","archive_config":{"archive_profile_id":"archive-profile-01","auth_mode":"oauth2","folder_id_secret_name":"drive_destination:dest-01:folder_id"}}`
	req := httptest.NewRequest(http.MethodPost, "/streams/start", bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer service-token")
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d body = %s", res.Code, res.Body.String())
	}
	if !strings.Contains(res.Body.String(), `"code":"runtime_secret_lease_active"`) {
		t.Fatalf("expected runtime secret lease code, got %s", res.Body.String())
	}
	if strings.Contains(res.Body.String(), "drive_destination") || strings.Contains(res.Body.String(), "folder_id") {
		t.Fatalf("runtime secret resolve response leaked context details: %s", res.Body.String())
	}
}

func TestPackageStreamReturnsConflictWhenRuntimeSecretLeaseActive(t *testing.T) {
	t.Setenv("SERVICE_CONTROL_TOKEN", "service-token")
	root := t.TempDir()
	handler := NewServerWithManagersAndSecretResolver("encoder_recorder", nil, workerevents.NewManager(root), TokenVerifier{PlainToken: "service-token"}, func(ctx context.Context, streamID, archiveProfileID, secretName string) (string, error) {
		return "", control.ErrRuntimeSecretLeaseActive
	})

	body := `{"stream_id":"stream-01","name":"Morning Stream","dry_run":true,"archive_config":{"archive_profile_id":"archive-profile-01","auth_mode":"oauth2","refresh_token_secret_name":"oauth_account:account-01:refresh_token"}}`
	req := httptest.NewRequest(http.MethodPost, "/streams/package", bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer service-token")
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
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

func TestStartStreamRejectsRawArchiveSecretFields(t *testing.T) {
	t.Setenv("SERVICE_CONTROL_TOKEN", "service-token")
	root := t.TempDir()
	processManager := &streamproc.Manager{ArchiveRoot: root, FFmpegBin: "ffmpeg", Starter: &httpFakeStarter{}, InputResolver: testInputResolver, AllowHostnameInputs: true}
	handler := NewServerWithManagersAndSecretResolver("encoder_recorder", processManager, workerevents.NewManager(root), TokenVerifier{PlainToken: "service-token"}, func(ctx context.Context, streamID, archiveProfileID, secretName string) (string, error) {
		t.Fatalf("runtime secret resolver should not be called for raw archive secret fields")
		return "", nil
	})

	body := `{"stream_id":"stream-01","name":"Morning Stream","input_url":"srt://input.example.com:9000","rtmp_url":"rtmps://youtube.example.com/live2","stream_key":"runtime-stream-key","archive_config":{"archive_profile_id":"archive-profile-01","auth_mode":"oauth2","folder_id":"raw-drive-folder-id","refresh_token_secret_name":"oauth_account:account-01:refresh_token"}}`
	req := httptest.NewRequest(http.MethodPost, "/streams/start", bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer service-token")
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d body = %s", res.Code, res.Body.String())
	}
	if !strings.Contains(res.Body.String(), `"code":"raw_archive_secret_fields_not_allowed"`) {
		t.Fatalf("expected raw archive secret error, got %s", res.Body.String())
	}
	if strings.Contains(res.Body.String(), "raw-drive-folder-id") || strings.Contains(res.Body.String(), "folder_id") || strings.Contains(res.Body.String(), "refresh_token") {
		t.Fatalf("raw archive secret rejection leaked field context: %s", res.Body.String())
	}
}

func TestStartStreamRejectsRawArchiveSecretFieldsWithoutResolver(t *testing.T) {
	t.Setenv("SERVICE_CONTROL_TOKEN", "service-token")
	root := t.TempDir()
	processManager := &streamproc.Manager{ArchiveRoot: root, FFmpegBin: "ffmpeg", Starter: &httpFakeStarter{}, InputResolver: testInputResolver, AllowHostnameInputs: true}
	handler := NewServerWithManagers("encoder_recorder", processManager, workerevents.NewManager(root), TokenVerifier{PlainToken: "service-token"})

	body := `{"stream_id":"stream-01","name":"Morning Stream","input_url":"srt://input.example.com:9000","rtmp_url":"rtmps://youtube.example.com/live2","stream_key":"runtime-stream-key","archive_config":{"archive_profile_id":"archive-profile-01","auth_mode":"oauth2","refresh_token":"raw-refresh-token","folder_id_secret_name":"drive_destination:dest-01:folder_id"}}`
	req := httptest.NewRequest(http.MethodPost, "/streams/start", bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer service-token")
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d body = %s", res.Code, res.Body.String())
	}
	if !strings.Contains(res.Body.String(), `"code":"raw_archive_secret_fields_not_allowed"`) {
		t.Fatalf("expected raw archive secret error, got %s", res.Body.String())
	}
	if strings.Contains(res.Body.String(), "raw-refresh-token") || strings.Contains(res.Body.String(), "refresh_token") || strings.Contains(res.Body.String(), "folder_id") {
		t.Fatalf("raw archive secret rejection leaked field context: %s", res.Body.String())
	}
}

func TestPackageStreamRejectsRawArchiveSecretFields(t *testing.T) {
	t.Setenv("SERVICE_CONTROL_TOKEN", "service-token")
	root := t.TempDir()
	handler := NewServerWithManagersAndSecretResolver("encoder_recorder", nil, workerevents.NewManager(root), TokenVerifier{PlainToken: "service-token"}, func(ctx context.Context, streamID, archiveProfileID, secretName string) (string, error) {
		t.Fatalf("runtime secret resolver should not be called for raw archive secret fields")
		return "", nil
	})

	body := `{"stream_id":"stream-01","name":"Morning Stream","dry_run":true,"archive_config":{"archive_profile_id":"archive-profile-01","auth_mode":"oauth2","refresh_token":"raw-refresh-token","folder_id_secret_name":"drive_destination:dest-01:folder_id"}}`
	req := httptest.NewRequest(http.MethodPost, "/streams/package", bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer service-token")
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d body = %s", res.Code, res.Body.String())
	}
	if !strings.Contains(res.Body.String(), `"code":"raw_archive_secret_fields_not_allowed"`) {
		t.Fatalf("expected raw archive secret error, got %s", res.Body.String())
	}
	if strings.Contains(res.Body.String(), "raw-refresh-token") || strings.Contains(res.Body.String(), "refresh_token") || strings.Contains(res.Body.String(), "folder_id") {
		t.Fatalf("raw archive secret rejection leaked field context: %s", res.Body.String())
	}
}

func TestControlEndpointsFailClosedWhenTokenIsNotConfigured(t *testing.T) {
	handler := NewServer("encoder_recorder")
	req := httptest.NewRequest(http.MethodPost, "/streams/dry-run", bytes.NewBufferString(`{}`))
	req.Header.Set("Authorization", "Bearer anything")
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 when token is not configured, got %d body = %s", res.Code, res.Body.String())
	}
}

func TestControlEndpointsRejectInvalidToken(t *testing.T) {
	handler := NewServerWithManagers("encoder_recorder", &streamproc.Manager{}, workerevents.NewManager(t.TempDir()), TokenVerifier{PlainToken: "expected-token"})
	tests := []struct {
		name   string
		method string
		path   string
		body   string
	}{
		{name: "heartbeat", method: http.MethodPost, path: "/heartbeat", body: `{}`},
		{name: "preflight", method: http.MethodGet, path: "/preflight"},
		{name: "dry run", method: http.MethodPost, path: "/streams/dry-run", body: `{}`},
		{name: "start", method: http.MethodPost, path: "/streams/start", body: `{}`},
		{name: "stop", method: http.MethodPost, path: "/streams/stream-01/stop", body: `{}`},
		{name: "status", method: http.MethodGet, path: "/streams/stream-01/process-status"},
		{name: "audio status", method: http.MethodGet, path: "/streams/stream-01/audio-status"},
		{name: "package", method: http.MethodPost, path: "/streams/package", body: `{}`},
		{name: "audio opus", method: http.MethodPost, path: "/streams/stream-01/audio/opus", body: `{}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, bytes.NewBufferString(tc.body))
			req.Header.Set("Authorization", "Bearer wrong-token")
			res := httptest.NewRecorder()
			handler.ServeHTTP(res, req)
			if res.Code != http.StatusUnauthorized {
				t.Fatalf("expected 401, got %d body = %s", res.Code, res.Body.String())
			}
		})
	}
}

func TestWorkerEventsRejectsOversizedBodyBeforeTokenValidation(t *testing.T) {
	handler := NewServerWithManagers("encoder_recorder", nil, workerevents.NewManager(t.TempDir()), TokenVerifier{WorkerEventsPlainToken: "worker-token"})
	body := `{"stream_id":"stream-01","type":"caption.telop","payload":{"text":"` + strings.Repeat("x", maxWorkerEventBodyBytes) + `"}}`
	req := httptest.NewRequest(http.MethodPost, "/worker-events", bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer wrong-token")
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected oversized worker event to be rejected with 413, got %d body = %s", res.Code, res.Body.String())
	}
}

func TestDiscordOpusAudioRejectsOversizedBodyBeforeTokenValidation(t *testing.T) {
	handler := NewServerWithManagers("encoder_recorder", nil, workerevents.NewManager(t.TempDir()), TokenVerifier{DiscordAudioPlainToken: "audio-token"})
	body := `{"stream_id":"stream-01","source":"discord-bot-01","packets":[{"ssrc":1,"sequence":1,"timestamp":1,"opus_base64":"` + strings.Repeat("A", defaultDiscordAudioBodyBytes) + `"}]}`
	req := httptest.NewRequest(http.MethodPost, "/streams/stream-01/audio/opus", bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer wrong-token")
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected oversized audio ingest to be rejected with 413, got %d body = %s", res.Code, res.Body.String())
	}
}

func TestStreamControlEndpointsRejectOversizedJSONBodies(t *testing.T) {
	t.Setenv("SERVICE_CONTROL_TOKEN", "service-token")
	handler := NewServerWithManagers("encoder_recorder", &streamproc.Manager{}, workerevents.NewManager(t.TempDir()), TokenVerifier{PlainToken: "service-token"})
	tests := []string{"/streams/dry-run", "/streams/start", "/streams/package"}
	for _, path := range tests {
		t.Run(path, func(t *testing.T) {
			body := `{}` + strings.Repeat(" ", maxControlBodyBytes)
			req := httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString(body))
			req.Header.Set("Authorization", "Bearer service-token")
			res := httptest.NewRecorder()
			handler.ServeHTTP(res, req)
			if res.Code != http.StatusRequestEntityTooLarge {
				t.Fatalf("expected oversized request to be rejected with 413, got %d body = %s", res.Code, res.Body.String())
			}
			if !strings.Contains(res.Body.String(), `"code":"request_body_too_large"`) {
				t.Fatalf("unexpected oversized request response: %s", res.Body.String())
			}
		})
	}
}

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
	t.Setenv("YOUTUBE_RTMP_URL", "rtmps://youtube.example.com/live2")
	t.Setenv("YOUTUBE_STREAM_KEY", "super-secret-stream-key")
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

func TestPreflightEndpointTreatsYouTubeEnvAsOptionalRuntimeConfig(t *testing.T) {
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
	if !hasPreflightCheck(body.Checks, "output_relay", "compatibility_mode") {
		t.Fatalf("missing output relay compatibility check: %#v", body.Checks)
	}
}

func TestPreflightEndpointRequiresOutputRelayInProduction(t *testing.T) {
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
	if body.Ready {
		t.Fatalf("expected production preflight to fail without output relay: %#v", body.Checks)
	}
	if !hasPreflightCheck(body.Checks, "output_relay", "missing") {
		t.Fatalf("missing output relay production failure: %#v", body.Checks)
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

func TestDiscordOpusAudioEndpointRejectsOfflineStream(t *testing.T) {
	root := t.TempDir()
	eventManager := workerevents.NewManager(root)
	sum := sha256.Sum256([]byte("audio-token"))
	handler := NewServerWithManagers("encoder_recorder", nil, eventManager, TokenVerifier{DiscordAudioSHA256Hex: hex.EncodeToString(sum[:])})

	body := `{"stream_id":"stream-01","source":"discord-bot-01","packets":[{"ssrc":1234,"user_id":"user-01","sequence":1,"timestamp":960,"received_at":"2026-05-28T00:00:00Z","opus_base64":"AQID"}]}`
	req := httptest.NewRequest(http.MethodPost, "/streams/stream-01/audio/opus", bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer audio-token")
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusConflict {
		t.Fatalf("expected offline audio ingest to be rejected, got %d body = %s", res.Code, res.Body.String())
	}
	if _, err := os.Stat(filepath.Join(root, "tmp", "stream-01", "discord-opus.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("offline audio ingest must not write sidecar, stat err=%v", err)
	}
}

func TestDryRunEndpoint(t *testing.T) {
	t.Setenv("SERVICE_CONTROL_TOKEN", "service-token")
	root := t.TempDir()
	t.Setenv("AUTOSTREAM_ARCHIVE_DIR", root)
	t.Setenv("YOUTUBE_STREAM_KEY", "super-secret-stream-key")
	t.Setenv("YOUTUBE_RTMP_URL", "rtmps://youtube.example.com/live2")
	handler := NewServer("encoder_recorder")
	body := `{"stream_id":"stream-01","name":"Morning Stream","input_url":"srt://input.example.com:9000","dry_run":true}`
	req := httptest.NewRequest(http.MethodPost, "/streams/dry-run", bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer service-token")
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusAccepted {
		t.Fatalf("status = %d body = %s", res.Code, res.Body.String())
	}
	if strings.Contains(res.Body.String(), "super-secret-stream-key") || strings.Contains(res.Body.String(), root) || strings.Contains(res.Body.String(), `\tmp\`) || strings.Contains(res.Body.String(), `/tmp/`) {
		t.Fatalf("secret or local archive path leaked in dry-run response: %s", res.Body.String())
	}
	var response struct {
		StreamID string `json:"stream_id"`
		Archive  struct {
			FinalMP4 string `json:"final_mp4"`
		} `json:"archive"`
	}
	if err := json.NewDecoder(res.Body).Decode(&response); err != nil {
		t.Fatal(err)
	}
	if response.StreamID != "stream-01" || response.Archive.FinalMP4 == "" {
		t.Fatalf("unexpected response: %#v", response)
	}
	if response.Archive.FinalMP4 != "final.mp4" {
		t.Fatalf("dry-run response should expose logical artifact name, got %#v", response.Archive)
	}
}

func TestDryRunEndpointRequiresRuntimeYouTubeConfigWhenFallbackDisabled(t *testing.T) {
	t.Setenv("SERVICE_CONTROL_TOKEN", "service-token")
	t.Setenv("AUTOSTREAM_REQUIRE_CONTROL_PANEL_RUNTIME_CONFIG", "true")
	t.Setenv("YOUTUBE_STREAM_KEY", "env-secret-stream-key")
	t.Setenv("YOUTUBE_RTMP_URL", "rtmps://youtube.example.com/live2")
	t.Setenv("AUTOSTREAM_ARCHIVE_DIR", t.TempDir())

	handler := NewServer("encoder_recorder")
	body := `{"stream_id":"stream-01","name":"Morning Stream","input_url":"srt://input.example.com:9000","dry_run":true}`
	req := httptest.NewRequest(http.MethodPost, "/streams/dry-run", bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer service-token")
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusBadRequest {
		t.Fatalf("expected runtime config failure, got %d body = %s", res.Code, res.Body.String())
	}
	if !strings.Contains(res.Body.String(), `"code":"youtube_runtime_config_required"`) ||
		!strings.Contains(res.Body.String(), `"rtmp_url"`) ||
		!strings.Contains(res.Body.String(), `"stream_key"`) {
		t.Fatalf("expected missing YouTube runtime fields, got %s", res.Body.String())
	}
	if strings.Contains(res.Body.String(), "env-secret-stream-key") {
		t.Fatalf("env stream key leaked in runtime config failure: %s", res.Body.String())
	}
}

func TestDryRunEndpointRejectsRawStreamKeyInProduction(t *testing.T) {
	t.Setenv("SERVICE_CONTROL_TOKEN", "service-token")
	t.Setenv("AUTOSTREAM_ENV", "production")
	t.Setenv("AUTOSTREAM_ARCHIVE_DIR", t.TempDir())

	handler := NewServer("encoder_recorder")
	body := `{"stream_id":"stream-01","name":"Morning Stream","input_url":"srt://input.example.com:9000","rtmp_url":"rtmps://youtube.example.com/live2","stream_key":"raw-production-stream-key","dry_run":true}`
	req := httptest.NewRequest(http.MethodPost, "/streams/dry-run", bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer service-token")
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusBadRequest || !strings.Contains(res.Body.String(), "raw_youtube_stream_key_not_allowed") {
		t.Fatalf("expected raw stream key rejection, status = %d body = %s", res.Code, res.Body.String())
	}
	if strings.Contains(res.Body.String(), "raw-production-stream-key") {
		t.Fatalf("raw stream key leaked in rejection response: %s", res.Body.String())
	}
}

func TestPackageEndpoint(t *testing.T) {
	t.Setenv("SERVICE_CONTROL_TOKEN", "service-token")
	root := t.TempDir()
	t.Setenv("AUTOSTREAM_ARCHIVE_DIR", root)
	tmpDir := filepath.Join(root, "tmp", "stream-01")
	finalDir := filepath.Join(root, "final", "stream-01")
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
	body := `{"stream_id":"stream-01","name":"Morning Stream","dry_run":true}`
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
						"base_path":                 "AutoStream/Archives",
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

	body := `{"stream_id":"stream-01","name":"Morning Stream","dry_run":true}`
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
	body := `{"stream_id":"stream-01","name":"Morning Stream","dry_run":true}`
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

func TestStartStopProcessEndpoints(t *testing.T) {
	t.Setenv("SERVICE_CONTROL_TOKEN", "service-token")
	root := t.TempDir()
	t.Setenv("YOUTUBE_STREAM_KEY", "super-secret-stream-key")
	t.Setenv("YOUTUBE_RTMP_URL", "rtmps://youtube.example.com/live2")
	processManager := &streamproc.Manager{ArchiveRoot: root, FFmpegBin: "ffmpeg", Starter: &httpFakeStarter{}, InputResolver: testInputResolver, AllowHostnameInputs: true}
	handler := NewServerWithProcessManager("encoder_recorder", processManager)

	body := `{"stream_id":"stream-01","name":"Morning Stream","input_url":"srt://input.example.com:9000"}`
	req := httptest.NewRequest(http.MethodPost, "/streams/start", bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer service-token")
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusAccepted {
		t.Fatalf("start status = %d body = %s", res.Code, res.Body.String())
	}
	if strings.Contains(res.Body.String(), "super-secret-stream-key") {
		t.Fatal("stream key leaked in start response")
	}
	if strings.Contains(res.Body.String(), `"pid"`) {
		t.Fatalf("process pid leaked in start response: %s", res.Body.String())
	}

	statusReq := httptest.NewRequest(http.MethodGet, "/streams/stream-01/process-status", nil)
	statusReq.Header.Set("Authorization", "Bearer service-token")
	statusRes := httptest.NewRecorder()
	handler.ServeHTTP(statusRes, statusReq)
	if statusRes.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", statusRes.Code, statusRes.Body.String())
	}
	if strings.Contains(statusRes.Body.String(), `"pid"`) {
		t.Fatalf("process pid leaked in status response: %s", statusRes.Body.String())
	}

	stopReq := httptest.NewRequest(http.MethodPost, "/streams/stream-01/stop", nil)
	stopReq.Header.Set("Authorization", "Bearer service-token")
	stopRes := httptest.NewRecorder()
	handler.ServeHTTP(stopRes, stopReq)
	if stopRes.Code != http.StatusAccepted {
		t.Fatalf("stop status = %d body = %s", stopRes.Code, stopRes.Body.String())
	}
	if strings.Contains(stopRes.Body.String(), `"pid"`) {
		t.Fatalf("process pid leaked in stop response: %s", stopRes.Body.String())
	}
}

func TestStartEndpointAcceptsRuntimeStreamKeyWithoutEnvLeak(t *testing.T) {
	t.Setenv("SERVICE_CONTROL_TOKEN", "service-token")
	root := t.TempDir()
	starter := &httpFakeStarter{}
	processManager := &streamproc.Manager{ArchiveRoot: root, FFmpegBin: "ffmpeg", Starter: starter, InputResolver: testInputResolver, AllowHostnameInputs: true, OutputRelayURL: "rtmp://127.0.0.1/autostream/{stream_id}"}
	handler := NewServerWithProcessManager("encoder_recorder", processManager)

	body := `{"stream_id":"stream-01","name":"Morning Stream","input_url":"srt://input.example.com:9000","rtmp_url":"rtmps://youtube.example.com/live2","stream_key":"runtime-secret-stream-key"}`
	req := httptest.NewRequest(http.MethodPost, "/streams/start", bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer service-token")
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusAccepted {
		t.Fatalf("start status = %d body = %s", res.Code, res.Body.String())
	}
	if strings.Contains(res.Body.String(), "runtime-secret-stream-key") {
		t.Fatal("runtime stream key leaked in start response")
	}
	args := strings.Join(starter.args, " ")
	if strings.Contains(args, "runtime-secret-stream-key") || strings.Contains(args, "rtmps://youtube.example.com/live2") {
		t.Fatalf("runtime stream key or upstream RTMPS URL leaked in ffmpeg args: %#v", starter.args)
	}
	if !strings.Contains(args, "rtmp://127.0.0.1/autostream/stream-01") {
		t.Fatalf("expected local relay target in ffmpeg args, got %#v", starter.args)
	}
}

func TestStartEndpointRejectsRawStreamKeyInProduction(t *testing.T) {
	t.Setenv("SERVICE_CONTROL_TOKEN", "service-token")
	t.Setenv("AUTOSTREAM_ENV", "production")
	root := t.TempDir()
	starter := &httpFakeStarter{}
	processManager := &streamproc.Manager{ArchiveRoot: root, FFmpegBin: "ffmpeg", Starter: starter, InputResolver: testInputResolver, AllowHostnameInputs: true, OutputRelayURL: "rtmp://127.0.0.1/autostream/{stream_id}"}
	handler := NewServerWithProcessManager("encoder_recorder", processManager)

	body := `{"stream_id":"stream-01","name":"Morning Stream","input_url":"srt://input.example.com:9000","rtmp_url":"rtmps://youtube.example.com/live2","stream_key":"raw-production-stream-key"}`
	req := httptest.NewRequest(http.MethodPost, "/streams/start", bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer service-token")
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusBadRequest || !strings.Contains(res.Body.String(), "raw_youtube_stream_key_not_allowed") {
		t.Fatalf("expected raw stream key rejection, status = %d body = %s", res.Code, res.Body.String())
	}
	if strings.Contains(res.Body.String(), "raw-production-stream-key") {
		t.Fatalf("raw stream key leaked in rejection response: %s", res.Body.String())
	}
	if starter.process != nil || len(starter.args) > 0 {
		t.Fatalf("ffmpeg must not start after raw production stream key rejection: %#v", starter.args)
	}
}

func TestStartEndpointResolvesRuntimeStreamKeySecretName(t *testing.T) {
	t.Setenv("SERVICE_CONTROL_TOKEN", "service-token")
	root := t.TempDir()
	starter := &httpFakeStarter{}
	processManager := &streamproc.Manager{ArchiveRoot: root, FFmpegBin: "ffmpeg", Starter: starter, InputResolver: testInputResolver, AllowHostnameInputs: true, OutputRelayURL: "rtmp://127.0.0.1/autostream/{stream_id}"}
	handler := NewServerWithManagersAndSecretResolver("encoder_recorder", processManager, workerevents.NewManager(root), TokenVerifier{PlainToken: "service-token"}, func(ctx context.Context, streamID, archiveProfileID, secretName string) (string, error) {
		if streamID != "stream-01" || archiveProfileID != "" || secretName != "youtube_stream_key_main" {
			t.Fatalf("unexpected resolve context stream=%q profile=%q secret=%q", streamID, archiveProfileID, secretName)
		}
		return "resolved-runtime-stream-key", nil
	})

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
	args := strings.Join(starter.args, " ")
	if strings.Contains(args, "resolved-runtime-stream-key") || strings.Contains(args, "rtmps://youtube.example.com/live2") {
		t.Fatalf("resolved runtime stream key or upstream RTMPS URL leaked in ffmpeg args: %#v", starter.args)
	}
	if !strings.Contains(args, "rtmp://127.0.0.1/autostream/stream-01") {
		t.Fatalf("expected local relay target in ffmpeg args, got %#v", starter.args)
	}
}

func TestStartEndpointAppliesControlPanelYouTubeRuntimeConfig(t *testing.T) {
	t.Setenv("SERVICE_CONTROL_TOKEN", "service-token")
	t.Setenv("AUTOSTREAM_REQUIRE_CONTROL_PANEL_RUNTIME_CONFIG", "true")
	t.Setenv("YOUTUBE_STREAM_KEY", "env-secret-stream-key")
	t.Setenv("YOUTUBE_RTMP_URL", "rtmps://env.example.com/live2")
	root := t.TempDir()
	starter := &httpFakeStarter{}
	processManager := &streamproc.Manager{ArchiveRoot: root, FFmpegBin: "ffmpeg", Starter: starter, InputResolver: testInputResolver, AllowHostnameInputs: true, OutputRelayURL: "rtmp://127.0.0.1/autostream/{stream_id}"}
	handler := NewServerWithManagersAndRuntimeConfig(
		"encoder_recorder",
		processManager,
		workerevents.NewManager(root),
		TokenVerifier{PlainToken: "service-token"},
		func(ctx context.Context, streamID, archiveProfileID, secretName string) (string, error) {
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
	args := strings.Join(starter.args, " ")
	if strings.Contains(args, "rtmps://control.example.com/live2") || strings.Contains(args, "resolved-control-panel-stream-key") {
		t.Fatalf("control panel youtube runtime secret material leaked in ffmpeg args: %#v", starter.args)
	}
	if !strings.Contains(args, "rtmp://127.0.0.1/autostream/stream-01") {
		t.Fatalf("expected local relay target in ffmpeg args, got %#v", starter.args)
	}
	if strings.Contains(args, "env-secret-stream-key") || strings.Contains(args, "rtmps://env.example.com/live2") {
		t.Fatalf("env youtube fallback should not be used when control panel runtime config is available: %#v", starter.args)
	}
}

func TestDryRunEndpointAppliesControlPanelYouTubeRuntimeConfigWithoutResolvingSecret(t *testing.T) {
	t.Setenv("SERVICE_CONTROL_TOKEN", "service-token")
	t.Setenv("AUTOSTREAM_REQUIRE_CONTROL_PANEL_RUNTIME_CONFIG", "true")
	t.Setenv("AUTOSTREAM_ARCHIVE_DIR", t.TempDir())
	handler := NewServerWithManagersAndRuntimeConfig(
		"encoder_recorder",
		nil,
		workerevents.NewManager(t.TempDir()),
		TokenVerifier{PlainToken: "service-token"},
		nil,
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
						"stream_key_secret_name": "youtube_stream_key_runtime_stream-01",
					},
				}},
			}, nil
		},
	)

	body := `{"stream_id":"stream-01","name":"Morning Stream","input_url":"srt://input.example.com:9000"}`
	req := httptest.NewRequest(http.MethodPost, "/streams/dry-run", bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer service-token")
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusAccepted {
		t.Fatalf("dry-run status = %d body = %s", res.Code, res.Body.String())
	}
	if strings.Contains(res.Body.String(), `"stream_key":`) || strings.Contains(res.Body.String(), "youtube_stream_key_runtime_stream-01") {
		t.Fatalf("dry-run response leaked raw or secret-name youtube key material: %s", res.Body.String())
	}
}

func TestStartEndpointAppliesControlPanelArchiveRuntimeConfig(t *testing.T) {
	t.Setenv("SERVICE_CONTROL_TOKEN", "service-token")
	root := t.TempDir()
	starter := &httpFakeStarter{}
	processManager := &streamproc.Manager{ArchiveRoot: root, FFmpegBin: "ffmpeg", Starter: starter, InputResolver: testInputResolver, AllowHostnameInputs: true, OutputRelayURL: "rtmp://127.0.0.1/autostream/{stream_id}"}
	resolvedSecrets := map[string]string{}
	handler := NewServerWithManagersAndRuntimeConfig(
		"encoder_recorder",
		processManager,
		workerevents.NewManager(root),
		TokenVerifier{PlainToken: "service-token"},
		func(ctx context.Context, streamID, archiveProfileID, secretName string) (string, error) {
			if streamID != "stream-01" || archiveProfileID != "archive-profile-01" {
				t.Fatalf("unexpected archive resolve context stream=%q profile=%q secret=%q", streamID, archiveProfileID, secretName)
			}
			resolvedSecrets[secretName] = archiveProfileID
			switch secretName {
			case "drive_destination:dest-01:folder_id":
				return "raw-drive-folder-id", nil
			case "oauth_provider:provider-01:client_secret":
				return "raw-google-client-secret", nil
			case "oauth_account:account-01:refresh_token":
				return "raw-google-refresh-token", nil
			default:
				return "", errors.New("unexpected archive runtime secret")
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
						"base_path":                 "AutoStream/Archives",
						"shared_drive":              true,
						"client_id":                 "google-client-id",
						"client_secret_secret_name": "oauth_provider:provider-01:client_secret",
						"refresh_token_secret_name": "oauth_account:account-01:refresh_token",
					},
				}},
			}, nil
		},
	)

	body := `{"stream_id":"stream-01","name":"Morning Stream","input_url":"srt://input.example.com:9000","rtmp_url":"rtmps://youtube.example.com/live2","stream_key":"runtime-stream-key"}`
	req := httptest.NewRequest(http.MethodPost, "/streams/start", bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer service-token")
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusAccepted {
		t.Fatalf("start status = %d body = %s", res.Code, res.Body.String())
	}
	for _, secretName := range []string{
		"drive_destination:dest-01:folder_id",
		"oauth_provider:provider-01:client_secret",
		"oauth_account:account-01:refresh_token",
	} {
		if resolvedSecrets[secretName] != "archive-profile-01" {
			t.Fatalf("expected runtime archive secret %q to be resolved, got %#v", secretName, resolvedSecrets)
		}
	}
	for _, leaked := range []string{"raw-drive-folder-id", "raw-google-client-secret", "raw-google-refresh-token", "drive_destination:dest-01:folder_id", "oauth_account:account-01:refresh_token"} {
		if strings.Contains(res.Body.String(), leaked) {
			t.Fatalf("start response leaked archive runtime config detail %q: %s", leaked, res.Body.String())
		}
	}
}

func TestStartEndpointRequiresRuntimeYouTubeConfigWhenFallbackDisabled(t *testing.T) {
	t.Setenv("SERVICE_CONTROL_TOKEN", "service-token")
	t.Setenv("AUTOSTREAM_REQUIRE_CONTROL_PANEL_RUNTIME_CONFIG", "true")
	t.Setenv("YOUTUBE_STREAM_KEY", "env-secret-stream-key")
	t.Setenv("YOUTUBE_RTMP_URL", "rtmps://youtube.example.com/live2")
	root := t.TempDir()
	starter := &httpFakeStarter{}
	processManager := &streamproc.Manager{ArchiveRoot: root, FFmpegBin: "ffmpeg", Starter: starter, InputResolver: testInputResolver, AllowHostnameInputs: true}
	handler := NewServerWithProcessManager("encoder_recorder", processManager)

	body := `{"stream_id":"stream-01","name":"Morning Stream","input_url":"srt://input.example.com:9000"}`
	req := httptest.NewRequest(http.MethodPost, "/streams/start", bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer service-token")
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusBadRequest {
		t.Fatalf("expected runtime config failure, got %d body = %s", res.Code, res.Body.String())
	}
	if !strings.Contains(res.Body.String(), `"code":"youtube_runtime_config_required"`) ||
		!strings.Contains(res.Body.String(), `"rtmp_url"`) ||
		!strings.Contains(res.Body.String(), `"stream_key"`) {
		t.Fatalf("expected missing YouTube runtime fields, got %s", res.Body.String())
	}
	if strings.Contains(res.Body.String(), "env-secret-stream-key") {
		t.Fatalf("env stream key leaked in runtime config failure: %s", res.Body.String())
	}
	if len(starter.args) != 0 {
		t.Fatalf("ffmpeg must not start with env fallback YouTube config when runtime config is required: %#v", starter.args)
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
	}
	handler := NewServerWithProcessManager("encoder_recorder", processManager)

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

func TestStartEndpointRejectsUnsafeRTMPTarget(t *testing.T) {
	t.Setenv("SERVICE_CONTROL_TOKEN", "service-token")
	t.Setenv("YOUTUBE_STREAM_KEY", "super-secret-stream-key")
	root := t.TempDir()
	processManager := &streamproc.Manager{ArchiveRoot: root, FFmpegBin: "ffmpeg", Starter: &httpFakeStarter{}, InputResolver: testInputResolver, AllowHostnameInputs: true}
	handler := NewServerWithProcessManager("encoder_recorder", processManager)
	body := `{"stream_id":"stream-01","name":"Morning Stream","input_url":"srt://input.example.com:9000","rtmp_url":"rtmps://youtube.example.com/live2|[f=matroska]/tmp/evil.mkv"}`
	req := httptest.NewRequest(http.MethodPost, "/streams/start", bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer service-token")
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusBadRequest {
		t.Fatalf("expected unsafe RTMP target to be rejected, got %d body = %s", res.Code, res.Body.String())
	}
	if processManager == nil {
		t.Fatal("unreachable")
	}
}

func TestStartEndpointUsesDiscordAudioBridgeWhenInputURLIsEmpty(t *testing.T) {
	t.Setenv("SERVICE_CONTROL_TOKEN", "service-token")
	root := t.TempDir()
	t.Setenv("YOUTUBE_STREAM_KEY", "super-secret-stream-key")
	t.Setenv("YOUTUBE_RTMP_URL", "rtmps://youtube.example.com/live2")
	starter := &httpFakeStarter{}
	processManager := &streamproc.Manager{ArchiveRoot: root, FFmpegBin: "ffmpeg", Starter: starter, InputResolver: testInputResolver, AllowHostnameInputs: true}
	handler := NewServerWithProcessManager("encoder_recorder", processManager)

	body := `{"stream_id":"stream-01","name":"Discord Audio Stream"}`
	req := httptest.NewRequest(http.MethodPost, "/streams/start", bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer service-token")
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusAccepted {
		t.Fatalf("start status = %d body = %s", res.Code, res.Body.String())
	}
	joinedArgs := strings.Join(starter.args, " ")
	if !strings.Contains(joinedArgs, "discord-opus.sdp") || !strings.Contains(joinedArgs, "color=c=black") {
		t.Fatalf("expected discord audio bridge ffmpeg args, got %#v", starter.args)
	}
	if _, err := os.Stat(filepath.Join(root, "tmp", "stream-01", "discord-opus.sdp")); err != nil {
		t.Fatal(err)
	}
}

func TestStartEndpointRejectsClientSuppliedInternalDiscordAudioURL(t *testing.T) {
	t.Setenv("SERVICE_CONTROL_TOKEN", "service-token")
	root := t.TempDir()
	t.Setenv("YOUTUBE_STREAM_KEY", "super-secret-stream-key")
	t.Setenv("YOUTUBE_RTMP_URL", "rtmps://youtube.example.com/live2")
	starter := &httpFakeStarter{}
	processManager := &streamproc.Manager{ArchiveRoot: root, FFmpegBin: "ffmpeg", Starter: starter, InputResolver: testInputResolver, AllowHostnameInputs: true}
	handler := NewServerWithProcessManager("encoder_recorder", processManager)

	body := `{"stream_id":"stream-01","name":"Discord Audio Stream","input_mode":"discord_opus_rtp","input_url":"internal_discord_audio:C:/tmp/attacker.sdp"}`
	req := httptest.NewRequest(http.MethodPost, "/streams/start", bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer service-token")
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusBadRequest {
		t.Fatalf("start status = %d body = %s", res.Code, res.Body.String())
	}
	if !strings.Contains(res.Body.String(), "internal_audio_input_not_allowed") {
		t.Fatalf("unexpected response: %s", res.Body.String())
	}
	if starter.process != nil {
		t.Fatalf("ffmpeg should not be started for client-supplied internal input: %#v", starter.process)
	}
}

func TestDiscordAudioStatusShowsBridgeBeforePacketsAndUpdatesAfterIngest(t *testing.T) {
	t.Setenv("SERVICE_CONTROL_TOKEN", "service-token")
	t.Setenv("ENCODER_DISCORD_AUDIO_TOKEN", "audio-token")
	t.Setenv("AUTOSTREAM_REQUIRE_SIGNED_INGEST_TOKENS", "false")
	root := t.TempDir()
	t.Setenv("YOUTUBE_STREAM_KEY", "super-secret-stream-key")
	t.Setenv("YOUTUBE_RTMP_URL", "rtmps://youtube.example.com/live2")
	processManager := &streamproc.Manager{ArchiveRoot: root, FFmpegBin: "ffmpeg", Starter: &httpFakeStarter{}, InputResolver: testInputResolver, AllowHostnameInputs: true}
	handler := NewServerWithProcessManager("encoder_recorder", processManager)

	startReq := httptest.NewRequest(http.MethodPost, "/streams/start", bytes.NewBufferString(`{"stream_id":"stream-01","name":"Discord Audio Stream"}`))
	startReq.Header.Set("Authorization", "Bearer service-token")
	startRes := httptest.NewRecorder()
	handler.ServeHTTP(startRes, startReq)
	if startRes.Code != http.StatusAccepted {
		t.Fatalf("start status = %d body = %s", startRes.Code, startRes.Body.String())
	}

	status := getAudioStatus(t, handler)
	if !status.BridgeActive {
		t.Fatalf("expected active bridge before packets: %#v", status)
	}
	if status.PacketsTotal != 0 || status.RTPForwarded != 0 {
		t.Fatalf("expected no packets before ingest: %#v", status)
	}
	if status.LastPacketAgeSec < 0 {
		t.Fatalf("expected non-negative packet age: %#v", status)
	}

	body := `{"stream_id":"stream-01","source":"discord-bot-01","packets":[{"ssrc":1234,"user_id":"user-01","sequence":1,"timestamp":960,"opus_base64":"AQID"}]}`
	audioReq := httptest.NewRequest(http.MethodPost, "/streams/stream-01/audio/opus", bytes.NewBufferString(body))
	audioReq.Header.Set("Authorization", "Bearer audio-token")
	audioRes := httptest.NewRecorder()
	handler.ServeHTTP(audioRes, audioReq)
	if audioRes.Code != http.StatusAccepted {
		t.Fatalf("audio status = %d body = %s", audioRes.Code, audioRes.Body.String())
	}
	if strings.Contains(audioRes.Body.String(), root) || strings.Contains(audioRes.Body.String(), `\tmp\`) || strings.Contains(audioRes.Body.String(), `/tmp/`) {
		t.Fatalf("local archive path leaked in audio ingest response: %s", audioRes.Body.String())
	}

	status = getAudioStatus(t, handler)
	if status.PacketsTotal != 1 || status.RTPForwarded != 1 {
		t.Fatalf("expected packet counters after ingest: %#v", status)
	}
	if status.LastPacketAgeSec < 0 || status.LastPacketAgeSec > 1 {
		t.Fatalf("expected fresh packet age after ingest: %#v", status)
	}
}

func TestDiscordAudioEndpointAcceptsConfiguredLargePacketBatch(t *testing.T) {
	t.Setenv("SERVICE_CONTROL_TOKEN", "service-token")
	t.Setenv("ENCODER_DISCORD_AUDIO_TOKEN", "audio-token")
	t.Setenv("AUTOSTREAM_REQUIRE_SIGNED_INGEST_TOKENS", "false")
	t.Setenv("AUDIO_INGEST_MAX_PACKETS", "150")
	t.Setenv("AUDIO_INGEST_MAX_BODY_BYTES", "1048576")
	root := t.TempDir()
	t.Setenv("YOUTUBE_STREAM_KEY", "super-secret-stream-key")
	t.Setenv("YOUTUBE_RTMP_URL", "rtmps://youtube.example.com/live2")
	processManager := &streamproc.Manager{ArchiveRoot: root, FFmpegBin: "ffmpeg", Starter: &httpFakeStarter{}, InputResolver: testInputResolver, AllowHostnameInputs: true}
	handler := NewServerWithProcessManager("encoder_recorder", processManager)

	startReq := httptest.NewRequest(http.MethodPost, "/streams/start", bytes.NewBufferString(`{"stream_id":"stream-01","name":"Large Discord Audio Batch"}`))
	startReq.Header.Set("Authorization", "Bearer service-token")
	startRes := httptest.NewRecorder()
	handler.ServeHTTP(startRes, startReq)
	if startRes.Code != http.StatusAccepted {
		t.Fatalf("start status = %d body = %s", startRes.Code, startRes.Body.String())
	}

	packets := make([]map[string]any, 120)
	for i := range packets {
		packets[i] = map[string]any{
			"ssrc":        1234,
			"user_id":     "user-01",
			"sequence":    i + 1,
			"timestamp":   960 * (i + 1),
			"opus_base64": "AQID",
		}
	}
	body, err := json.Marshal(map[string]any{
		"stream_id": "stream-01",
		"source":    "discord-bot-01",
		"packets":   packets,
	})
	if err != nil {
		t.Fatal(err)
	}
	audioReq := httptest.NewRequest(http.MethodPost, "/streams/stream-01/audio/opus", bytes.NewReader(body))
	audioReq.Header.Set("Authorization", "Bearer audio-token")
	audioRes := httptest.NewRecorder()
	handler.ServeHTTP(audioRes, audioReq)
	if audioRes.Code != http.StatusAccepted {
		t.Fatalf("audio status = %d body = %s", audioRes.Code, audioRes.Body.String())
	}
	if !strings.Contains(audioRes.Body.String(), `"accepted_count":120`) {
		t.Fatalf("expected all configured packets to be accepted: %s", audioRes.Body.String())
	}
	status := getAudioStatus(t, handler)
	if status.PacketsTotal != 120 || status.RTPForwarded != 120 {
		t.Fatalf("expected configured large batch counters after ingest: %#v", status)
	}
}

func TestDiscordAudioEndpointUsesScopedTokenWhenConfigured(t *testing.T) {
	root := t.TempDir()
	t.Setenv("YOUTUBE_STREAM_KEY", "super-secret-stream-key")
	t.Setenv("YOUTUBE_RTMP_URL", "rtmps://youtube.example.com/live2")
	processManager := &streamproc.Manager{ArchiveRoot: root, FFmpegBin: "ffmpeg", Starter: &httpFakeStarter{}, InputResolver: testInputResolver, AllowHostnameInputs: true}
	handler := NewServerWithManagers("encoder_recorder", processManager, workerevents.NewManager(root), TokenVerifier{
		PlainToken:             "control-token",
		DiscordAudioPlainToken: "discord-audio-token",
	})

	startReq := httptest.NewRequest(http.MethodPost, "/streams/start", bytes.NewBufferString(`{"stream_id":"stream-01","name":"Discord Audio Stream"}`))
	startReq.Header.Set("Authorization", "Bearer control-token")
	startRes := httptest.NewRecorder()
	handler.ServeHTTP(startRes, startReq)
	if startRes.Code != http.StatusAccepted {
		t.Fatalf("start status = %d body = %s", startRes.Code, startRes.Body.String())
	}

	body := `{"stream_id":"stream-01","source":"discord-bot-01","packets":[{"ssrc":1234,"user_id":"user-01","sequence":1,"timestamp":960,"opus_base64":"AQID"}]}`
	controlReq := httptest.NewRequest(http.MethodPost, "/streams/stream-01/audio/opus", bytes.NewBufferString(body))
	controlReq.Header.Set("Authorization", "Bearer control-token")
	controlRes := httptest.NewRecorder()
	handler.ServeHTTP(controlRes, controlReq)
	if controlRes.Code != http.StatusUnauthorized {
		t.Fatalf("expected control token to be rejected for scoped audio ingest, got %d body = %s", controlRes.Code, controlRes.Body.String())
	}

	audioReq := httptest.NewRequest(http.MethodPost, "/streams/stream-01/audio/opus", bytes.NewBufferString(body))
	audioReq.Header.Set("Authorization", "Bearer discord-audio-token")
	audioRes := httptest.NewRecorder()
	handler.ServeHTTP(audioRes, audioReq)
	if audioRes.Code != http.StatusAccepted {
		t.Fatalf("expected scoped audio token to be accepted, got %d body = %s", audioRes.Code, audioRes.Body.String())
	}
	if strings.Contains(audioRes.Body.String(), root) || strings.Contains(audioRes.Body.String(), `\tmp\`) || strings.Contains(audioRes.Body.String(), `/tmp/`) {
		t.Fatalf("local archive path leaked in audio ingest response: %s", audioRes.Body.String())
	}
}

func TestDiscordAudioEndpointAcceptsSignedStreamIngestToken(t *testing.T) {
	root := t.TempDir()
	t.Setenv("YOUTUBE_STREAM_KEY", "super-secret-stream-key")
	t.Setenv("YOUTUBE_RTMP_URL", "rtmps://youtube.example.com/live2")
	processManager := &streamproc.Manager{ArchiveRoot: root, FFmpegBin: "ffmpeg", Starter: &httpFakeStarter{}, InputResolver: testInputResolver, AllowHostnameInputs: true}
	signingKey := "test-stream-ingest-signing-key"
	handler := NewServerWithManagers("encoder_recorder", processManager, workerevents.NewManager(root), TokenVerifier{
		PlainToken:             "control-token",
		IngestTokenSigningKey:  signingKey,
		DiscordAudioPlainToken: "static-audio-token",
	})

	startReq := httptest.NewRequest(http.MethodPost, "/streams/start", bytes.NewBufferString(`{"stream_id":"stream-01","name":"Discord Audio Stream"}`))
	startReq.Header.Set("Authorization", "Bearer control-token")
	startRes := httptest.NewRecorder()
	handler.ServeHTTP(startRes, startReq)
	if startRes.Code != http.StatusAccepted {
		t.Fatalf("start status = %d body = %s", startRes.Code, startRes.Body.String())
	}

	token, err := ingesttoken.Issue(signingKey, ingesttoken.Claims{
		StreamID:    "stream-01",
		ServiceID:   "discord-bot-01",
		ServiceType: "discord_bot",
		Purpose:     "discord_audio",
		Audience:    "encoder_recorder",
		ExpiresAt:   time.Now().Add(time.Hour).Unix(),
	})
	if err != nil {
		t.Fatal(err)
	}
	body := `{"stream_id":"stream-01","source":"discord-bot-01","packets":[{"ssrc":1234,"user_id":"user-01","sequence":1,"timestamp":960,"opus_base64":"AQID"}]}`
	audioReq := httptest.NewRequest(http.MethodPost, "/streams/stream-01/audio/opus", bytes.NewBufferString(body))
	audioReq.Header.Set("Authorization", "Bearer "+token)
	audioRes := httptest.NewRecorder()
	handler.ServeHTTP(audioRes, audioReq)
	if audioRes.Code != http.StatusAccepted {
		t.Fatalf("expected signed ingest token to be accepted, got %d body = %s", audioRes.Code, audioRes.Body.String())
	}

	mismatchedSourceBody := `{"stream_id":"stream-01","source":"discord-bot-other","packets":[{"ssrc":1234,"user_id":"user-01","sequence":2,"timestamp":1920,"opus_base64":"AQID"}]}`
	mismatchedSourceReq := httptest.NewRequest(http.MethodPost, "/streams/stream-01/audio/opus", bytes.NewBufferString(mismatchedSourceBody))
	mismatchedSourceReq.Header.Set("Authorization", "Bearer "+token)
	mismatchedSourceRes := httptest.NewRecorder()
	handler.ServeHTTP(mismatchedSourceRes, mismatchedSourceReq)
	if mismatchedSourceRes.Code != http.StatusUnauthorized || !strings.Contains(mismatchedSourceRes.Body.String(), "discord_service_id_mismatch") {
		t.Fatalf("expected Discord source identity mismatch to be rejected, got %d body = %s", mismatchedSourceRes.Code, mismatchedSourceRes.Body.String())
	}

	wrongStreamToken, err := ingesttoken.Issue(signingKey, ingesttoken.Claims{
		StreamID:    "stream-02",
		ServiceID:   "discord-bot-01",
		ServiceType: "discord_bot",
		Purpose:     "discord_audio",
		Audience:    "encoder_recorder",
		ExpiresAt:   time.Now().Add(time.Hour).Unix(),
	})
	if err != nil {
		t.Fatal(err)
	}
	badReq := httptest.NewRequest(http.MethodPost, "/streams/stream-01/audio/opus", bytes.NewBufferString(body))
	badReq.Header.Set("Authorization", "Bearer "+wrongStreamToken)
	badRes := httptest.NewRecorder()
	handler.ServeHTTP(badRes, badReq)
	if badRes.Code != http.StatusUnauthorized {
		t.Fatalf("expected stream-mismatched signed token to be rejected, got %d body = %s", badRes.Code, badRes.Body.String())
	}
}

func TestRequireSignedIngestTokensRejectsStaticFallback(t *testing.T) {
	signingKey := "test-stream-ingest-signing-key"
	verifier := TokenVerifier{
		WorkerEventsPlainToken: "static-worker-token",
		DiscordAudioPlainToken: "static-audio-token",
		IngestTokenSigningKey:  signingKey,
		RequireSignedIngest:    true,
	}

	if verifier.VerifyWorkerEvents("Bearer static-worker-token", "stream-01") {
		t.Fatal("static worker ingest token must be rejected when signed ingest is required")
	}
	if verifier.VerifyDiscordAudio("Bearer static-audio-token", "stream-01") {
		t.Fatal("static discord audio token must be rejected when signed ingest is required")
	}

	workerToken, err := ingesttoken.Issue(signingKey, ingesttoken.Claims{
		StreamID:    "stream-01",
		ServiceID:   "worker-01",
		ServiceType: "worker",
		Purpose:     "worker_events",
		Audience:    "encoder_recorder",
		ExpiresAt:   time.Now().Add(time.Hour).Unix(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !verifier.VerifyWorkerEvents("Bearer "+workerToken, "stream-01") {
		t.Fatal("signed worker ingest token must be accepted when signed ingest is required")
	}

	audioToken, err := ingesttoken.Issue(signingKey, ingesttoken.Claims{
		StreamID:    "stream-01",
		ServiceID:   "discord-bot-01",
		ServiceType: "discord_bot",
		Purpose:     "discord_audio",
		Audience:    "encoder_recorder",
		ExpiresAt:   time.Now().Add(time.Hour).Unix(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !verifier.VerifyDiscordAudio("Bearer "+audioToken, "stream-01") {
		t.Fatal("signed discord audio ingest token must be accepted when signed ingest is required")
	}
}

func TestTokenVerifierFromEnvRequiresSignedIngestByDefault(t *testing.T) {
	t.Setenv("ENCODER_WORKER_EVENTS_TOKEN", "static-worker-token")
	t.Setenv("ENCODER_DISCORD_AUDIO_TOKEN", "static-audio-token")
	verifier := TokenVerifierFromEnv()
	if !verifier.RequireSignedIngest {
		t.Fatal("signed ingest must be required by default")
	}
	if verifier.VerifyWorkerEvents("Bearer static-worker-token", "stream-01") {
		t.Fatal("static worker token must not be accepted by default")
	}
	if verifier.VerifyDiscordAudio("Bearer static-audio-token", "stream-01") {
		t.Fatal("static discord audio token must not be accepted by default")
	}
}

func TestTokenVerifierFromEnvAllowsStaticFallbackOnlyWhenExplicitlyDisabled(t *testing.T) {
	t.Setenv("AUTOSTREAM_REQUIRE_SIGNED_INGEST_TOKENS", "false")
	t.Setenv("ENCODER_WORKER_EVENTS_TOKEN", "static-worker-token")
	t.Setenv("ENCODER_DISCORD_AUDIO_TOKEN", "static-audio-token")
	verifier := TokenVerifierFromEnv()
	if verifier.RequireSignedIngest {
		t.Fatal("signed ingest should be disabled only when explicitly configured")
	}
	if !verifier.VerifyWorkerEvents("Bearer static-worker-token", "stream-01") {
		t.Fatal("static worker token should be accepted when fallback is explicitly enabled")
	}
	if !verifier.VerifyDiscordAudio("Bearer static-audio-token", "stream-01") {
		t.Fatal("static discord audio token should be accepted when fallback is explicitly enabled")
	}
}

func TestTokenVerifierReadsNodeRuntimeTokenAfterStartup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	t.Setenv("AUTOSTREAM_NODE_CONFIG", path)
	verifier := TokenVerifierFromEnv()
	if !verifier.UseNodeRuntimeToken || verifier.PlainToken != "" || verifier.SHA256Hex != "" {
		t.Fatalf("expected config-backed runtime token verifier: %#v", verifier)
	}
	if verifier.Verify("Bearer runtime-secret") {
		t.Fatal("runtime token should not verify before config exists")
	}
	if check := serviceTokenPreflight(verifier); check.Status != "missing" || !strings.Contains(check.Message, "Auto Configure") {
		t.Fatalf("pending node config should be actionable in preflight: %#v", check)
	}
	writeNodeConfigForVerifierTest(t, path, "encoder_recorder")
	if !verifier.Verify("Bearer runtime-secret") {
		t.Fatal("runtime token should verify after config is written")
	}
	if check := serviceTokenPreflight(verifier); check.Status != "ok" || !strings.Contains(check.Message, "Node Runtime Token") || strings.Contains(check.Message, "SERVICE_CONTROL_TOKEN") {
		t.Fatalf("node runtime token should be identified accurately in preflight: %#v", check)
	}
	handler := NewServerWithManagers("encoder_recorder", nil, workerevents.NewManager(t.TempDir()), verifier)
	statusReq := httptest.NewRequest(http.MethodGet, "/status", nil)
	statusRes := httptest.NewRecorder()
	handler.ServeHTTP(statusRes, statusReq)
	var status Status
	if err := json.NewDecoder(statusRes.Body).Decode(&status); err != nil {
		t.Fatal(err)
	}
	if statusRes.Code != http.StatusOK || status.ServiceID != "encoder-recorder-01" {
		t.Fatalf("status should use node config identity: status=%d body=%s", statusRes.Code, statusRes.Body.String())
	}
	signedToken, err := ingesttoken.Issue("node-config-signing-key", ingesttoken.Claims{StreamID: "stream-01", ServiceID: "worker-01", ServiceType: "worker", Purpose: "worker_events", Audience: "encoder_recorder", ExpiresAt: time.Now().Add(time.Hour).Unix()})
	if err != nil {
		t.Fatal(err)
	}
	if !verifier.VerifyWorkerEvents("Bearer "+signedToken, "stream-01") {
		t.Fatal("stream ingest signing key should be reloaded from node config")
	}
	writeNodeConfigForVerifierTestWithValues(t, path, "encoder_recorder", "rotated-runtime-secret", "rotated-node-config-signing-key")
	if verifier.Verify("Bearer runtime-secret") || !verifier.Verify("Bearer rotated-runtime-secret") {
		t.Fatal("Node Runtime Token rotation should take effect without accepting the previous token")
	}
	if verifier.VerifyWorkerEvents("Bearer "+signedToken, "stream-01") {
		t.Fatal("previous stream ingest signing key must stop working after config rotation")
	}
	rotatedSignedToken, err := ingesttoken.Issue("rotated-node-config-signing-key", ingesttoken.Claims{StreamID: "stream-01", ServiceID: "worker-01", ServiceType: "worker", Purpose: "worker_events", Audience: "encoder_recorder", ExpiresAt: time.Now().Add(time.Hour).Unix()})
	if err != nil {
		t.Fatal(err)
	}
	if !verifier.VerifyWorkerEvents("Bearer "+rotatedSignedToken, "stream-01") {
		t.Fatal("rotated stream ingest signing key should be loaded from node config")
	}
}

func TestNodeConfigPathDisablesLegacyControlTokenEvenWhilePending(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	t.Setenv("AUTOSTREAM_NODE_CONFIG", path)
	t.Setenv("SERVICE_CONTROL_TOKEN", "legacy-control-token")
	verifier := TokenVerifierFromEnv()
	if !verifier.UseNodeRuntimeToken || verifier.Verify("Bearer legacy-control-token") {
		t.Fatal("configured node path must fail closed instead of accepting the legacy control token")
	}
	if check := serviceTokenPreflight(verifier); check.Status != "missing" || !strings.Contains(check.Message, "Auto Configure") {
		t.Fatalf("pending node config must not report the legacy token as ready: %#v", check)
	}
	writeNodeConfigForVerifierTest(t, path, "encoder_recorder")
	if verifier.Verify("Bearer legacy-control-token") {
		t.Fatal("legacy control token must be disabled after node config becomes available")
	}
	if !verifier.Verify("Bearer runtime-secret") {
		t.Fatal("Node Runtime Token should take precedence after Auto Configure")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if verifier.Verify("Bearer legacy-control-token") {
		t.Fatal("legacy control token must not reactivate when node config disappears")
	}
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
	body := `panel:
  url: "https://panel.example.jp"
node:
  id: "encoder-recorder-01"
  name: "Encoder Recorder 01"
  type: "` + nodeType + `"
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

func attrsString(attrs map[string]any) string {
	out := ""
	for key, value := range attrs {
		if text, ok := value.(string); ok {
			out += key + "=" + text + ";"
		}
	}
	return out
}

func TestWorkerEventsEndpointWritesArchiveSidecars(t *testing.T) {
	root := t.TempDir()
	eventManager := workerevents.NewManager(root)
	starter := &httpFakeStarter{}
	processManager := &streamproc.Manager{ArchiveRoot: root, FFmpegBin: "ffmpeg", Starter: starter, InputResolver: testInputResolver, AllowHostnameInputs: true}
	controlSum := sha256.Sum256([]byte("control-token"))
	workerSum := sha256.Sum256([]byte("worker-token"))
	handler := NewServerWithManagers("encoder_recorder", processManager, eventManager, TokenVerifier{SHA256Hex: hex.EncodeToString(controlSum[:]), WorkerEventsSHA256Hex: hex.EncodeToString(workerSum[:])})
	startReq := httptest.NewRequest(http.MethodPost, "/streams/start", bytes.NewBufferString(`{"stream_id":"stream-01","name":"Worker Event Stream","input_url":"srt://input.example.com:9000","rtmp_url":"rtmps://youtube.example.com/live2"}`))
	startReq.Header.Set("Authorization", "Bearer control-token")
	t.Setenv("YOUTUBE_STREAM_KEY", "super-secret-stream-key")
	startRes := httptest.NewRecorder()
	handler.ServeHTTP(startRes, startReq)
	if startRes.Code != http.StatusAccepted {
		t.Fatalf("start status = %d body = %s", startRes.Code, startRes.Body.String())
	}

	body := `{"id":"event-01","stream_id":"stream-01","type":"caption.telop","payload":{"text":"hello","speaker_user_id":"user-01"},"timestamp":"2026-05-28T00:00:00Z"}`
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

func TestWorkerEventsEndpointUsesScopedTokenWhenConfigured(t *testing.T) {
	root := t.TempDir()
	eventManager := workerevents.NewManager(root)
	processManager := &streamproc.Manager{ArchiveRoot: root, FFmpegBin: "ffmpeg", Starter: &httpFakeStarter{}, InputResolver: testInputResolver, AllowHostnameInputs: true}
	handler := NewServerWithManagers("encoder_recorder", processManager, eventManager, TokenVerifier{
		PlainToken:             "control-token",
		WorkerEventsPlainToken: "worker-events-token",
	})
	startReq := httptest.NewRequest(http.MethodPost, "/streams/start", bytes.NewBufferString(`{"stream_id":"stream-01","name":"Worker Event Stream","input_url":"srt://input.example.com:9000","rtmp_url":"rtmps://youtube.example.com/live2"}`))
	startReq.Header.Set("Authorization", "Bearer control-token")
	t.Setenv("YOUTUBE_STREAM_KEY", "super-secret-stream-key")
	startRes := httptest.NewRecorder()
	handler.ServeHTTP(startRes, startReq)
	if startRes.Code != http.StatusAccepted {
		t.Fatalf("start status = %d body = %s", startRes.Code, startRes.Body.String())
	}

	body := `{"id":"event-01","stream_id":"stream-01","service_id":"worker-01","type":"caption.telop","payload":{"text":"hello"},"timestamp":"2026-05-28T00:00:00Z"}`
	controlReq := httptest.NewRequest(http.MethodPost, "/worker-events", bytes.NewBufferString(body))
	controlReq.Header.Set("Authorization", "Bearer control-token")
	controlRes := httptest.NewRecorder()
	handler.ServeHTTP(controlRes, controlReq)
	if controlRes.Code != http.StatusUnauthorized {
		t.Fatalf("expected control token to be rejected for scoped worker events, got %d body = %s", controlRes.Code, controlRes.Body.String())
	}

	workerReq := httptest.NewRequest(http.MethodPost, "/worker-events", bytes.NewBufferString(body))
	workerReq.Header.Set("Authorization", "Bearer worker-events-token")
	workerRes := httptest.NewRecorder()
	handler.ServeHTTP(workerRes, workerReq)
	if workerRes.Code != http.StatusAccepted {
		t.Fatalf("expected scoped worker events token to be accepted, got %d body = %s", workerRes.Code, workerRes.Body.String())
	}
}

func TestWorkerEventsEndpointAcceptsSignedStreamIngestToken(t *testing.T) {
	root := t.TempDir()
	eventManager := workerevents.NewManager(root)
	processManager := &streamproc.Manager{ArchiveRoot: root, FFmpegBin: "ffmpeg", Starter: &httpFakeStarter{}, InputResolver: testInputResolver, AllowHostnameInputs: true}
	signingKey := "test-stream-ingest-signing-key"
	handler := NewServerWithManagers("encoder_recorder", processManager, eventManager, TokenVerifier{
		PlainToken:             "control-token",
		IngestTokenSigningKey:  signingKey,
		WorkerEventsPlainToken: "static-worker-token",
	})
	startReq := httptest.NewRequest(http.MethodPost, "/streams/start", bytes.NewBufferString(`{"stream_id":"stream-01","name":"Worker Event Stream","input_url":"srt://input.example.com:9000","rtmp_url":"rtmps://youtube.example.com/live2"}`))
	startReq.Header.Set("Authorization", "Bearer control-token")
	t.Setenv("YOUTUBE_STREAM_KEY", "super-secret-stream-key")
	startRes := httptest.NewRecorder()
	handler.ServeHTTP(startRes, startReq)
	if startRes.Code != http.StatusAccepted {
		t.Fatalf("start status = %d body = %s", startRes.Code, startRes.Body.String())
	}

	token, err := ingesttoken.Issue(signingKey, ingesttoken.Claims{
		StreamID:    "stream-01",
		ServiceID:   "worker-01",
		ServiceType: "worker",
		Purpose:     "worker_events",
		Audience:    "encoder_recorder",
		ExpiresAt:   time.Now().Add(time.Hour).Unix(),
	})
	if err != nil {
		t.Fatal(err)
	}
	body := `{"id":"event-01","stream_id":"stream-01","service_id":"worker-01","type":"caption.telop","payload":{"text":"hello"},"timestamp":"2026-05-28T00:00:00Z"}`
	workerReq := httptest.NewRequest(http.MethodPost, "/worker-events", bytes.NewBufferString(body))
	workerReq.Header.Set("Authorization", "Bearer "+token)
	workerRes := httptest.NewRecorder()
	handler.ServeHTTP(workerRes, workerReq)
	if workerRes.Code != http.StatusAccepted {
		t.Fatalf("expected signed worker ingest token to be accepted, got %d body = %s", workerRes.Code, workerRes.Body.String())
	}

	mismatchedBody := `{"id":"event-02","stream_id":"stream-01","service_id":"worker-other","type":"caption.telop","payload":{"text":"hello"},"timestamp":"2026-05-28T00:00:00Z"}`
	mismatchedReq := httptest.NewRequest(http.MethodPost, "/worker-events", bytes.NewBufferString(mismatchedBody))
	mismatchedReq.Header.Set("Authorization", "Bearer "+token)
	mismatchedRes := httptest.NewRecorder()
	handler.ServeHTTP(mismatchedRes, mismatchedReq)
	if mismatchedRes.Code != http.StatusUnauthorized || !strings.Contains(mismatchedRes.Body.String(), "worker_service_id_mismatch") {
		t.Fatalf("expected worker service id mismatch to be rejected, got %d body = %s", mismatchedRes.Code, mismatchedRes.Body.String())
	}

	wrongPurposeToken, err := ingesttoken.Issue(signingKey, ingesttoken.Claims{
		StreamID:    "stream-01",
		ServiceID:   "worker-01",
		ServiceType: "worker",
		Purpose:     "discord_audio",
		Audience:    "encoder_recorder",
		ExpiresAt:   time.Now().Add(time.Hour).Unix(),
	})
	if err != nil {
		t.Fatal(err)
	}
	badReq := httptest.NewRequest(http.MethodPost, "/worker-events", bytes.NewBufferString(body))
	badReq.Header.Set("Authorization", "Bearer "+wrongPurposeToken)
	badRes := httptest.NewRecorder()
	handler.ServeHTTP(badRes, badReq)
	if badRes.Code != http.StatusUnauthorized {
		t.Fatalf("expected wrong-purpose signed token to be rejected, got %d body = %s", badRes.Code, badRes.Body.String())
	}
}

func TestWorkerEventsEndpointRejectsOfflineStream(t *testing.T) {
	root := t.TempDir()
	eventManager := workerevents.NewManager(root)
	sum := sha256.Sum256([]byte("worker-token"))
	handler := NewServerWithManagers("encoder_recorder", nil, eventManager, TokenVerifier{WorkerEventsSHA256Hex: hex.EncodeToString(sum[:])})
	body := `{"id":"event-01","stream_id":"stream-01","type":"caption.telop","payload":{"text":"hello"},"timestamp":"2026-05-28T00:00:00Z"}`
	req := httptest.NewRequest(http.MethodPost, "/worker-events", bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer worker-token")
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusConflict {
		t.Fatalf("expected offline worker event to be rejected, got %d body = %s", res.Code, res.Body.String())
	}
	if _, err := os.Stat(filepath.Join(root, "tmp", "stream-01", "captions.vtt")); !os.IsNotExist(err) {
		t.Fatalf("offline worker event must not write captions, stat err=%v", err)
	}
}

func TestWorkerEventsEndpointRejectsInvalidToken(t *testing.T) {
	sum := sha256.Sum256([]byte("worker-token"))
	handler := NewServerWithManagers("encoder_recorder", nil, workerevents.NewManager(t.TempDir()), TokenVerifier{WorkerEventsSHA256Hex: hex.EncodeToString(sum[:])})
	req := httptest.NewRequest(http.MethodPost, "/worker-events", bytes.NewBufferString(`{}`))
	req.Header.Set("Authorization", "Bearer wrong")
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", res.Code)
	}
}

func TestRecentWorkerEventsEndpoint(t *testing.T) {
	root := t.TempDir()
	manager := workerevents.NewManager(root)
	if _, err := manager.Add(workerevents.Event{ID: "event-01", StreamID: "stream-01", Type: "overlay.current_time"}); err != nil {
		t.Fatal(err)
	}
	handler := NewServerWithManagers("encoder_recorder", nil, manager, TokenVerifier{PlainToken: "token"})
	req := httptest.NewRequest(http.MethodGet, "/streams/stream-01/worker-events", nil)
	req.Header.Set("Authorization", "Bearer token")
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", res.Code, res.Body.String())
	}
	if !strings.Contains(res.Body.String(), "event-01") {
		t.Fatalf("unexpected response: %s", res.Body.String())
	}
}

type httpFakeStarter struct {
	process *httpFakeProcess
	bin     string
	args    []string
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
