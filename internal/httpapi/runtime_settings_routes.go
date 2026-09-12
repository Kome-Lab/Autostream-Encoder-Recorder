package httpapi

import (
	"errors"
	"net/http"
	"strings"

	"github.com/example/autostream-encoder-recorder/internal/lifecycle"
	"github.com/example/autostream-encoder-recorder/internal/streamproc"
	"github.com/example/autostream-encoder-recorder/internal/videocover"
)

type runtimeSettingsRequest struct {
	EncoderAudioGainDB float64 `json:"encoder_audio_gain_db"`
	OverlayProfileID   string  `json:"overlay_profile_id"`
}

func updateStreamRuntimeSettings(processManager *streamproc.Manager, verifier TokenVerifier, runtimeConfig RuntimeConfigProvider) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireServiceToken(w, r, verifier) {
			return
		}
		if processManager == nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"code": "process_manager_not_configured"})
			return
		}
		var body runtimeSettingsRequest
		if status, err := decodeLimitedJSON(w, r, maxControlBodyBytes, &body); err != nil {
			writeJSON(w, status, map[string]string{"code": limitedJSONErrorCode(status)})
			return
		}
		job := lifecycle.StreamJob{StreamID: r.PathValue("id"), OverlayProfileID: strings.TrimSpace(body.OverlayProfileID)}
		if err := applyOverlayRuntimeConfig(r.Context(), &job, runtimeConfig); err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"code": "runtime_config_fetch_failed"})
			return
		}
		snapshot, err := processManager.UpdateRuntimeSettings(job.StreamID, streamproc.RuntimeSettings{
			EncoderAudioGainDB: body.EncoderAudioGainDB,
			OverlayProfileID:   job.OverlayProfileID,
			OverlayConfig:      job.OverlayConfig,
		})
		switch {
		case errors.Is(err, streamproc.ErrInvalidRuntimeSettings):
			writeJSON(w, http.StatusBadRequest, map[string]string{"code": "invalid_encoder_runtime_settings"})
		case errors.Is(err, streamproc.ErrNotRunning):
			writeJSON(w, http.StatusConflict, map[string]string{"code": "stream_not_running"})
		case err != nil:
			writeJSON(w, http.StatusBadGateway, map[string]string{"code": "encoder_runtime_settings_apply_failed"})
		default:
			writeJSON(w, http.StatusOK, publicProcessSnapshot(snapshot))
		}
	}
}

func getVideoCoverState(processManager *streamproc.Manager, verifier TokenVerifier) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if !requireServiceToken(w, r, verifier) {
			return
		}
		if processManager == nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"code": "process_manager_not_configured"})
			return
		}
		state, err := processManager.VideoCoverState(r.PathValue("id"))
		switch {
		case errors.Is(err, streamproc.ErrNotRunning):
			writeJSON(w, http.StatusNotFound, map[string]string{"code": "stream_not_running"})
		case videocover.ErrorCodeOf(err) == videocover.ErrorCapabilityRequired:
			writeJSON(w, http.StatusNotFound, map[string]videocover.ErrorCode{"code": videocover.ErrorCapabilityRequired})
		case videocover.ErrorCodeOf(err) != "":
			writeJSON(w, http.StatusInternalServerError, map[string]videocover.ErrorCode{"code": videocover.ErrorCodeOf(err)})
		case err != nil:
			writeJSON(w, http.StatusInternalServerError, map[string]string{"code": "video_cover_state_unavailable"})
		default:
			writeJSON(w, http.StatusOK, state)
		}
	}
}

func putVideoCoverState(processManager *streamproc.Manager, verifier TokenVerifier) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if !requireServiceToken(w, r, verifier) {
			return
		}
		if processManager == nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"code": "process_manager_not_configured"})
			return
		}
		var request videocover.ApplyRequest
		if status, err := decodeLimitedStrictJSON(w, r, maxControlBodyBytes, &request); err != nil {
			writeJSON(w, status, map[string]string{"code": limitedJSONErrorCode(status)})
			return
		}
		response, err := processManager.ApplyVideoCover(r.Context(), r.PathValue("id"), request)
		if err != nil {
			code := videocover.ErrorCodeOf(err)
			switch {
			case errors.Is(err, streamproc.ErrNotRunning):
				if rejected, ok := processManager.VideoCoverRejection(r.PathValue("id"), request, videocover.ErrorCoverGraphUnavailable); ok {
					writeJSON(w, coverRejectionStatus(rejected.Error), rejected)
					return
				}
				writeJSON(w, http.StatusNotFound, map[string]string{"code": "stream_not_running"})
			case code == videocover.ErrorInvalidRequest:
				writeJSON(w, http.StatusBadRequest, map[string]videocover.ErrorCode{"code": code})
			case code == videocover.ErrorCapabilityRequired:
				if rejected, ok := processManager.VideoCoverRejection(r.PathValue("id"), request, code); ok {
					writeJSON(w, coverRejectionStatus(rejected.Error), rejected)
					return
				}
				// A legacy start has no negotiated job generation, so it cannot
				// truthfully produce the versioned ApplyResponse contract.
				writeJSON(w, http.StatusNotFound, map[string]videocover.ErrorCode{"code": code})
			case code != "":
				writeJSON(w, http.StatusConflict, map[string]videocover.ErrorCode{"code": code})
			default:
				writeJSON(w, http.StatusBadGateway, map[string]string{"code": "video_cover_apply_failed"})
			}
			return
		}
		status := http.StatusOK
		if response.Outcome == videocover.OutcomeAmbiguous {
			status = http.StatusAccepted
		} else if response.Outcome == videocover.OutcomeRejected {
			status = coverRejectionStatus(response.Error)
		}
		writeJSON(w, status, response)
	}
}

func coverRejectionStatus(safeError *videocover.SafeError) int {
	if safeError == nil {
		return http.StatusConflict
	}
	switch safeError.Code {
	case videocover.ErrorMediaAssetUnauthorized, videocover.ErrorMediaAssetNotFound,
		videocover.ErrorMediaAssetHashMismatch, videocover.ErrorMediaAssetDimensionMismatch,
		videocover.ErrorMediaAssetTimeout, videocover.ErrorMediaAssetFormatUnsupported,
		videocover.ErrorMediaAssetTooLarge, videocover.ErrorMediaAssetDecodeFailed,
		videocover.ErrorMediaAssetAspectRatioInvalid, videocover.ErrorMediaAssetVariantProcessing,
		videocover.ErrorMediaAssetVariantFailed,
		videocover.ErrorCoverGraphUnavailable, videocover.ErrorCapabilityRequired:
		return http.StatusBadGateway
	default:
		return http.StatusConflict
	}
}
