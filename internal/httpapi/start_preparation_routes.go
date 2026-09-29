package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/example/autostream-contracts/pkg/contracts"
	"github.com/example/autostream-encoder-recorder/internal/audioingest"
	"github.com/example/autostream-encoder-recorder/internal/control"
	"github.com/example/autostream-encoder-recorder/internal/streamproc"
	"github.com/example/autostream-encoder-recorder/internal/videoingest"
)

func registerStartPreparationRoutes(mux *http.ServeMux, pm *streamproc.Manager, am *audioingest.Manager, vm *videoingest.Manager, verifier TokenVerifier, resolver RuntimeSecretResolver, config RuntimeConfigProvider) {
	mux.HandleFunc("POST /streams/start-preparations", prepareStreamStart(pm, am, vm, verifier, resolver, config))
	mux.HandleFunc("GET /streams/{id}/start-preparations/{start_id}", startPreparationAction(pm, verifier, "status"))
	mux.HandleFunc("POST /streams/{id}/start-preparations/{start_id}/commit", startPreparationAction(pm, verifier, "commit"))
	mux.HandleFunc("POST /streams/{id}/start-preparations/{start_id}/abort", startPreparationAction(pm, verifier, "abort"))
}
func preparationBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, maxControlBodyBytes)
	b, err := io.ReadAll(r.Body)
	if err != nil {
		status := limitedJSONStatus(err)
		writeJSON(w, status, map[string]string{"code": limitedJSONErrorCode(status)})
		return nil, false
	}
	return b, true
}
func preparationError(w http.ResponseWriter, err error) {
	status, code := http.StatusBadGateway, "start_preparation_runtime_failed"
	if errors.Is(err, streamproc.ErrAlreadyRunning) || errors.Is(err, streamproc.ErrPreparationConflict) {
		status, code = http.StatusConflict, "start_preparation_conflict"
	}
	if errors.Is(err, streamproc.ErrPreparationUnknown) {
		status, code = http.StatusNotFound, "start_preparation_unknown"
	}
	writeJSON(w, status, map[string]string{"code": code})
}
func prepareStreamStart(pm *streamproc.Manager, am *audioingest.Manager, vm *videoingest.Manager, verifier TokenVerifier, resolver RuntimeSecretResolver, config RuntimeConfigProvider) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if !requireServiceToken(w, r, verifier) {
			return
		}
		if pm == nil || am == nil || vm == nil || !vm.Available() {
			writeJSON(w, 503, map[string]string{"code": "start_preparation_unavailable"})
			return
		}
		raw, ok := preparationBody(w, r)
		if !ok {
			return
		}
		wire, err := contracts.DecodeEncoderStartPreparation(raw)
		if err != nil {
			writeJSON(w, 400, map[string]string{"code": "bad_request"})
			return
		}
		node := control.ConfigFromEnv()
		if node.ConfigError != "" || node.ServiceID == "" || node.ServiceID != wire.EncoderServiceID {
			preparationError(w, streamproc.ErrPreparationConflict)
			return
		}
		nested, _ := json.Marshal(wire.StartRequest)
		var request startStreamRequest
		if json.Unmarshal(nested, &request) != nil || request.validateArchiveRun() != nil {
			writeJSON(w, 400, map[string]string{"code": "bad_request"})
			return
		}
		if _, valid := verifier.WorkerVideoClaims(request.WorkerVideoIngestToken, request.StreamID); !valid {
			writeJSON(w, 401, map[string]string{"code": "missing_or_invalid_worker_video_ingest_token"})
			return
		}
		id := streamproc.StartPreparationIdentity{StreamID: request.StreamID, StartID: wire.StartID, EncoderServiceID: wire.EncoderServiceID, JobGeneration: request.VideoCoverStart.JobGeneration, ArchiveRunID: request.ArchiveRunID}
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		defer cancel()
		owned, ownedCtx, err := pm.BeginStartPreparation(ctx, id)
		if err != nil {
			preparationError(w, err)
			return
		}
		pm.SetPreparationIdentityCheck(owned, func() bool {
			current := control.ConfigFromEnv()
			return current.ConfigError == "" && current.ServiceID == node.ServiceID && current.ConfigRevision == node.ConfigRevision && current.ControlPanelURL == node.ControlPanelURL && current.ServicePublicURL == node.ServicePublicURL
		})
		finished := false
		defer func() {
			if !finished {
				pm.FinishStartPreparation(owned)
				_ = pm.FailStartPreparation(owned)
			}
		}()
		job := request.streamJob()
		if err := prepareRuntimeJob(ownedCtx, &job, request, pm, resolver, config); err != nil {
			preparationError(w, err)
			return
		}
		video, err := vm.StartBridge(job.StreamID, request.WorkerVideoIngestToken)
		if err != nil {
			preparationError(w, err)
			return
		}
		audio, err := am.StartBridge(job.StreamID)
		if err != nil {
			vm.StopOwnedBridge(video)
			preparationError(w, err)
			return
		}
		cleanup := func() { vm.StopOwnedBridge(video); am.StopOwnedBridge(audio) }
		if err := pm.SetPreparationCleanup(owned, cleanup); err != nil {
			cleanup()
			preparationError(w, err)
			return
		}
		job.InputURL = video.InputURL
		job.AudioInputURL = audio.InputURL
		job.InputMode = "worker_scene_frames_srt"
		logStartPreparationPhase(id, "bridge_allocated")
		state, err := pm.PrepareStart(ownedCtx, owned, job)
		pm.FinishStartPreparation(owned)
		finished = true
		if err != nil {
			_ = pm.FailStartPreparation(owned)
			preparationError(w, err)
			return
		}
		if state.Phase != "prepared" || state.ExpiresAt == nil {
			preparationError(w, streamproc.ErrPreparationConflict)
			return
		}
		result := contracts.EncoderStartPreparationPrepared{SchemaVersion: 2, Identity: contractPreparationIdentity(id), Phase: "prepared", ExpiresAt: *state.ExpiresAt, VideoIngest: contracts.EncoderVideoIngest{URL: video.URL, Passphrase: video.Passphrase, PBKeyLen: video.PBKeylen}}
		logStartPreparationPhase(id, "prepare_response")
		writeJSON(w, http.StatusAccepted, result)
	}
}
func startPreparationAction(pm *streamproc.Manager, verifier TokenVerifier, action string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if !requireServiceToken(w, r, verifier) {
			return
		}
		if pm == nil {
			writeJSON(w, 503, map[string]string{"code": "start_preparation_unavailable"})
			return
		}
		streamID, startID := r.PathValue("id"), r.PathValue("start_id")
		var state streamproc.StartPreparationState
		var err error
		if action == "status" {
			state, err = pm.StartPreparationStatus(streamID, startID)
		} else {
			raw, ok := preparationBody(w, r)
			if !ok {
				return
			}
			request, e := contracts.DecodeEncoderStartPreparationAction(raw, streamID, startID)
			if e != nil {
				writeJSON(w, 400, map[string]string{"code": "bad_request"})
				return
			}
			node := control.ConfigFromEnv()
			if node.ConfigError != "" || node.ServiceID != request.EncoderServiceID {
				preparationError(w, streamproc.ErrPreparationConflict)
				return
			}
			id := streamproc.StartPreparationIdentity{StreamID: streamID, StartID: startID, EncoderServiceID: request.EncoderServiceID, JobGeneration: request.JobGeneration}
			if action == "commit" {
				state, err = pm.CommitStartPreparation(r.Context(), id)
			} else {
				state, err = pm.AbortStartPreparation(r.Context(), id)
			}
		}
		if err != nil {
			preparationError(w, err)
			return
		}
		if state.Identity.EncoderServiceID != control.ConfigFromEnv().ServiceID {
			preparationError(w, streamproc.ErrPreparationConflict)
			return
		}
		writeJSON(w, http.StatusOK, publicStartPreparation(state))
	}
}
