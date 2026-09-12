package lifecycle

import (
	"time"

	"github.com/example/autostream-encoder-recorder/internal/archive"
	"github.com/example/autostream-encoder-recorder/internal/ffmpeg"
	"github.com/example/autostream-encoder-recorder/internal/videocover"
)

type Manager struct {
	ArchiveRoot    string
	FFmpegBin      string
	Runner         ffmpeg.Runner
	Uploader       archive.ArchiveUploader
	UploaderForJob func(PackageJob) archive.ArchiveUploader
	Profile        ffmpeg.EncoderProfile
}

type StreamJob struct {
	StreamID             string                    `json:"stream_id"`
	ArchiveRunID         string                    `json:"archive_run_id"`
	Name                 string                    `json:"name"`
	InputURL             string                    `json:"input_url"`
	InputMode            string                    `json:"input_mode,omitempty"`
	AudioInputURL        string                    `json:"-"`
	RTMPURL              string                    `json:"rtmp_url"`
	StreamKey            string                    `json:"-"`
	StreamKeySecretName  string                    `json:"stream_key_secret_name,omitempty"`
	YouTubeOutputMode    string                    `json:"-"`
	OutputRelayBindingID string                    `json:"-"`
	YouTubeOutputReady   bool                      `json:"-"`
	EncoderProfileID     string                    `json:"encoder_profile_id,omitempty"`
	EncoderProfile       ffmpeg.EncoderProfile     `json:"-"`
	OverlayProfileID     string                    `json:"overlay_profile_id,omitempty"`
	OverlayConfig        map[string]any            `json:"-"`
	EncoderAudioGainDB   float64                   `json:"encoder_audio_gain_db,omitempty"`
	WatermarkInputURL    string                    `json:"-"`
	CoverInputURL        string                    `json:"-"`
	VideoCoverStart      *videocover.StartSnapshot `json:"video_cover_start,omitempty"`
	StartedAt            time.Time                 `json:"started_at"`
	DryRun               bool                      `json:"dry_run"`
	ArchiveConfig        ArchiveConfig             `json:"archive_config,omitempty"`
}

type PackageJob struct {
	StreamID      string        `json:"stream_id"`
	ArchiveRunID  string        `json:"archive_run_id"`
	Name          string        `json:"name"`
	StartedAt     time.Time     `json:"started_at"`
	DryRun        bool          `json:"dry_run"`
	ArchiveConfig ArchiveConfig `json:"archive_config,omitempty"`
}

type ArchiveConfig struct {
	DriveDestinationID                  string `json:"drive_destination_id,omitempty"`
	ArchiveProfileID                    string `json:"archive_profile_id,omitempty"`
	AuthMode                            string `json:"auth_mode,omitempty"`
	OAuthAccountID                      string `json:"oauth_account_id,omitempty"`
	OAuthProviderID                     string `json:"oauth_provider_id,omitempty"`
	FolderID                            string `json:"folder_id,omitempty"`
	FolderIDSecretName                  string `json:"folder_id_secret_name,omitempty"`
	ServiceAccountJSON                  string `json:"service_account_json,omitempty"`
	ServiceAccountSecretName            string `json:"service_account_json_secret_name,omitempty"`
	ServiceAccountCredentialsSecretName string `json:"service_account_credentials_secret_name,omitempty"`
	SharedDrive                         bool   `json:"shared_drive,omitempty"`
	SharedDriveID                       string `json:"shared_drive_id,omitempty"`
	ArchiveFileName                     string `json:"archive_file_name,omitempty"`
	ClientID                            string `json:"client_id,omitempty"`
	ClientSecret                        string `json:"client_secret,omitempty"`
	ClientSecretSecretName              string `json:"client_secret_secret_name,omitempty"`
	RefreshToken                        string `json:"refresh_token,omitempty"`
	RefreshTokenSecretName              string `json:"refresh_token_secret_name,omitempty"`
	RetentionDays                       int    `json:"retention_days,omitempty"`
}

type Metadata struct {
	StreamID     string               `json:"stream_id"`
	Name         string               `json:"name"`
	StartedAtJST string               `json:"started_at_jst"`
	Archive      map[string]string    `json:"archive"`
	Upload       archive.UploadResult `json:"upload"`
	Commands     []ffmpeg.Command     `json:"commands,omitempty"`
	Extra        map[string]any       `json:"extra,omitempty"`
}

type Result struct {
	Layout          archive.Layout `json:"layout"`
	Metadata        Metadata       `json:"metadata"`
	RemuxDurationMS float64        `json:"remux_duration_ms,omitempty"`
	ArchiveSource   string         `json:"archive_source,omitempty"`
	Partial         bool           `json:"partial,omitempty"`
}
