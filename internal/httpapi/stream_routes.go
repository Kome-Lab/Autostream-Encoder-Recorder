package httpapi

import (
	"errors"
	"net/http"
	"os"
	"strings"

	"github.com/example/autostream-encoder-recorder/internal/archive"
	"github.com/example/autostream-encoder-recorder/internal/audioingest"
	"github.com/example/autostream-encoder-recorder/internal/ffmpeg"
	"github.com/example/autostream-encoder-recorder/internal/lifecycle"
	"github.com/example/autostream-encoder-recorder/internal/outputrelay"
	"github.com/example/autostream-encoder-recorder/internal/streamproc"
	"github.com/example/autostream-encoder-recorder/internal/videocover"
	"github.com/example/autostream-encoder-recorder/internal/videoingest"
)

func dryRunStream(verifier TokenVerifier, runtimeConfig RuntimeConfigProvider) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireServiceToken(w, r, verifier) {
			return
		}
		var request startStreamRequest
		if status, err := decodeLimitedStrictJSON(w, r, maxControlBodyBytes, &request); err != nil {
			writeJSON(w, status, map[string]string{"code": limitedJSONErrorCode(status)})
			return
		}
		job := request.streamJob()
		if err := request.validateArchiveRun(); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"code": "bad_request"})
			return
		}
		if err := applyEncoderRuntimeConfig(r.Context(), &job, runtimeConfig); err != nil {
			if errors.Is(err, errEncoderRuntimeProfileNotFound) {
				writeJSON(w, http.StatusConflict, map[string]string{"code": "encoder_runtime_profile_not_found"})
			} else {
				writeJSON(w, http.StatusBadGateway, map[string]string{"code": "runtime_config_fetch_failed"})
			}
			return
		}
		if err := applyYouTubeRuntimeConfig(r.Context(), &job, runtimeConfig); err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"code": "runtime_config_fetch_failed"})
			return
		}
		if err := request.applyArchiveRuntimeConfig(r.Context(), &job, runtimeConfig); err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"code": "runtime_config_fetch_failed"})
			return
		}
		if err := applyOverlayRuntimeConfig(r.Context(), &job, runtimeConfig); err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"code": "runtime_config_fetch_failed"})
			return
		}
		if strings.TrimSpace(job.YouTubeOutputMode) == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"code": "youtube_runtime_config_required", "missing": []string{"output_mode"}})
			return
		}
		relayPolicy := outputrelay.FromEnv()
		usesLocalRelay, err := relayPolicy.AuthorizeYouTubeOutput(job.YouTubeOutputMode, job.YouTubeOutputReady, job.OutputRelayBindingID)
		if err != nil {
			writeOutputRelayPolicyError(w, err, "dry_run_failed")
			return
		}
		outputTarget := ""
		if usesLocalRelay {
			outputTarget = relayOutputTargetForPreflight(relayPolicy.URL, job.StreamID)
			if err := ffmpeg.ValidateRelayOutputTarget(outputTarget); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"code": "dry_run_failed"})
				return
			}
			clearUnusedYouTubeOutputTarget(&job)
		} else {
			if strings.TrimSpace(job.StreamKey) == "" && strings.TrimSpace(job.StreamKeySecretName) != "" {
				job.StreamKey = "<RUNTIME_STREAM_KEY>"
			}
			if missing := missingYouTubeRuntimeFields(job); len(missing) > 0 {
				writeJSON(w, http.StatusBadRequest, map[string]any{"code": "youtube_runtime_config_required", "missing": missing})
				return
			}
			outputTarget = strings.TrimRight(job.RTMPURL, "/") + "/" + strings.TrimLeft(job.StreamKey, "/")
		}
		archiveRoot := os.Getenv("AUTOSTREAM_ARCHIVE_DIR")
		if archiveRoot == "" {
			archiveRoot = "/var/lib/autostream/archives"
		}
		ffmpegBin := os.Getenv("FFMPEG_BIN")
		if ffmpegBin == "" {
			ffmpegBin = "ffmpeg"
		}
		manager := lifecycle.Manager{
			ArchiveRoot: archiveRoot,
			FFmpegBin:   ffmpegBin,
			Runner:      &ffmpeg.DryRunRunner{},
			Uploader:    archive.DryRunUploader{},
		}
		result, err := manager.DryRunToOutputTarget(r.Context(), job, outputTarget)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"code": "dry_run_failed"})
			return
		}
		writeJSON(w, http.StatusAccepted, result.Metadata)
	}
}

