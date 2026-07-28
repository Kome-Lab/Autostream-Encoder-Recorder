package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/example/autostream-encoder-recorder/internal/control"
	"github.com/example/autostream-encoder-recorder/internal/httpapi"
)

func TestEncoderRecorderBindAddrFromEnvPreservesLegacyFallbackPort8080(t *testing.T) {
	t.Setenv("AUTOSTREAM_BIND_ADDR", "")

	got, err := encoderRecorderBindAddrFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if got != "127.0.0.1:8080" {
		t.Fatalf("default bind address = %q, want bridge-compatible 127.0.0.1:8080", got)
	}
}

func TestEncoderRecorderBindAddrFromEnvAcceptsConfigurableUnprivilegedPort(t *testing.T) {
	for _, value := range []string{
		"127.0.0.1:1024",
		"127.0.0.1:18081",
		"127.0.0.1:65535",
	} {
		t.Run(value, func(t *testing.T) {
			t.Setenv("AUTOSTREAM_BIND_ADDR", value)
			got, err := encoderRecorderBindAddrFromEnv()
			if err != nil {
				t.Fatal(err)
			}
			if got != value {
				t.Fatalf("bind address = %q, want %q", got, value)
			}
		})
	}
}

func TestEncoderRecorderBindAddrFromEnvAcceptsIPv6(t *testing.T) {
	t.Setenv("AUTOSTREAM_BIND_ADDR", "[::1]:18081")

	got, err := encoderRecorderBindAddrFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if got != "[::1]:18081" {
		t.Fatalf("bind address = %q, want [::1]:18081", got)
	}
}

func TestEncoderRecorderBindAddrFromEnvRejectsInvalidOrPrivilegedPort(t *testing.T) {
	for _, value := range []string{
		"127.0.0.1",
		"127.0.0.1:0",
		"127.0.0.1:1023",
		"127.0.0.1:65536",
		"127.0.0.1:not-a-port",
	} {
		t.Run(strings.ReplaceAll(value, ":", "_"), func(t *testing.T) {
			t.Setenv("AUTOSTREAM_BIND_ADDR", value)
			if _, err := encoderRecorderBindAddrFromEnv(); err == nil {
				t.Fatalf("encoderRecorderBindAddrFromEnv() accepted %q", value)
			}
		})
	}
}

func TestEncoderRecorderStartupAddrFromEnvRejectsInvalidConfigRevision(t *testing.T) {
	t.Setenv("AUTOSTREAM_BIND_ADDR", "127.0.0.1:18081")
	t.Setenv("AUTOSTREAM_CONFIG_REVISION", "0")

	if _, err := encoderRecorderStartupAddrFromEnv(); err == nil ||
		!strings.Contains(err.Error(), "AUTOSTREAM_CONFIG_REVISION") {
		t.Fatalf("encoderRecorderStartupAddrFromEnv() error = %v, want invalid config revision", err)
	}
}

func TestRequireMatchingUpdaterIdentityRejectsRegistrationIDDrift(t *testing.T) {
	t.Setenv("AUTOSTREAM_NODE_CONFIG", "")
	t.Setenv("SERVICE_ID", "encoder-authoritative")
	latch := httpapi.NewUpdaterIdentityLatch(control.ServiceType)

	if err := requireMatchingUpdaterIdentity(latch, "encoder-authoritative"); err != nil {
		t.Fatalf("matching registration identity failed: %v", err)
	}
	if err := requireMatchingUpdaterIdentity(latch, "encoder-drifted"); !errors.Is(err, httpapi.ErrUpdaterIdentityDrift) {
		t.Fatalf("registration identity drift error = %v", err)
	}
}

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
