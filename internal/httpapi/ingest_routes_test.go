package httpapi

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/example/autostream-encoder-recorder/internal/ingesttoken"
	"github.com/example/autostream-encoder-recorder/internal/outputrelay"
	"github.com/example/autostream-encoder-recorder/internal/streamproc"
	"github.com/example/autostream-encoder-recorder/internal/workerevents"
)

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

func TestDiscordAudioStatusShowsBridgeBeforePacketsAndUpdatesAfterIngest(t *testing.T) {
	t.Setenv("SERVICE_CONTROL_TOKEN", "service-token")
	t.Setenv("ENCODER_DISCORD_AUDIO_TOKEN", "audio-token")
	t.Setenv("AUTOSTREAM_REQUIRE_SIGNED_INGEST_TOKENS", "false")
	root := t.TempDir()
	processManager := &streamproc.Manager{ArchiveRoot: root, FFmpegBin: "ffmpeg", Starter: &httpFakeStarter{}, InputResolver: testInputResolver, AllowHostnameInputs: true, OutputRelayMode: outputrelay.ModeDirect}
	handler := newV2TestServerWithManagers(t, processManager, workerevents.NewManager(root), TokenVerifier{PlainToken: "service-token", DiscordAudioPlainToken: "audio-token"}, "stream-01")

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
	processManager := &streamproc.Manager{ArchiveRoot: root, FFmpegBin: "ffmpeg", Starter: &httpFakeStarter{}, InputResolver: testInputResolver, AllowHostnameInputs: true, OutputRelayMode: outputrelay.ModeDirect}
	handler := newV2TestServerWithManagers(t, processManager, workerevents.NewManager(root), TokenVerifier{PlainToken: "service-token", DiscordAudioPlainToken: "audio-token"}, "stream-01")

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
	processManager := &streamproc.Manager{ArchiveRoot: root, FFmpegBin: "ffmpeg", Starter: &httpFakeStarter{}, InputResolver: testInputResolver, AllowHostnameInputs: true, OutputRelayMode: outputrelay.ModeDirect}
	handler := newV2TestServerWithManagers(t, processManager, workerevents.NewManager(root), TokenVerifier{
		PlainToken:             "control-token",
		DiscordAudioPlainToken: "discord-audio-token",
	}, "stream-01")

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
	processManager := &streamproc.Manager{ArchiveRoot: root, FFmpegBin: "ffmpeg", Starter: &httpFakeStarter{}, InputResolver: testInputResolver, AllowHostnameInputs: true, OutputRelayMode: outputrelay.ModeDirect}
	signingKey := "test-stream-ingest-signing-key"
	handler := newV2TestServerWithManagers(t, processManager, workerevents.NewManager(root), TokenVerifier{
		PlainToken:             "control-token",
		IngestTokenSigningKey:  signingKey,
		DiscordAudioPlainToken: "static-audio-token",
	}, "stream-01")

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

func TestWorkerEventsEndpointUsesScopedTokenWhenConfigured(t *testing.T) {
	root := t.TempDir()
	eventManager := workerevents.NewManager(root)
	processManager := &streamproc.Manager{ArchiveRoot: root, FFmpegBin: "ffmpeg", Starter: &httpFakeStarter{}, InputResolver: testInputResolver, AllowHostnameInputs: true, OutputRelayMode: outputrelay.ModeDirect}
	handler := newV2TestServerWithManagers(t, processManager, eventManager, TokenVerifier{
		PlainToken:             "control-token",
		WorkerEventsPlainToken: "worker-events-token",
	}, "stream-01")
	startReq := httptest.NewRequest(http.MethodPost, "/streams/start", bytes.NewBufferString(`{"stream_id":"stream-01","name":"Worker Event Stream","input_url":"srt://input.example.com:9000","rtmp_url":"rtmps://youtube.example.com/live2"}`))
	startReq.Header.Set("Authorization", "Bearer control-token")
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
	processManager := &streamproc.Manager{ArchiveRoot: root, FFmpegBin: "ffmpeg", Starter: &httpFakeStarter{}, InputResolver: testInputResolver, AllowHostnameInputs: true, OutputRelayMode: outputrelay.ModeDirect}
	signingKey := "test-stream-ingest-signing-key"
	handler := newV2TestServerWithManagers(t, processManager, eventManager, TokenVerifier{
		PlainToken:             "control-token",
		IngestTokenSigningKey:  signingKey,
		WorkerEventsPlainToken: "static-worker-token",
	}, "stream-01")
	startReq := httptest.NewRequest(http.MethodPost, "/streams/start", bytes.NewBufferString(`{"stream_id":"stream-01","name":"Worker Event Stream","input_url":"srt://input.example.com:9000","rtmp_url":"rtmps://youtube.example.com/live2"}`))
	startReq.Header.Set("Authorization", "Bearer control-token")
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
