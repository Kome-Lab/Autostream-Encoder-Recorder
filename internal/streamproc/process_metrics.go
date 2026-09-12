package streamproc

import (
	"context"
	"log"
	"os"
	"time"

	"github.com/example/autostream-encoder-recorder/internal/ffmpeg"
	"github.com/example/autostream-encoder-recorder/internal/observability"
)

type Reporter interface {
	Report(ctx context.Context, signal observability.Signal) error
}

func (m *Manager) monitor(streamID, finalMKV, progressPath, audioStatsPath string) {
	interval := m.MetricsInterval
	if interval <= 0 {
		interval = 10 * time.Second
	}
	silenceThresholdDB := envFloat("ENCODER_AUDIO_SILENCE_THRESHOLD_DB", -50)
	clippingThresholdDB := envFloat("ENCODER_AUDIO_CLIPPING_THRESHOLD_DB", -1)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	var previousSize int64 = -1
	var previousAt time.Time
	lastProgressAt := time.Now().UTC()
	var lastProgressMod time.Time
	var silenceSec float64
	var clippingTotal float64
	for {
		if !m.isRunning(streamID) {
			return
		}
		now := time.Now().UTC()
		elapsedForAudio := interval.Seconds()
		if !previousAt.IsZero() {
			elapsedForAudio = now.Sub(previousAt).Seconds()
		}
		size := fileSize(finalMKV)
		m.reportMetric(streamID, "encoder.process_alive", 1)
		m.reportMetric(streamID, "recorder.file_size_bytes", float64(size))
		if previousSize >= 0 && !previousAt.IsZero() {
			elapsed := now.Sub(previousAt).Seconds()
			if elapsed > 0 {
				kbps := float64(size-previousSize) * 8 / 1000 / elapsed
				if kbps < 0 {
					kbps = 0
				}
				m.reportMetric(streamID, "recorder.write_bitrate_kbps", kbps)
			}
		}
		if free, ok := diskFreeBytes(finalMKV); ok {
			m.reportMetric(streamID, "recorder.disk_free_bytes", float64(free))
		}
		m.reportFFmpegProgress(streamID, progressPath)
		m.reportAudioStats(streamID, audioStatsPath, elapsedForAudio, silenceThresholdDB, clippingThresholdDB, &silenceSec, &clippingTotal)
		if modTime, ok := fileModTime(progressPath); ok && modTime.After(lastProgressMod) {
			lastProgressMod = modTime
			lastProgressAt = now
		}
		m.reportMetric(streamID, "media.input_timeout_sec", maxFloat(now.Sub(lastProgressAt).Seconds(), 0))
		previousSize = size
		previousAt = now
		<-ticker.C
	}
}

func (m *Manager) isRunning(streamID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	tracked, ok := m.processes[streamID]
	return ok && tracked.snapshot.Status == "running"
}

func (m *Manager) report(signal observability.Signal) {
	if m.Reporter == nil {
		return
	}
	if err := m.Reporter.Report(context.Background(), signal); err != nil && isProcessDiagnosticSignal(signal.Name) {
		log.Printf("encoder diagnostic report failed: event=%s stream_id=%s error_class=observability_request_failed", signal.Name, signal.StreamID)
	}
}

func (m *Manager) reportMetric(streamID, name string, value float64) {
	m.report(observability.Signal{
		Type:      "metric",
		Name:      name,
		StreamID:  streamID,
		Value:     &value,
		Timestamp: time.Now().UTC(),
	})
}

func (m *Manager) reportFFmpegProgress(streamID, progressPath string) {
	body, err := os.ReadFile(progressPath)
	if err != nil {
		return
	}
	progress := ffmpeg.ParseProgress(string(body))
	if progress.FPS > 0 {
		m.reportMetric(streamID, "encoder.output_fps", progress.FPS)
	}
	if progress.BitrateKbps > 0 {
		m.reportMetric(streamID, "encoder.output_bitrate_kbps", progress.BitrateKbps)
	}
	if progress.SpeedRatio > 0 {
		m.reportMetric(streamID, "encoder.output_speed_ratio", progress.SpeedRatio)
	}
	m.reportMetric(streamID, "encoder.dropped_frames_total", progress.DroppedFrames)
}

func (m *Manager) reportAudioStats(streamID, audioStatsPath string, elapsedSec, silenceThresholdDB, clippingThresholdDB float64, silenceSec, clippingTotal *float64) {
	body, err := os.ReadFile(audioStatsPath)
	if err != nil {
		return
	}
	stats := ffmpeg.ParseAudioStats(string(body))
	if stats.HasRMS {
		m.reportMetric(streamID, "encoder.audio_level_db", stats.RMSLevelDB)
		if stats.RMSLevelDB <= silenceThresholdDB {
			*silenceSec += maxFloat(elapsedSec, 0)
		} else {
			*silenceSec = 0
		}
		m.reportMetric(streamID, "encoder.audio_silence_sec", *silenceSec)
	}
	if stats.HasPeak {
		if stats.PeakLevelDB >= clippingThresholdDB {
			*clippingTotal++
		}
		m.reportMetric(streamID, "encoder.audio_clipping_total", *clippingTotal)
	}
}

func fileSize(path string) int64 {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return info.Size()
}

func fileModTime(path string) (time.Time, bool) {
	info, err := os.Stat(path)
	if err != nil {
		return time.Time{}, false
	}
	return info.ModTime(), true
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func boolMetric(ok bool) float64 {
	if ok {
		return 1
	}
	return 0
}

func maxFloat(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}
