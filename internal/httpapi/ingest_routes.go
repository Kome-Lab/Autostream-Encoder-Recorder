package httpapi

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/example/autostream-encoder-recorder/internal/audioingest"
	"github.com/example/autostream-encoder-recorder/internal/observability"
	"github.com/example/autostream-encoder-recorder/internal/streamproc"
	"github.com/example/autostream-encoder-recorder/internal/workerevents"
)

func discordAudioStatus(audioManager *audioingest.Manager, verifier TokenVerifier) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireServiceToken(w, r, verifier) {
			return
		}
		if audioManager == nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"code": "audio_ingest_not_configured"})
			return
		}
		writeJSON(w, http.StatusOK, audioManager.Status(r.PathValue("id"), time.Now().UTC()))
	}
}

func workerEvents(eventManager *workerevents.Manager, processManager *streamproc.Manager, verifier TokenVerifier) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var event workerevents.Event
		if status, err := decodeLimitedJSON(w, r, maxWorkerEventBodyBytes, &event); err != nil {
			writeJSON(w, status, map[string]string{"code": limitedJSONErrorCode(status)})
			return
		}
		claims, authorized := verifier.WorkerEventsClaims(r.Header.Get("Authorization"), event.StreamID)
		if !authorized {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"code": "missing_or_invalid_worker_events_token"})
			return
		}
		if claims.ServiceID != "" && claims.ServiceID != event.ServiceID {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"code": "worker_service_id_mismatch"})
			return
		}
		if !isRunningStream(processManager, event.StreamID) {
			writeJSON(w, http.StatusConflict, map[string]string{"code": "stream_not_running"})
			return
		}
		result, err := eventManager.Add(event)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"code": "worker_event_rejected"})
			return
		}
		reportWorkerEventReceived(r.Context(), event)
		writeJSON(w, http.StatusAccepted, result)
	}
}

func recentWorkerEvents(eventManager *workerevents.Manager, verifier TokenVerifier) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !verifier.Verify(r.Header.Get("Authorization")) {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"code": "missing_or_invalid_service_token"})
			return
		}
		events, err := eventManager.Recent(r.PathValue("id"))
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"code": "worker_events_unavailable"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"events": events})
	}
}

func discordOpusAudio(audioManager *audioingest.Manager, processManager *streamproc.Manager, verifier TokenVerifier) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if audioManager == nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"code": "audio_ingest_not_configured"})
			return
		}
		var req audioingest.IngestRequest
		if status, err := decodeLimitedJSON(w, r, int64(envInt("AUDIO_INGEST_MAX_BODY_BYTES", defaultDiscordAudioBodyBytes)), &req); err != nil {
			writeJSON(w, status, map[string]string{"code": limitedJSONErrorCode(status)})
			return
		}
		if req.StreamID == "" {
			req.StreamID = r.PathValue("id")
		}
		if req.StreamID != r.PathValue("id") {
			writeJSON(w, http.StatusBadRequest, map[string]string{"code": "stream_id_mismatch"})
			return
		}
		claims, authorized := verifier.DiscordAudioClaims(r.Header.Get("Authorization"), req.StreamID)
		if !authorized {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"code": "missing_or_invalid_discord_audio_token"})
			return
		}
		if claims.ServiceID != "" && claims.ServiceID != req.Source {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"code": "discord_service_id_mismatch"})
			return
		}
		if !isRunningStream(processManager, req.StreamID) {
			writeJSON(w, http.StatusConflict, map[string]string{"code": "stream_not_running"})
			return
		}
		if !audioManager.Status(req.StreamID, time.Now().UTC()).BridgeActive {
			writeJSON(w, http.StatusConflict, map[string]string{"code": "audio_bridge_not_active"})
			return
		}
		result, err := audioManager.Add(req)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"code": "audio_ingest_rejected"})
			return
		}
		reportDiscordAudioReceived(r.Context(), req.StreamID, result.AcceptedCount)
		writeJSON(w, http.StatusAccepted, result)
	}
}

func isRunningStream(processManager *streamproc.Manager, streamID string) bool {
	if processManager == nil || strings.TrimSpace(streamID) == "" {
		return false
	}
	snapshot, err := processManager.Status(streamID)
	return err == nil && snapshot.Status == "running"
}

func reportWorkerEventReceived(ctx context.Context, event workerevents.Event) {
	reporter := observability.NewClientFromEnv()
	if !reporter.Enabled() {
		return
	}
	_ = reporter.Event(ctx, event.StreamID, "worker.event.received", "accepted", map[string]any{"event_type": event.Type, "attempt": event.Attempt})
}

func reportDiscordAudioReceived(ctx context.Context, streamID string, count int) {
	reporter := observability.NewClientFromEnv()
	if !reporter.Enabled() {
		return
	}
	reportMetric(ctx, reporter, streamID, "discord.audio_receiving", 1)
	_ = reporter.Event(ctx, streamID, "discord.audio_ingest.received", "accepted", map[string]any{"packet_count": count})
}

func reportDiscordAudioHealth(processManager *streamproc.Manager, audioManager *audioingest.Manager, streamID string) {
	if processManager == nil || audioManager == nil || streamID == "" {
		return
	}
	reporter := observability.NewClientFromEnv()
	if !reporter.Enabled() {
		return
	}
	interval := time.Duration(envInt("AUDIO_INGEST_METRICS_INTERVAL_SEC", 5)) * time.Second
	timeoutSec := float64(envInt("AUDIO_INGEST_TIMEOUT_SEC", 5))
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		snapshot, err := processManager.Status(streamID)
		if err != nil || (snapshot.Status != "running" && snapshot.Status != "stopping") {
			return
		}
		stats := audioManager.Status(streamID, time.Now().UTC())
		receiving := 0.0
		timeout := stats.LastPacketAgeSec
		if stats.PacketsTotal > 0 && stats.LastPacketAgeSec < timeoutSec {
			receiving = 1
			timeout = 0
		}
		reportMetric(context.Background(), reporter, streamID, "discord.audio_receiving", receiving)
		reportMetric(context.Background(), reporter, streamID, "discord.audio_packets_total", float64(stats.PacketsTotal))
		reportMetric(context.Background(), reporter, streamID, "media.input_timeout_sec", timeout)
		<-ticker.C
	}
}
