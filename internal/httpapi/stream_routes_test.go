package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/example/autostream-encoder-recorder/internal/control"
	"github.com/example/autostream-encoder-recorder/internal/ingesttoken"
	"github.com/example/autostream-encoder-recorder/internal/lifecycle"
	"github.com/example/autostream-encoder-recorder/internal/outputrelay"
	"github.com/example/autostream-encoder-recorder/internal/streamproc"
	"github.com/example/autostream-encoder-recorder/internal/workerevents"
)

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

func TestStartStreamReturnsConflictWhenRuntimeSecretLeaseActive(t *testing.T) {
	t.Setenv("SERVICE_CONTROL_TOKEN", "service-token")
	root := t.TempDir()
	processManager := &streamproc.Manager{ArchiveRoot: root, FFmpegBin: "ffmpeg", Starter: &httpFakeStarter{}, InputResolver: testInputResolver, AllowHostnameInputs: true, OutputRelayMode: outputrelay.ModeDirect}
	resolved := 0
	handler := archiveWireTestHandler("/streams/start", processManager, func(ctx context.Context, streamID, archiveProfileID, secretName string) (string, error) {
		resolved++
		if streamID != "stream-01" || archiveProfileID != "archive-profile-01" || secretName != "drive_destination:dest-01:folder_id" {
			t.Error("lease resolve context differs from assigned archive runtime")
		}
		return "", control.ErrRuntimeSecretLeaseActive
	}, archiveWireRuntimeProvider(map[string]any{"auth_mode": "oauth2", "folder_id_secret_name": "drive_destination:dest-01:folder_id"}))

	body := `{"stream_id":"stream-01","name":"Morning Stream","input_url":"srt://input.example.com:9000","rtmp_url":"rtmps://youtube.example.com/live2","archive_profile_id":"archive-profile-01"}`
	req := httptest.NewRequest(http.MethodPost, "/streams/start", bytes.NewBufferString(body))
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
	if strings.Contains(res.Body.String(), "drive_destination") || strings.Contains(res.Body.String(), "folder_id") {
		t.Fatalf("runtime secret resolve response leaked context details: %s", res.Body.String())
	}
}

func TestStartStreamRejectsRawArchiveSecretFields(t *testing.T) {
	t.Setenv("SERVICE_CONTROL_TOKEN", "service-token")
	root := t.TempDir()
	processManager := &streamproc.Manager{ArchiveRoot: root, FFmpegBin: "ffmpeg", Starter: &httpFakeStarter{}, InputResolver: testInputResolver, AllowHostnameInputs: true, OutputRelayMode: outputrelay.ModeDirect}
	handler := archiveWireTestHandler("/streams/start", processManager, func(ctx context.Context, streamID, archiveProfileID, secretName string) (string, error) {
		t.Fatalf("runtime secret resolver should not be called for raw archive secret fields")
		return "", nil
	}, nil)

	body := `{"stream_id":"stream-01","name":"Morning Stream","input_url":"srt://input.example.com:9000","rtmp_url":"rtmps://youtube.example.com/live2","archive_config":{"archive_profile_id":"archive-profile-01","auth_mode":"oauth2","folder_id":"raw-drive-folder-id","refresh_token_secret_name":"oauth_account:account-01:refresh_token"}}`
	req := httptest.NewRequest(http.MethodPost, "/streams/start", bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer service-token")
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d body = %s", res.Code, res.Body.String())
	}
	if !strings.Contains(res.Body.String(), `"code":"bad_request"`) {
		t.Fatalf("expected raw archive secret error, got %s", res.Body.String())
	}
	if strings.Contains(res.Body.String(), "raw-drive-folder-id") || strings.Contains(res.Body.String(), "folder_id") || strings.Contains(res.Body.String(), "refresh_token") {
		t.Fatalf("raw archive secret rejection leaked field context: %s", res.Body.String())
	}
}

