package streamproc

import (
	"context"
	"errors"
	"log"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/example/autostream-encoder-recorder/internal/observability"
	"github.com/example/autostream-encoder-recorder/internal/videocover"
	"github.com/example/autostream-encoder-recorder/internal/watermarkfeed"
)

type RuntimeSettings struct {
	EncoderAudioGainDB float64
	OverlayProfileID   string
	OverlayConfig      map[string]any
}

func (m *Manager) UpdateRuntimeSettings(streamID string, settings RuntimeSettings) (Snapshot, error) {
	if strings.TrimSpace(streamID) == "" || math.IsNaN(settings.EncoderAudioGainDB) || math.IsInf(settings.EncoderAudioGainDB, 0) || settings.EncoderAudioGainDB < -60 || settings.EncoderAudioGainDB > 24 {
		return Snapshot{}, ErrInvalidRuntimeSettings
	}
	frame, err := watermarkfeed.Frame(settings.OverlayConfig)
	if err != nil {
		return Snapshot{}, errors.Join(ErrInvalidRuntimeSettings, err)
	}
	m.mu.Lock()
	tracked, ok := m.processes[streamID]
	if !ok || tracked.snapshot.Status != "running" || tracked.process == nil || tracked.watermark == nil {
		m.mu.Unlock()
		return Snapshot{}, ErrNotRunning
	}
	m.mu.Unlock()

	tracked.runtimeMu.Lock()
	defer tracked.runtimeMu.Unlock()
	argument := strconv.FormatFloat(settings.EncoderAudioGainDB, 'f', 1, 64) + "dB"
	commander, ok := tracked.process.(runtimeCommander)
	if !ok {
		return Snapshot{}, errors.New("ffmpeg runtime commands are unavailable")
	}
	if err := commander.Command("volume@gain", "volume", argument); err != nil {
		return Snapshot{}, err
	}
	// Serialize the two independently mutable visual inputs only for the short
	// witness window. This prevents a concurrent Cover apply from publishing a
	// witness that pairs its new Cover revision with a pre-witness Watermark.
	tracked.coverMu.Lock()
	witnessCtx, cancelWitness := context.WithTimeout(context.Background(), m.coverApplyTimeout())
	witnessErr := m.watermarkGraphWitness().Apply(witnessCtx, tracked.watermark, frame, tracked.progressPath)
	cancelWitness()
	if witnessErr != nil {
		tracked.markWatermarkWitnessUnknownLocked()
		tracked.coverMu.Unlock()
		return Snapshot{}, videocover.NewError(videocover.ErrorCoverGraphUnavailable)
	}
	tracked.watermarkMu.Lock()
	tracked.watermarkState.Revision++
	tracked.watermarkState.Enabled = watermarkEnabled(settings.OverlayConfig)
	if tracked.watermarkState.Enabled {
		tracked.watermarkState.VariantID = strings.TrimSpace(settings.OverlayProfileID)
	} else {
		tracked.watermarkState.VariantID = ""
	}
	watermarkState := tracked.watermarkState
	tracked.watermarkMu.Unlock()
	if tracked.coverState.JobGeneration != 0 {
		tracked.coverState.Watermark = watermarkState
		if tracked.coverState.AppliedWitness != nil {
			witness := *tracked.coverState.AppliedWitness
			witness.Watermark = watermarkState
			tracked.coverState.AppliedWitness = &witness
		}
	}
	tracked.coverMu.Unlock()

	m.mu.Lock()
	current, ok := m.processes[streamID]
	if !ok || current != tracked || current.snapshot.Status != "running" {
		m.mu.Unlock()
		return Snapshot{}, ErrNotRunning
	}
	current.job.EncoderAudioGainDB = settings.EncoderAudioGainDB
	current.job.OverlayProfileID = strings.TrimSpace(settings.OverlayProfileID)
	current.snapshot.EncoderAudioGainDB = settings.EncoderAudioGainDB
	current.snapshot.OverlayProfileID = strings.TrimSpace(settings.OverlayProfileID)
	snapshot := current.snapshot
	m.mu.Unlock()

	log.Printf("encoder diagnostic: event=encoder.runtime_settings.dispatched stream_id=%s status=dispatched audio_gain_db=%.1f overlay_profile_id=%s audio_command_written=true watermark_frame_updated=true", streamID, settings.EncoderAudioGainDB, strings.TrimSpace(settings.OverlayProfileID))
	m.report(observability.Signal{
		Type:      "event",
		Name:      "encoder.runtime_settings.dispatched",
		StreamID:  streamID,
		Status:    "dispatched",
		Timestamp: time.Now().UTC(),
		Attributes: map[string]any{
			"audio_gain_db":           settings.EncoderAudioGainDB,
			"overlay_profile_id":      strings.TrimSpace(settings.OverlayProfileID),
			"audio_command_written":   true,
			"watermark_frame_updated": true,
		},
	})
	return snapshot, nil
}
