package ffmpeg

import "testing"

func TestProfileFromConfigUsesSelectedRuntimeValues(t *testing.T) {
	profile := ProfileFromConfig(map[string]any{
		"width":                 float64(1280),
		"height":                float64(720),
		"fps":                   float64(30),
		"video_bitrate_kbps":    float64(4500),
		"audio_bitrate_kbps":    float64(128),
		"audio_sample_rate_hz":  float64(48000),
		"keyframe_interval_sec": float64(2),
	})
	if profile.Width != 1280 || profile.Height != 720 || profile.FPS != 30 || profile.VideoBitrate != "4500k" || profile.AudioBitrate != "128k" || profile.SampleRate != 48000 || profile.KeyframeSec != 2 {
		t.Fatalf("unexpected profile: %#v", profile)
	}
}

func TestProfileFromConfigKeepsDefaultsForOmittedValues(t *testing.T) {
	profile := ProfileFromConfig(map[string]any{"width": float64(854), "height": float64(480), "fps": float64(30)})
	defaults := DefaultProfile()
	if profile.VideoBitrate != defaults.VideoBitrate || profile.AudioBitrate != defaults.AudioBitrate || profile.SampleRate != defaults.SampleRate || profile.KeyframeSec != defaults.KeyframeSec {
		t.Fatalf("omitted settings did not keep defaults: %#v", profile)
	}
}
