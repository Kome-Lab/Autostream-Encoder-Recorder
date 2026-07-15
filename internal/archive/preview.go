package archive

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

const PreviewPlaylistName = "index.m3u8"

func IsPreviewFileName(name string) bool {
	if name == PreviewPlaylistName {
		return true
	}
	const prefix = "segment-"
	const suffix = ".ts"
	if len(name) != len(prefix)+6+len(suffix) || !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, suffix) {
		return false
	}
	for _, char := range name[len(prefix) : len(prefix)+6] {
		if char < '0' || char > '9' {
			return false
		}
	}
	return true
}

func PreparePreviewDir(layout Layout) error {
	validated, err := NewLayout(layout.RootDir, layout.StreamID)
	if err != nil {
		return err
	}
	if err := EnsureDirNoSymlinks(validated.RootDir, validated.PreviewDir()); err != nil {
		return err
	}
	entries, err := os.ReadDir(validated.PreviewDir())
	if err != nil {
		return err
	}
	for _, entry := range entries {
		name := entry.Name()
		if !IsPreviewFileName(name) && !(strings.HasSuffix(name, ".tmp") && IsPreviewFileName(strings.TrimSuffix(name, ".tmp"))) {
			return errors.New("preview directory contains an unexpected entry")
		}
		path := filepath.Join(validated.PreviewDir(), name)
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return errors.New("preview path must not be a symlink")
		}
		if !info.Mode().IsRegular() {
			return errors.New("preview path must be a regular file")
		}
		if err := os.Remove(path); err != nil {
			return err
		}
	}
	return validateExistingDirsNoSymlinks(validated.RootDir, validated.PreviewDir())
}

func OpenPreviewFile(layout Layout, name string) (*os.File, os.FileInfo, error) {
	validated, err := NewLayout(layout.RootDir, layout.StreamID)
	if err != nil {
		return nil, nil, err
	}
	if !IsPreviewFileName(name) {
		return nil, nil, errors.New("unsafe preview file name")
	}
	return openRegularFileNoSymlinks(validated.RootDir, filepath.Join(validated.PreviewDir(), name))
}

func openRegularFileNoSymlinks(rootDir, path string) (*os.File, os.FileInfo, error) {
	rootAbs, pathAbs, err := pathUnderRoot(rootDir, path)
	if err != nil {
		return nil, nil, err
	}
	parent := filepath.Dir(pathAbs)
	if err := validateExistingDirsNoSymlinks(rootAbs, parent); err != nil {
		return nil, nil, err
	}
	before, err := os.Lstat(pathAbs)
	if err != nil {
		return nil, nil, err
	}
	if before.Mode()&os.ModeSymlink != 0 || !before.Mode().IsRegular() {
		return nil, nil, errors.New("preview path must be a regular file")
	}
	file, err := os.Open(pathAbs)
	if err != nil {
		return nil, nil, err
	}
	closeOnError := func(err error) (*os.File, os.FileInfo, error) {
		_ = file.Close()
		return nil, nil, err
	}
	opened, err := file.Stat()
	if err != nil {
		return closeOnError(err)
	}
	if !opened.Mode().IsRegular() || !os.SameFile(before, opened) {
		return closeOnError(errors.New("preview path changed while opening"))
	}
	after, err := os.Lstat(pathAbs)
	if err != nil {
		return closeOnError(err)
	}
	if after.Mode()&os.ModeSymlink != 0 || !after.Mode().IsRegular() || !os.SameFile(opened, after) {
		return closeOnError(errors.New("preview path changed while opening"))
	}
	if err := validateExistingDirsNoSymlinks(rootAbs, parent); err != nil {
		return closeOnError(err)
	}
	return file, opened, nil
}

func pathUnderRoot(rootDir, path string) (string, string, error) {
	rootAbs, err := filepath.Abs(rootDir)
	if err != nil {
		return "", "", err
	}
	pathAbs, err := filepath.Abs(path)
	if err != nil {
		return "", "", err
	}
	rel, err := filepath.Rel(rootAbs, pathAbs)
	if err != nil {
		return "", "", err
	}
	if rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return "", "", errors.New("preview path must stay under archive root")
	}
	return rootAbs, pathAbs, nil
}

func validateExistingDirsNoSymlinks(rootDir, dir string) error {
	rootAbs, dirAbs, err := pathUnderRoot(rootDir, dir)
	if err != nil {
		return err
	}
	rootInfo, err := os.Lstat(rootAbs)
	if err != nil {
		return err
	}
	if rootInfo.Mode()&os.ModeSymlink != 0 || !rootInfo.IsDir() {
		return errors.New("archive root must be a non-symlink directory")
	}
	rel, err := filepath.Rel(rootAbs, dirAbs)
	if err != nil {
		return err
	}
	current := rootAbs
	for _, part := range strings.Split(rel, string(os.PathSeparator)) {
		if part == "" || part == "." {
			continue
		}
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return errors.New("preview directory must not contain symlinks")
		}
		if !info.IsDir() {
			return errors.New("preview parent path must be a directory")
		}
	}
	return nil
}
