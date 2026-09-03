package lifecycle

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/example/autostream-encoder-recorder/internal/archive"
	"github.com/example/autostream-encoder-recorder/internal/ffmpeg"
)

func TestDryRunCreatesArchiveLayoutMetadataAndCommands(t *testing.T) {
	root := t.TempDir()
	runner := &ffmpeg.DryRunRunner{}
	manager := Manager{ArchiveRoot: root, FFmpegBin: "ffmpeg", Runner: runner, Uploader: archive.DryRunUploader{}}
	result, err := manager.DryRunToOutputTarget(context.Background(), StreamJob{
		StreamID: "stream-01", Name: "Morning Stream", InputURL: "srt://input.example.com:9000?mode=caller&passphrase=input-secret",
		RTMPURL: "rtmps://youtube.example.com/live2", StreamKey: "<YOUTUBE_STREAM_KEY>",
		StartedAt: time.Date(2026, 5, 29, 1, 2, 3, 0, time.UTC), DryRun: true,
	}, "rtmps://youtube.example.com/live2/<YOUTUBE_STREAM_KEY>")
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Metadata.Commands) != 2 {
		t.Fatalf("expected 2 ffmpeg commands, got %#v", result.Metadata.Commands)
	}
	for _, command := range result.Metadata.Commands {
		for _, arg := range command.Args {
			if arg == "<YOUTUBE_STREAM_KEY>" || arg == "rtmps://youtube.example.com/live2/<YOUTUBE_STREAM_KEY>" {
				t.Fatalf("stream key leaked in command metadata: %#v", result.Metadata.Commands)
			}
			if strings.Contains(arg, "input-secret") {
				t.Fatalf("input URL secret leaked in command metadata: %#v", result.Metadata.Commands)
			}
		}
	}
	if result.Metadata.StartedAtJST != "2026-05-29T10:02:03+09:00" {
		t.Fatalf("unexpected JST timestamp: %s", result.Metadata.StartedAtJST)
	}
	if result.Metadata.Upload.FileIDs["metadata.json"] == "" || result.Metadata.Upload.FileIDs["logs.jsonl"] == "" {
		t.Fatalf("expected dry-run upload IDs in metadata: %#v", result.Metadata.Upload)
	}
	if strings.Contains(result.Metadata.Archive["final_mp4"], root) || result.Metadata.Archive["final_mp4"] != "final.mp4" {
		t.Fatalf("archive metadata should expose logical artifact names only: %#v", result.Metadata.Archive)
	}
	for _, path := range []string{result.Layout.TmpMetadata(), result.Layout.FinalMetadata(), result.Layout.TmpLogs(), result.Layout.FinalLogs()} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("expected %s: %v", path, err)
		}
	}
	if filepath.Base(result.Layout.FinalMP4()) != "final.mp4" {
		t.Fatalf("unexpected final mp4: %s", result.Layout.FinalMP4())
	}
}

func TestDryRunRejectsUnsafeStreamID(t *testing.T) {
	manager := Manager{ArchiveRoot: t.TempDir(), Runner: &ffmpeg.DryRunRunner{}}
	if _, err := manager.DryRunToOutputTarget(context.Background(), StreamJob{StreamID: "../secret", Name: "bad"}, "rtmps://youtube.example.com/live2/key"); err == nil {
		t.Fatal("expected unsafe stream id to fail")
	}
}

func TestDryRunRejectsArchiveParentSymlinkWithoutCreatingOutside(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(root, "outside")
	if err := os.MkdirAll(outside, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "tmp")); err != nil {
		t.Skipf("directory symlink creation is not available in this environment: %v", err)
	}
	manager := Manager{ArchiveRoot: root, Runner: &ffmpeg.DryRunRunner{}}
	_, err := manager.DryRunToOutputTarget(context.Background(), StreamJob{
		StreamID: "stream-01",
		Name:     "Morning Stream",
		InputURL: "srt://input.example.com:9000?mode=caller",
		RTMPURL:  "rtmps://youtube.example.com/live2",
		DryRun:   true,
	}, "rtmps://youtube.example.com/live2/key")
	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("expected symlinked archive parent rejection, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(outside, "stream-01")); !os.IsNotExist(err) {
		t.Fatalf("symlink target should not receive dry-run artifacts, stat err=%v", err)
	}
}

func TestWriteFileNoSymlinkRejectsExistingSymlink(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(root, "outside.txt")
	if err := os.WriteFile(outside, []byte("outside"), 0o640); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "metadata.json")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlink creation is not available in this environment: %v", err)
	}
	if err := WriteFileNoSymlink(link, []byte("{}\n"), 0o640); err == nil {
		t.Fatal("expected symlink write to fail")
	}
	body, err := os.ReadFile(outside)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "outside" {
		t.Fatalf("symlink target was modified: %q", string(body))
	}
}