type startStreamResponse struct {
	processSnapshotResponse
	VideoIngest *videoingest.Bridge `json:"video_ingest,omitempty"`
}

func startStream(processManager *streamproc.Manager, audioManager *audioingest.Manager, videoManager *videoingest.Manager, verifier TokenVerifier, resolver RuntimeSecretResolver, runtimeConfig RuntimeConfigProvider) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireServiceToken(w, r, verifier) {
			return
		}
		if processManager == nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"code": "process_manager_not_configured"})
			return
		}
		var startRequest startStreamRequest
		if status, err := decodeLimitedStrictJSON(w, r, maxControlBodyBytes, &startRequest); err != nil {
			writeJSON(w, status, map[string]string{"code": limitedJSONErrorCode(status)})
			return
		}
		job := startRequest.streamJob()
		if err := startRequest.validateArchiveRun(); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"code": "bad_request"})
			return
		}
		if !startRequest.WorkerVideoIngest && strings.TrimSpace(startRequest.WorkerVideoIngestToken) != "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"code": "worker_video_ingest_not_enabled"})
			return
		}
		if err := applyEncoderRuntimeConfig(r.Context(), &job, runtimeConfig); err != nil {
			if errors.Is(err, errEncoderRuntimeProfileNotFound) {
				writeJSON(w, http.StatusConflict, map[string]string{"code": "encoder_runtime_profile_not_found"})
			} else {
				writeJSON(w, http.StatusBadGateway, map[string]string{"code": "runtime_config_fetch_failed"})
			}
			return
		}
		if err := applyYouTubeRuntimeConfig(r.Context(), &job, runtimeConfig); err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"code": "runtime_config_fetch_failed"})
			return
		}
		if err := startRequest.applyArchiveRuntimeConfig(r.Context(), &job, runtimeConfig); err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"code": "runtime_config_fetch_failed"})
			return
		}
		if err := applyOverlayRuntimeConfig(r.Context(), &job, runtimeConfig); err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"code": "runtime_config_fetch_failed"})
			return
		}
		if err := resolveArchiveRuntimeSecrets(r.Context(), &job, resolver); err != nil {
			writeRuntimeSecretResolveError(w, err)
			return
		}
		if strings.TrimSpace(job.YouTubeOutputMode) == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"code": "youtube_runtime_config_required", "missing": []string{"output_mode"}})
			return
		}
		usesLocalRelay, err := processManager.AuthorizeOutputRelay(job)
		if err != nil {
			writeOutputRelayPolicyError(w, err, "start_stream_failed")
			return
		}
		if usesLocalRelay {
			clearUnusedYouTubeOutputTarget(&job)
		} else {
			if err := resolveYouTubeRuntimeSecrets(r.Context(), &job, resolver); err != nil {
				writeRuntimeSecretResolveError(w, err)
				return
			}
			if missing := missingYouTubeRuntimeFields(job); len(missing) > 0 {
				writeJSON(w, http.StatusBadRequest, map[string]any{"code": "youtube_runtime_config_required", "missing": missing})
				return
			}
		}
		audioBridgeMode := false
		videoBridgeMode := false
		var videoBridge videoingest.Bridge
		if startRequest.WorkerVideoIngest {
			if strings.TrimSpace(job.InputURL) != "" || strings.TrimSpace(job.InputMode) != "" {
				writeJSON(w, http.StatusBadRequest, map[string]string{"code": "worker_video_input_must_be_service_managed"})
				return
			}
			if _, ok := verifier.WorkerVideoClaims(startRequest.WorkerVideoIngestToken, job.StreamID); !ok {
				writeJSON(w, http.StatusUnauthorized, map[string]string{"code": "missing_or_invalid_worker_video_ingest_token"})
				return
			}
			if audioManager == nil || videoManager == nil {
				writeJSON(w, http.StatusServiceUnavailable, map[string]string{"code": "worker_video_ingest_unavailable"})
				return
			}
			if existing, statusErr := processManager.Status(job.StreamID); statusErr == nil && (existing.Status == "starting" || existing.Status == "running" || existing.Status == "stopping" || existing.Status == "packaging") {
				writeJSON(w, http.StatusConflict, map[string]string{"code": "stream_already_running"})
				return
			}
			videoBridge, err = videoManager.StartBridge(job.StreamID, startRequest.WorkerVideoIngestToken)
			if err != nil {
				if errors.Is(err, videoingest.ErrAlreadyRunning) {
					writeJSON(w, http.StatusConflict, map[string]string{"code": "stream_already_running"})
					return
				}
				writeJSON(w, http.StatusServiceUnavailable, map[string]string{"code": "worker_video_ingest_unavailable"})
				return
			}
			videoBridgeMode = true
			audioBridge, err := audioManager.StartBridge(job.StreamID)
			if err != nil {
				videoManager.StopBridge(job.StreamID)
				writeJSON(w, http.StatusBadRequest, map[string]string{"code": "audio_bridge_failed"})
				return
			}
			audioBridgeMode = true
			job.InputURL = videoBridge.InputURL
			job.AudioInputURL = audioBridge.InputURL
			job.InputMode = "worker_scene_frames_srt"
		} else if strings.TrimSpace(job.InputURL) == "" {
			if audioManager == nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"code": "input_url_required"})
				return
			}
			bridge, err := audioManager.StartBridge(job.StreamID)
			if err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"code": "audio_bridge_failed"})
				return
			}
			job.InputURL = bridge.InputURL
			job.InputMode = "discord_opus_rtp"
			audioBridgeMode = true
		} else if strings.HasPrefix(strings.TrimSpace(job.InputURL), "internal_discord_audio:") || strings.HasPrefix(strings.TrimSpace(job.InputURL), "internal_worker_video:") {
			writeJSON(w, http.StatusBadRequest, map[string]string{"code": "internal_audio_input_not_allowed"})
			return
		}
		snapshot, err := processManager.Start(job)
		if errors.Is(err, streamproc.ErrAlreadyRunning) {
			if audioBridgeMode && audioManager != nil {
				audioManager.StopBridge(job.StreamID)
			}
			if videoBridgeMode && videoManager != nil {
				videoManager.StopBridge(job.StreamID)
			}
			writeJSON(w, http.StatusConflict, map[string]string{"code": "stream_already_running"})
			return
		}
		if err != nil {
			if audioBridgeMode && audioManager != nil {
				audioManager.StopBridge(job.StreamID)
			}
			if videoBridgeMode && videoManager != nil {
				videoManager.StopBridge(job.StreamID)
			}
			if code := videocover.ErrorCodeOf(err); code != "" {
				status := http.StatusBadRequest
				if code == videocover.ErrorCapabilityRequired {
					status = http.StatusConflict
				} else if code != videocover.ErrorInvalidRequest {
					status = http.StatusBadGateway
				}
				writeJSON(w, status, map[string]videocover.ErrorCode{"code": code})
				return
			}
			writeJSON(w, http.StatusBadRequest, map[string]string{"code": "start_stream_failed"})
			return
		}
		if audioBridgeMode {
			go reportDiscordAudioHealth(processManager, audioManager, job.StreamID)
		}
		response := startStreamResponse{processSnapshotResponse: publicProcessSnapshot(snapshot)}
		if videoBridgeMode {
			response.VideoIngest = &videoBridge
			w.Header().Set("Cache-Control", "no-store")
		}
		writeJSON(w, http.StatusAccepted, response)
	}
}

