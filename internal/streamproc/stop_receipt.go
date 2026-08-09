package streamproc

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/example/autostream-encoder-recorder/internal/archive"
)

const (
	defaultStopReceiptTTL = 15 * time.Minute
	maxStopReceiptBytes   = 4 << 10
)

// stopReceipt is intentionally limited to a target ID and expiry. It never
// contains media, OAuth, or runtime-secret material.
type stopReceipt struct {
	StreamID  string    `json:"stream_id"`
	ExpiresAt time.Time `json:"expires_at"`
}

func (m *Manager) stopReceiptTTL() time.Duration {
	if m.StopReceiptTTL > 0 {
		return m.StopReceiptTTL
	}
	return defaultStopReceiptTTL
}

func (m *Manager) stopReceiptPaths(streamID string) (root, dir, path string, err error) {
	layout, err := archive.NewLayout(m.archiveRoot(), streamID)
	if err != nil {
		return "", "", "", err
	}
	root = layout.RootDir
	dir = filepath.Join(root, "state", "stop-receipts")
	return root, dir, filepath.Join(dir, streamID+".json"), nil
}

func (m *Manager) recordStopReceipt(streamID string) error {
	return m.writeStopReceipt(streamID, stopReceipt{
		StreamID:  streamID,
		ExpiresAt: time.Now().UTC().Add(m.stopReceiptTTL()),
	})
}

func (m *Manager) writeStopReceipt(streamID string, receipt stopReceipt) error {
	root, dir, path, err := m.stopReceiptPaths(streamID)
	if err != nil {
		return err
	}
	if receipt.StreamID != streamID || receipt.ExpiresAt.IsZero() {
		return errors.New("invalid stop receipt")
	}
	if err := archive.EnsureDirNoSymlinks(root, dir); err != nil {
		return err
	}
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return errors.New("stop receipt path must be a regular file")
		}
	} else if !os.IsNotExist(err) {
		return err
	}

	body, err := json.Marshal(receipt)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".stop-receipt-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(body, '\n')); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	cleanup = false
	return syncStopReceiptDirectory(dir)
}

func (m *Manager) hasStopReceipt(streamID string) (bool, error) {
	root, dir, path, err := m.stopReceiptPaths(streamID)
	if err != nil {
		// An invalid target cannot have a durable receipt and remains rejected as
		// not-running. Do not make an invalid URL path an internal server error.
		return false, nil
	}
	exists, err := existingStopReceiptDir(root, dir)
	if err != nil || !exists {
		return false, err
	}
	before, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if before.Mode()&os.ModeSymlink != 0 || !before.Mode().IsRegular() {
		return false, errors.New("stop receipt path must be a regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer file.Close()
	after, err := file.Stat()
	if err != nil {
		return false, err
	}
	if !after.Mode().IsRegular() || !os.SameFile(before, after) {
		return false, errors.New("stop receipt path changed while opening")
	}
	body, err := io.ReadAll(io.LimitReader(file, maxStopReceiptBytes+1))
	if err != nil {
		return false, err
	}
	if len(body) > maxStopReceiptBytes {
		return false, errors.New("stop receipt is too large")
	}
	var receipt stopReceipt
	if err := json.Unmarshal(body, &receipt); err != nil {
		return false, nil
	}
	if receipt.StreamID != streamID || receipt.ExpiresAt.IsZero() || !receipt.ExpiresAt.After(time.Now().UTC()) {
		return false, nil
	}
	return true, nil
}

func (m *Manager) clearStopReceipt(streamID string) error {
	root, dir, path, err := m.stopReceiptPaths(streamID)
	if err != nil {
		return err
	}
	exists, err := existingStopReceiptDir(root, dir)
	if err != nil || !exists {
		return err
	}
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return errors.New("stop receipt path must be a regular file")
	}
	if err := os.Remove(path); err != nil {
		return err
	}
	// Like receipt creation, receipt removal changes durable state. Sync the
	// directory so a power loss cannot resurrect a stale receipt after Start.
	return syncStopReceiptDirectory(dir)
}

func existingStopReceiptDir(root, dir string) (bool, error) {
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return false, err
	}
	dirAbs, err := filepath.Abs(dir)
	if err != nil {
		return false, err
	}
	rel, err := filepath.Rel(rootAbs, dirAbs)
	if err != nil {
		return false, err
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return false, errors.New("stop receipt directory must stay under archive root")
	}
	info, err := os.Lstat(rootAbs)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return false, errors.New("archive root must be a non-symlink directory")
	}
	current := rootAbs
	for _, part := range strings.Split(rel, string(os.PathSeparator)) {
		if part == "" || part == "." {
			continue
		}
		current = filepath.Join(current, part)
		info, err = os.Lstat(current)
		if os.IsNotExist(err) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return false, errors.New("stop receipt directory must be a non-symlink directory")
		}
	}
	return true, nil
}

func syncStopReceiptDirectory(dir string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	file, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer file.Close()
	return file.Sync()
}
