package main

import (
	"testing"

	"github.com/example/autostream-encoder-recorder/internal/control"
)

func TestEncoderProfileFromRuntimeConfig(t *testing.T) {
	profile, ok := encoderProfileFromRuntimeConfig(control.RuntimeConfig{
		Profiles: map[string][]control.RuntimeProfile{
			"encoder": {{
				ID:   "profile-01",
				Kind: "encoder",
				Config: map[string]any{
					"width":                 float64(1280),
					"height":                float64(720),
					"fps":                   float64(30),
					"video_bitrate_kbps":    float64(4500),
					"audio_bitrate_kbps":    float64(128),
					"audio_sample_rate_hz":  float64(48000),
					"keyframe_interval_sec": float64(2),
				},
			}},
		},
	})
	if !ok {
		t.Fatal("expected encoder profile to be applied")
	}
	if profile.Width != 1280 || profile.Height != 720 || profile.FPS != 30 || profile.VideoBitrate != "4500k" || profile.AudioBitrate != "128k" || profile.SampleRate != 48000 || profile.KeyframeSec != 2 {
		t.Fatalf("unexpected encoder profile: %#v", profile)
	}
}

func TestEncoderProfileFromRuntimeConfigUsesOnlyOwnServiceProfile(t *testing.T) {
	profile, ok := encoderProfileFromRuntimeConfig(control.RuntimeConfig{
		Service: control.RegisteredService{ServiceID: "encoder-recorder-01"},
		Profiles: map[string][]control.RuntimeProfile{
			"encoder": {
				{
					ID:   "encoder-other",
					Kind: "encoder",
					Config: map[string]any{
						"service_id": "encoder-recorder-02",
						"width":      float64(640),
						"height":     float64(360),
						"fps":        float64(15),
					},
				},
				{
					ID:   "encoder-own",
					Kind: "encoder",
					Config: map[string]any{
						"service_id": "encoder-recorder-01",
						"width":      float64(1920),
						"height":     float64(1080),
						"fps":        float64(60),
					},
				},
			},
		},
	})
	if !ok {
		t.Fatal("expected own encoder profile to be applied")
	}
	if profile.Width != 1920 || profile.Height != 1080 || profile.FPS != 60 {
		t.Fatalf("expected own encoder profile, got %#v", profile)
	}
}

func TestEncoderProfileFromRuntimeConfigAllowsUnscopedFallback(t *testing.T) {
	profile, ok := encoderProfileFromRuntimeConfig(control.RuntimeConfig{
		Service: control.RegisteredService{ServiceID: "encoder-recorder-01"},
		Profiles: map[string][]control.RuntimeProfile{
			"encoder": {
				{
					ID:   "encoder-other",
					Kind: "encoder",
					Config: map[string]any{
						"service_id": "encoder-recorder-02",
						"width":      float64(640),
						"height":     float64(360),
					},
				},
				{
					ID:   "encoder-global",
					Kind: "encoder",
					Config: map[string]any{
						"width":  float64(1280),
						"height": float64(720),
					},
				},
			},
		},
	})
	if !ok {
		t.Fatal("expected unscoped encoder profile fallback")
	}
	if profile.Width != 1280 || profile.Height != 720 {
		t.Fatalf("expected unscoped encoder fallback profile, got %#v", profile)
	}
}

func TestEncoderProfileFromRuntimeConfigRejectsMalformedServiceID(t *testing.T) {
	profile, ok := encoderProfileFromRuntimeConfig(control.RuntimeConfig{
		Service: control.RegisteredService{ServiceID: "encoder-recorder-01"},
		Profiles: map[string][]control.RuntimeProfile{
			"encoder": {{
				ID:   "encoder-malformed",
				Kind: "encoder",
				Config: map[string]any{
					"service_id": []string{"encoder-recorder-01"},
					"width":      float64(1920),
					"height":     float64(1080),
				},
			}},
		},
	})
	if ok {
		t.Fatalf("malformed service-scoped profile should not be applied: %#v", profile)
	}
}

func TestRequireControlPanelRuntimeConfigInProduction(t *testing.T) {
	t.Setenv("AUTOSTREAM_ENV", "production")
	if !requireControlPanelRuntimeConfig() {
		t.Fatal("expected production Encoder/Recorder to require Control Panel runtime config")
	}
}

func TestRequireControlPanelRuntimeConfigExplicitEnv(t *testing.T) {
	t.Setenv("AUTOSTREAM_REQUIRE_CONTROL_PANEL_RUNTIME_CONFIG", "true")
	if !requireControlPanelRuntimeConfig() {
		t.Fatal("expected explicit runtime config requirement")
	}
}

func TestRequireControlPanelRuntimeConfigDefaultFalseOutsideProduction(t *testing.T) {
	if requireControlPanelRuntimeConfig() {
		t.Fatal("expected local Encoder/Recorder startup to allow compatibility mode by default")
	}
}
