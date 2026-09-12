package streamproc

import (
	"context"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/example/autostream-encoder-recorder/internal/videocover"
)

// VideoCoverState returns only a negotiated Cover runtime. Legacy starts that
// omitted video_cover_start remain transparently inactive and cannot be
// mutated through the capability path.
func (m *Manager) VideoCoverState(streamID string) (videocover.RuntimeState, error) {
	m.mu.Lock()
	tracked, ok := m.processes[streamID]
	if !ok || tracked.snapshot.Status != "running" || tracked.cover == nil {
		m.mu.Unlock()
		return videocover.RuntimeState{}, ErrNotRunning
	}
	m.mu.Unlock()

	tracked.coverMu.Lock()
	defer tracked.coverMu.Unlock()
	if tracked.coverState.JobGeneration == 0 {
		return videocover.RuntimeState{}, videocover.NewError(videocover.ErrorCapabilityRequired)
	}
	return tracked.coverStateSnapshot(), nil
}

// ApplyVideoCover performs a single fenced mutation. Asset failures are
// rejected before the feed changes. Once feed delivery begins, any missing
// graph witness is ambiguous and is never retried automatically; exact replay
// returns the stored response without another fetch or feed write.
func (m *Manager) ApplyVideoCover(ctx context.Context, streamID string, request videocover.ApplyRequest) (videocover.ApplyResponse, error) {
	if err := validateCoverApply(streamID, request); err != nil {
		return videocover.ApplyResponse{}, err
	}
	m.mu.Lock()
	tracked, ok := m.processes[streamID]
	if !ok {
		m.mu.Unlock()
		return videocover.ApplyResponse{}, ErrNotRunning
	}
	tracked.coverMu.Lock()
	running := tracked.snapshot.Status == "running" && tracked.cover != nil
	m.mu.Unlock()
	defer tracked.coverMu.Unlock()
	if !running {
		if state, exists := tracked.videoCoverRejectionStateLocked(); exists {
			switch {
			case request.JobGeneration != state.JobGeneration:
				return rejectedCoverResponseFromState(state, request, videocover.ErrorStaleJobGeneration), nil
			case request.ExpectedGeneration != state.Generation:
				return rejectedCoverResponseFromState(state, request, videocover.ErrorStaleCoverGeneration), nil
			case request.Revision < state.Desired.Revision:
				return rejectedCoverResponseFromState(state, request, videocover.ErrorStaleCoverRevision), nil
			case request.Revision == state.Desired.Revision:
				return rejectedCoverResponseFromState(state, request, videocover.ErrorRevisionPayloadConflict), nil
			}
			return rejectedCoverResponseFromState(terminalVideoCoverState(state), request, videocover.ErrorCoverGraphUnavailable), nil
		}
		return videocover.ApplyResponse{}, ErrNotRunning
	}
	if tracked.coverState.JobGeneration == 0 {
		return videocover.ApplyResponse{}, videocover.NewError(videocover.ErrorCapabilityRequired)
	}
	fingerprint := coverRequestFingerprint(request)
	if replay, exists := tracked.coverReplay[request.IdempotencyKey]; exists {
		if replay.fingerprint != fingerprint {
			return rejectedCoverResponse(tracked, request, videocover.ErrorIdempotencyConflict), nil
		}
		return replay.response, nil
	}
	if request.JobGeneration != tracked.coverState.JobGeneration {
		response := rejectedCoverResponse(tracked, request, videocover.ErrorStaleJobGeneration)
		tracked.storeCoverReplay(request.IdempotencyKey, coverReplay{fingerprint: fingerprint, response: response})
		return response, nil
	}
	if request.ExpectedGeneration != tracked.coverState.Generation {
		response := rejectedCoverResponse(tracked, request, videocover.ErrorStaleCoverGeneration)
		tracked.storeCoverReplay(request.IdempotencyKey, coverReplay{fingerprint: fingerprint, response: response})
		return response, nil
	}
	if request.Revision < tracked.coverState.Desired.Revision {
		response := rejectedCoverResponse(tracked, request, videocover.ErrorStaleCoverRevision)
		tracked.storeCoverReplay(request.IdempotencyKey, coverReplay{fingerprint: fingerprint, response: response})
		return response, nil
	}
	if request.Revision == tracked.coverState.Desired.Revision {
		response := rejectedCoverResponse(tracked, request, videocover.ErrorRevisionPayloadConflict)
		tracked.storeCoverReplay(request.IdempotencyKey, coverReplay{fingerprint: fingerprint, response: response})
		return response, nil
	}

	frame := tracked.transparentCover
	if request.Active {
		if m.CoverAssets == nil {
			response := rejectedCoverResponse(tracked, request, videocover.ErrorCapabilityRequired)
			tracked.storeCoverReplay(request.IdempotencyKey, coverReplay{fingerprint: fingerprint, response: response})
			return response, nil
		}
		fetchCtx, cancelFetch := context.WithTimeout(ctx, m.coverFetchTimeout())
		loaded, err := m.CoverAssets.Load(fetchCtx, streamID, *request.CoverAsset, tracked.job.EncoderProfile.Width, tracked.job.EncoderProfile.Height)
		cancelFetch()
		if err != nil {
			code := videocover.ErrorCodeOf(err)
			if code == videocover.ErrorInvalidRequest {
				return videocover.ApplyResponse{}, err
			}
			if code == "" {
				code = videocover.ErrorMediaAssetTimeout
			}
			response := rejectedCoverResponse(tracked, request, code)
			tracked.storeCoverReplay(request.IdempotencyKey, coverReplay{fingerprint: fingerprint, response: response})
			return response, nil
		}
		frame = loaded
	}

	lastGood := tracked.coverState.Applied
	if tracked.coverState.LastGoodApplied != nil {
		lastGood = *tracked.coverState.LastGoodApplied
	}
	tracked.coverState.Desired = desiredFromRequest(request)
	tracked.coverState.CoverAsset = nil
	if request.Active && request.CoverAsset != nil {
		asset := *request.CoverAsset
		tracked.coverState.CoverAsset = &asset
	}
	witnessCtx, cancelWitness := context.WithTimeout(ctx, m.coverApplyTimeout())
	witnessErr := m.coverGraphWitness().Apply(witnessCtx, tracked.cover, frame, false, tracked.progressPath)
	cancelWitness()
	watermark := tracked.currentWatermarkState()
	if witnessErr != nil {
		tracked.coverState.Readiness = videocover.ReadinessUnknown
		tracked.coverState.Applied = videocover.AppliedState{State: "unknown"}
		tracked.coverState.AppliedWitness = nil
		tracked.coverState.LastGoodApplied = &lastGood
		tracked.coverState.Error = &videocover.SafeError{Code: videocover.ErrorCoverApplyAmbiguous}
		tracked.coverState.Watermark = watermark
		response := videocover.ApplyResponse{
			StreamID: streamID, JobGeneration: request.JobGeneration, RequestedRevision: request.Revision,
			ActualGeneration: tracked.coverState.Generation, Accepted: true, Outcome: videocover.OutcomeAmbiguous,
			Actual: tracked.coverState, Error: &videocover.SafeError{Code: videocover.ErrorCoverApplyAmbiguous},
		}
		tracked.storeCoverReplay(request.IdempotencyKey, coverReplay{fingerprint: fingerprint, response: response})
		return response, nil
	}
	markCoverApplied(&tracked.coverState, request.Revision, request.Active, request.CoverAsset, watermark)
	response := videocover.ApplyResponse{
		StreamID: streamID, JobGeneration: request.JobGeneration, RequestedRevision: request.Revision,
		ActualGeneration: tracked.coverState.Generation, Accepted: true, Applied: true,
		Outcome: videocover.OutcomeApplied, Actual: tracked.coverState,
	}
	tracked.storeCoverReplay(request.IdempotencyKey, coverReplay{fingerprint: fingerprint, response: response})
	return response, nil
}

