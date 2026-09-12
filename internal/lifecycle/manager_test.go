package lifecycle

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/example/autostream-encoder-recorder/internal/archive"
)

func googleDriveUploaderFromRetry(t *testing.T, uploader archive.ArchiveUploader) archive.GoogleDriveAPIUploader {
	t.Helper()
	retry, ok := uploader.(archive.RetryUploader)
	if !ok {
		t.Fatalf("expected retry uploader, got %#v", uploader)
	}
	driveUploader, ok := retry.Inner.(archive.GoogleDriveAPIUploader)
	if !ok {
		t.Fatalf("expected Google Drive uploader, got %#v", retry.Inner)
	}
	return driveUploader
}

type archiveFileNameCheckingUploader struct {
	t        *testing.T
	want     string
	observed bool
}

func (u *archiveFileNameCheckingUploader) Upload(ctx context.Context, streamName, streamID string, startedAtJST time.Time, files []archive.File) (archive.UploadResult, error) {
	if err := ctx.Err(); err != nil {
		return archive.UploadResult{}, err
	}
	result := archive.UploadResult{DryRun: true, FolderID: "folder", FileIDs: map[string]string{}}
	for _, file := range files {
		if file.DrivePath == u.want {
			u.observed = true
		}
		if file.DrivePath == "final.mp4" {
			u.t.Fatalf("default final.mp4 drive name should be replaced by %q: %#v", u.want, files)
		}
		result.FileIDs[file.DrivePath] = "id-" + file.DrivePath
	}
	return result, nil
}

type metadataCheckingUploader struct {
	t                *testing.T
	metadataObserved bool
}

func (u *metadataCheckingUploader) Upload(ctx context.Context, streamName, streamID string, startedAtJST time.Time, files []archive.File) (archive.UploadResult, error) {
	if err := ctx.Err(); err != nil {
		return archive.UploadResult{}, err
	}
	result := archive.UploadResult{DryRun: true, FolderID: "folder", FileIDs: map[string]string{}}
	for _, file := range files {
		if file.DrivePath == "metadata.json" {
			u.metadataObserved = true
			body, err := os.ReadFile(file.LocalPath)
			if err != nil {
				u.t.Fatal(err)
			}
			var metadata Metadata
			if err := json.Unmarshal(body, &metadata); err != nil {
				u.t.Fatal(err)
			}
			bodyText := string(body)
			if strings.Contains(bodyText, "id-final.mp4") || strings.Contains(bodyText, `"file_ids"`) || strings.Contains(bodyText, `"folder_id"`) {
				u.t.Fatalf("metadata upload leaked raw Drive IDs: %s", bodyText)
			}
			var metadataJSON map[string]any
			if err := json.Unmarshal(body, &metadataJSON); err != nil {
				u.t.Fatal(err)
			}
			uploadJSON, ok := metadataJSON["upload"].(map[string]any)
			if !ok || uploadJSON["file_count"] != float64(4) {
				u.t.Fatalf("metadata uploaded before archive file fingerprints were written: %s", bodyText)
			}
		}
		result.FileIDs[file.DrivePath] = "id-" + file.DrivePath
	}
	return result, nil
}

type archiveConfigMetadataCheckingUploader struct {
	t                *testing.T
	metadataObserved bool
}

func (u *archiveConfigMetadataCheckingUploader) Upload(ctx context.Context, streamName, streamID string, startedAtJST time.Time, files []archive.File) (archive.UploadResult, error) {
	if err := ctx.Err(); err != nil {
		return archive.UploadResult{}, err
	}
	result := archive.UploadResult{DryRun: true, FolderID: "folder", FileIDs: map[string]string{}}
	for _, file := range files {
		if file.DrivePath == "metadata.json" {
			u.metadataObserved = true
			body, err := os.ReadFile(file.LocalPath)
			if err != nil {
				u.t.Fatal(err)
			}
			if strings.Contains(string(body), "raw-drive-folder-id") || strings.Contains(string(body), "raw-google-client-secret") || strings.Contains(string(body), "raw-google-refresh-token") {
				u.t.Fatalf("metadata upload leaked raw archive secret: %s", string(body))
			}
		}
		result.FileIDs[file.DrivePath] = "id-" + file.DrivePath
	}
	return result, nil
}

func writeFinalArchiveForTest(t *testing.T, root, streamID string, modifiedAt time.Time) string {
	t.Helper()
	layout, err := archive.NewLayout(root, streamID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(layout.FinalDir(), 0o750); err != nil {
		t.Fatal(err)
	}
	for _, item := range []struct {
		path string
		body []byte
	}{
		{layout.FinalMP4(), []byte("mp4")},
		{layout.FinalMetadata(), []byte("{}\n")},
	} {
		if err := os.WriteFile(item.path, item.body, 0o640); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(item.path, modifiedAt, modifiedAt); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chtimes(layout.FinalDir(), modifiedAt, modifiedAt); err != nil {
		t.Fatal(err)
	}
	return layout.FinalDir()
}

type blockingUploader struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (u *blockingUploader) Upload(ctx context.Context, streamName, streamID string, startedAtJST time.Time, files []archive.File) (archive.UploadResult, error) {
	u.once.Do(func() {
		close(u.started)
	})
	select {
	case <-u.release:
	case <-ctx.Done():
		return archive.UploadResult{}, ctx.Err()
	}
	result := archive.UploadResult{DryRun: true, FolderID: "folder", FileIDs: map[string]string{}, Attempts: 1}
	for _, file := range files {
		result.FileIDs[file.DrivePath] = "id-" + file.DrivePath
	}
	return result, nil
}
