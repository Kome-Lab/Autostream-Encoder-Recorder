package lifecycle

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/example/autostream-encoder-recorder/internal/archive"
)

func cleanupExpiredLocalArchives(rootDir, currentStreamID, currentArchiveRunID string, retentionDays int, now time.Time) error {
	if retentionDays <= 0 {
		return nil
	}
	rootAbs, err := filepath.Abs(rootDir)
	if err != nil {
		return err
	}
	finalRoot := filepath.Join(rootAbs, "final")
	info, err := os.Lstat(finalRoot)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return errors.New("archive final directory must not be a symlink")
	}
	if !info.IsDir() {
		return errors.New("archive final path must be a directory")
	}
	if err := archive.EnsureDirNoSymlinks(rootAbs, finalRoot); err != nil {
		return err
	}
	entries, err := os.ReadDir(finalRoot)
	if err != nil {
		return err
	}
	cutoff := now.Add(-time.Duration(retentionDays) * 24 * time.Hour)
	for _, entry := range entries {
		streamID := entry.Name()
		if entry.Type()&os.ModeSymlink != 0 {
			return errors.New("archive stream directory must not be a symlink")
		}
		if !entry.IsDir() {
			continue
		}
		layout, err := archive.NewLayout(rootAbs, streamID)
		if err != nil {
			continue
		}
		finalDir := layout.FinalDir()
		if err := archive.EnsureDirNoSymlinks(rootAbs, finalDir); err != nil {
			return err
		}
		runEntries, err := os.ReadDir(finalDir)
		if err != nil {
			return err
		}
		for _, runEntry := range runEntries {
			if !runEntry.IsDir() {
				continue
			}
			if runEntry.Type()&os.ModeSymlink != 0 {
				return errors.New("archive run directory must not be a symlink")
			}
			runID := runEntry.Name()
			runLayout, err := archive.NewRunLayout(rootAbs, streamID, runID)
			if err != nil {
				continue
			}
			if streamID == currentStreamID && (currentArchiveRunID == "" || runID == currentArchiveRunID) {
				continue
			}
			runDir := runLayout.FinalDir()
			if err := archive.EnsureDirNoSymlinks(rootAbs, runDir); err != nil {
				return err
			}
			modifiedAt, err := archiveFinalDirLastModified(runDir)
			if err != nil {
				return err
			}
			if modifiedAt.Before(cutoff) {
				if err := os.RemoveAll(runDir); err != nil {
					return err
				}
			}
		}
		// Runless final/<stream_id> files predate archive_run_id. They are
		// retained as migrated historical data and are no longer owned by the
		// runtime retention path. Only explicit run directories are eligible.
	}
	return nil
}

func archiveFinalDirLastModified(dir string) (time.Time, error) {
	info, err := os.Lstat(dir)
	if err != nil {
		return time.Time{}, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return time.Time{}, errors.New("archive stream directory must not be a symlink")
	}
	if !info.IsDir() {
		return time.Time{}, errors.New("archive stream path must be a directory")
	}
	latest := info.ModTime()
	err = filepath.WalkDir(dir, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == dir {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return errors.New("archive artifact path must not be a symlink")
		}
		if info.ModTime().After(latest) {
			latest = info.ModTime()
		}
		return nil
	})
	return latest, err
}
