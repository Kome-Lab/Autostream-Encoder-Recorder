package archive

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestIsPreviewFileName(t *testing.T) {
	tests := map[string]bool{
		"index.m3u8":         true,
		"segment-000000.ts":  true,
		"segment-999999.ts":  true,
		"segment-00000.ts":   false,
		"segment-0000000.ts": false,
		"segment-00000a.ts":  false,
		"segment-000000.m4s": false,
		"other.m3u8":         false,
		"../index.m3u8":      false,
		"preview/index.m3u8": false,
	}
	for name, want := range tests {
		if got := IsPreviewFileName(name); got != want {
			t.Errorf("IsPreviewFileName(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestPreparePreviewDirCleansKnownRegularFiles(t *testing.T) {
	layout, err := NewLayout(t.TempDir(), "stream-01")
	if err != nil {
		t.Fatal(err)
	}
	if err := EnsureDirNoSymlinks(layout.RootDir, layout.PreviewDir()); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"index.m3u8", "index.m3u8.tmp", "segment-000000.ts", "segment-000001.ts.tmp"} {
		if err := os.WriteFile(filepath.Join(layout.PreviewDir(), name), []byte(name), 0o640); err != nil {
			t.Fatal(err)
		}
	}
	if err := PreparePreviewDir(layout); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(layout.PreviewDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("expected clean preview directory, got %#v", entries)
	}
}

func TestPreparePreviewDirRejectsUnexpectedEntry(t *testing.T) {
	layout, err := NewLayout(t.TempDir(), "stream-01")
	if err != nil {
		t.Fatal(err)
	}
	if err := EnsureDirNoSymlinks(layout.RootDir, layout.PreviewDir()); err != nil {
		t.Fatal(err)
	}
	unexpected := filepath.Join(layout.PreviewDir(), "do-not-delete.txt")
	if err := os.WriteFile(unexpected, []byte("keep"), 0o640); err != nil {
		t.Fatal(err)
	}
	err = PreparePreviewDir(layout)
	if err == nil || !strings.Contains(err.Error(), "unexpected") {
		t.Fatalf("expected unexpected entry rejection, got %v", err)
	}
	if body, readErr := os.ReadFile(unexpected); readErr != nil || string(body) != "keep" {
		t.Fatalf("unexpected entry must not be modified: body=%q err=%v", body, readErr)
	}
}

func TestPreparePreviewDirRejectsDirectorySymlink(t *testing.T) {
	layout, err := NewLayout(t.TempDir(), "stream-01")
	if err != nil {
		t.Fatal(err)
	}
	if err := EnsureDirNoSymlinks(layout.RootDir, layout.TmpDir()); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(layout.RootDir, "outside")
	if err := os.MkdirAll(outside, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, layout.PreviewDir()); err != nil {
		t.Skipf("symlink creation is not available in this environment: %v", err)
	}
	if err := PreparePreviewDir(layout); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("expected preview directory symlink rejection, got %v", err)
	}
}

func TestPreparePreviewDirRejectsFileSymlink(t *testing.T) {
	layout, err := NewLayout(t.TempDir(), "stream-01")
	if err != nil {
		t.Fatal(err)
	}
	if err := EnsureDirNoSymlinks(layout.RootDir, layout.PreviewDir()); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(layout.RootDir, "outside.m3u8")
	if err := os.WriteFile(outside, []byte("outside"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, layout.PreviewPlaylist()); err != nil {
		t.Skipf("symlink creation is not available in this environment: %v", err)
	}
	if err := PreparePreviewDir(layout); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("expected preview file symlink rejection, got %v", err)
	}
	if body, readErr := os.ReadFile(outside); readErr != nil || string(body) != "outside" {
		t.Fatalf("symlink target must not be modified: body=%q err=%v", body, readErr)
	}
}

func TestOpenPreviewFileReturnsOpenedRegularFile(t *testing.T) {
	layout, err := NewLayout(t.TempDir(), "stream-01")
	if err != nil {
		t.Fatal(err)
	}
	if err := PreparePreviewDir(layout); err != nil {
		t.Fatal(err)
	}
	want := []byte("#EXTM3U\n")
	if err := os.WriteFile(layout.PreviewPlaylist(), want, 0o640); err != nil {
		t.Fatal(err)
	}
	file, info, err := OpenPreviewFile(layout, PreviewPlaylistName)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if !info.Mode().IsRegular() {
		t.Fatalf("expected regular file, got %s", info.Mode())
	}
	body := make([]byte, len(want))
	if _, err := file.Read(body); err != nil {
		t.Fatal(err)
	}
	if string(body) != string(want) {
		t.Fatalf("unexpected body: %q", body)
	}
}

func TestOpenPreviewFileRejectsUnsafeNameAndSymlink(t *testing.T) {
	layout, err := NewLayout(t.TempDir(), "stream-01")
	if err != nil {
		t.Fatal(err)
	}
	if err := PreparePreviewDir(layout); err != nil {
		t.Fatal(err)
	}
	if _, _, err := OpenPreviewFile(layout, "../index.m3u8"); err == nil {
		t.Fatal("expected traversal name rejection")
	}
	outside := filepath.Join(layout.RootDir, "outside.ts")
	if err := os.WriteFile(outside, []byte("outside"), 0o640); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(layout.PreviewDir(), "segment-000000.ts")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlink creation is not available in this environment: %v", err)
	}
	if _, _, err := OpenPreviewFile(layout, "segment-000000.ts"); err == nil || !strings.Contains(err.Error(), "regular file") {
		t.Fatalf("expected preview symlink rejection, got %v", err)
	}
}
