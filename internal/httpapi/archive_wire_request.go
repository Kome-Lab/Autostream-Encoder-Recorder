package httpapi

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/example/autostream-contracts/pkg/contracts"
	"github.com/example/autostream-encoder-recorder/internal/archive"
	"github.com/example/autostream-encoder-recorder/internal/lifecycle"
	"github.com/example/autostream-encoder-recorder/internal/videocover"
)

// HTTP inputs deliberately do not embed lifecycle jobs: ArchiveConfig belongs
// to resolved runtime state and persisted jobs, never to start/package wire data.
type startStreamRequest struct {
	StreamID               string                    `json:"stream_id"`
	ArchiveRunID           string                    `json:"archive_run_id,omitempty"`
	Name                   string                    `json:"name"`
	InputURL               string                    `json:"input_url,omitempty"`
	InputMode              string                    `json:"input_mode,omitempty"`
	WorkerVideoIngest      bool                      `json:"worker_video_ingest,omitempty"`
	WorkerVideoIngestToken string                    `json:"worker_video_ingest_token,omitempty"`
	RTMPURL                string                    `json:"rtmp_url"`
	StreamKeySecretName    string                    `json:"stream_key_secret_name,omitempty"`
	EncoderProfileID       string                    `json:"encoder_profile_id,omitempty"`
	OverlayProfileID       string                    `json:"overlay_profile_id,omitempty"`
	EncoderAudioGainDB     float64                   `json:"encoder_audio_gain_db,omitempty"`
	ArchiveProfileID       string                    `json:"archive_profile_id,omitempty"`
	StartedAt              time.Time                 `json:"started_at,omitempty"`
	YouTubeRuntime         *youtubeRuntimeMetadata   `json:"youtube_runtime,omitempty"`
	VideoCoverStart        *videocover.StartSnapshot `json:"video_cover_start,omitempty"`
	DryRun                 bool                      `json:"dry_run,omitempty"`
}

// CP owns this lifecycle metadata. Decode its closed field set strictly, but
// obtain output policy and credentials from the assigned runtime provider.
// The fixed schema also permits rtmp_url, which the shared Go metadata type
// does not yet declare.
type youtubeRuntimeMetadata struct {
	contracts.YouTubeRuntimeConfig
	RTMPURL string `json:"rtmp_url,omitempty"`
}

func (request startStreamRequest) validateArchiveRun() error {
	if request.ArchiveRunID == "" {
		return nil
	}
	if request.StartedAt.IsZero() {
		return errors.New("archive run start time is required")
	}
	// Reuse the recording layout's identity rules without creating any files.
	_, err := archive.NewRunLayout(".", request.StreamID, request.ArchiveRunID)
	return err
}

func (request startStreamRequest) streamJob() lifecycle.StreamJob {
	return lifecycle.StreamJob{
		StreamID: request.StreamID, ArchiveRunID: request.ArchiveRunID, Name: request.Name,
		InputURL: request.InputURL, InputMode: request.InputMode, RTMPURL: request.RTMPURL,
		StreamKeySecretName: request.StreamKeySecretName, EncoderProfileID: request.EncoderProfileID,
		OverlayProfileID: request.OverlayProfileID, EncoderAudioGainDB: request.EncoderAudioGainDB,
		StartedAt: request.StartedAt, VideoCoverStart: request.VideoCoverStart, DryRun: request.DryRun,
	}
}

func (request startStreamRequest) applyArchiveRuntimeConfig(ctx context.Context, job *lifecycle.StreamJob, provider RuntimeConfigProvider) error {
	if err := applyArchiveRuntimeConfig(ctx, job, provider); err != nil {
		return err
	}
	// A selected profile must resolve through runtime authority. Do not let the
	// request seed merge priority or silently replace a missing/mismatched profile.
	if selected := strings.TrimSpace(request.ArchiveProfileID); selected != "" && selected != job.ArchiveConfig.ArchiveProfileID {
		return errors.New("selected archive runtime profile unavailable")
	}
	return nil
}

type packageStreamRequest struct {
	StreamID     string    `json:"stream_id"`
	ArchiveRunID string    `json:"archive_run_id"`
	Name         string    `json:"name"`
	StartedAt    time.Time `json:"started_at"`
	DryRun       bool      `json:"dry_run,omitempty"`
}

func (request packageStreamRequest) packageJob() lifecycle.PackageJob {
	return lifecycle.PackageJob{
		StreamID: request.StreamID, ArchiveRunID: request.ArchiveRunID,
		Name: request.Name, StartedAt: request.StartedAt, DryRun: request.DryRun,
	}
}
