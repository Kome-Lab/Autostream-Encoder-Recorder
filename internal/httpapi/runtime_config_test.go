package httpapi

import (
	"context"
	"errors"
	"testing"

	"github.com/example/autostream-encoder-recorder/internal/control"
	"github.com/example/autostream-encoder-recorder/internal/lifecycle"
)

func TestResolveArchiveRuntimeSecrets(t *testing.T) {
	job := lifecycle.StreamJob{
		StreamID: "stream-01",
		ArchiveConfig: lifecycle.ArchiveConfig{
			ArchiveProfileID:       "archive-profile-01",
			AuthMode:               "oauth2",
			FolderIDSecretName:     "drive_destination:dest-01:folder_id",
			ClientSecretSecretName: "oauth_provider:provider-01:client_secret",
			RefreshTokenSecretName: "oauth_account:account-01:refresh_token",
		},
	}
	resolved := map[string]string{
		"drive_destination:dest-01:folder_id":      "drive-folder-id",
		"oauth_provider:provider-01:client_secret": "google-client-secret",
		"oauth_account:account-01:refresh_token":   "google-refresh-token",
	}
	err := resolveArchiveRuntimeSecrets(context.Background(), &job, func(ctx context.Context, streamID, archiveProfileID, secretName string) (string, error) {
		if streamID != "stream-01" || archiveProfileID != "archive-profile-01" {
			t.Fatalf("unexpected resolve context stream=%q profile=%q", streamID, archiveProfileID)
		}
		value, ok := resolved[secretName]
		if !ok {
			return "", errors.New("unexpected secret name")
		}
		return value, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if job.ArchiveConfig.FolderID != "drive-folder-id" || job.ArchiveConfig.ServiceAccountJSON != "" || job.ArchiveConfig.ClientSecret != "google-client-secret" || job.ArchiveConfig.RefreshToken != "google-refresh-token" {
		t.Fatalf("archive secrets were not resolved: %#v", job.ArchiveConfig)
	}
}

func TestResolveYouTubeRuntimeSecrets(t *testing.T) {
	job := lifecycle.StreamJob{
		StreamID:            "stream-01",
		StreamKeySecretName: "youtube_stream_key_main",
	}
	err := resolveYouTubeRuntimeSecrets(context.Background(), &job, func(ctx context.Context, streamID, archiveProfileID, secretName string) (string, error) {
		if streamID != "stream-01" || archiveProfileID != "" || secretName != "youtube_stream_key_main" {
			t.Fatalf("unexpected resolve context stream=%q profile=%q secret=%q", streamID, archiveProfileID, secretName)
		}
		return "runtime-secret-stream-key", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if job.StreamKey != "runtime-secret-stream-key" {
		t.Fatalf("youtube stream key was not resolved: %#v", job)
	}
}

func TestResolvePackageArchiveRuntimeSecrets(t *testing.T) {
	job := lifecycle.PackageJob{
		StreamID: "stream-01",
		ArchiveConfig: lifecycle.ArchiveConfig{
			ArchiveProfileID:       "archive-profile-01",
			AuthMode:               "oauth2",
			FolderIDSecretName:     "drive_destination:dest-01:folder_id",
			ClientSecretSecretName: "oauth_provider:provider-01:client_secret",
			RefreshTokenSecretName: "oauth_account:account-01:refresh_token",
		},
	}
	resolved := map[string]string{
		"drive_destination:dest-01:folder_id":      "drive-folder-id",
		"oauth_provider:provider-01:client_secret": "google-client-secret",
		"oauth_account:account-01:refresh_token":   "google-refresh-token",
	}
	err := resolvePackageArchiveRuntimeSecrets(context.Background(), &job, func(ctx context.Context, streamID, archiveProfileID, secretName string) (string, error) {
		if streamID != "stream-01" || archiveProfileID != "archive-profile-01" {
			t.Fatalf("unexpected resolve context stream=%q profile=%q", streamID, archiveProfileID)
		}
		value, ok := resolved[secretName]
		if !ok {
			return "", errors.New("unexpected secret name")
		}
		return value, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if job.ArchiveConfig.FolderID != "drive-folder-id" || job.ArchiveConfig.ServiceAccountJSON != "" || job.ArchiveConfig.ClientSecret != "google-client-secret" || job.ArchiveConfig.RefreshToken != "google-refresh-token" {
		t.Fatalf("package archive secrets were not resolved: %#v", job.ArchiveConfig)
	}
}

func TestApplyOverlayRuntimeConfigSelectsImageProfileWithoutExposingSecrets(t *testing.T) {
	job := lifecycle.StreamJob{StreamID: "stream-01", OverlayProfileID: "overlay-01"}
	provider := func(ctx context.Context) (control.RuntimeConfig, error) {
		return control.RuntimeConfig{Profiles: map[string][]control.RuntimeProfile{
			"overlay": {{ID: "overlay-01", Kind: "overlay", Config: map[string]any{
				"watermark_enabled":        true,
				"watermark_image_data_url": "data:image/png;base64,iVBORw0KGgo=",
				"api_key_secret_name":      "must-not-be-used",
			}}},
		}}, nil
	}
	if err := applyOverlayRuntimeConfig(context.Background(), &job, provider); err != nil {
		t.Fatal(err)
	}
	if job.OverlayConfig["watermark_image_data_url"] == nil || job.OverlayConfig["watermark_enabled"] != true {
		t.Fatalf("overlay runtime profile was not applied: %#v", job.OverlayConfig)
	}
	if _, ok := job.OverlayConfig["api_key_secret_name"]; ok {
		t.Fatalf("secret-like overlay config was copied into the job: %#v", job.OverlayConfig)
	}
}

func TestApplyEncoderRuntimeConfigSelectsRequestedProfile(t *testing.T) {
	job := lifecycle.StreamJob{StreamID: "stream-01", EncoderProfileID: "encoder-720p"}
	provider := func(context.Context) (control.RuntimeConfig, error) {
		return control.RuntimeConfig{Profiles: map[string][]control.RuntimeProfile{
			"encoder": {
				{ID: "encoder-1080p", Kind: "encoder", Config: map[string]any{"width": float64(1920), "height": float64(1080), "fps": float64(60)}},
				{ID: "encoder-720p", Kind: "encoder", Config: map[string]any{"width": float64(1280), "height": float64(720), "fps": float64(30), "video_bitrate_kbps": float64(4500)}},
			},
		}}, nil
	}

	if err := applyEncoderRuntimeConfig(context.Background(), &job, provider); err != nil {
		t.Fatal(err)
	}
	if job.EncoderProfile.Width != 1280 || job.EncoderProfile.Height != 720 || job.EncoderProfile.FPS != 30 || job.EncoderProfile.VideoBitrate != "4500k" {
		t.Fatalf("requested encoder profile was not selected: %#v", job.EncoderProfile)
	}
}

func TestApplyEncoderRuntimeConfigRejectsMissingRequestedProfile(t *testing.T) {
	job := lifecycle.StreamJob{StreamID: "stream-01", EncoderProfileID: "encoder-missing"}
	provider := func(context.Context) (control.RuntimeConfig, error) {
		return control.RuntimeConfig{Profiles: map[string][]control.RuntimeProfile{"encoder": {{ID: "encoder-other"}}}}, nil
	}

	err := applyEncoderRuntimeConfig(context.Background(), &job, provider)
	if !errors.Is(err, errEncoderRuntimeProfileNotFound) {
		t.Fatalf("missing profile error = %v", err)
	}
}

func TestApplyEncoderRuntimeConfigRejectsProfileBoundToAnotherService(t *testing.T) {
	job := lifecycle.StreamJob{StreamID: "stream-01", EncoderProfileID: "encoder-selected"}
	provider := func(context.Context) (control.RuntimeConfig, error) {
		return control.RuntimeConfig{
			Service: control.RegisteredService{ServiceID: "encoder-primary"},
			Profiles: map[string][]control.RuntimeProfile{"encoder": {{
				ID: "encoder-selected", Config: map[string]any{"service_id": "encoder-other", "width": float64(1280), "height": float64(720)},
			}}},
		}, nil
	}

	err := applyEncoderRuntimeConfig(context.Background(), &job, provider)
	if !errors.Is(err, errEncoderRuntimeProfileNotFound) {
		t.Fatalf("cross-service profile error = %v", err)
	}
}
