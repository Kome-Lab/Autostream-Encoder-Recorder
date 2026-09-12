package streamproc

import (
	"crypto/sha256"
	"encoding/json"
	"strings"

	"github.com/example/autostream-encoder-recorder/internal/videocover"
)

type coverGeneration struct {
	JobGeneration uint64
	Generation    uint64
}

type coverReplay struct {
	fingerprint [32]byte
	response    videocover.ApplyResponse
}

const maxCoverReplayEntries = 256

func initialCoverRuntimeState(streamID string, generation uint64, snapshot *videocover.StartSnapshot, watermark videocover.LayerState) videocover.RuntimeState {
	state := videocover.RuntimeState{
		StreamID: streamID, Generation: generation, Capability: videocover.Capability,
		Readiness: videocover.ReadinessNotReady, Applied: videocover.AppliedState{State: "unknown"},
		Cover: videocover.LayerState{Revision: 1}, Watermark: watermark,
		Pipeline: videocover.FixedPipelineInvariant(), NoAutomaticResend: true,
		Error: &videocover.SafeError{Code: videocover.ErrorCapabilityRequired},
	}
	if snapshot == nil {
		return state
	}
	state.JobGeneration = snapshot.JobGeneration
	state.Desired = videocover.DesiredState{Active: snapshot.Active, Revision: snapshot.Revision, Source: "none"}
	if snapshot.Active && snapshot.CoverAsset != nil {
		state.Desired.Source = "upload"
		state.Desired.VariantID = snapshot.CoverAsset.VariantID
	}
	state.Error = &videocover.SafeError{Code: videocover.ErrorCoverGraphUnavailable}
	return state
}

func markCoverApplied(state *videocover.RuntimeState, revision uint64, active bool, asset *videocover.MediaAssetDescriptor, watermark videocover.LayerState) {
	if state == nil {
		return
	}
	knownActive := active
	variantID := ""
	state.CoverAsset = nil
	if active && asset != nil {
		copyAsset := *asset
		state.CoverAsset = &copyAsset
		variantID = asset.VariantID
	}
	state.Desired = videocover.DesiredState{Active: active, Revision: revision, Source: "none", VariantID: variantID}
	if active {
		state.Desired.Source = "upload"
	}
	state.Applied = videocover.AppliedState{State: "known", Active: &knownActive, Revision: revision, VariantID: variantID}
	state.Cover = videocover.LayerState{Enabled: active, Revision: revision, VariantID: variantID}
	state.Watermark = watermark
	state.Readiness = videocover.ReadinessReady
	state.Error = nil
	state.LastGoodApplied = nil
	state.AppliedWitness = &videocover.AppliedWitness{
		GraphApplied: true, Generation: state.Generation, Revision: revision, Active: active,
		Cover: state.Cover, Watermark: watermark, Pipeline: videocover.FixedPipelineInvariant(),
	}
}

func desiredFromRequest(request videocover.ApplyRequest) videocover.DesiredState {
	desired := videocover.DesiredState{Active: request.Active, Revision: request.Revision, Source: "none"}
	if request.Active && request.CoverAsset != nil {
		desired.Source = "upload"
		desired.VariantID = request.CoverAsset.VariantID
	}
	return desired
}

func rejectedCoverResponse(tracked *trackedProcess, request videocover.ApplyRequest, code videocover.ErrorCode) videocover.ApplyResponse {
	return rejectedCoverResponseFromState(tracked.coverStateSnapshot(), request, code)
}

func rejectedCoverResponseFromState(state videocover.RuntimeState, request videocover.ApplyRequest, code videocover.ErrorCode) videocover.ApplyResponse {
	safeError := &videocover.SafeError{Code: code}
	if isCoverGraphOrAssetError(code) {
		// Rejection does not mutate the authoritative graph state. The response
		// still reports this operation as not-ready with the exact safe error,
		// as required by the cross-repository response contract.
		state.Readiness = videocover.ReadinessNotReady
		state.Error = safeError
	}
	return videocover.ApplyResponse{
		StreamID: state.StreamID, JobGeneration: state.JobGeneration, RequestedRevision: request.Revision,
		ActualGeneration: state.Generation, Rejected: true, Outcome: videocover.OutcomeRejected,
		Actual: state, Error: safeError,
	}
}

// VideoCoverRejection returns a contract response only when the manager still
// has an authoritative negotiated runtime snapshot. It deliberately refuses to
// derive job/generation state from the rejected request.
func (m *Manager) VideoCoverRejection(streamID string, request videocover.ApplyRequest, code videocover.ErrorCode) (videocover.ApplyResponse, bool) {
	m.mu.Lock()
	tracked, ok := m.processes[streamID]
	if !ok {
		m.mu.Unlock()
		return videocover.ApplyResponse{}, false
	}
	tracked.coverMu.Lock()
	m.mu.Unlock()
	defer tracked.coverMu.Unlock()
	state, ok := tracked.videoCoverRejectionStateLocked()
	if !ok {
		return videocover.ApplyResponse{}, false
	}
	return rejectedCoverResponseFromState(state, request, code), true
}

