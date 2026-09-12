package lifecycle

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
)

func copyIfExists(src, dst string) error {
	if _, err := safeRegularFileInfo(src); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	return copyFile(src, dst)
}

func copyFile(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		return err
	}
	body, err := readFileNoSymlink(src)
	if err != nil {
		return err
	}
	return writeFileNoSymlink(dst, body, 0o640)
}

func createRemuxOutput(rootDir, dir string) (string, func(), error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", func() {}, err
	}
	if err := rejectArchiveDirSymlinks(rootDir, dir); err != nil {
		return "", func() {}, err
	}
	file, err := os.CreateTemp(dir, ".final-remux-*.mp4")
	if err != nil {
		return "", func() {}, err
	}
	path := file.Name()
	if err := file.Chmod(0o640); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return "", func() {}, err
	}
	if err := verifyOpenRegularFile(file, nil); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return "", func() {}, err
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(path)
		return "", func() {}, err
	}
	return path, func() { _ = os.Remove(path) }, nil
}

func replaceFileNoSymlink(src, dst string) error {
	if _, err := safeRegularFileInfo(src); err != nil {
		return err
	}
	if err := rejectExistingSymlink(dst); err != nil {
		return err
	}
	if err := os.Remove(dst); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := os.Rename(src, dst); err != nil {
		return err
	}
	if _, err := safeRegularFileInfo(dst); err != nil {
		return err
	}
	return nil
}

func safeRegularFileInfo(path string) (os.FileInfo, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("archive path must not be a symlink")
	}
	if info.IsDir() {
		return nil, errors.New("archive path must be a regular file")
	}
	return info, nil
}

func rejectExistingSymlink(path string) error {
	info, err := os.Lstat(path)
	if err == nil && info.Mode()&os.ModeSymlink != 0 {
		return errors.New("archive path must not be a symlink")
	}
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

func rejectArchiveDirSymlinks(rootDir, dir string) error {
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

func readFileNoSymlink(path string) ([]byte, error) {
	info, err := safeRegularFileInfo(path)
	if err != nil {
		return nil, err
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	if err := verifyOpenRegularFile(file, info); err != nil {
		return nil, err
	}
	return io.ReadAll(file)
}

func writeFileNoSymlink(path string, body []byte, perm os.FileMode) error {
	return WriteFileNoSymlink(path, body, perm)
}

func WriteFileNoSymlink(path string, body []byte, perm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	var file *os.File
	if err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return errors.New("archive path must not be a symlink")
		}
		if info.IsDir() {
			return errors.New("archive path must be a regular file")
		}
		file, err = os.OpenFile(path, os.O_WRONLY, perm)
		if err != nil {
			return err
		}
		if err := verifyOpenRegularFile(file, info); err != nil {
			_ = file.Close()
			return err
		}
		if err := file.Truncate(0); err != nil {
			_ = file.Close()
			return err
		}
		if _, err := file.Seek(0, 0); err != nil {
			_ = file.Close()
			return err
		}
	} else if os.IsNotExist(err) {
		file, err = os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
		if err != nil {
			return err
		}
		if err := verifyOpenRegularFile(file, nil); err != nil {
			_ = file.Close()
			return err
		}
	} else {
		return err
	}
	defer file.Close()
	if err := file.Chmod(perm); err != nil {
		return err
	}
	if _, err := file.Write(body); err != nil {
		return err
	}
	return file.Sync()
}

func verifyOpenRegularFile(file *os.File, expected os.FileInfo) error {
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("archive path must be a regular file")
	}
	if expected != nil && !os.SameFile(expected, info) {
		return errors.New("archive path changed while opening")
	}
	return nil
}
