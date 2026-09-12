package lifecycle

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/example/autostream-encoder-recorder/internal/archive"
	"github.com/example/autostream-encoder-recorder/internal/ffmpeg"
)

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
