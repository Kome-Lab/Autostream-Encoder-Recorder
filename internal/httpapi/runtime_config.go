package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/example/autostream-encoder-recorder/internal/control"
	"github.com/example/autostream-encoder-recorder/internal/ffmpeg"
	"github.com/example/autostream-encoder-recorder/internal/lifecycle"
	"github.com/example/autostream-encoder-recorder/internal/outputrelay"
)

var (
	errRawArchiveSecretFieldsNotAllowed   = errors.New("raw_archive_secret_fields_not_allowed")
	errRuntimeSecretResolverNotConfigured = errors.New("runtime_secret_resolver_not_configured")
	errUnsupportedArchiveAuthMode         = errors.New("unsupported_archive_auth_mode")
)

type RuntimeSecretResolver func(ctx context.Context, streamID, archiveProfileID, secretName string) (string, error)

type RuntimeConfigProvider func(ctx context.Context) (control.RuntimeConfig, error)

var (
	errEncoderRuntimeProfileNotFound = errors.New("encoder runtime profile not found")
	errEncoderRuntimeConfigMissing   = errors.New("encoder runtime config provider is unavailable")
)

func writeOutputRelayPolicyError(w http.ResponseWriter, err error, fallbackCode string) {
	switch {
	case errors.Is(err, outputrelay.ErrLiveAPIRelayStaticNotReady):
		writeJSON(w, http.StatusConflict, map[string]string{"code": "live_api_relay_static_not_ready"})
	case errors.Is(err, outputrelay.ErrLiveAPIRelayBindingMismatch):
		writeJSON(w, http.StatusConflict, map[string]string{"code": "live_api_relay_binding_mismatch"})
	case errors.Is(err, outputrelay.ErrLiveAPIRequiresManagedOutputRelay):
		writeJSON(w, http.StatusConflict, map[string]string{"code": "live_api_requires_managed_output_relay"})
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{"code": fallbackCode})
	}
}

func clearUnusedYouTubeOutputTarget(job *lifecycle.StreamJob) {
	if job == nil {
		return
	}
	job.RTMPURL = ""
	job.StreamKey = ""
	job.StreamKeySecretName = ""
}

func applyYouTubeRuntimeConfig(ctx context.Context, job *lifecycle.StreamJob, provider RuntimeConfigProvider) error {
	if job == nil || provider == nil || strings.TrimSpace(job.StreamID) == "" {
		return nil
	}
	cfg, err := provider(ctx)
	if err != nil {
		return err
	}
	if policy, ok := cfg.YouTubeOutputPolicyForStream(job.StreamID); ok {
		job.YouTubeOutputMode = policy.Mode
		job.OutputRelayBindingID = policy.RelayBindingID
		job.YouTubeOutputReady = policy.Ready
	}
	youtube, ok := cfg.YouTubeConfigForStream(job.StreamID)
	if !ok {
		return nil
	}
	if strings.TrimSpace(job.RTMPURL) == "" {
		job.RTMPURL = youtube.RTMPURL()
	}
	if strings.TrimSpace(job.StreamKey) == "" && strings.TrimSpace(job.StreamKeySecretName) == "" {
		job.StreamKeySecretName = youtube.StreamKeySecretName()
	}
	return nil
}

func applyEncoderRuntimeConfig(ctx context.Context, job *lifecycle.StreamJob, provider RuntimeConfigProvider) error {
	if job == nil || strings.TrimSpace(job.EncoderProfileID) == "" {
		return nil
	}
	if provider == nil {
		return errEncoderRuntimeConfigMissing
	}
	cfg, err := provider(ctx)
	if err != nil {
		return err
	}
	profileID := strings.TrimSpace(job.EncoderProfileID)
	for _, profile := range cfg.Profiles["encoder"] {
		if strings.TrimSpace(profile.ID) != profileID || !runtimeEncoderProfileBelongsToService(profile, cfg.Service.ServiceID) {
			continue
		}
		job.EncoderProfileID = profileID
		job.EncoderProfile = ffmpeg.ProfileFromConfig(profile.Config)
		return nil
	}
	return errEncoderRuntimeProfileNotFound
}

