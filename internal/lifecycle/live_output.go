package lifecycle

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/example/autostream-encoder-recorder/internal/archive"
	"github.com/example/autostream-encoder-recorder/internal/ffmpeg"
)

// DryRunToOutputTarget keeps dry-run's command and metadata behavior aligned
// with a live process. The caller must choose the canonical output route and
// provide the complete target explicitly.
func (m Manager) DryRunToOutputTarget(ctx context.Context, job StreamJob, outputTarget string) (Result, error) {
	if job.StreamID == "" || job.Name == "" {
		return Result{}, errors.New("stream id and name are required")
	}
	if job.ArchiveRunID != "" && job.StartedAt.IsZero() {
		return Result{}, errors.New("archive run started_at is required when archive_run_id is set")
	}
	layout, err := archiveLayout(m.ArchiveRoot, job.StreamID, job.ArchiveRunID)
	if err != nil {
		return Result{}, err
	}
	if err := archive.EnsureDirNoSymlinks(layout.RootDir, layout.TmpDir()); err != nil {
		return Result{}, err
	}
	if err := archive.EnsureDirNoSymlinks(layout.RootDir, layout.FinalDir()); err != nil {
		return Result{}, err
	}
	if err := ffmpeg.ValidateInputTarget(job.InputURL); err != nil {
		return Result{}, err
	}
	outputTarget = strings.TrimSpace(outputTarget)
	if outputTarget == "" {
		return Result{}, errors.New("explicit output target is required")
	}
	if err := validateExplicitOutputTarget(job, outputTarget); err != nil {
		return Result{}, err
	}
	runner := m.Runner
	if runner == nil {
		runner = &ffmpeg.DryRunRunner{}
	}
	profile := job.EncoderProfile
	if profile.Width == 0 {
		profile = m.Profile
	}
	if profile.Width == 0 {
		profile = ffmpeg.DefaultProfile()
	}
	ffmpegBin := m.FFmpegBin
	if ffmpegBin == "" {
		ffmpegBin = "ffmpeg"
	}
	liveArgs := BuildLiveArgsToOutputTarget(job, outputTarget, layout.FinalMKV(), "", "", profile)
	if err := runner.Run(ctx, ffmpegBin, liveArgs); err != nil {
		return Result{}, err
	}
	remuxArgs := ffmpeg.BuildRemuxArgs(layout.FinalMKV(), layout.FinalMP4())
	remuxStarted := time.Now()
	if err := runner.Run(ctx, ffmpegBin, remuxArgs); err != nil {
		return Result{}, PackageError{Phase: "remux", Err: err}
	}
	remuxDurationMS := time.Since(remuxStarted).Seconds() * 1000
	logLine := map[string]any{"timestamp": time.Now().UTC().Format(time.RFC3339), "event": "dry_run.completed", "stream_id": job.StreamID}
	logJSON, _ := json.Marshal(logLine)
	if err := writeFileNoSymlink(layout.TmpLogs(), append(logJSON, '\n'), 0o640); err != nil {
		return Result{}, err
	}
	if err := copyFile(layout.TmpLogs(), layout.FinalLogs()); err != nil {
		return Result{}, err
	}
	uploader := m.uploaderForJob(PackageJob{
		StreamID:      job.StreamID,
		ArchiveRunID:  job.ArchiveRunID,
		Name:          job.Name,
		StartedAt:     job.StartedAt,
		DryRun:        job.DryRun,
		ArchiveConfig: job.ArchiveConfig,
	})
	if uploader == nil {
		uploader = m.Uploader
	}
	if uploader == nil {
		uploader = archive.DryRunUploader{}
	}
	startedAt := job.StartedAt
	if startedAt.IsZero() {
		startedAt = time.Now().UTC()
	}
	jst := time.FixedZone("JST", 9*60*60)
	extra := metadataExtra(true, remuxDurationMS, job.ArchiveConfig)
	extra["archive_source"] = "final_mkv"
	extra["archive_partial"] = false
	extra["archive_run_id"] = job.ArchiveRunID
	metadata := Metadata{
		StreamID: job.StreamID, Name: job.Name, StartedAtJST: startedAt.In(jst).Format(time.RFC3339),
		Archive: ArchiveArtifactsForRun(job.StreamID, job.ArchiveRunID),
		Extra:   extra,
	}
	if dryRunner, ok := runner.(*ffmpeg.DryRunRunner); ok {
		metadata.Commands = RedactCommandsForLayout(layout, dryRunner.Commands, job.StreamKey, job.InputURL, job.RTMPURL)
	}
	files := []archive.File{
		{LocalPath: layout.FinalMP4(), DrivePath: archiveUploadFileName(job.ArchiveConfig, "final.mp4")},
		{LocalPath: layout.FinalLogs(), DrivePath: "logs.jsonl"},
	}
	upload, err := uploadArchiveFiles(ctx, uploader, job.Name, job.StreamID, startedAt.In(jst), files, layout.FinalMetadata(), &metadata)
	if err != nil {
		return Result{}, err
	}
	metadata.Upload = upload
	if err := writeJSON(layout.TmpMetadata(), metadata); err != nil {
		return Result{}, err
	}
	return Result{Layout: layout, Metadata: metadata, RemuxDurationMS: remuxDurationMS, ArchiveSource: "final_mkv"}, nil
}