func validateCoverStart(snapshot videocover.StartSnapshot) error {
	if snapshot.JobGeneration < 1 || snapshot.Revision < 1 || !validIdempotencyKey(snapshot.IdempotencyKey) {
		return videocover.NewError(videocover.ErrorInvalidRequest)
	}
	if snapshot.Active && snapshot.CoverAsset == nil || !snapshot.Active && snapshot.CoverAsset != nil {
		return videocover.NewError(videocover.ErrorInvalidRequest)
	}
	if snapshot.CoverAsset != nil {
		if err := videocover.ValidateDescriptor(*snapshot.CoverAsset); err != nil {
			return err
		}
	}
	return nil
}

func validateCoverApply(streamID string, request videocover.ApplyRequest) error {
	if strings.TrimSpace(streamID) == "" || request.StreamID != streamID || request.JobGeneration < 1 || request.ExpectedGeneration < 1 || request.Revision < 1 || !validIdempotencyKey(request.IdempotencyKey) {
		return videocover.NewError(videocover.ErrorInvalidRequest)
	}
	if request.Active {
		if request.CoverAsset == nil || request.HideConfirmed {
			return videocover.NewError(videocover.ErrorInvalidRequest)
		}
		if err := videocover.ValidateDescriptor(*request.CoverAsset); err != nil {
			return err
		}
	} else if request.CoverAsset != nil || !request.HideConfirmed {
		return videocover.NewError(videocover.ErrorInvalidRequest)
	}
	return nil
}

func validIdempotencyKey(value string) bool {
	if !utf8.ValidString(value) {
		return false
	}
	length := utf8.RuneCountInString(value)
	if length < 1 || length > 128 {
		return false
	}
	first, _ := utf8.DecodeRuneInString(value)
	last, _ := utf8.DecodeLastRuneInString(value)
	if idempotencyEdgeSpace(first) || idempotencyEdgeSpace(last) {
		return false
	}
	for _, char := range value {
		if char < 0x20 || char == 0x7f {
			return false
		}
	}
	return true
}

func idempotencyEdgeSpace(character rune) bool {
	return unicode.IsSpace(character) || character == '\ufeff'
}