func isCoverGraphOrAssetError(code videocover.ErrorCode) bool {
	switch code {
	case videocover.ErrorMediaAssetUnauthorized,
		videocover.ErrorMediaAssetNotFound,
		videocover.ErrorMediaAssetHashMismatch,
		videocover.ErrorMediaAssetDimensionMismatch,
		videocover.ErrorMediaAssetTimeout,
		videocover.ErrorMediaAssetFormatUnsupported,
		videocover.ErrorMediaAssetTooLarge,
		videocover.ErrorMediaAssetDecodeFailed,
		videocover.ErrorMediaAssetAspectRatioInvalid,
		videocover.ErrorMediaAssetVariantProcessing,
		videocover.ErrorMediaAssetVariantFailed,
		videocover.ErrorCoverGraphUnavailable,
		videocover.ErrorCapabilityRequired:
		return true
	default:
		return false
	}
}

func coverRequestFingerprint(request videocover.ApplyRequest) [32]byte {
	body, _ := json.Marshal(request)
	return sha256.Sum256(body)
}

func (tracked *trackedProcess) storeCoverReplay(key string, replay coverReplay) {
	if tracked.coverReplay == nil {
		tracked.coverReplay = map[string]coverReplay{}
	}
	if _, exists := tracked.coverReplay[key]; !exists {
		tracked.coverReplayOrder = append(tracked.coverReplayOrder, key)
	}
	tracked.coverReplay[key] = replay
	for len(tracked.coverReplayOrder) > maxCoverReplayEntries {
		oldest := tracked.coverReplayOrder[0]
		tracked.coverReplayOrder = tracked.coverReplayOrder[1:]
		delete(tracked.coverReplay, oldest)
	}
}

func initialWatermarkState(profileID string, config map[string]any) videocover.LayerState {
	state := videocover.LayerState{Enabled: watermarkEnabled(config), Revision: 1}
	if state.Enabled {
		state.VariantID = strings.TrimSpace(profileID)
	}
	return state
}

func watermarkEnabled(config map[string]any) bool {
	enabled, ok := config["watermark_enabled"].(bool)
	return ok && enabled
}

func (tracked *trackedProcess) currentWatermarkState() videocover.LayerState {
	tracked.watermarkMu.Lock()
	defer tracked.watermarkMu.Unlock()
	return tracked.watermarkState
}

// markWatermarkWitnessUnknownLocked requires coverMu. The previous known Cover
// remains as last-good state, but the combined graph witness is no longer safe
// after a Watermark delivery whose downstream effect could not be observed.
func (tracked *trackedProcess) markWatermarkWitnessUnknownLocked() {
	if tracked.coverState.JobGeneration == 0 {
		return
	}
	if tracked.coverState.Applied.State == "known" {
		lastGood := tracked.coverState.Applied
		tracked.coverState.LastGoodApplied = &lastGood
	}
	tracked.coverState.Readiness = videocover.ReadinessUnknown
	tracked.coverState.Applied = videocover.AppliedState{State: "unknown"}
	tracked.coverState.AppliedWitness = nil
	tracked.coverState.Error = &videocover.SafeError{Code: videocover.ErrorCoverGraphUnavailable}
	tracked.coverState.Watermark = tracked.currentWatermarkState()
}

// coverStateSnapshot binds the independently current Watermark observation to
// both runtime state and its graph witness without changing any Cover-owned
// revision or desired/applied field. Exact idempotency replays deliberately use
// their stored historical response instead.
func (tracked *trackedProcess) coverStateSnapshot() videocover.RuntimeState {
	state := tracked.coverState
	watermark := tracked.currentWatermarkState()
	state.Watermark = watermark
	if state.AppliedWitness != nil {
		witness := *state.AppliedWitness
		witness.Watermark = watermark
		state.AppliedWitness = &witness
	}
	return state
}

func (tracked *trackedProcess) videoCoverRejectionStateLocked() (videocover.RuntimeState, bool) {
	state := tracked.coverStateSnapshot()
	if state.StreamID != "" && state.JobGeneration > 0 && state.Generation > 0 {
		return state, true
	}
	if tracked.terminalCoverState == nil {
		return videocover.RuntimeState{}, false
	}
	state = *tracked.terminalCoverState
	return state, state.StreamID != "" && state.JobGeneration > 0 && state.Generation > 0
}

func terminalVideoCoverState(state videocover.RuntimeState) videocover.RuntimeState {
	if state.Applied.State == "known" {
		lastGood := state.Applied
		state.LastGoodApplied = &lastGood
	}
	state.Readiness = videocover.ReadinessNotReady
	state.Applied = videocover.AppliedState{State: "unknown"}
	state.AppliedWitness = nil
	state.Error = &videocover.SafeError{Code: videocover.ErrorCoverGraphUnavailable}
	return state
}