func TestPackageRunsRemuxAndUpload(t *testing.T) {
	root := t.TempDir()
	layout, err := archive.NewRunLayout(root, "stream-01", "run-01")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(layout.TmpDir(), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(layout.FinalDir(), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(layout.FinalMKV(), []byte("mkv"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(layout.FinalMP4(), []byte("mp4"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(layout.TmpLogs(), []byte("{}\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(layout.TmpCaptions(), []byte("WEBVTT\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(layout.TmpTranscript(), []byte("{}\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	runner := &ffmpeg.DryRunRunner{}
	checkingUploader := &metadataCheckingUploader{t: t}
	uploader := archive.RetryUploader{Inner: checkingUploader, Policy: archive.RetryPolicy{MaxAttempts: 2, Sleep: func(context.Context, time.Duration) error { return nil }}}
	manager := Manager{ArchiveRoot: root, FFmpegBin: "ffmpeg", Runner: runner, Uploader: uploader}
	result, err := manager.Package(context.Background(), PackageJob{StreamID: "stream-01", ArchiveRunID: "run-01", Name: "Morning Stream", StartedAt: time.Date(2026, 5, 29, 1, 2, 3, 0, time.UTC), DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if !checkingUploader.metadataObserved {
		t.Fatal("expected metadata.json to be uploaded after data files")
	}
	if len(result.Metadata.Commands) != 1 {
		t.Fatalf("expected remux command, got %#v", result.Metadata.Commands)
	}
	if result.Metadata.Upload.Attempts != 1 {
		t.Fatalf("unexpected upload attempts: %#v", result.Metadata.Upload)
	}
	if result.RemuxDurationMS < 0 {
		t.Fatalf("expected remux duration to be recorded: %#v", result)
	}
	if _, err := os.Stat(layout.FinalMetadata()); err != nil {
		t.Fatalf("expected metadata: %v", err)
	}
	metadataBody, err := os.ReadFile(layout.FinalMetadata())
	if err != nil {
		t.Fatal(err)
	}
	var metadata Metadata
	if err := json.Unmarshal(metadataBody, &metadata); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(metadataBody), root) || strings.Contains(string(metadataBody), `\tmp\`) || strings.Contains(string(metadataBody), `/tmp/`) {
		t.Fatalf("local archive path leaked in metadata.json: %s", string(metadataBody))
	}
	if metadata.Archive["recording_mkv"] != "final.mkv" || metadata.Archive["final_mp4"] != "final.mp4" {
		t.Fatalf("archive metadata should expose logical artifact names only: %#v", metadata.Archive)
	}
	if metadata.Extra["remux_duration_ms"] == nil {
		t.Fatalf("expected remux duration in metadata extra: %#v", metadata.Extra)
	}
	if strings.Contains(string(metadataBody), "id-final.mp4") || strings.Contains(string(metadataBody), "id-metadata.json") || strings.Contains(string(metadataBody), `"folder_id"`) || strings.Contains(string(metadataBody), `"file_ids"`) {
		t.Fatalf("metadata.json leaked raw Drive IDs: %s", string(metadataBody))
	}
	var metadataJSON map[string]any
	if err := json.Unmarshal(metadataBody, &metadataJSON); err != nil {
		t.Fatal(err)
	}
	uploadJSON, ok := metadataJSON["upload"].(map[string]any)
	if !ok {
		t.Fatalf("expected upload object in metadata: %s", string(metadataBody))
	}
	if uploadJSON["file_count"] != float64(5) {
		t.Fatalf("expected redacted upload file_count=5, got %#v in %s", uploadJSON["file_count"], string(metadataBody))
	}
	if _, ok := uploadJSON["file_fingerprints"].(map[string]any); !ok {
		t.Fatalf("expected file fingerprints instead of raw Drive file IDs: %s", string(metadataBody))
	}
	for _, path := range []string{layout.FinalCaptions(), layout.FinalTranscript()} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("expected copied optional artifact %s: %v", path, err)
		}
	}
}

func TestPackagePreservesMultipleRunsForSameStream(t *testing.T) {
	root := t.TempDir()
	legacy, err := archive.NewLayout(root, "stream-history")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(legacy.TmpDir(), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacy.FinalMKV(), []byte("mkv"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacy.TmpLogs(), []byte("{}\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	manager := Manager{ArchiveRoot: root, FFmpegBin: "ffmpeg", Runner: &ffmpeg.DryRunRunner{}, Uploader: archive.DryRunUploader{}}
	runs := []string{"20260818_140629_000000001_JST", "20260818_150629_000000002_JST"}
	for _, runID := range runs {
		result, err := manager.Package(context.Background(), PackageJob{
			StreamID: "stream-history", ArchiveRunID: runID, Name: "History", StartedAt: time.Now().UTC(), DryRun: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		if result.Layout.ArchiveRunID != runID {
			t.Fatalf("packaged run = %q, want %q", result.Layout.ArchiveRunID, runID)
		}
	}
	for _, runID := range runs {
		layout, err := archive.NewRunLayout(root, "stream-history", runID)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(layout.FinalMP4()); err != nil {
			t.Fatalf("run %s was overwritten or missing: %v", runID, err)
		}
	}
}

func TestPackageRequiresStartedAtForArchiveRun(t *testing.T) {
	manager := Manager{ArchiveRoot: t.TempDir()}
	_, err := manager.Package(context.Background(), PackageJob{
		StreamID:     "stream-history",
		ArchiveRunID: "20260818_140629_000000001_JST",
		Name:         "History",
	})
	if err == nil || !strings.Contains(err.Error(), "archive_run_id and started_at are required") {
		t.Fatalf("expected archive run started_at validation error, got %v", err)
	}
}

func TestPackageUsesRealFFmpegRemuxWhenAvailable(t *testing.T) {
	ffmpegBin, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skipf("ffmpeg is not available: %v", err)
	}
	ffprobeBin, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skipf("ffprobe is not available: %v", err)
	}
	root := t.TempDir()
	layout, err := archive.NewRunLayout(root, "stream-real-remux", "run-01")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(layout.TmpDir(), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(layout.FinalDir(), 0o750); err != nil {
		t.Fatal(err)
	}
	generate := exec.Command(ffmpegBin,
		"-hide_banner", "-y",
		"-f", "lavfi", "-i", "testsrc=size=160x90:rate=15",
		"-f", "lavfi", "-i", "sine=frequency=1000:sample_rate=48000",
		"-t", "1",
		"-c:v", "libx264", "-pix_fmt", "yuv420p",
		"-c:a", "aac",
		layout.FinalMKV(),
	)
	if output, err := generate.CombinedOutput(); err != nil {
		t.Fatalf("generate final.mkv failed: %v\n%s", err, string(output))
	}
	if err := os.WriteFile(layout.TmpLogs(), []byte("{}\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	manager := Manager{ArchiveRoot: root, FFmpegBin: ffmpegBin, Uploader: archive.DryRunUploader{}}
	result, err := manager.Package(context.Background(), PackageJob{StreamID: "stream-real-remux", ArchiveRunID: "run-01", Name: "Real Remux", StartedAt: time.Date(2026, 6, 11, 1, 2, 3, 0, time.UTC), DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(layout.FinalMP4())
	if err != nil {
		t.Fatalf("expected final.mp4 after package: %v", err)
	}
	if info.Size() == 0 {
		t.Fatal("final.mp4 is empty")
	}
	probe := exec.Command(ffprobeBin, "-v", "error", "-show_entries", "format=format_name,duration", "-of", "default=noprint_wrappers=1", layout.FinalMP4())
	probeOutput, err := probe.CombinedOutput()
	if err != nil {
		t.Fatalf("ffprobe final.mp4 failed: %v\n%s", err, string(probeOutput))
	}
	if !strings.Contains(string(probeOutput), "format_name=") || !strings.Contains(string(probeOutput), "duration=") {
		t.Fatalf("ffprobe output did not confirm media container: %s", string(probeOutput))
	}
	if result.RemuxDurationMS <= 0 {
		t.Fatalf("expected positive remux duration: %#v", result)
	}
}

func TestPackageFallsBackToPreviewWhenFinalMKVRemuxFails(t *testing.T) {
	ffmpegBin, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skipf("ffmpeg is not available: %v", err)
	}
	root := t.TempDir()
	layout, err := archive.NewRunLayout(root, "stream-preview-fallback", "run-01")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(layout.TmpDir(), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(layout.FinalDir(), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(layout.PreviewDir(), 0o750); err != nil {
		t.Fatal(err)
	}
	generate := exec.Command(ffmpegBin,
		"-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "testsrc=size=160x90:rate=15",
		"-t", "1",
		"-c:v", "libx264", "-pix_fmt", "yuv420p",
		"-f", "hls", "-hls_time", "0.5", "-hls_list_size", "0",
		"-hls_flags", "independent_segments",
		layout.PreviewPlaylist(),
	)
	if output, err := generate.CombinedOutput(); err != nil {
		t.Fatalf("generate preview HLS failed: %v\n%s", err, string(output))
	}
	// This is the truncated Matroska header produced when another tee slave
	// aborts the original recording process. The HLS output is still valid.
	if err := os.WriteFile(layout.FinalMKV(), []byte{0x1a, 0x45, 0xdf, 0xa3}, 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(layout.TmpLogs(), []byte("{}\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	manager := Manager{ArchiveRoot: root, FFmpegBin: ffmpegBin, Uploader: archive.DryRunUploader{}}
	result, err := manager.Package(context.Background(), PackageJob{
		StreamID:     "stream-preview-fallback",
		ArchiveRunID: "run-01",
		Name:         "Preview Fallback",
		StartedAt:    time.Date(2026, 6, 11, 1, 2, 3, 0, time.UTC),
		DryRun:       true,
	})
	if err != nil {
		t.Fatalf("expected HLS fallback package to succeed: %v", err)
	}
	info, err := os.Stat(layout.FinalMP4())
	if err != nil {
		t.Fatalf("expected final.mp4 after HLS fallback: %v", err)
	}
	if info.Size() == 0 {
		t.Fatal("final.mp4 is empty after HLS fallback")
	}
	if result.ArchiveSource != "hls_preview_fallback" || !result.Partial {
		t.Fatalf("HLS fallback provenance was not exposed: %#v", result)
	}
	if result.Metadata.Extra["archive_source"] != "hls_preview_fallback" || result.Metadata.Extra["archive_partial"] != true {
		t.Fatalf("HLS fallback metadata was not exposed: %#v", result.Metadata.Extra)
	}
	if result.RemuxDurationMS <= 0 {
		t.Fatalf("expected positive remux duration: %#v", result)
	}
}

func TestPackageUsesArchiveConfigUploaderFactory(t *testing.T) {
	root := t.TempDir()
	layout, err := archive.NewRunLayout(root, "stream-01", "run-01")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(layout.TmpDir(), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(layout.FinalDir(), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(layout.FinalMKV(), []byte("mkv"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(layout.FinalMP4(), []byte("mp4"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(layout.TmpLogs(), []byte("{}\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	var observed PackageJob
	manager := Manager{
		ArchiveRoot: root,
		FFmpegBin:   "ffmpeg",
		Runner:      &ffmpeg.DryRunRunner{},
		Uploader:    archive.MockUploader{Err: errors.New("default uploader must not be used")},
		UploaderForJob: func(job PackageJob) archive.ArchiveUploader {
			observed = job
			return archive.DryRunUploader{}
		},
	}
	_, err = manager.Package(context.Background(), PackageJob{
		StreamID:     "stream-01",
		ArchiveRunID: "run-01",
		Name:         "Morning Stream",
		StartedAt:    time.Date(2026, 6, 11, 1, 2, 3, 0, time.UTC),
		ArchiveConfig: ArchiveConfig{
			AuthMode:    "service_account",
			FolderID:    "drive-folder-id",
			SharedDrive: true,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if observed.ArchiveConfig.FolderID != "drive-folder-id" || !observed.ArchiveConfig.SharedDrive {
		t.Fatalf("archive config was not passed to uploader factory: %#v", observed.ArchiveConfig)
	}
}

func TestPackageUsesArchiveConfigFileNameForDriveUpload(t *testing.T) {
	root := t.TempDir()
	layout, err := archive.NewRunLayout(root, "stream-01", "run-01")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(layout.TmpDir(), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(layout.FinalDir(), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(layout.FinalMKV(), []byte("mkv"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(layout.FinalMP4(), []byte("mp4"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(layout.TmpLogs(), []byte("{}\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	uploader := &archiveFileNameCheckingUploader{t: t, want: "Council Meeting.mp4"}
	manager := Manager{
		ArchiveRoot: root,
		FFmpegBin:   "ffmpeg",
		Runner:      &ffmpeg.DryRunRunner{},
		UploaderForJob: func(PackageJob) archive.ArchiveUploader {
			return uploader
		},
	}
	if _, err := manager.Package(context.Background(), PackageJob{
		StreamID:     "stream-01",
		ArchiveRunID: "run-01",
		Name:         "Morning Stream",
		StartedAt:    time.Date(2026, 6, 11, 1, 2, 3, 0, time.UTC),
		ArchiveConfig: ArchiveConfig{
			ArchiveFileName: "Council Meeting",
		},
	}); err != nil {
		t.Fatal(err)
	}
	if !uploader.observed {
		t.Fatal("expected configured archive file name to be uploaded")
	}
}

func TestCleanupExpiredLocalArchivesPreservesMigratedRunlessData(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	oldDir := writeFinalArchiveForTest(t, root, "stream-old", now.Add(-45*24*time.Hour))
	recentDir := writeFinalArchiveForTest(t, root, "stream-recent", now.Add(-10*24*time.Hour))
	currentDir := writeFinalArchiveForTest(t, root, "stream-current", now.Add(-90*24*time.Hour))

	if err := cleanupExpiredLocalArchives(root, "stream-current", "", 30, now); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(oldDir); err != nil {
		t.Fatalf("expected migrated runless archive to remain: %v", err)
	}
	if _, err := os.Stat(recentDir); err != nil {
		t.Fatalf("expected recent archive to remain: %v", err)
	}
	if _, err := os.Stat(currentDir); err != nil {
		t.Fatalf("expected current archive to remain: %v", err)
	}
}

func TestCleanupExpiredLocalArchivesRemovesOldRunWithinCurrentStream(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)
	writeRun := func(runID string, modifiedAt time.Time) archive.Layout {
		t.Helper()
		layout, err := archive.NewRunLayout(root, "stream-current", runID)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(layout.FinalDir(), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(layout.FinalMP4(), []byte("mp4"), 0o640); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(layout.FinalMP4(), modifiedAt, modifiedAt); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(layout.FinalDir(), modifiedAt, modifiedAt); err != nil {
			t.Fatal(err)
		}
		return layout
	}
	oldRun := writeRun("run-old", now.Add(-45*24*time.Hour))
	currentRun := writeRun("run-current", now.Add(-90*24*time.Hour))
	if err := cleanupExpiredLocalArchives(root, "stream-current", "run-current", 30, now); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(oldRun.FinalDir()); !os.IsNotExist(err) {
		t.Fatalf("expired prior run should be removed, err=%v", err)
	}
	if _, err := os.Stat(currentRun.FinalDir()); err != nil {
		t.Fatalf("current run should remain: %v", err)
	}
}

func TestPackageAppliesLocalArchiveRetention(t *testing.T) {
	root := t.TempDir()
	oldLayout, err := archive.NewRunLayout(root, "stream-old", "run-old")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(oldLayout.FinalDir(), 0o750); err != nil {
		t.Fatal(err)
	}
	oldTime := time.Now().Add(-72 * time.Hour)
	if err := os.Chtimes(oldLayout.FinalDir(), oldTime, oldTime); err != nil {
		t.Fatal(err)
	}
	layout, err := archive.NewRunLayout(root, "stream-01", "run-01")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(layout.TmpDir(), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(layout.FinalDir(), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(layout.FinalMKV(), []byte("mkv"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(layout.TmpLogs(), []byte("{}\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	manager := Manager{ArchiveRoot: root, FFmpegBin: "ffmpeg", Runner: &ffmpeg.DryRunRunner{}, Uploader: archive.DryRunUploader{}}
	if _, err := manager.Package(context.Background(), PackageJob{StreamID: "stream-01", ArchiveRunID: "run-01", Name: "Morning Stream", StartedAt: time.Date(2026, 6, 11, 1, 2, 3, 0, time.UTC), ArchiveConfig: ArchiveConfig{RetentionDays: 1}}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(oldLayout.FinalDir()); !os.IsNotExist(err) {
		t.Fatalf("expected package retention cleanup to remove expired archive, err=%v", err)
	}
	if _, err := os.Stat(layout.FinalDir()); err != nil {
		t.Fatalf("expected current package archive to remain: %v", err)
	}
}

func TestArchiveConfigOAuth2BuildsGoogleDriveUploader(t *testing.T) {
	manager := Manager{}
	uploader := manager.uploaderForJob(PackageJob{
		StreamID: "stream-01",
		Name:     "Morning Stream",
		ArchiveConfig: ArchiveConfig{
			AuthMode:      "oauth2",
			FolderID:      "drive-folder-id",
			SharedDrive:   true,
			SharedDriveID: "shared-drive-01",
			ClientID:      "google-client-id",
			ClientSecret:  "google-client-secret",
			RefreshToken:  "google-refresh-token",
		},
	})
	retry, ok := uploader.(archive.RetryUploader)
	if !ok {
		t.Fatalf("expected retry uploader, got %#v", uploader)
	}
	driveUploader, ok := retry.Inner.(archive.GoogleDriveAPIUploader)
	if !ok {
		t.Fatalf("expected Google Drive uploader, got %#v", retry.Inner)
	}
	if driveUploader.Config.AuthMode != "oauth2" || driveUploader.Config.ClientSecret != "google-client-secret" || driveUploader.Config.RefreshToken != "google-refresh-token" || !driveUploader.Config.SharedDrive || driveUploader.Config.SharedDriveID != "shared-drive-01" {
		t.Fatalf("unexpected OAuth Drive config: %#v", driveUploader.Config)
	}
}

func TestArchiveConfigServiceAccountJSONIsRejectedByGoogleDriveConfig(t *testing.T) {
	manager := Manager{}
	uploader := manager.uploaderForJob(PackageJob{
		StreamID: "stream-01",
		Name:     "Morning Stream",
		ArchiveConfig: ArchiveConfig{
			AuthMode:           "service_account",
			FolderID:           "drive-folder-id",
			SharedDrive:        true,
			ServiceAccountJSON: `{"type":"service_account","client_email":"svc@example.com","private_key":"-----BEGIN PRIVATE KEY-----\n...\n-----END PRIVATE KEY-----\n"}`,
		},
	})
	driveUploader := googleDriveUploaderFromRetry(t, uploader)
	if driveUploader.Config.AuthMode != "service_account" || driveUploader.Config.ServiceAccountJSON != "" || driveUploader.Config.ApplicationCredential != "" || !driveUploader.Config.SharedDrive {
		t.Fatalf("unexpected unsupported Service Account Drive config: %#v", driveUploader.Config)
	}
	if err := driveUploader.Config.Validate(); err == nil {
		t.Fatal("expected service account config to be rejected")
	}
}

func TestArchiveConfigDoesNotFallBackToGoogleDriveEnvSecrets(t *testing.T) {
	t.Setenv("GOOGLE_DRIVE_AUTH_MODE", "oauth2")
	t.Setenv("GOOGLE_DRIVE_FOLDER_ID", "env-folder-id")
	t.Setenv("GDRIVE_BASE_PATH", "EnvBase")
	t.Setenv("GOOGLE_DRIVE_SHARED_DRIVE", "false")
	t.Setenv("GOOGLE_OAUTH_CLIENT_ID", "env-client-id")
	t.Setenv("GOOGLE_OAUTH_CLIENT_SECRET", "env-client-secret")
	t.Setenv("GOOGLE_OAUTH_REFRESH_TOKEN", "env-refresh-token")

	manager := Manager{}
	uploader := manager.uploaderForJob(PackageJob{
		StreamID: "stream-01",
		Name:     "Morning Stream",
		ArchiveConfig: ArchiveConfig{
			AuthMode:     "oauth2",
			FolderID:     "job-folder-id",
			SharedDrive:  true,
			ClientID:     "job-client-id",
			ClientSecret: "job-client-secret",
			RefreshToken: "job-refresh-token",
		},
	})
	driveUploader := googleDriveUploaderFromRetry(t, uploader)
	cfg := driveUploader.Config
	if cfg.FolderID != "job-folder-id" || !cfg.SharedDrive {
		t.Fatalf("archive config was not isolated from env folder/shared-drive values: %#v", cfg)
	}
	if cfg.ClientID != "job-client-id" || cfg.ClientSecret != "job-client-secret" || cfg.RefreshToken != "job-refresh-token" {
		t.Fatalf("archive config was not isolated from env OAuth secrets: %#v", cfg)
	}
}

func TestIncompleteArchiveConfigDoesNotUseGoogleDriveEnvSecrets(t *testing.T) {
	t.Setenv("GOOGLE_DRIVE_AUTH_MODE", "oauth2")
	t.Setenv("GOOGLE_DRIVE_FOLDER_ID", "env-folder-id")
	t.Setenv("GOOGLE_OAUTH_CLIENT_ID", "env-client-id")
	t.Setenv("GOOGLE_OAUTH_CLIENT_SECRET", "env-client-secret")
	t.Setenv("GOOGLE_OAUTH_REFRESH_TOKEN", "env-refresh-token")

	manager := Manager{}
	uploader := manager.uploaderForJob(PackageJob{
		StreamID: "stream-01",
		Name:     "Morning Stream",
		ArchiveConfig: ArchiveConfig{
			AuthMode: "oauth2",
			FolderID: "job-folder-id",
		},
	})
	driveUploader := googleDriveUploaderFromRetry(t, uploader)
	cfg := driveUploader.Config
	if cfg.ClientID != "" || cfg.ClientSecret != "" || cfg.RefreshToken != "" {
		t.Fatalf("incomplete Control Panel archive_config must not be completed from env secrets: %#v", cfg)
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected incomplete OAuth archive_config to fail validation")
	}
}

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

func TestArchiveMetadataIncludesSafeArchiveConfigOnly(t *testing.T) {
	root := t.TempDir()
	layout, err := archive.NewRunLayout(root, "stream-01", "run-01")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(layout.TmpDir(), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(layout.FinalDir(), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(layout.FinalMKV(), []byte("mkv"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(layout.TmpLogs(), []byte("{}\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	uploader := &archiveConfigMetadataCheckingUploader{t: t}
	manager := Manager{
		ArchiveRoot: root,
		FFmpegBin:   "ffmpeg",
		Runner:      &ffmpeg.DryRunRunner{},
		UploaderForJob: func(PackageJob) archive.ArchiveUploader {
			return uploader
		},
	}
	_, err = manager.Package(context.Background(), PackageJob{
		StreamID:     "stream-01",
		ArchiveRunID: "run-01",
		Name:         "Morning Stream",
		StartedAt:    time.Date(2026, 6, 11, 1, 2, 3, 0, time.UTC),
		ArchiveConfig: ArchiveConfig{
			DriveDestinationID:     "drive-destination-01",
			ArchiveProfileID:       "archive-profile-01",
			AuthMode:               "oauth2",
			OAuthAccountID:         "oauth-account-01",
			OAuthProviderID:        "oauth-provider-01",
			FolderID:               "raw-drive-folder-id",
			FolderIDSecretName:     "drive_destination:drive-destination-01:folder_id",
			SharedDrive:            true,
			SharedDriveID:          "raw-shared-drive-id",
			ArchiveFileName:        "Council Meeting.mp4",
			ClientID:               "google-client-id",
			ClientSecret:           "raw-google-client-secret",
			ClientSecretSecretName: "oauth_provider:oauth-provider-01:client_secret",
			RefreshToken:           "raw-google-refresh-token",
			RefreshTokenSecretName: "oauth_account:oauth-account-01:refresh_token",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !uploader.metadataObserved {
		t.Fatal("expected metadata.json to be uploaded")
	}
	metadataBody, err := os.ReadFile(layout.FinalMetadata())
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"raw-drive-folder-id", "raw-shared-drive-id", "raw-service-account-private-key", "raw-google-client-secret", "raw-google-refresh-token"} {
		if strings.Contains(string(metadataBody), secret) {
			t.Fatalf("archive metadata leaked raw secret %q: %s", secret, string(metadataBody))
		}
	}
	var metadata Metadata
	if err := json.Unmarshal(metadataBody, &metadata); err != nil {
		t.Fatal(err)
	}
	cfg, ok := metadata.Extra["archive_config"].(map[string]any)
	if !ok {
		t.Fatalf("expected archive_config summary in metadata extra: %#v", metadata.Extra)
	}
	if cfg["drive_destination_id"] != "drive-destination-01" || cfg["auth_mode"] != "oauth2" || cfg["shared_drive"] != true {
		t.Fatalf("unexpected archive config summary: %#v", cfg)
	}
	for _, key := range []string{"folder_id_configured", "client_secret_configured", "refresh_token_configured"} {
		if cfg[key] != true {
			t.Fatalf("expected %s in archive config summary: %#v", key, cfg)
		}
	}
	if _, ok := cfg["service_account_json_configured"]; ok {
		t.Fatalf("service account summary should not be emitted: %#v", cfg)
	}
	if cfg["shared_drive_id_configured"] != true || cfg["archive_file_name"] != "Council Meeting.mp4" {
		t.Fatalf("expected archive file/shared drive summary in metadata: %#v", cfg)
	}
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

func TestPackageRequiresFinalMKV(t *testing.T) {
	manager := Manager{ArchiveRoot: t.TempDir(), Runner: &ffmpeg.DryRunRunner{}, Uploader: archive.DryRunUploader{}}
	if _, err := manager.Package(context.Background(), PackageJob{StreamID: "stream-01", ArchiveRunID: "run-01", Name: "Morning Stream", StartedAt: time.Date(2026, 6, 11, 1, 2, 3, 0, time.UTC)}); err == nil {
		t.Fatal("expected missing final.mkv to fail")
	} else if ErrorPhase(err) != "input" {
		t.Fatalf("expected input failure phase, got %q: %v", ErrorPhase(err), err)
	}
}

func TestPackageClassifiesUploadFailure(t *testing.T) {
	root := t.TempDir()
	layout, err := archive.NewRunLayout(root, "stream-01", "run-01")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(layout.TmpDir(), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(layout.FinalDir(), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(layout.FinalMKV(), []byte("mkv"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(layout.FinalMP4(), []byte("mp4"), 0o640); err != nil {
		t.Fatal(err)
	}
	manager := Manager{ArchiveRoot: root, Runner: &ffmpeg.DryRunRunner{}, Uploader: archive.MockUploader{Err: errors.New("https://example.com/secret-token")}}
	if _, err := manager.Package(context.Background(), PackageJob{StreamID: "stream-01", ArchiveRunID: "run-01", Name: "Morning Stream", StartedAt: time.Date(2026, 6, 11, 1, 2, 3, 0, time.UTC), DryRun: true}); err == nil {
		t.Fatal("expected upload failure")
	} else if ErrorPhase(err) != "upload" {
		t.Fatalf("expected upload failure phase, got %q: %v", ErrorPhase(err), err)
	}
}

func TestPackageRejectsConcurrentSameStream(t *testing.T) {
	root := t.TempDir()
	layout, err := archive.NewRunLayout(root, "stream-01", "run-01")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(layout.TmpDir(), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(layout.FinalDir(), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(layout.FinalMKV(), []byte("mkv"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(layout.FinalMP4(), []byte("mp4"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(layout.TmpLogs(), []byte("{}\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	uploader := &blockingUploader{started: make(chan struct{}), release: make(chan struct{})}
	manager := Manager{ArchiveRoot: root, Runner: &ffmpeg.DryRunRunner{}, Uploader: uploader}
	done := make(chan error, 1)
	go func() {
		_, err := manager.Package(context.Background(), PackageJob{StreamID: "stream-01", ArchiveRunID: "run-01", Name: "Morning Stream", StartedAt: time.Date(2026, 6, 11, 1, 2, 3, 0, time.UTC), DryRun: true})
		done <- err
	}()
	<-uploader.started
	if _, err := manager.Package(context.Background(), PackageJob{StreamID: "stream-01", ArchiveRunID: "run-01", Name: "Morning Stream", StartedAt: time.Date(2026, 6, 11, 1, 2, 3, 0, time.UTC), DryRun: true}); !errors.Is(err, ErrPackageInProgress) {
		t.Fatalf("expected package-in-progress rejection, got %v", err)
	}
	close(uploader.release)
	if err := <-done; err != nil {
		t.Fatalf("first package should complete after release: %v", err)
	}
}

func TestPackageRejectsFinalMKVSymlink(t *testing.T) {
	root := t.TempDir()
	layout, err := archive.NewRunLayout(root, "stream-01", "run-01")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(layout.TmpDir(), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(layout.FinalDir(), 0o750); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(root, "outside.mkv")
	if err := os.WriteFile(outside, []byte("outside"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, layout.FinalMKV()); err != nil {
		t.Skipf("symlink creation is not available in this environment: %v", err)
	}
	if err := os.WriteFile(layout.FinalMP4(), []byte("mp4"), 0o640); err != nil {
		t.Fatal(err)
	}
	manager := Manager{ArchiveRoot: root, Runner: &ffmpeg.DryRunRunner{}, Uploader: archive.DryRunUploader{}}
	if _, err := manager.Package(context.Background(), PackageJob{StreamID: "stream-01", ArchiveRunID: "run-01", Name: "Morning Stream", StartedAt: time.Date(2026, 6, 11, 1, 2, 3, 0, time.UTC), DryRun: true}); err == nil {
		t.Fatal("expected final.mkv symlink to be rejected")
	} else if ErrorPhase(err) != "input" {
		t.Fatalf("expected input failure phase, got %q: %v", ErrorPhase(err), err)
	}
}

func TestPackageRejectsExistingFinalMP4SymlinkBeforeRemux(t *testing.T) {
	root := t.TempDir()
	layout, err := archive.NewRunLayout(root, "stream-01", "run-01")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(layout.TmpDir(), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(layout.FinalDir(), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(layout.FinalMKV(), []byte("mkv"), 0o640); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(root, "outside.mp4")
	if err := os.WriteFile(outside, []byte("outside"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, layout.FinalMP4()); err != nil {
		t.Skipf("symlink creation is not available in this environment: %v", err)
	}
	manager := Manager{ArchiveRoot: root, Runner: &ffmpeg.DryRunRunner{}, Uploader: archive.DryRunUploader{}}
	if _, err := manager.Package(context.Background(), PackageJob{StreamID: "stream-01", ArchiveRunID: "run-01", Name: "Morning Stream", StartedAt: time.Date(2026, 6, 11, 1, 2, 3, 0, time.UTC), DryRun: true}); err == nil {
		t.Fatal("expected final.mp4 symlink to be rejected")
	} else if ErrorPhase(err) != "remux" {
		t.Fatalf("expected remux failure phase, got %q: %v", ErrorPhase(err), err)
	}
	if body, err := os.ReadFile(outside); err != nil {
		t.Fatal(err)
	} else if string(body) != "outside" {
		t.Fatalf("symlink target was modified: %q", string(body))
	}
}

func TestPackageRejectsFinalDirSymlink(t *testing.T) {
	root := t.TempDir()
	layout, err := archive.NewRunLayout(root, "stream-01", "run-01")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(layout.TmpDir(), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(layout.FinalMKV(), []byte("mkv"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "final"), 0o750); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(root, "outside-final")
	if err := os.MkdirAll(outside, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, layout.FinalDir()); err != nil {
		t.Skipf("directory symlink creation is not available in this environment: %v", err)
	}
	manager := Manager{ArchiveRoot: root, Runner: &ffmpeg.DryRunRunner{}, Uploader: archive.DryRunUploader{}}
	if _, err := manager.Package(context.Background(), PackageJob{StreamID: "stream-01", ArchiveRunID: "run-01", Name: "Morning Stream", StartedAt: time.Date(2026, 6, 11, 1, 2, 3, 0, time.UTC), DryRun: true}); err == nil {
		t.Fatal("expected final directory symlink to be rejected")
	} else if ErrorPhase(err) != "package" {
		t.Fatalf("expected package failure phase, got %q: %v", ErrorPhase(err), err)
	}
	if _, err := os.Stat(filepath.Join(outside, "final.mp4")); !os.IsNotExist(err) {
		t.Fatalf("symlink target should not receive final.mp4, stat err=%v", err)
	}
}

func TestPackageRejectsTmpDirSymlink(t *testing.T) {
	root := t.TempDir()
	layout, err := archive.NewRunLayout(root, "stream-01", "run-01")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "tmp"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(layout.FinalDir(), 0o750); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(root, "outside-tmp")
	if err := os.MkdirAll(outside, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "final.mkv"), []byte("mkv"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, layout.TmpDir()); err != nil {
		t.Skipf("directory symlink creation is not available in this environment: %v", err)
	}
	manager := Manager{ArchiveRoot: root, Runner: &ffmpeg.DryRunRunner{}, Uploader: archive.DryRunUploader{}}
	if _, err := manager.Package(context.Background(), PackageJob{StreamID: "stream-01", ArchiveRunID: "run-01", Name: "Morning Stream", StartedAt: time.Date(2026, 6, 11, 1, 2, 3, 0, time.UTC), DryRun: true}); err == nil {
		t.Fatal("expected tmp directory symlink to be rejected")
	} else if ErrorPhase(err) != "input" {
		t.Fatalf("expected input failure phase, got %q: %v", ErrorPhase(err), err)
	}
}

func TestPackageRejectsTmpLogSymlink(t *testing.T) {
	root := t.TempDir()
	layout, err := archive.NewRunLayout(root, "stream-01", "run-01")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(layout.TmpDir(), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(layout.FinalDir(), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(layout.FinalMKV(), []byte("mkv"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(layout.FinalMP4(), []byte("mp4"), 0o640); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(root, "outside.log")
	if err := os.WriteFile(outside, []byte("outside"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, layout.TmpLogs()); err != nil {
		t.Skipf("symlink creation is not available in this environment: %v", err)
	}
	manager := Manager{ArchiveRoot: root, Runner: &ffmpeg.DryRunRunner{}, Uploader: archive.DryRunUploader{}}
	if _, err := manager.Package(context.Background(), PackageJob{StreamID: "stream-01", ArchiveRunID: "run-01", Name: "Morning Stream", StartedAt: time.Date(2026, 6, 11, 1, 2, 3, 0, time.UTC), DryRun: true}); err == nil {
		t.Fatal("expected tmp logs symlink to be rejected")
	} else if ErrorPhase(err) != "package" {
		t.Fatalf("expected package failure phase, got %q: %v", ErrorPhase(err), err)
	}
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
