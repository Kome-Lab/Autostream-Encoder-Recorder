package lifecycle

import (
	"context"
	"strings"
	"time"

	"github.com/example/autostream-encoder-recorder/internal/archive"
)

func (m Manager) uploaderForJob(job PackageJob) archive.ArchiveUploader {
	if m.UploaderForJob != nil {
		return m.UploaderForJob(job)
	}
	if job.DryRun || job.ArchiveConfig.AuthMode == "" {
		return nil
	}
	cfg := googleDriveConfigFromArchiveConfig(job.ArchiveConfig)
	return archive.RetryUploader{
		Inner:  archive.GoogleDriveAPIUploader{Config: cfg},
		Policy: archive.RetryPolicy{MaxAttempts: 5, BaseDelay: 2 * time.Second},
	}
}

func googleDriveConfigFromArchiveConfig(cfg ArchiveConfig) archive.GoogleDriveConfig {
	return archive.GoogleDriveConfig{
		AuthMode:      cfg.AuthMode,
		FolderID:      cfg.FolderID,
		SharedDrive:   cfg.SharedDrive,
		SharedDriveID: cfg.SharedDriveID,
		ClientID:      cfg.ClientID,
		ClientSecret:  cfg.ClientSecret,
		RefreshToken:  cfg.RefreshToken,
	}
}

func uploadArchiveFiles(ctx context.Context, uploader archive.ArchiveUploader, streamName, streamID string, startedAtJST time.Time, files []archive.File, metadataPath string, metadata *Metadata) (archive.UploadResult, error) {
	if uploader == nil {
		uploader = archive.DryRunUploader{}
	}
	metadataFile := archive.File{LocalPath: metadataPath, DrivePath: "metadata.json"}
	dataFiles := make([]archive.File, 0, len(files))
	for _, file := range files {
		if file.DrivePath == "metadata.json" {
			metadataFile = file
			continue
		}
		dataFiles = append(dataFiles, file)
	}

	upload := archive.UploadResult{FileIDs: map[string]string{}}
	if len(dataFiles) > 0 {
		var err error
		upload, err = uploader.Upload(ctx, streamName, streamID, startedAtJST, dataFiles)
		if err != nil {
			return archive.UploadResult{}, err
		}
	}
	metadata.Upload = upload
	if err := writeJSON(metadataPath, *metadata); err != nil {
		return archive.UploadResult{}, err
	}
	if info, err := safeRegularFileInfo(metadataFile.LocalPath); err == nil {
		metadataFile.SizeBytes = info.Size()
	}
	metadataUpload, err := uploader.Upload(ctx, streamName, streamID, startedAtJST, []archive.File{metadataFile})
	if err != nil {
		return archive.UploadResult{}, err
	}
	merged := mergeUploadResults(upload, metadataUpload)
	metadata.Upload = merged
	if err := writeJSON(metadataPath, *metadata); err != nil {
		return archive.UploadResult{}, err
	}
	return merged, nil
}

func mergeUploadResults(primary, metadata archive.UploadResult) archive.UploadResult {
	merged := primary
	if merged.FileIDs == nil {
		merged.FileIDs = map[string]string{}
	}
	if merged.FolderID == "" {
		merged.FolderID = metadata.FolderID
	}
	merged.DryRun = primary.DryRun || metadata.DryRun
	if metadata.Attempts > merged.Attempts {
		merged.Attempts = metadata.Attempts
	}
	for drivePath, fileID := range metadata.FileIDs {
		merged.FileIDs[drivePath] = fileID
	}
	return merged
}

func collectArchiveFiles(layout archive.Layout, cfg ArchiveConfig) ([]archive.File, error) {
	candidates := []struct {
		local    string
		drive    string
		required bool
	}{
		{layout.FinalMP4(), archiveUploadFileName(cfg, "final.mp4"), true},
		{layout.FinalCaptions(), "captions.vtt", false},
		{layout.FinalTranscript(), "transcript.json", false},
		{layout.FinalMetadata(), "metadata.json", false},
		{layout.FinalLogs(), "logs.jsonl", false},
	}
	files := make([]archive.File, 0, len(candidates))
	for _, candidate := range candidates {
		info, err := safeRegularFileInfo(candidate.local)
		if err != nil {
			if candidate.required {
				return nil, err
			}
			continue
		}
		files = append(files, archive.File{LocalPath: candidate.local, DrivePath: candidate.drive, SizeBytes: info.Size()})
	}
	return files, nil
}

func archiveUploadFileName(cfg ArchiveConfig, fallback string) string {
	value := strings.TrimSpace(cfg.ArchiveFileName)
	if value == "" {
		value = fallback
	}
	value = strings.ReplaceAll(value, "/", "_")
	value = strings.ReplaceAll(value, "\\", "_")
	value = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return '_'
		}
		return r
	}, value)
	value = strings.Trim(value, " .")
	if value == "" {
		value = fallback
	}
	if !strings.HasSuffix(strings.ToLower(value), ".mp4") {
		value += ".mp4"
	}
	return value
}
