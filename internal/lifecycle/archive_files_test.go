package lifecycle

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/example/autostream-encoder-recorder/internal/archive"
	"github.com/example/autostream-encoder-recorder/internal/ffmpeg"
)

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
	parent := filepath.Dir(layout.FinalDir())
	if err := os.MkdirAll(parent, 0o750); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(parent); err != nil {
		t.Fatal(err)
	} else if !info.IsDir() {
		t.Fatal("final directory parent is not a directory")
	}
	if _, err := os.Lstat(layout.FinalDir()); !os.IsNotExist(err) {
		t.Fatalf("final directory leaf must not exist before symlink creation, lstat err=%v", err)
	}
	outside := filepath.Join(root, "outside-final")
	if err := os.MkdirAll(outside, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, layout.FinalDir()); err != nil {
		if runtime.GOOS == "windows" && errors.Is(err, syscall.Errno(1314)) { // ERROR_PRIVILEGE_NOT_HELD
			t.Skipf("Windows directory symlink privilege is unavailable: %v", err)
		}
		t.Fatalf("create final directory symlink: %v", err)
	}
	if info, err := os.Lstat(layout.FinalDir()); err != nil {
		t.Fatal(err)
	} else if info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("final directory leaf is not a symlink")
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
