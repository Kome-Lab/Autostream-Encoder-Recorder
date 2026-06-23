package archive

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestEnsureDirNoSymlinksRejectsDirectorySymlink(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "tmp"), 0o750); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(root, "outside")
	if err := os.MkdirAll(outside, 0o750); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "tmp", "stream-01")
	if err := os.Symlink(outside, link); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("directory symlink creation requires privileges on this Windows host: %v", err)
		}
		t.Fatal(err)
	}
	err := EnsureDirNoSymlinks(root, link)
	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("expected directory symlink rejection, got %v", err)
	}
}

func TestEnsureDirNoSymlinksDoesNotCreateInsideSymlinkedParent(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(root, "outside")
	if err := os.MkdirAll(outside, 0o750); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "tmp")
	if err := os.Symlink(outside, link); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("directory symlink creation requires privileges on this Windows host: %v", err)
		}
		t.Fatal(err)
	}
	err := EnsureDirNoSymlinks(root, filepath.Join(link, "stream-01"))
	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("expected symlinked parent rejection, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(outside, "stream-01")); !os.IsNotExist(err) {
		t.Fatalf("symlink target should not receive child directory, stat err=%v", err)
	}
}

func TestEnsureDirNoSymlinksRejectsOutsideRoot(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	err := EnsureDirNoSymlinks(root, filepath.Join(outside, "stream-01"))
	if err == nil || !strings.Contains(err.Error(), "archive root") {
		t.Fatalf("expected outside-root rejection, got %v", err)
	}
}

func TestReserveOutputFileNoSymlinkCreatesRegularFile(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "tmp", "stream-01", "final.mkv")
	if err := ReserveOutputFileNoSymlink(root, path); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !info.Mode().IsRegular() {
		t.Fatalf("expected regular file, got %s", info.Mode())
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("reserved output file is too permissive: %s", info.Mode().Perm())
	}
}

func TestReserveOutputFileNoSymlinkRejectsOutsideRoot(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "final.mkv")
	err := ReserveOutputFileNoSymlink(root, outside)
	if err == nil || !strings.Contains(err.Error(), "archive root") {
		t.Fatalf("expected outside-root rejection, got %v", err)
	}
}

func TestReserveOutputFileNoSymlinkRejectsDirectory(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "tmp", "stream-01", "final.mkv")
	if err := os.MkdirAll(path, 0o750); err != nil {
		t.Fatal(err)
	}
	err := ReserveOutputFileNoSymlink(root, path)
	if err == nil || !strings.Contains(err.Error(), "regular file") {
		t.Fatalf("expected directory output rejection, got %v", err)
	}
}

func TestReserveOutputFileNoSymlinkRejectsSymlink(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "tmp", "stream-01"), 0o750); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(root, "outside.mkv")
	if err := os.WriteFile(outside, []byte("outside"), 0o640); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "tmp", "stream-01", "final.mkv")
	if err := os.Symlink(outside, link); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("file symlink creation requires privileges on this Windows host: %v", err)
		}
		t.Fatal(err)
	}
	err := ReserveOutputFileNoSymlink(root, link)
	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("expected symlink output rejection, got %v", err)
	}
}
