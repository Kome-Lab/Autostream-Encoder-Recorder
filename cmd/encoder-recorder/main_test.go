package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/example/autostream-encoder-recorder/internal/control"
	"github.com/example/autostream-encoder-recorder/internal/httpapi"
)

func TestArchiveV2MigrationCommandRequiresPrepareBeforeApply(t *testing.T) {
	root := t.TempDir()
	backup := filepath.Join(t.TempDir(), "backup")
	streamID := "11111111-1111-4111-8111-111111111111"
	legacyDir := filepath.Join(root, "final", streamID)
	if err := os.MkdirAll(legacyDir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacyDir, "final.mp4"), []byte("archive"), 0o640); err != nil {
		t.Fatal(err)
	}

	var output bytes.Buffer
	common := []string{"--archive-root", root, "--backup-dir", backup}
	if err := runArchiveV2Migration(append([]string{"--operation", "apply"}, common...), &output); err == nil {
		t.Fatal("apply without immutable backup was accepted")
	}
	if err := runArchiveV2Migration(append([]string{"--operation", "prepare"}, common...), &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), `"operation":"prepare"`) || !strings.Contains(output.String(), `"backup_status":"PASS"`) {
		t.Fatalf("prepare output = %s", output.String())
	}

	output.Reset()
	if err := runArchiveV2Migration(append([]string{"--operation", "apply"}, common...), &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), `"physical_deletion":true`) {
		t.Fatalf("apply output = %s", output.String())
	}
	want := filepath.Join(root, "final", streamID, "legacy-11111111111141118111111111111111", "final.mp4")
	if _, err := os.Stat(want); err != nil {
		t.Fatalf("migrated artifact: %v", err)
	}
}

func TestEncoderRecorderBindAddrRequiresNodeConfigValue(t *testing.T) {
	if _, err := encoderRecorderBindAddr(""); err == nil {
		t.Fatal("missing node-config bind address was accepted")
	}
}

func TestEncoderRecorderBindAddrAcceptsConfiguredUnprivilegedPort(t *testing.T) {
	for _, value := range []string{
		"127.0.0.1:1024",
		"127.0.0.1:18081",
		"127.0.0.1:65535",
	} {
		t.Run(value, func(t *testing.T) {
			got, err := encoderRecorderBindAddr(value)
			if err != nil {
				t.Fatal(err)
			}
			if got != value {
				t.Fatalf("bind address = %q, want %q", got, value)
			}
		})
	}
}

func TestEncoderRecorderBindAddrAcceptsIPv6(t *testing.T) {
	got, err := encoderRecorderBindAddr("[::1]:18081")
	if err != nil {
		t.Fatal(err)
	}
	if got != "[::1]:18081" {
		t.Fatalf("bind address = %q, want [::1]:18081", got)
	}
}

func TestEncoderRecorderBindAddrRejectsInvalidOrPrivilegedPort(t *testing.T) {
	for _, value := range []string{
		"127.0.0.1",
		"127.0.0.1:0",
		"127.0.0.1:1023",
		"127.0.0.1:65536",
		"127.0.0.1:not-a-port",
	} {
		t.Run(strings.ReplaceAll(value, ":", "_"), func(t *testing.T) {
			if _, err := encoderRecorderBindAddr(value); err == nil {
				t.Fatalf("encoderRecorderBindAddr() accepted %q", value)
			}
		})
	}
}

func TestRequireMatchingUpdaterIdentityRejectsRegistrationIDDrift(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")
	credentialDir := filepath.Join(dir, "credentials")
	if err := os.Mkdir(credentialDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(credentialDir, "node-listener.json"), []byte(`{"schema_version":2,"service_type":"encoder_recorder","bind_address":"127.0.0.1:18081","config_revision":1}`), 0600); err != nil {
		t.Fatal(err)
	}
	body := "panel:\n  url: https://panel.example.com\nnode:\n  id: encoder-authoritative\n  name: Encoder\n  type: encoder_recorder\nlistener:\n  credential: node-listener.json\napi:\n  host: encoder.example.com\n  port: 8443\n  ssl_enabled: true\nauth:\n  token: runtime-token\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AUTOSTREAM_NODE_CONFIG", path)
	t.Setenv("CREDENTIALS_DIRECTORY", credentialDir)
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