func runtimeEncoderProfileBelongsToService(profile control.RuntimeProfile, serviceID string) bool {
	rawServiceID, configured := profile.Config["service_id"]
	if !configured {
		return true
	}
	profileServiceID, ok := rawServiceID.(string)
	if !ok {
		return false
	}
	profileServiceID = strings.TrimSpace(profileServiceID)
	return profileServiceID == "" || profileServiceID == strings.TrimSpace(serviceID)
}

func applyArchiveRuntimeConfig(ctx context.Context, job *lifecycle.StreamJob, provider RuntimeConfigProvider) error {
	if job == nil || provider == nil || strings.TrimSpace(job.StreamID) == "" {
		return nil
	}
	cfg, err := provider(ctx)
	if err != nil {
		return err
	}
	archiveRuntime, ok := cfg.ArchiveConfigForStream(job.StreamID)
	if !ok {
		return nil
	}
	mergeRuntimeArchiveConfig(&job.ArchiveConfig, archiveRuntime)
	return nil
}

func applyOverlayRuntimeConfig(ctx context.Context, job *lifecycle.StreamJob, provider RuntimeConfigProvider) error {
	if job == nil || provider == nil || strings.TrimSpace(job.OverlayProfileID) == "" {
		return nil
	}
	cfg, err := provider(ctx)
	if err != nil {
		return err
	}
	profileID := strings.TrimSpace(job.OverlayProfileID)
	for _, profile := range cfg.Profiles["overlay"] {
		if strings.TrimSpace(profile.ID) != profileID {
			continue
		}
		job.OverlayConfig = make(map[string]any, 6)
		for _, key := range []string{"watermark_enabled", "watermark_image_data_url", "watermark_canvas_width", "watermark_canvas_height", "watermark_fit_mode", "watermark_file_name", "watermark_image_name"} {
			if value, ok := profile.Config[key]; ok {
				job.OverlayConfig[key] = value
			}
		}
		return nil
	}
	return errors.New("overlay runtime profile not found")
}

func mergeRuntimeArchiveConfig(dst *lifecycle.ArchiveConfig, src control.RuntimeArchiveStreamConfig) {
	if dst == nil {
		return
	}
	setStringIfEmpty(&dst.DriveDestinationID, src.DriveDestinationID())
	setStringIfEmpty(&dst.ArchiveProfileID, src.ArchiveProfileIDValue())
	setStringIfEmpty(&dst.AuthMode, src.AuthMode())
	setStringIfEmpty(&dst.OAuthAccountID, src.OAuthAccountID())
	setStringIfEmpty(&dst.OAuthProviderID, src.OAuthProviderID())
	setStringIfEmpty(&dst.FolderIDSecretName, src.FolderIDSecretName())
	if value, ok := src.SharedDrive(); ok && value {
		dst.SharedDrive = true
	}
	setStringIfEmpty(&dst.SharedDriveID, src.SharedDriveID())
	setStringIfEmpty(&dst.ArchiveFileName, src.ArchiveFileName())
	if dst.RetentionDays <= 0 {
		dst.RetentionDays = src.RetentionDays()
	}
	setStringIfEmpty(&dst.ClientID, src.ClientID())
	setStringIfEmpty(&dst.ClientSecretSecretName, src.ClientSecretSecretName())
	setStringIfEmpty(&dst.RefreshTokenSecretName, src.RefreshTokenSecretName())
}

func setStringIfEmpty(dst *string, value string) {
	if dst == nil || strings.TrimSpace(*dst) != "" || strings.TrimSpace(value) == "" {
		return
	}
	*dst = strings.TrimSpace(value)
}

func missingYouTubeRuntimeFields(job lifecycle.StreamJob) []string {
	var missing []string
	if strings.TrimSpace(job.RTMPURL) == "" {
		missing = append(missing, "rtmp_url")
	}
	if strings.TrimSpace(job.StreamKey) == "" {
		missing = append(missing, "stream_key")
	}
	if strings.TrimSpace(job.YouTubeOutputMode) == "" {
		missing = append([]string{"output_mode"}, missing...)
	}
	return missing
}

func resolveYouTubeRuntimeSecrets(ctx context.Context, job *lifecycle.StreamJob, resolver RuntimeSecretResolver) error {
	if job == nil || strings.TrimSpace(job.StreamKeySecretName) == "" {
		return nil
	}
	if resolver == nil {
		return errRuntimeSecretResolverNotConfigured
	}
	value, err := resolver(ctx, job.StreamID, "", job.StreamKeySecretName)
	if err != nil {
		return err
	}
	job.StreamKey = value
	return nil
}

