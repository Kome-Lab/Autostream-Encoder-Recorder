package control

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/example/autostream-encoder-recorder/internal/archive"
)

func TestArchiveArtifactsReturnsOnlyExistingLogicalArtifacts(t *testing.T) {
	root := t.TempDir()
	layout, err := archive.NewLayout(root, "stream-01")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(layout.FinalDir(), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(layout.FinalMP4(), []byte("video"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(layout.FinalMetadata(), []byte("{}"), 0o640); err != nil {
		t.Fatal(err)
	}
	artifacts := ArchiveArtifacts(layout)
	if len(artifacts) != 2 {
		t.Fatalf("unexpected artifacts: %#v", artifacts)
	}
	for _, artifact := range artifacts {
		if filepath.IsAbs(artifact.RelativePath) || artifact.RelativePath == "" {
			t.Fatalf("artifact path is not logical and relative: %#v", artifact)
		}
	}
}

func TestArchiveArtifactsUsesRunScopedLogicalPath(t *testing.T) {
	layout, err := archive.NewRunLayout(t.TempDir(), "stream-01", "run-01")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(layout.FinalDir(), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(layout.FinalMP4(), []byte("mp4"), 0o640); err != nil {
		t.Fatal(err)
	}
	artifacts := ArchiveArtifacts(layout)
	if len(artifacts) != 1 || artifacts[0].RelativePath != "final/stream-01/run-01/final.mp4" {
		t.Fatalf("run-scoped artifacts = %#v", artifacts)
	}
}

func TestArchiveArtifactsSkipsSymlinks(t *testing.T) {
	root := t.TempDir()
	layout, err := archive.NewLayout(root, "stream-01")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(layout.FinalDir(), 0o750); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(root, "outside.mp4")
	if err := os.WriteFile(outside, []byte("video"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, layout.FinalMP4()); err != nil {
		t.Skipf("symlink creation is not available in this environment: %v", err)
	}
	if artifacts := ArchiveArtifacts(layout); len(artifacts) != 0 {
		t.Fatalf("symlink artifact should be skipped: %#v", artifacts)
	}
}
