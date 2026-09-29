package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/example/autostream-contracts/pkg/contracts"
	"github.com/example/autostream-encoder-recorder/internal/lifecycle"
	"github.com/example/autostream-encoder-recorder/internal/streamproc"
	"log"
	"strings"
)

func prepareRuntimeJob(ctx context.Context, job *lifecycle.StreamJob, request startStreamRequest, pm *streamproc.Manager, resolver RuntimeSecretResolver, config RuntimeConfigProvider) error {
	if err := applyEncoderRuntimeConfig(ctx, job, config); err != nil {
		return err
	}
	if err := applyYouTubeRuntimeConfig(ctx, job, config); err != nil {
		return err
	}
	if err := request.applyArchiveRuntimeConfig(ctx, job, config); err != nil {
		return err
	}
	if err := applyOverlayRuntimeConfig(ctx, job, config); err != nil {
		return err
	}
	if err := resolveArchiveRuntimeSecrets(ctx, job, resolver); err != nil {
		return err
	}
	if strings.TrimSpace(job.YouTubeOutputMode) == "" {
		return errors.New("youtube runtime required")
	}
	relay, err := pm.AuthorizeOutputRelay(*job)
	if err != nil {
		return err
	}
	if relay {
		clearUnusedYouTubeOutputTarget(job)
	} else {
		if err := resolveYouTubeRuntimeSecrets(ctx, job, resolver); err != nil {
			return err
		}
		if len(missingYouTubeRuntimeFields(*job)) > 0 {
			return errors.New("youtube runtime fields missing")
		}
	}
	return ctx.Err()
}
func logStartPreparationPhase(id streamproc.StartPreparationIdentity, event string) {
	log.Printf("encoder start: event=%s stream_id=%s start_id=%s job_generation=%d", event, id.StreamID, id.StartID, id.JobGeneration)
}
func contractPreparationIdentity(id streamproc.StartPreparationIdentity) contracts.EncoderStartPreparationIdentity {
	return contracts.EncoderStartPreparationIdentity{StreamID: id.StreamID, StartID: id.StartID, EncoderServiceID: id.EncoderServiceID, JobGeneration: id.JobGeneration, ArchiveRunID: id.ArchiveRunID}
}
func publicStartPreparation(state streamproc.StartPreparationState) contracts.EncoderStartPreparationStatus {
	result := contracts.EncoderStartPreparationStatus{SchemaVersion: 2, Identity: contractPreparationIdentity(state.Identity), Phase: state.Phase, ExpiresAt: state.ExpiresAt, Code: state.Code}
	if state.Process != nil {
		p := state.Process
		result.Process = &contracts.EncoderStartStreamResponse{StreamID: p.StreamID, Name: p.Name, Status: p.Status, StartedAtJST: p.StartedAtJST, StoppedAtJST: p.StoppedAtJST, Archive: p.Archive, Error: p.Error}
	}
	if state.CoverState != nil {
		encoded, _ := json.Marshal(state.CoverState)
		var cover contracts.VideoCoverRuntimeState
		_ = json.Unmarshal(encoded, &cover)
		result.CoverState = &cover
	}
	return result
}