func resolveArchiveRuntimeSecrets(ctx context.Context, job *lifecycle.StreamJob, resolver RuntimeSecretResolver) error {
	if job == nil {
		return nil
	}
	cfg := &job.ArchiveConfig
	if archiveConfigHasRawSecretFields(*cfg) {
		return errRawArchiveSecretFieldsNotAllowed
	}
	if unsupportedServiceAccountArchiveConfig(*cfg) || (strings.TrimSpace(cfg.AuthMode) != "" && cfg.AuthMode != "oauth2") {
		return errUnsupportedArchiveAuthMode
	}
	if cfg.AuthMode == "" || resolver == nil {
		return nil
	}
	if cfg.FolderID == "" && cfg.FolderIDSecretName != "" {
		value, err := resolver(ctx, job.StreamID, cfg.ArchiveProfileID, cfg.FolderIDSecretName)
		if err != nil {
			return err
		}
		cfg.FolderID = value
	}
	if cfg.ClientSecret == "" && cfg.ClientSecretSecretName != "" {
		value, err := resolver(ctx, job.StreamID, cfg.ArchiveProfileID, cfg.ClientSecretSecretName)
		if err != nil {
			return err
		}
		cfg.ClientSecret = value
	}
	if cfg.RefreshToken == "" && cfg.RefreshTokenSecretName != "" {
		value, err := resolver(ctx, job.StreamID, cfg.ArchiveProfileID, cfg.RefreshTokenSecretName)
		if err != nil {
			return err
		}
		cfg.RefreshToken = value
	}
	return nil
}

func archiveConfigHasRawSecretFields(cfg lifecycle.ArchiveConfig) bool {
	return strings.TrimSpace(cfg.FolderID) != "" ||
		strings.TrimSpace(cfg.ServiceAccountJSON) != "" ||
		strings.TrimSpace(cfg.ClientSecret) != "" ||
		strings.TrimSpace(cfg.RefreshToken) != ""
}

func unsupportedServiceAccountArchiveConfig(cfg lifecycle.ArchiveConfig) bool {
	return strings.TrimSpace(cfg.ServiceAccountSecretName) != "" ||
		strings.TrimSpace(cfg.ServiceAccountCredentialsSecretName) != ""
}

func resolvePackageArchiveRuntimeSecrets(ctx context.Context, job *lifecycle.PackageJob, resolver RuntimeSecretResolver) error {
	if job == nil {
		return nil
	}
	streamJob := lifecycle.StreamJob{StreamID: job.StreamID, ArchiveConfig: job.ArchiveConfig}
	if err := resolveArchiveRuntimeSecrets(ctx, &streamJob, resolver); err != nil {
		return err
	}
	job.ArchiveConfig = streamJob.ArchiveConfig
	return nil
}

func applyPackageArchiveRuntimeConfig(ctx context.Context, job *lifecycle.PackageJob, provider RuntimeConfigProvider) error {
	if job == nil {
		return nil
	}
	streamJob := lifecycle.StreamJob{StreamID: job.StreamID, ArchiveConfig: job.ArchiveConfig}
	if err := applyArchiveRuntimeConfig(ctx, &streamJob, provider); err != nil {
		return err
	}
	job.ArchiveConfig = streamJob.ArchiveConfig
	return nil
}

func writeRuntimeSecretResolveError(w http.ResponseWriter, err error) {
	if errors.Is(err, control.ErrRuntimeSecretLeaseActive) {
		writeJSON(w, http.StatusConflict, map[string]string{"code": control.RuntimeSecretLeaseActiveCode})
		return
	}
	if errors.Is(err, errRawArchiveSecretFieldsNotAllowed) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"code": "raw_archive_secret_fields_not_allowed"})
		return
	}
	if errors.Is(err, errRuntimeSecretResolverNotConfigured) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"code": "runtime_secret_resolver_not_configured"})
		return
	}
	if errors.Is(err, errUnsupportedArchiveAuthMode) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"code": "unsupported_archive_auth_mode"})
		return
	}
	writeJSON(w, http.StatusBadRequest, map[string]string{"code": "archive_secret_resolve_failed"})
}
