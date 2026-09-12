package lifecycle

import (
	"encoding/json"
	"strings"

	"github.com/example/autostream-encoder-recorder/internal/archive"
	"github.com/example/autostream-encoder-recorder/internal/ffmpeg"
	"github.com/example/autostream-encoder-recorder/internal/redaction"
)

func ArchiveArtifactsForRun(streamID, archiveRunID string) map[string]string {
	finalArtifactSet := "final/" + streamID
	if archiveRunID != "" {
		finalArtifactSet += "/" + archiveRunID
	}
	return map[string]string{
		"tmp_artifact_set":   "tmp/" + streamID,
		"final_artifact_set": finalArtifactSet,
		"recording_mkv":      "final.mkv",
		"preview_playlist":   "preview/index.m3u8",
		"final_mp4":          "final.mp4",
		"metadata":           "metadata.json",
		"logs":               "logs.jsonl",
		"captions":           "captions.vtt",
		"transcript":         "transcript.json",
		"ffmpeg_progress":    "ffmpeg-progress.txt",
		"ffmpeg_audio":       "ffmpeg-audio-stats.txt",
	}
}

func metadataExtra(dryRun bool, remuxDurationMS float64, cfg ArchiveConfig) map[string]any {
	extra := map[string]any{"dry_run": dryRun, "remux_duration_ms": remuxDurationMS}
	if summary := archiveConfigMetadata(cfg); len(summary) > 0 {
		extra["archive_config"] = summary
	}
	return extra
}

func archiveConfigMetadata(cfg ArchiveConfig) map[string]any {
	out := map[string]any{}
	if cfg.DriveDestinationID != "" {
		out["drive_destination_id"] = cfg.DriveDestinationID
	}
	if cfg.ArchiveProfileID != "" {
		out["archive_profile_id"] = cfg.ArchiveProfileID
	}
	if cfg.AuthMode != "" {
		out["auth_mode"] = cfg.AuthMode
	}
	if cfg.OAuthAccountID != "" {
		out["oauth_account_id"] = cfg.OAuthAccountID
	}
	if cfg.OAuthProviderID != "" {
		out["oauth_provider_id"] = cfg.OAuthProviderID
	}
	if cfg.SharedDrive {
		out["shared_drive"] = true
	}
	if cfg.SharedDriveID != "" {
		out["shared_drive_id_configured"] = true
	}
	if cfg.ArchiveFileName != "" {
		out["archive_file_name"] = archiveUploadFileName(cfg, "final.mp4")
	}
	if cfg.RetentionDays > 0 {
		out["retention_days"] = cfg.RetentionDays
	}
	if cfg.FolderID != "" || cfg.FolderIDSecretName != "" {
		out["folder_id_configured"] = true
	}
	if cfg.ClientID != "" {
		out["client_id_configured"] = true
	}
	if cfg.ClientSecret != "" || cfg.ClientSecretSecretName != "" {
		out["client_secret_configured"] = true
	}
	if cfg.RefreshToken != "" || cfg.RefreshTokenSecretName != "" {
		out["refresh_token_configured"] = true
	}
	return out
}

func RedactCommandsForLayout(layout archive.Layout, commands []ffmpeg.Command, secrets ...string) []ffmpeg.Command {
	out := make([]ffmpeg.Command, 0, len(commands))
	for _, command := range commands {
		args := redaction.Args(command.Args, secrets...)
		for i := range args {
			for _, secret := range secrets {
				replacement := "<REDACTED>"
				if masked, ok := redaction.MaskSensitiveURL(secret); ok {
					replacement = masked
				}
				args[i] = ffmpeg.RedactTeeValue(args[i], strings.TrimSpace(secret), replacement)
			}
		}
		redacted := ffmpeg.Command{Bin: command.Bin, Args: redactArchivePaths(layout, args)}
		out = append(out, redacted)
	}
	return out
}

func redactArchivePaths(layout archive.Layout, args []string) []string {
	finalArtifactSet := "final/" + layout.StreamID
	if layout.ArchiveRunID != "" {
		finalArtifactSet += "/" + layout.ArchiveRunID
	}
	replacements := []struct {
		from string
		to   string
	}{
		{layout.PreviewSegmentPattern(), "preview/segment-%06d.ts"},
		{layout.PreviewPlaylist(), "preview/index.m3u8"},
		{layout.PreviewDir(), "preview"},
		{layout.TmpFFmpegAudioStats(), "ffmpeg-audio-stats.txt"},
		{layout.TmpFFmpegProgress(), "ffmpeg-progress.txt"},
		{layout.TmpDiscordOpusSDP(), "discord-opus.sdp"},
		{layout.TmpDiscordOpus(), "discord-opus.jsonl"},
		{layout.FinalTranscript(), "transcript.json"},
		{layout.TmpTranscript(), "transcript.json"},
		{layout.FinalMetadata(), "metadata.json"},
		{layout.TmpMetadata(), "metadata.json"},
		{layout.FinalCaptions(), "captions.vtt"},
		{layout.TmpCaptions(), "captions.vtt"},
		{layout.FinalLogs(), "logs.jsonl"},
		{layout.TmpLogs(), "logs.jsonl"},
		{layout.FinalMKV(), "final.mkv"},
		{layout.FinalMP4(), "final.mp4"},
		{layout.FinalDir(), finalArtifactSet},
		{layout.TmpDir(), "tmp/" + layout.StreamID},
		{layout.RootDir, "<ARCHIVE_ROOT>"},
	}
	out := append([]string(nil), args...)
	for i := range out {
		for _, replacement := range replacements {
			out[i] = ffmpeg.RedactTeePath(out[i], replacement.from, replacement.to)
		}
	}
	return out
}

func writeJSON(path string, value any) error {
	body, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return writeFileNoSymlink(path, append(body, '\n'), 0o640)
}
