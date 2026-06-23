package ffmpeg

import (
	"strconv"
	"strings"
)

type Progress struct {
	FPS           float64
	BitrateKbps   float64
	DroppedFrames float64
}

type AudioStats struct {
	RMSLevelDB  float64
	PeakLevelDB float64
	HasRMS      bool
	HasPeak     bool
}

func ParseProgress(body string) Progress {
	var progress Progress
	for _, line := range strings.Split(body, "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		switch key {
		case "fps":
			progress.FPS = parseFloat(value)
		case "bitrate":
			progress.BitrateKbps = parseBitrateKbps(value)
		case "drop_frames":
			progress.DroppedFrames = parseFloat(value)
		}
	}
	return progress
}

func ParseAudioStats(body string) AudioStats {
	var stats AudioStats
	for _, line := range strings.Split(body, "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		switch {
		case strings.HasSuffix(key, "RMS_level"):
			stats.RMSLevelDB = parseLevelDB(value)
			stats.HasRMS = true
		case strings.HasSuffix(key, "Peak_level"):
			stats.PeakLevelDB = parseLevelDB(value)
			stats.HasPeak = true
		}
	}
	return stats
}

func parseFloat(value string) float64 {
	parsed, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
	if err != nil {
		return 0
	}
	return parsed
}

func parseLevelDB(value string) float64 {
	value = strings.TrimSpace(value)
	switch strings.ToLower(value) {
	case "-inf", "-infinity":
		return -120
	case "inf", "+inf", "infinity", "+infinity":
		return 0
	}
	return parseFloat(value)
}

func parseBitrateKbps(value string) float64 {
	value = strings.TrimSpace(strings.TrimSuffix(value, "bits/s"))
	value = strings.TrimSpace(value)
	multiplier := 1.0
	switch {
	case strings.HasSuffix(value, "k"):
		value = strings.TrimSuffix(value, "k")
	case strings.HasSuffix(value, "M"):
		value = strings.TrimSuffix(value, "M")
		multiplier = 1000
	}
	return parseFloat(value) * multiplier
}
