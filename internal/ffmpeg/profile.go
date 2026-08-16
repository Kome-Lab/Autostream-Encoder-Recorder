package ffmpeg

import "fmt"

// ProfileFromConfig converts the non-secret Control Panel encoder profile
// shape into the complete FFmpeg profile used for one stream. Omitted values
// retain the documented Encoder defaults.
func ProfileFromConfig(config map[string]any) EncoderProfile {
	profile := DefaultProfile()
	if width := profileConfigInt(config, "width"); width > 0 {
		profile.Width = width
	}
	if height := profileConfigInt(config, "height"); height > 0 {
		profile.Height = height
	}
	if fps := profileConfigInt(config, "fps"); fps > 0 {
		profile.FPS = fps
	}
	if kbps := profileConfigInt(config, "video_bitrate_kbps"); kbps > 0 {
		profile.VideoBitrate = fmt.Sprintf("%dk", kbps)
	}
	if kbps := profileConfigInt(config, "audio_bitrate_kbps"); kbps > 0 {
		profile.AudioBitrate = fmt.Sprintf("%dk", kbps)
	}
	if sampleRate := profileConfigInt(config, "audio_sample_rate_hz"); sampleRate > 0 {
		profile.SampleRate = sampleRate
	}
	if keyframe := profileConfigInt(config, "keyframe_interval_sec"); keyframe > 0 {
		profile.KeyframeSec = keyframe
	}
	return profile
}

func profileConfigInt(config map[string]any, key string) int {
	switch value := config[key].(type) {
	case int:
		return value
	case int32:
		return int(value)
	case int64:
		return int(value)
	case float32:
		return int(value)
	case float64:
		return int(value)
	default:
		return 0
	}
}
