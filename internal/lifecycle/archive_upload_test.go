package lifecycle

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/example/autostream-encoder-recorder/internal/archive"
	"github.com/example/autostream-encoder-recorder/internal/ffmpeg"
)

func TestPackageUsesArchiveConfigUploaderFactory(t *testing.T) {
	root := t.TempDir()
	layout, err := archive.NewRunLayout(root, "stream-01", "run-01")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(layout.TmpDir(), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(layout.FinalDir(), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(layout.FinalMKV(), []byte("mkv"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(layout.FinalMP4(), []byte("mp4"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(layout.TmpLogs(), []byte("{}\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	var observed PackageJob
	manager := Manager{
		ArchiveRoot: root,
		FFmpegBin:   "ffmpeg",
		Runner:      &ffmpeg.DryRunRunner{},
		Uploader:    archive.MockUploader{Err: errors.New("default uploader must not be used")},
		UploaderForJob: func(job PackageJob) archive.ArchiveUploader {
			observed = job
			return archive.DryRunUploader{}
		},
	}
	_, err = manager.Package(context.Background(), PackageJob{
		StreamID:     "stream-01",
		ArchiveRunID: "run-01",
		Name:         "Morning Stream",
		StartedAt:    time.Date(2026, 6, 11, 1, 2, 3, 0, time.UTC),
		ArchiveConfig: ArchiveConfig{
			AuthMode:    "service_account",
			FolderID:    "drive-folder-id",
			SharedDrive: true,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if observed.ArchiveConfig.FolderID != "drive-folder-id" || !observed.ArchiveConfig.SharedDrive {
		t.Fatalf("archive config was not passed to uploader factory: %#v", observed.ArchiveConfig)
	}
}

func TestPackageUsesArchiveConfigFileNameForDriveUpload(t *testing.T) {
	root := t.TempDir()
	layout, err := archive.NewRunLayout(root, "stream-01", "run-01")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(layout.TmpDir(), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(layout.FinalDir(), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(layout.FinalMKV(), []byte("mkv"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(layout.FinalMP4(), []byte("mp4"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(layout.TmpLogs(), []byte("{}\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	uploader := &archiveFileNameCheckingUploader{t: t, want: "Council Meeting.mp4"}
	manager := Manager{
		ArchiveRoot: root,
		FFmpegBin:   "ffmpeg",
		Runner:      &ffmpeg.DryRunRunner{},
		UploaderForJob: func(PackageJob) archive.ArchiveUploader {
			return uploader
		},
	}
	if _, err := manager.Package(context.Background(), PackageJob{
		StreamID:     "stream-01",
		ArchiveRunID: "run-01",
		Name:         "Morning Stream",
		StartedAt:    time.Date(2026, 6, 11, 1, 2, 3, 0, time.UTC),
		ArchiveConfig: ArchiveConfig{
			ArchiveFileName: "Council Meeting",
		},
	}); err != nil {
		t.Fatal(err)
	}
	if !uploader.observed {
		t.Fatal("expected configured archive file name to be uploaded")
	}
}

func TestArchiveConfigOAuth2BuildsGoogleDriveUploader(t *testing.T) {
	manager := Manager{}
	uploader := manager.uploaderForJob(PackageJob{
		StreamID: "stream-01",
		Name:     "Morning Stream",
		ArchiveConfig: ArchiveConfig{
			AuthMode:      "oauth2",
			FolderID:      "drive-folder-id",
			SharedDrive:   true,
			SharedDriveID: "shared-drive-01",
			ClientID:      "google-client-id",
			ClientSecret:  "google-client-secret",
			RefreshToken:  "google-refresh-token",
		},
	})
	retry, ok := uploader.(archive.RetryUploader)
	if !ok {
		t.Fatalf("expected retry uploader, got %#v", uploader)
	}
	driveUploader, ok := retry.Inner.(archive.GoogleDriveAPIUploader)
	if !ok {
		t.Fatalf("expected Google Drive uploader, got %#v", retry.Inner)
	}
	if driveUploader.Config.AuthMode != "oauth2" || driveUploader.Config.ClientSecret != "google-client-secret" || driveUploader.Config.RefreshToken != "google-refresh-token" || !driveUploader.Config.SharedDrive || driveUploader.Config.SharedDriveID != "shared-drive-01" {
		t.Fatalf("unexpected OAuth Drive config: %#v", driveUploader.Config)
	}
}

func TestArchiveConfigServiceAccountJSONIsRejectedByGoogleDriveConfig(t *testing.T) {
	manager := Manager{}
	uploader := manager.uploaderForJob(PackageJob{
		StreamID: "stream-01",
		Name:     "Morning Stream",
		ArchiveConfig: ArchiveConfig{
			AuthMode:           "service_account",
			FolderID:           "drive-folder-id",
			SharedDrive:        true,
			ServiceAccountJSON: `{"type":"service_account","client_email":"svc@example.com","private_key":"-----BEGIN PRIVATE KEY-----\n...\n-----END PRIVATE KEY-----\n"}`,
		},
	})
	driveUploader := googleDriveUploaderFromRetry(t, uploader)
	if driveUploader.Config.AuthMode != "service_account" || driveUploader.Config.ServiceAccountJSON != "" || driveUploader.Config.ApplicationCredential != "" || !driveUploader.Config.SharedDrive {
		t.Fatalf("unexpected unsupported Service Account Drive config: %#v", driveUploader.Config)
	}
	if err := driveUploader.Config.Validate(); err == nil {
		t.Fatal("expected service account config to be rejected")
	}
}

func TestArchiveConfigDoesNotFallBackToGoogleDriveEnvSecrets(t *testing.T) {
	t.Setenv("GOOGLE_DRIVE_AUTH_MODE", "oauth2")
	t.Setenv("GOOGLE_DRIVE_FOLDER_ID", "env-folder-id")
	t.Setenv("GDRIVE_BASE_PATH", "EnvBase")
	t.Setenv("GOOGLE_DRIVE_SHARED_DRIVE", "false")
	t.Setenv("GOOGLE_OAUTH_CLIENT_ID", "env-client-id")
	t.Setenv("GOOGLE_OAUTH_CLIENT_SECRET", "env-client-secret")
	t.Setenv("GOOGLE_OAUTH_REFRESH_TOKEN", "env-refresh-token")

	manager := Manager{}
	uploader := manager.uploaderForJob(PackageJob{
		StreamID: "stream-01",
		Name:     "Morning Stream",
		ArchiveConfig: ArchiveConfig{
			AuthMode:     "oauth2",
			FolderID:     "job-folder-id",
			SharedDrive:  true,
			ClientID:     "job-client-id",
			ClientSecret: "job-client-secret",
			RefreshToken: "job-refresh-token",
		},
	})
	driveUploader := googleDriveUploaderFromRetry(t, uploader)
	cfg := driveUploader.Config
	if cfg.FolderID != "job-folder-id" || !cfg.SharedDrive {
		t.Fatalf("archive config was not isolated from env folder/shared-drive values: %#v", cfg)
	}
	if cfg.ClientID != "job-client-id" || cfg.ClientSecret != "job-client-secret" || cfg.RefreshToken != "job-refresh-token" {
		t.Fatalf("archive config was not isolated from env OAuth secrets: %#v", cfg)
	}
}

func TestIncompleteArchiveConfigDoesNotUseGoogleDriveEnvSecrets(t *testing.T) {
	t.Setenv("GOOGLE_DRIVE_AUTH_MODE", "oauth2")
	t.Setenv("GOOGLE_DRIVE_FOLDER_ID", "env-folder-id")
	t.Setenv("GOOGLE_OAUTH_CLIENT_ID", "env-client-id")
	t.Setenv("GOOGLE_OAUTH_CLIENT_SECRET", "env-client-secret")
	t.Setenv("GOOGLE_OAUTH_REFRESH_TOKEN", "env-refresh-token")

	manager := Manager{}
	uploader := manager.uploaderForJob(PackageJob{
		StreamID: "stream-01",
		Name:     "Morning Stream",
		ArchiveConfig: ArchiveConfig{
			AuthMode: "oauth2",
			FolderID: "job-folder-id",
		},
	})
	driveUploader := googleDriveUploaderFromRetry(t, uploader)
	cfg := driveUploader.Config
	if cfg.ClientID != "" || cfg.ClientSecret != "" || cfg.RefreshToken != "" {
		t.Fatalf("incomplete Control Panel archive_config must not be completed from env secrets: %#v", cfg)
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected incomplete OAuth archive_config to fail validation")
	}
}

func TestArchiveMetadataIncludesSafeArchiveConfigOnly(t *testing.T) {
	root := t.TempDir()
	layout, err := archive.NewRunLayout(root, "stream-01", "run-01")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(layout.TmpDir(), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(layout.FinalDir(), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(layout.FinalMKV(), []byte("mkv"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(layout.TmpLogs(), []byte("{}\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	uploader := &archiveConfigMetadataCheckingUploader{t: t}
	manager := Manager{
		ArchiveRoot: root,
		FFmpegBin:   "ffmpeg",
		Runner:      &ffmpeg.DryRunRunner{},
		UploaderForJob: func(PackageJob) archive.ArchiveUploader {
			return uploader
		},
	}
	_, err = manager.Package(context.Background(), PackageJob{
		StreamID:     "stream-01",
		ArchiveRunID: "run-01",
		Name:         "Morning Stream",
		StartedAt:    time.Date(2026, 6, 11, 1, 2, 3, 0, time.UTC),
		ArchiveConfig: ArchiveConfig{
			DriveDestinationID:     "drive-destination-01",
			ArchiveProfileID:       "archive-profile-01",
			AuthMode:               "oauth2",
			OAuthAccountID:         "oauth-account-01",
			OAuthProviderID:        "oauth-provider-01",
			FolderID:               "raw-drive-folder-id",
			FolderIDSecretName:     "drive_destination:drive-destination-01:folder_id",
			SharedDrive:            true,
			SharedDriveID:          "raw-shared-drive-id",
			ArchiveFileName:        "Council Meeting.mp4",
			ClientID:               "google-client-id",
			ClientSecret:           "raw-google-client-secret",
			ClientSecretSecretName: "oauth_provider:oauth-provider-01:client_secret",
			RefreshToken:           "raw-google-refresh-token",
			RefreshTokenSecretName: "oauth_account:oauth-account-01:refresh_token",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !uploader.metadataObserved {
		t.Fatal("expected metadata.json to be uploaded")
	}
	metadataBody, err := os.ReadFile(layout.FinalMetadata())
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"raw-drive-folder-id", "raw-shared-drive-id", "raw-service-account-private-key", "raw-google-client-secret", "raw-google-refresh-token"} {
		if strings.Contains(string(metadataBody), secret) {
			t.Fatalf("archive metadata leaked raw secret %q: %s", secret, string(metadataBody))
		}
	}
	var metadata Metadata
	if err := json.Unmarshal(metadataBody, &metadata); err != nil {
		t.Fatal(err)
	}
	cfg, ok := metadata.Extra["archive_config"].(map[string]any)
	if !ok {
		t.Fatalf("expected archive_config summary in metadata extra: %#v", metadata.Extra)
	}
	if cfg["drive_destination_id"] != "drive-destination-01" || cfg["auth_mode"] != "oauth2" || cfg["shared_drive"] != true {
		t.Fatalf("unexpected archive config summary: %#v", cfg)
	}
	for _, key := range []string{"folder_id_configured", "client_secret_configured", "refresh_token_configured"} {
		if cfg[key] != true {
			t.Fatalf("expected %s in archive config summary: %#v", key, cfg)
		}
	}
	if _, ok := cfg["service_account_json_configured"]; ok {
		t.Fatalf("service account summary should not be emitted: %#v", cfg)
	}
	if cfg["shared_drive_id_configured"] != true || cfg["archive_file_name"] != "Council Meeting.mp4" {
		t.Fatalf("expected archive file/shared drive summary in metadata: %#v", cfg)
	}
}
