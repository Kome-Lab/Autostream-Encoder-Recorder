package archive

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

func EnsureDirNoSymlinks(rootDir, dir string) error {
	rootAbs, err := filepath.Abs(rootDir)
	if err != nil {
		return err
	}
	dirAbs, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(rootAbs, dirAbs)
	if err != nil {
		return err
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return errors.New("archive directory must stay under archive root")
	}
	if err := os.MkdirAll(rootAbs, 0o750); err != nil {
		return err
	}
	rootInfo, err := os.Lstat(rootAbs)
	if err != nil {
		return err
	}
	if rootInfo.Mode()&os.ModeSymlink != 0 {
		return errors.New("archive directory must not be a symlink")
	}
	if !rootInfo.IsDir() {
		return errors.New("archive root must be a directory")
	}
	if rel == "." {
		return nil
	}
	current := rootAbs
	for _, part := range strings.Split(rel, string(os.PathSeparator)) {
		if part == "" || part == "." {
			continue
		}
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if os.IsNotExist(err) {
			if mkdirErr := os.Mkdir(current, 0o750); mkdirErr != nil && !os.IsExist(mkdirErr) {
				return mkdirErr
			}
			info, err = os.Lstat(current)
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return errors.New("archive directory must not be a symlink")
		}
		if !info.IsDir() {
			return errors.New("archive path component must be a directory")
		}
	}
	return nil
}

func ReserveOutputFileNoSymlink(rootDir, path string) error {
	rootAbs, err := filepath.Abs(rootDir)
	if err != nil {
		return err
	}
	pathAbs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(rootAbs, pathAbs)
	if err != nil {
		return err
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return errors.New("archive output path must stay under archive root")
	}
	if err := EnsureDirNoSymlinks(rootAbs, filepath.Dir(pathAbs)); err != nil {
		return err
	}
	file, err := os.OpenFile(pathAbs, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err == nil {
		if closeErr := file.Close(); closeErr != nil {
			return closeErr
		}
		return ensureRegularOutputFile(pathAbs)
	}
	if !os.IsExist(err) {
		if _, lstatErr := os.Lstat(pathAbs); lstatErr == nil {
			return ensureRegularOutputFile(pathAbs)
		}
		return err
	}
	return ensureRegularOutputFile(pathAbs)
}

func ensureRegularOutputFile(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return errors.New("archive output path must not be a symlink")
	}
	if !info.Mode().IsRegular() {
		return errors.New("archive output path must be a regular file")
	}
	return nil
}

func RootFromSidecarPath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	dir := filepath.Dir(abs)
	streamDir := filepath.Dir(dir)
	root := filepath.Dir(streamDir)
	if root == "" || root == "." || root == string(os.PathSeparator) {
		return "", errors.New("archive sidecar path must stay under archive root")
	}
	return root, nil
}