func stopStream(processManager *streamproc.Manager, audioManager *audioingest.Manager, videoManager *videoingest.Manager, verifier TokenVerifier) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireServiceToken(w, r, verifier) {
			return
		}
		if processManager == nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"code": "process_manager_not_configured"})
			return
		}
		streamID := strings.TrimSpace(r.PathValue("id"))
		if videoManager != nil {
			if current, statusErr := processManager.Status(streamID); statusErr == nil && (current.Status == "running" || current.Status == "stopping") {
				videoManager.MarkStopRequested(streamID)
			}
		}
		snapshot, err := processManager.Stop(streamID)
		if errors.Is(err, streamproc.ErrAlreadyStopped) {
			if audioManager != nil {
				audioManager.StopBridge(streamID)
			}
			if videoManager != nil {
				videoManager.StopBridge(streamID)
			}
			writeJSON(w, http.StatusAccepted, map[string]string{"status": "already_stopped"})
			return
		}
		if errors.Is(err, streamproc.ErrNotRunning) {
			if currentStreamID := processManager.CurrentStreamID(); currentStreamID != "" && currentStreamID != streamID {
				writeJSON(w, http.StatusConflict, map[string]string{"code": "stream_already_running"})
				return
			}
			writeJSON(w, http.StatusNotFound, map[string]string{"code": "stream_not_running"})
			return
		}
		if errors.Is(err, streamproc.ErrStarting) {
			writeJSON(w, http.StatusConflict, map[string]string{"code": "stream_starting"})
			return
		}
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"code": "stop_stream_failed"})
			return
		}
		if audioManager != nil {
			audioManager.StopBridge(streamID)
		}
		if videoManager != nil {
			videoManager.StopBridge(streamID)
		}
		writeJSON(w, http.StatusAccepted, publicProcessSnapshot(snapshot))
	}
}

