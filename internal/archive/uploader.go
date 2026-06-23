package archive

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"
)

type File struct {
	LocalPath string
	DrivePath string
	SizeBytes int64
}

type UploadResult struct {
	DryRun   bool              `json:"dry_run"`
	FolderID string            `json:"-"`
	FileIDs  map[string]string `json:"-"`
	Attempts int               `json:"attempts"`
}

func (r UploadResult) UploadedFileCount() int {
	return len(r.FileIDs)
}

func (r UploadResult) HasFolderFingerprint() bool {
	return r.FolderID != ""
}

func (r UploadResult) HasFileFingerprint(drivePath string) bool {
	return r.FileIDs[drivePath] != ""
}

func (r UploadResult) MarshalJSON() ([]byte, error) {
	type publicUploadResult struct {
		DryRun              bool              `json:"dry_run"`
		FolderIDConfigured  bool              `json:"folder_id_configured,omitempty"`
		FolderIDFingerprint string            `json:"folder_id_fingerprint,omitempty"`
		FileCount           int               `json:"file_count"`
		FileFingerprints    map[string]string `json:"file_fingerprints,omitempty"`
		Attempts            int               `json:"attempts"`
	}
	out := publicUploadResult{
		DryRun:   r.DryRun,
		Attempts: r.Attempts,
	}
	if r.FolderID != "" {
		out.FolderIDConfigured = true
		out.FolderIDFingerprint = secretFingerprint(r.FolderID)
	}
	if len(r.FileIDs) > 0 {
		out.FileCount = len(r.FileIDs)
		out.FileFingerprints = make(map[string]string, len(r.FileIDs))
		for drivePath, fileID := range r.FileIDs {
			if fileID == "" {
				continue
			}
			out.FileFingerprints[drivePath] = secretFingerprint(fileID)
		}
	}
	return json.Marshal(out)
}

type ArchiveUploader interface {
	Upload(ctx context.Context, streamName, streamID string, startedAtJST time.Time, files []File) (UploadResult, error)
}

type DryRunUploader struct{}

func (DryRunUploader) Upload(ctx context.Context, streamName, streamID string, startedAtJST time.Time, files []File) (UploadResult, error) {
	if err := ctx.Err(); err != nil {
		return UploadResult{}, err
	}
	result := UploadResult{DryRun: true, FolderID: "dry-run-folder", FileIDs: map[string]string{}}
	for _, file := range files {
		result.FileIDs[file.DrivePath] = "dry-run-file"
	}
	return result, nil
}

type MockUploader struct{ Err error }

func (m MockUploader) Upload(ctx context.Context, streamName, streamID string, startedAtJST time.Time, files []File) (UploadResult, error) {
	if m.Err != nil {
		return UploadResult{}, m.Err
	}
	if len(files) == 0 {
		return UploadResult{}, errors.New("no files to upload")
	}
	return DryRunUploader{}.Upload(ctx, streamName, streamID, startedAtJST, files)
}

func secretFingerprint(value string) string {
	sum := sha256.Sum256([]byte(value))
	return "sha256:" + hex.EncodeToString(sum[:])[:12]
}
