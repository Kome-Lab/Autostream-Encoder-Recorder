package streamproc

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/example/autostream-encoder-recorder/internal/archive"
	"github.com/example/autostream-encoder-recorder/internal/ffmpeg"
	"github.com/example/autostream-encoder-recorder/internal/lifecycle"
)

func writeStartMetadata(layout archive.Layout, job lifecycle.StreamJob, snapshot Snapshot, args []string, bin, outputRoute string) error {
	extra := map[string]any{"live_process": true}
	if archiveRunID := strings.TrimSpace(job.ArchiveRunID); archiveRunID != "" {
		extra["archive_run_id"] = archiveRunID
	}
	if job.InputMode != "" {
		extra["input_mode"] = job.InputMode
	}
	if strings.TrimSpace(job.EncoderProfileID) != "" {
		extra["encoder_profile_id"] = strings.TrimSpace(job.EncoderProfileID)
	}
	if job.EncoderProfile.Width > 0 {
		extra["output_width"] = job.EncoderProfile.Width
		extra["output_height"] = job.EncoderProfile.Height
		extra["output_fps"] = job.EncoderProfile.FPS
	}
	if youtubeOutputMode := strings.TrimSpace(job.YouTubeOutputMode); youtubeOutputMode != "" {
		extra["youtube_output_mode"] = youtubeOutputMode
	}
	if outputRoute = strings.TrimSpace(outputRoute); outputRoute == "direct" || outputRoute == "local_relay" {
		extra["output_route"] = outputRoute
	}
	metadata := map[string]any{
		"stream_id":      job.StreamID,
		"name":           job.Name,
		"started_at_jst": snapshot.StartedAtJST,
		"archive":        snapshot.Archive,
		"commands": lifecycle.RedactCommandsForLayout(layout, []ffmpeg.Command{{
			Bin:  bin,
			Args: args,
		}}, job.StreamKey, job.InputURL, job.RTMPURL),
		"extra": extra,
	}
	body, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		return err
	}
	if err := lifecycle.WriteFileNoSymlink(layout.TmpMetadata(), append(body, '\n'), 0o640); err != nil {
		return err
	}
	logLine, _ := json.Marshal(map[string]any{"timestamp": time.Now().UTC().Format(time.RFC3339), "event": "stream_process.started", "stream_id": job.StreamID})
	return lifecycle.WriteFileNoSymlink(layout.TmpLogs(), append(logLine, '\n'), 0o640)
}

func ensureLiveArchiveDir(rootDir string, dirs ...string) error {
	for _, dir := range append([]string{rootDir}, dirs...) {
		if err := ensureSingleLiveArchiveDir(rootDir, dir); err != nil {
			return err
		}
	}
	return nil
}

func ensureSingleLiveArchiveDir(rootDir, dir string) error {
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
	if err := os.MkdirAll(dirAbs, 0o750); err != nil {
		return err
	}
	info, err := os.Lstat(dirAbs)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return errors.New("archive directory must not be a symlink")
	}
	if !info.IsDir() {
		return errors.New("archive path component must be a directory")
	}
	return nil
}

func rejectExistingArchiveOutputSymlink(path string) error {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return errors.New("archive path must not be a symlink")
	}
	return nil
}