func streamProcessStatus(processManager *streamproc.Manager, verifier TokenVerifier) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireServiceToken(w, r, verifier) {
			return
		}
		if processManager == nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"code": "process_manager_not_configured"})
			return
		}
		snapshot, err := processManager.Status(r.PathValue("id"))
		if errors.Is(err, streamproc.ErrNotRunning) {
			writeJSON(w, http.StatusNotFound, map[string]string{"code": "stream_not_running"})
			return
		}
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"code": "get_process_status_failed"})
			return
		}
		writeJSON(w, http.StatusOK, publicProcessSnapshot(snapshot))
	}
}

type processSnapshotResponse struct {
	StreamID           string            `json:"stream_id"`
	Name               string            `json:"name"`
	Status             string            `json:"status"`
	StartedAtJST       string            `json:"started_at_jst"`
	StoppedAtJST       string            `json:"stopped_at_jst,omitempty"`
	Archive            map[string]string `json:"archive"`
	Error              string            `json:"error,omitempty"`
	EncoderAudioGainDB float64           `json:"encoder_audio_gain_db"`
	OverlayProfileID   string            `json:"overlay_profile_id,omitempty"`
}

func publicProcessSnapshot(snapshot streamproc.Snapshot) processSnapshotResponse {
	return processSnapshotResponse{
		StreamID:           snapshot.StreamID,
		Name:               snapshot.Name,
		Status:             snapshot.Status,
		StartedAtJST:       snapshot.StartedAtJST,
		StoppedAtJST:       snapshot.StoppedAtJST,
		Archive:            snapshot.Archive,
		Error:              snapshot.Error,
		EncoderAudioGainDB: snapshot.EncoderAudioGainDB,
		OverlayProfileID:   snapshot.OverlayProfileID,
	}
}
