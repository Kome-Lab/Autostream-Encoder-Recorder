package streamproc

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"

	"github.com/example/autostream-encoder-recorder/internal/archive"
)

const maxCoverGenerationBytes = 4 << 10

type durableCoverGeneration struct {
	StreamID      string `json:"stream_id"`
	JobGeneration uint64 `json:"job_generation"`
	Generation    uint64 `json:"generation"`
}

func (m *Manager) nextCoverGeneration(streamID string, jobGeneration uint64) (uint64, error) {
	if jobGeneration == 0 {
		return 1, nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.coverGenerations == nil {
		m.coverGenerations = map[string]coverGeneration{}
	}
	previous, err := m.readCoverGeneration(streamID)
	if err != nil {
		return 0, err
	}
	if memory := m.coverGenerations[streamID]; memory.Generation > previous.Generation && memory.JobGeneration == jobGeneration {
		previous = durableCoverGeneration{StreamID: streamID, JobGeneration: memory.JobGeneration, Generation: memory.Generation}
	}
	next := uint64(1)
	if previous.JobGeneration == jobGeneration && previous.Generation > 0 {
		next = previous.Generation + 1
	}
	record := durableCoverGeneration{StreamID: streamID, JobGeneration: jobGeneration, Generation: next}
	if err := m.writeCoverGeneration(record); err != nil {
		return 0, err
	}
	m.coverGenerations[streamID] = coverGeneration{JobGeneration: jobGeneration, Generation: next}
	return next, nil
}

func (m *Manager) coverGenerationPaths(streamID string) (root, dir, path string, err error) {
	layout, err := archive.NewLayout(m.archiveRoot(), streamID)
	if err != nil {
		return "", "", "", err
	}
	root = layout.RootDir
	dir = filepath.Join(root, "state", "video-cover-generations")
	return root, dir, filepath.Join(dir, streamID+".json"), nil
}

func (m *Manager) readCoverGeneration(streamID string) (durableCoverGeneration, error) {
	root, dir, path, err := m.coverGenerationPaths(streamID)
	if err != nil {
		return durableCoverGeneration{}, err
	}
	if _, err := os.Lstat(dir); os.IsNotExist(err) {
		return durableCoverGeneration{}, nil
	} else if err != nil {
		return durableCoverGeneration{}, err
	}
	if err := archive.EnsureDirNoSymlinks(root, dir); err != nil {
		return durableCoverGeneration{}, err
	}
	before, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return durableCoverGeneration{}, nil
	}
	if err != nil {
		return durableCoverGeneration{}, err
	}
	if before.Mode()&os.ModeSymlink != 0 || !before.Mode().IsRegular() {
		return durableCoverGeneration{}, errors.New("video cover generation path must be a regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return durableCoverGeneration{}, err
	}
	defer file.Close()
	after, err := file.Stat()
	if err != nil || !after.Mode().IsRegular() || !os.SameFile(before, after) {
		return durableCoverGeneration{}, errors.New("video cover generation path changed while opening")
	}
	body, err := io.ReadAll(io.LimitReader(file, maxCoverGenerationBytes+1))
	if err != nil {
		return durableCoverGeneration{}, err
	}
	if len(body) > maxCoverGenerationBytes {
		return durableCoverGeneration{}, errors.New("video cover generation record is too large")
	}
	var record durableCoverGeneration
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&record); err != nil {
		return durableCoverGeneration{}, errors.New("invalid video cover generation record")
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return durableCoverGeneration{}, errors.New("invalid video cover generation record")
	}
	if record.StreamID != streamID || record.JobGeneration < 1 || record.Generation < 1 {
		return durableCoverGeneration{}, errors.New("invalid video cover generation record")
	}
	return record, nil
}

func (m *Manager) writeCoverGeneration(record durableCoverGeneration) error {
	root, dir, path, err := m.coverGenerationPaths(record.StreamID)
	if err != nil {
		return err
	}
	if record.JobGeneration < 1 || record.Generation < 1 {
		return errors.New("invalid video cover generation record")
	}
	if err := archive.EnsureDirNoSymlinks(root, dir); err != nil {
		return err
	}
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return errors.New("video cover generation path must be a regular file")
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	body, err := json.Marshal(record)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".video-cover-generation-*")
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