func validateExplicitOutputTarget(job StreamJob, outputTarget string) error {
	directTarget := strings.TrimRight(strings.TrimSpace(job.RTMPURL), "/") + "/" + strings.TrimLeft(job.StreamKey, "/")
	if strings.TrimSpace(job.RTMPURL) != "" && strings.TrimSpace(job.StreamKey) != "" && outputTarget == directTarget {
		return ffmpeg.ValidateOutputTarget(job.RTMPURL, job.StreamKey)
	}
	return ffmpeg.ValidateRelayOutputTarget(outputTarget)
}

func BuildLiveArgsToOutputTarget(job StreamJob, outputTarget, archivePath, progressPath, audioStatsPath string, profile ffmpeg.EncoderProfile) []string {
	return BuildLiveArgsToOutputTargetWithPreview(job, outputTarget, archivePath, "", progressPath, audioStatsPath, profile)
}

func BuildLiveArgsToOutputTargetWithPreview(job StreamJob, outputTarget, archivePath, previewPlaylistPath, progressPath, audioStatsPath string, profile ffmpeg.EncoderProfile) []string {
	return BuildLiveArgsToOutputTargetWithPreviewAndOverlay(job, outputTarget, archivePath, previewPlaylistPath, progressPath, audioStatsPath, "", profile)
}

func BuildLiveArgsToOutputTargetWithPreviewAndOverlay(job StreamJob, outputTarget, archivePath, previewPlaylistPath, progressPath, audioStatsPath, watermarkPath string, profile ffmpeg.EncoderProfile) []string {
	watermarkInput := watermarkPath
	if strings.TrimSpace(job.WatermarkInputURL) != "" {
		watermarkInput = job.WatermarkInputURL
	}
	if strings.TrimSpace(job.CoverInputURL) != "" {
		if job.InputMode == "worker_scene_frames_srt" {
			return ffmpeg.BuildWorkerVideoDiscordAudioLiveArchiveArgsToOutputTargetWithRuntimeSettingsAndVisualLayers(job.InputURL, job.AudioInputURL, outputTarget, archivePath, previewPlaylistPath, progressPath, audioStatsPath, job.CoverInputURL, watermarkInput, job.EncoderAudioGainDB, profile)
		}
		if job.InputMode == "discord_opus_rtp" {
			return ffmpeg.BuildDiscordAudioLiveArchiveArgsToOutputTargetWithRuntimeSettingsAndVisualLayers(job.InputURL, outputTarget, archivePath, previewPlaylistPath, progressPath, audioStatsPath, job.CoverInputURL, watermarkInput, job.EncoderAudioGainDB, profile)
		}
		return ffmpeg.BuildLiveArchiveArgsToOutputTargetWithRuntimeSettingsAndVisualLayers(job.InputURL, outputTarget, archivePath, previewPlaylistPath, progressPath, audioStatsPath, job.CoverInputURL, watermarkInput, job.EncoderAudioGainDB, profile)
	}
	if job.InputMode == "worker_scene_frames_srt" {
		return ffmpeg.BuildWorkerVideoDiscordAudioLiveArchiveArgsToOutputTargetWithRuntimeSettings(job.InputURL, job.AudioInputURL, outputTarget, archivePath, previewPlaylistPath, progressPath, audioStatsPath, watermarkInput, job.EncoderAudioGainDB, profile)
	}
	if job.InputMode == "discord_opus_rtp" {
		return ffmpeg.BuildDiscordAudioLiveArchiveArgsToOutputTargetWithRuntimeSettings(job.InputURL, outputTarget, archivePath, previewPlaylistPath, progressPath, audioStatsPath, watermarkInput, job.EncoderAudioGainDB, profile)
	}
	return ffmpeg.BuildLiveArchiveArgsToOutputTargetWithRuntimeSettings(job.InputURL, outputTarget, archivePath, previewPlaylistPath, progressPath, audioStatsPath, watermarkInput, job.EncoderAudioGainDB, profile)
}
