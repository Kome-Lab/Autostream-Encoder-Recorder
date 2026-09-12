package lifecycle

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/example/autostream-encoder-recorder/internal/archive"
	"github.com/example/autostream-encoder-recorder/internal/ffmpeg"
)

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