func TestStartStreamRejectsRawArchiveSecretFieldsWithoutResolver(t *testing.T) {
	t.Setenv("SERVICE_CONTROL_TOKEN", "service-token")
	root := t.TempDir()
	processManager := &streamproc.Manager{ArchiveRoot: root, FFmpegBin: "ffmpeg", Starter: &httpFakeStarter{}, InputResolver: testInputResolver, AllowHostnameInputs: true, OutputRelayMode: outputrelay.ModeDirect}
	handler := archiveWireTestHandler("/streams/start", processManager, nil, nil)

	body := `{"stream_id":"stream-01","name":"Morning Stream","input_url":"srt://input.example.com:9000","rtmp_url":"rtmps://youtube.example.com/live2","archive_config":{"archive_profile_id":"archive-profile-01","auth_mode":"oauth2","refresh_token":"raw-refresh-token","folder_id_secret_name":"drive_destination:dest-01:folder_id"}}`
	req := httptest.NewRequest(http.MethodPost, "/streams/start", bytes.NewBufferString(body))
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

func TestDryRunEndpoint(t *testing.T) {
	t.Setenv("SERVICE_CONTROL_TOKEN", "service-token")
	root := t.TempDir()
	t.Setenv("AUTOSTREAM_ARCHIVE_DIR", root)
	handler := newV2TestServer(t, nil, "stream-01")
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
		!strings.Contains(res.Body.String(), `"output_mode"`) {
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
	if res.Code != http.StatusBadRequest || !strings.Contains(res.Body.String(), `"code":"bad_request"`) {
		t.Fatalf("expected raw stream key rejection, status = %d body = %s", res.Code, res.Body.String())
	}
	if strings.Contains(res.Body.String(), "raw-production-stream-key") {
		t.Fatalf("raw stream key leaked in rejection response: %s", res.Body.String())
	}
}

func TestStartStopProcessEndpoints(t *testing.T) {
	t.Setenv("SERVICE_CONTROL_TOKEN", "service-token")
	root := t.TempDir()
	processManager := &streamproc.Manager{ArchiveRoot: root, FFmpegBin: "ffmpeg", Starter: &httpFakeStarter{}, InputResolver: testInputResolver, AllowHostnameInputs: true, OutputRelayMode: outputrelay.ModeDirect}
	handler := newV2TestServer(t, processManager, "stream-01")

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

	repeatedStopReq := httptest.NewRequest(http.MethodPost, "/streams/stream-01/stop", nil)
	repeatedStopReq.Header.Set("Authorization", "Bearer service-token")
	repeatedStopRes := httptest.NewRecorder()
	handler.ServeHTTP(repeatedStopRes, repeatedStopReq)
	if repeatedStopRes.Code != http.StatusAccepted || !strings.Contains(repeatedStopRes.Body.String(), "already_stopped") {
		t.Fatalf("repeated stop status = %d body = %s", repeatedStopRes.Code, repeatedStopRes.Body.String())
	}
}

func TestStopEndpointRejectsDifferentActiveStream(t *testing.T) {
	t.Setenv("SERVICE_CONTROL_TOKEN", "service-token")
	root := t.TempDir()
	processManager := &streamproc.Manager{ArchiveRoot: root, FFmpegBin: "ffmpeg", Starter: &httpFakeStarter{}, InputResolver: testInputResolver, AllowHostnameInputs: true, OutputRelayMode: outputrelay.ModeDirect}
	handler := newV2TestServer(t, processManager, "active-stream")

	startReq := httptest.NewRequest(http.MethodPost, "/streams/start", bytes.NewBufferString(`{"stream_id":"active-stream","name":"Active Stream","input_url":"srt://input.example.com:9000"}`))
	startReq.Header.Set("Authorization", "Bearer service-token")
	startRes := httptest.NewRecorder()
	handler.ServeHTTP(startRes, startReq)
	if startRes.Code != http.StatusAccepted {
		t.Fatalf("start status = %d body = %s", startRes.Code, startRes.Body.String())
	}

	stopReq := httptest.NewRequest(http.MethodPost, "/streams/requested-stream/stop", nil)
	stopReq.Header.Set("Authorization", "Bearer service-token")
	stopRes := httptest.NewRecorder()
	handler.ServeHTTP(stopRes, stopReq)
	if stopRes.Code != http.StatusConflict || !strings.Contains(stopRes.Body.String(), `"code":"stream_already_running"`) {
		t.Fatalf("mismatched stop status = %d body = %s", stopRes.Code, stopRes.Body.String())
	}
	if current := processManager.CurrentStreamID(); current != "active-stream" {
		t.Fatalf("active stream changed after mismatched stop: %q", current)
	}
}

func TestStopEndpointRejectsStartingStream(t *testing.T) {
	t.Setenv("SERVICE_CONTROL_TOKEN", "service-token")
	starter := &httpBlockingStarter{started: make(chan struct{}), release: make(chan struct{})}
	processManager := &streamproc.Manager{ArchiveRoot: t.TempDir(), FFmpegBin: "ffmpeg", Starter: starter, InputResolver: testInputResolver, AllowHostnameInputs: true, OutputRelayMode: outputrelay.ModeDirect}
	handler := newV2TestServer(t, processManager, "stream-01")
	job := lifecycle.StreamJob{StreamID: "stream-01", Name: "Morning Stream", InputURL: "srt://input.example.com:9000", RTMPURL: "rtmps://youtube.example.com/live2", StreamKey: "key", YouTubeOutputMode: "stream_key"}
	startErr := make(chan error, 1)
	go func() {
		_, err := processManager.Start(job)
		startErr <- err
	}()
	<-starter.started

	stopReq := httptest.NewRequest(http.MethodPost, "/streams/stream-01/stop", nil)
	stopReq.Header.Set("Authorization", "Bearer service-token")
	stopRes := httptest.NewRecorder()
	handler.ServeHTTP(stopRes, stopReq)
	if stopRes.Code != http.StatusConflict || !strings.Contains(stopRes.Body.String(), `"code":"stream_starting"`) {
		t.Fatalf("starting stop status = %d body = %s", stopRes.Code, stopRes.Body.String())
	}

	close(starter.release)
	if err := <-startErr; err != nil {
		t.Fatalf("start stream: %v", err)
	}
	if _, err := processManager.Stop(job.StreamID); err != nil {
		t.Fatalf("stop running stream: %v", err)
	}
}

func TestStopEndpointRetriesKnownStoppedTargetWithoutAffectingSuccessor(t *testing.T) {
	t.Setenv("SERVICE_CONTROL_TOKEN", "service-token")
	root := t.TempDir()
	processManager := &streamproc.Manager{ArchiveRoot: root, FFmpegBin: "ffmpeg", Starter: &httpFakeStarter{}, InputResolver: testInputResolver, AllowHostnameInputs: true, OutputRelayMode: outputrelay.ModeDirect}
	handler := newV2TestServer(t, processManager, "stream-a", "stream-b")

	startA := httptest.NewRequest(http.MethodPost, "/streams/start", bytes.NewBufferString(`{"stream_id":"stream-a","name":"Stream A","input_url":"srt://input.example.com:9000"}`))
	startA.Header.Set("Authorization", "Bearer service-token")
	startARes := httptest.NewRecorder()
	handler.ServeHTTP(startARes, startA)
	if startARes.Code != http.StatusAccepted {
		t.Fatalf("start stream-a status = %d body = %s", startARes.Code, startARes.Body.String())
	}

	stopA := httptest.NewRequest(http.MethodPost, "/streams/stream-a/stop", nil)
	stopA.Header.Set("Authorization", "Bearer service-token")
	stopARes := httptest.NewRecorder()
	handler.ServeHTTP(stopARes, stopA)
	if stopARes.Code != http.StatusAccepted {
		t.Fatalf("initial stop stream-a status = %d body = %s", stopARes.Code, stopARes.Body.String())
	}

	deadline := time.After(time.Second)
	for {
		snapshot, err := processManager.Status("stream-a")
		if err == nil && snapshot.Status == "stopped" {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("stream-a did not become stopped: snapshot=%#v err=%v", snapshot, err)
		default:
			time.Sleep(5 * time.Millisecond)
		}
	}

	startB := httptest.NewRequest(http.MethodPost, "/streams/start", bytes.NewBufferString(`{"stream_id":"stream-b","name":"Stream B","input_url":"srt://input.example.com:9000"}`))
	startB.Header.Set("Authorization", "Bearer service-token")
	startBRes := httptest.NewRecorder()
	handler.ServeHTTP(startBRes, startB)
	if startBRes.Code != http.StatusAccepted {
		t.Fatalf("start stream-b status = %d body = %s", startBRes.Code, startBRes.Body.String())
	}

	retryA := httptest.NewRequest(http.MethodPost, "/streams/stream-a/stop", nil)
	retryA.Header.Set("Authorization", "Bearer service-token")
	retryARes := httptest.NewRecorder()
	handler.ServeHTTP(retryARes, retryA)
	if retryARes.Code != http.StatusAccepted || !strings.Contains(retryARes.Body.String(), `"status":"already_stopped"`) {
		t.Fatalf("retry stop stream-a status = %d body = %s", retryARes.Code, retryARes.Body.String())
	}
	if snapshot, err := processManager.Status("stream-b"); err != nil || snapshot.Status != "running" {
		t.Fatalf("stream-b changed after retrying stream-a stop: snapshot=%#v err=%v", snapshot, err)
	}
}

func TestStopEndpointUsesDurableTargetReceiptAfterRestartWithoutAffectingSuccessor(t *testing.T) {
	t.Setenv("SERVICE_CONTROL_TOKEN", "service-token")
	root := t.TempDir()

	beforeRestart := &streamproc.Manager{ArchiveRoot: root, FFmpegBin: "ffmpeg", Starter: &httpFakeStarter{}, InputResolver: testInputResolver, AllowHostnameInputs: true, OutputRelayMode: outputrelay.ModeDirect}
	if _, err := beforeRestart.Start(lifecycle.StreamJob{StreamID: "stream-a", Name: "Stream A", InputURL: "srt://input.example.com:9000", RTMPURL: "rtmps://youtube.example.com/live2", StreamKey: "key", YouTubeOutputMode: "stream_key"}); err != nil {
		t.Fatal(err)
	}
	if _, err := beforeRestart.Stop("stream-a"); err != nil {
		t.Fatal(err)
	}

	afterRestart := &streamproc.Manager{ArchiveRoot: root, FFmpegBin: "ffmpeg", Starter: &httpFakeStarter{}, InputResolver: testInputResolver, AllowHostnameInputs: true, OutputRelayMode: outputrelay.ModeDirect}
	handler := newV2TestServer(t, afterRestart, "stream-a", "stream-b")
	startB := httptest.NewRequest(http.MethodPost, "/streams/start", bytes.NewBufferString(`{"stream_id":"stream-b","name":"Stream B","input_url":"srt://input.example.com:9000"}`))
	startB.Header.Set("Authorization", "Bearer service-token")
	startBRes := httptest.NewRecorder()
	handler.ServeHTTP(startBRes, startB)
	if startBRes.Code != http.StatusAccepted {
		t.Fatalf("start stream-b status = %d body = %s", startBRes.Code, startBRes.Body.String())
	}

	retryA := httptest.NewRequest(http.MethodPost, "/streams/stream-a/stop", nil)
	retryA.Header.Set("Authorization", "Bearer service-token")
	retryARes := httptest.NewRecorder()
	handler.ServeHTTP(retryARes, retryA)
	if retryARes.Code != http.StatusAccepted || !strings.Contains(retryARes.Body.String(), `"status":"already_stopped"`) {
		t.Fatalf("post-restart retry status = %d body = %s", retryARes.Code, retryARes.Body.String())
	}
	if snapshot, err := afterRestart.Status("stream-b"); err != nil || snapshot.Status != "running" {
		t.Fatalf("stream-b changed after post-restart retry for stream-a: snapshot=%#v err=%v", snapshot, err)
	}
}

func TestStopEndpointRejectsUnknownStreamWhenIdle(t *testing.T) {
	t.Setenv("SERVICE_CONTROL_TOKEN", "service-token")
	processManager := &streamproc.Manager{ArchiveRoot: t.TempDir(), FFmpegBin: "ffmpeg", Starter: &httpFakeStarter{}, InputResolver: testInputResolver, AllowHostnameInputs: true, OutputRelayMode: outputrelay.ModeDirect}
	handler := newV2TestServer(t, processManager)

	req := httptest.NewRequest(http.MethodPost, "/streams/unknown-stream/stop", nil)
	req.Header.Set("Authorization", "Bearer service-token")
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusNotFound || !strings.Contains(res.Body.String(), `"code":"stream_not_running"`) {
		t.Fatalf("unknown stop status = %d body = %s", res.Code, res.Body.String())
	}
}

func TestStartEndpointUsesSelectedEncoderRuntimeProfile(t *testing.T) {
	t.Setenv("SERVICE_CONTROL_TOKEN", "service-token")
	root := t.TempDir()
	starter := &httpFakeStarter{}
	processManager := &streamproc.Manager{ArchiveRoot: root, FFmpegBin: "ffmpeg", Starter: starter, InputResolver: testInputResolver, AllowHostnameInputs: true, OutputRelayMode: outputrelay.ModeDirect}
	handler := NewServerWithManagersAndRuntimeConfig(
		"encoder_recorder",
		processManager,
		workerevents.NewManager(root),
		TokenVerifier{PlainToken: "service-token"},
		testYouTubeSecretResolver,
		func(context.Context) (control.RuntimeConfig, error) {
			cfg, _ := testYouTubeRuntimeProvider("stream-profile")(context.Background())
			cfg.Profiles = map[string][]control.RuntimeProfile{
				"encoder": {{ID: "encoder-720p", Kind: "encoder", Config: map[string]any{
					"width": float64(1280), "height": float64(720), "fps": float64(30), "video_bitrate_kbps": float64(4500),
				}}},
			}
			return cfg, nil
		},
	)

	body := `{"stream_id":"stream-profile","name":"Profile Stream","input_url":"srt://input.example.com:9000","encoder_profile_id":"encoder-720p"}`
	req := httptest.NewRequest(http.MethodPost, "/streams/start", bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer service-token")
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusAccepted {
		t.Fatalf("start status = %d body = %s", res.Code, res.Body.String())
	}
	joined := strings.Join(starter.args, " ")
	for _, want := range []string{"scale=1280:720", "-b:v 4500k", "-r 30"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("selected encoder profile option %q missing from FFmpeg args: %s", want, joined)
		}
	}
}

func TestStartEndpointRejectsRawStreamKeyInProduction(t *testing.T) {
	t.Setenv("SERVICE_CONTROL_TOKEN", "service-token")
	t.Setenv("AUTOSTREAM_ENV", "production")
	root := t.TempDir()
	starter := &httpFakeStarter{}
	processManager := &streamproc.Manager{ArchiveRoot: root, FFmpegBin: "ffmpeg", Starter: starter, InputResolver: testInputResolver, AllowHostnameInputs: true, OutputRelayMode: outputrelay.ModeDirect}
	handler := newV2TestServer(t, processManager, "stream-01")

	body := `{"stream_id":"stream-01","name":"Morning Stream","input_url":"srt://input.example.com:9000","rtmp_url":"rtmps://youtube.example.com/live2","stream_key":"raw-production-stream-key"}`
	req := httptest.NewRequest(http.MethodPost, "/streams/start", bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer service-token")
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusBadRequest || !strings.Contains(res.Body.String(), `"code":"bad_request"`) {
		t.Fatalf("expected raw stream key rejection, status = %d body = %s", res.Code, res.Body.String())
	}
	if strings.Contains(res.Body.String(), "raw-production-stream-key") {
		t.Fatalf("raw stream key leaked in rejection response: %s", res.Body.String())
	}
	if starter.process != nil || len(starter.args) > 0 {
		t.Fatalf("ffmpeg must not start after raw production stream key rejection: %#v", starter.args)
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
	processManager := &streamproc.Manager{ArchiveRoot: root, FFmpegBin: "ffmpeg", Starter: starter, InputResolver: testInputResolver, AllowHostnameInputs: true, OutputRelayURL: "rtmp://127.0.0.1/autostream/{stream_id}", OutputRelayMode: outputrelay.ModeManagedLiveAPI, OutputRelayBindingID: testStaticRelayBindingID}
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
				StreamYouTubeConfigs: []control.RuntimeYouTubeStreamConfig{{
					StreamID:        "stream-01",
					AssignmentRole:  "primary",
					YouTubeOutputID: "youtube-output-01",
					Ready:           true,
					YouTubeConfig: map[string]any{
						"mode":             "live_api_relay_static",
						"relay_binding_id": testStaticRelayBindingID,
					},
				}},
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

	body := `{"stream_id":"stream-01","name":"Morning Stream","input_url":"srt://input.example.com:9000"}`
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
	root := t.TempDir()
	starter := &httpFakeStarter{}
	processManager := &streamproc.Manager{ArchiveRoot: root, FFmpegBin: "ffmpeg", Starter: starter, InputResolver: testInputResolver, AllowHostnameInputs: true, OutputRelayMode: outputrelay.ModeDirect}
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
		!strings.Contains(res.Body.String(), `"output_mode"`) {
		t.Fatalf("expected missing YouTube runtime fields, got %s", res.Body.String())
	}
	if strings.Contains(res.Body.String(), "env-secret-stream-key") {
		t.Fatalf("env stream key leaked in runtime config failure: %s", res.Body.String())
	}
	if len(starter.args) != 0 {
		t.Fatalf("ffmpeg must not start with env fallback YouTube config when runtime config is required: %#v", starter.args)
	}
}

func TestStartEndpointRejectsUnsafeRTMPTarget(t *testing.T) {
	t.Setenv("SERVICE_CONTROL_TOKEN", "service-token")
	root := t.TempDir()
	processManager := &streamproc.Manager{ArchiveRoot: root, FFmpegBin: "ffmpeg", Starter: &httpFakeStarter{}, InputResolver: testInputResolver, AllowHostnameInputs: true, OutputRelayMode: outputrelay.ModeDirect}
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
	starter := &httpFakeStarter{}
	processManager := &streamproc.Manager{ArchiveRoot: root, FFmpegBin: "ffmpeg", Starter: starter, InputResolver: testInputResolver, AllowHostnameInputs: true, OutputRelayMode: outputrelay.ModeDirect}
	handler := newV2TestServer(t, processManager, "stream-01")

	body := `{"stream_id":"stream-01","name":"Discord Audio Stream"}`
	req := httptest.NewRequest(http.MethodPost, "/streams/start", bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer service-token")
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusAccepted {
		t.Fatalf("start status = %d body = %s", res.Code, res.Body.String())
	}
	joinedArgs := strings.Join(starter.args, " ")
	if !strings.Contains(joinedArgs, "discord-opus.sdp") || !strings.Contains(joinedArgs, "showwaves") || !strings.Contains(joinedArgs, "color=c=0x0b1020") {
		t.Fatalf("expected discord audio visualizer ffmpeg args, got %#v", starter.args)
	}
	if _, err := os.Stat(filepath.Join(root, "tmp", "stream-01", "discord-opus.sdp")); err != nil {
		t.Fatal(err)
	}
}

func TestStartEndpointOptInWorkerVideoReturnsOneTimeSRTCredentialWithoutLeakingItToFFmpegOrMetadata(t *testing.T) {
	t.Setenv("AUTOSTREAM_ENV", "development")
	t.Setenv("AUTOSTREAM_WORKER_VIDEO_BIND_ADDR", "127.0.0.1:0")
	t.Setenv("AUTOSTREAM_WORKER_VIDEO_ADVERTISE_HOST", "127.0.0.1")
	const signingKey = "worker-video-signing-key"
	token, err := ingesttoken.Issue(signingKey, ingesttoken.Claims{
		StreamID:    "stream-worker-video",
		ServiceID:   "worker-01",
		ServiceType: "worker",
		Purpose:     "worker_video",
		Audience:    "encoder_recorder",
		ExpiresAt:   time.Now().Add(time.Hour).Unix(),
	})
	if err != nil {
		t.Fatal(err)
	}

	root := t.TempDir()
	starter := &httpFakeStarter{}
	processManager := &streamproc.Manager{ArchiveRoot: root, FFmpegBin: "ffmpeg", Starter: starter, InputResolver: testInputResolver, AllowHostnameInputs: true, OutputRelayMode: outputrelay.ModeDirect}
	handler := newV2TestServerWithManagers(t, processManager, workerevents.NewManager(root), TokenVerifier{PlainToken: "service-token", IngestTokenSigningKey: signingKey, RequireSignedIngest: true}, "stream-worker-video")

	body, err := json.Marshal(map[string]any{
		"stream_id":                 "stream-worker-video",
		"name":                      "Worker Scene Stream",
		"worker_video_ingest":       true,
		"worker_video_ingest_token": token,
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/streams/start", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer service-token")
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusAccepted {
		t.Fatalf("start status = %d body = %s", res.Code, res.Body.String())
	}
	t.Cleanup(func() {
		stopReq := httptest.NewRequest(http.MethodPost, "/streams/stream-worker-video/stop", nil)
		stopReq.SetPathValue("id", "stream-worker-video")
		stopReq.Header.Set("Authorization", "Bearer service-token")
		handler.ServeHTTP(httptest.NewRecorder(), stopReq)
	})

	var response struct {
		Status      string `json:"status"`
		VideoIngest struct {
			URL        string `json:"url"`
			Passphrase string `json:"passphrase"`
			PBKeylen   int    `json:"pbkeylen"`
		} `json:"video_ingest"`
	}
	if err := json.Unmarshal(res.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode start response: %v", err)
	}
	if response.Status != "running" || !strings.HasPrefix(response.VideoIngest.URL, "srt://127.0.0.1:") || response.VideoIngest.Passphrase == "" || response.VideoIngest.PBKeylen != 32 {
		t.Fatalf("unexpected start response: %s", res.Body.String())
	}
	if got := res.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("credential-bearing start response Cache-Control = %q, want no-store", got)
	}
	if strings.Contains(res.Body.String(), token) {
		t.Fatalf("start response leaked the signed job token: %s", res.Body.String())
	}
	joinedArgs := strings.Join(starter.args, " ")
	for _, leaked := range []string{token, response.VideoIngest.Passphrase, "passphrase"} {
		if strings.Contains(joinedArgs, leaked) {
			t.Fatalf("FFmpeg args leaked worker video credential %q: %s", leaked, joinedArgs)
		}
	}
	for _, want := range []string{"-f mjpeg", "tcp://127.0.0.1:", "discord-opus.sdp", "-map [v] -map [aout_stats]"} {
		if !strings.Contains(joinedArgs, want) {
			t.Fatalf("FFmpeg args missing %q: %s", want, joinedArgs)
		}
	}
	metadata, err := os.ReadFile(filepath.Join(root, "tmp", "stream-worker-video", "metadata.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, leaked := range []string{token, response.VideoIngest.Passphrase} {
		if strings.Contains(string(metadata), leaked) {
			t.Fatalf("metadata leaked worker video credential %q: %s", leaked, metadata)
		}
	}
	statusReq := httptest.NewRequest(http.MethodGet, "/streams/stream-worker-video/process-status", nil)
	statusReq.SetPathValue("id", "stream-worker-video")
	statusReq.Header.Set("Authorization", "Bearer service-token")
	statusRes := httptest.NewRecorder()
	handler.ServeHTTP(statusRes, statusReq)
	if statusRes.Code != http.StatusOK {
		t.Fatalf("process status = %d body = %s", statusRes.Code, statusRes.Body.String())
	}
	for _, leaked := range []string{token, response.VideoIngest.Passphrase, "video_ingest"} {
		if strings.Contains(statusRes.Body.String(), leaked) {
			t.Fatalf("process status leaked one-time Worker video material %q: %s", leaked, statusRes.Body.String())
		}
	}
}

func TestStartEndpointRejectsInvalidWorkerVideoTokenBeforeAllocatingMediaBridges(t *testing.T) {
	t.Setenv("AUTOSTREAM_ENV", "development")
	root := t.TempDir()
	starter := &httpFakeStarter{}
	processManager := &streamproc.Manager{ArchiveRoot: root, FFmpegBin: "ffmpeg", Starter: starter, InputResolver: testInputResolver, AllowHostnameInputs: true, OutputRelayMode: outputrelay.ModeDirect}
	handler := newV2TestServerWithManagers(t, processManager, workerevents.NewManager(root), TokenVerifier{PlainToken: "service-token", IngestTokenSigningKey: "expected-signing-key", RequireSignedIngest: true}, "stream-worker-video")

	req := httptest.NewRequest(http.MethodPost, "/streams/start", bytes.NewBufferString(`{"stream_id":"stream-worker-video","name":"Worker Scene Stream","worker_video_ingest":true,"worker_video_ingest_token":"not-a-signed-token"}`))
	req.Header.Set("Authorization", "Bearer service-token")
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusUnauthorized || !strings.Contains(res.Body.String(), "missing_or_invalid_worker_video_ingest_token") {
		t.Fatalf("status = %d body = %s", res.Code, res.Body.String())
	}
	if starter.process != nil {
		t.Fatalf("FFmpeg started with an invalid Worker video token: %#v", starter.args)
	}
	if _, err := os.Stat(filepath.Join(root, "tmp", "stream-worker-video", "discord-opus.sdp")); !os.IsNotExist(err) {
		t.Fatalf("audio bridge was allocated before token verification: %v", err)
	}
}

func TestStartEndpointRejectsWorkerVideoTokenWithoutExplicitOptIn(t *testing.T) {
	root := t.TempDir()
	starter := &httpFakeStarter{}
	processManager := &streamproc.Manager{ArchiveRoot: root, FFmpegBin: "ffmpeg", Starter: starter, InputResolver: testInputResolver, AllowHostnameInputs: true, OutputRelayMode: outputrelay.ModeDirect}
	handler := NewServerWithManagers("encoder_recorder", processManager, workerevents.NewManager(root), TokenVerifier{PlainToken: "service-token"})

	req := httptest.NewRequest(http.MethodPost, "/streams/start", bytes.NewBufferString(`{"stream_id":"stream-worker-video","name":"Worker Scene Stream","worker_video_ingest_token":"must-not-be-ignored"}`))
	req.Header.Set("Authorization", "Bearer service-token")
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusBadRequest || !strings.Contains(res.Body.String(), "worker_video_ingest_not_enabled") {
		t.Fatalf("status = %d body = %s", res.Code, res.Body.String())
	}
	if starter.process != nil {
		t.Fatalf("FFmpeg started with an unscoped Worker video token: %#v", starter.args)
	}
}

func TestStartEndpointRejectsDifferentStreamWhileEncoderVideoBridgeIsActive(t *testing.T) {
	t.Setenv("AUTOSTREAM_ENV", "development")
	t.Setenv("AUTOSTREAM_WORKER_VIDEO_BIND_ADDR", "127.0.0.1:0")
	t.Setenv("AUTOSTREAM_WORKER_VIDEO_ADVERTISE_HOST", "127.0.0.1")
	const signingKey = "worker-video-signing-key"
	issueToken := func(streamID string) string {
		t.Helper()
		token, err := ingesttoken.Issue(signingKey, ingesttoken.Claims{
			StreamID: streamID, ServiceID: "worker-01", ServiceType: "worker",
			Purpose: "worker_video", Audience: "encoder_recorder", ExpiresAt: time.Now().Add(time.Hour).Unix(),
		})
		if err != nil {
			t.Fatal(err)
		}
		return token
	}

	root := t.TempDir()
	processManager := &streamproc.Manager{ArchiveRoot: root, FFmpegBin: "ffmpeg", Starter: &httpFakeStarter{}, InputResolver: testInputResolver, AllowHostnameInputs: true, OutputRelayMode: outputrelay.ModeDirect}
	handler := newV2TestServerWithManagers(t, processManager, workerevents.NewManager(root), TokenVerifier{PlainToken: "service-token", IngestTokenSigningKey: signingKey, RequireSignedIngest: true}, "stream-worker-video-01", "stream-worker-video-02")
	start := func(streamID string) *httptest.ResponseRecorder {
		t.Helper()
		body, err := json.Marshal(map[string]any{
			"stream_id": streamID, "name": streamID,
			"worker_video_ingest": true, "worker_video_ingest_token": issueToken(streamID),
		})
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(http.MethodPost, "/streams/start", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer service-token")
		res := httptest.NewRecorder()
		handler.ServeHTTP(res, req)
		return res
	}

	if first := start("stream-worker-video-01"); first.Code != http.StatusAccepted {
		t.Fatalf("first start status = %d body = %s", first.Code, first.Body.String())
	}
	t.Cleanup(func() {
		stopReq := httptest.NewRequest(http.MethodPost, "/streams/stream-worker-video-01/stop", nil)
		stopReq.SetPathValue("id", "stream-worker-video-01")
		stopReq.Header.Set("Authorization", "Bearer service-token")
		handler.ServeHTTP(httptest.NewRecorder(), stopReq)
	})
	if second := start("stream-worker-video-02"); second.Code != http.StatusConflict || !strings.Contains(second.Body.String(), "stream_already_running") {
		t.Fatalf("second start status = %d body = %s", second.Code, second.Body.String())
	}
}

func TestStartEndpointRejectsClientSuppliedInternalDiscordAudioURL(t *testing.T) {
	t.Setenv("SERVICE_CONTROL_TOKEN", "service-token")
	root := t.TempDir()
	starter := &httpFakeStarter{}
	processManager := &streamproc.Manager{ArchiveRoot: root, FFmpegBin: "ffmpeg", Starter: starter, InputResolver: testInputResolver, AllowHostnameInputs: true, OutputRelayMode: outputrelay.ModeDirect}
	handler := newV2TestServerWithManagers(t, processManager, workerevents.NewManager(root), TokenVerifier{PlainToken: "service-token", DiscordAudioPlainToken: "audio-token"}, "stream-01")

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
