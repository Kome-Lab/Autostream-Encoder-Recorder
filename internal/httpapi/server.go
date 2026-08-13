package httpapi

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"mime"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/example/autostream-encoder-recorder/internal/archive"
	"github.com/example/autostream-encoder-recorder/internal/audioingest"
	"github.com/example/autostream-encoder-recorder/internal/control"
	"github.com/example/autostream-encoder-recorder/internal/ffmpeg"
	"github.com/example/autostream-encoder-recorder/internal/ingesttoken"
	"github.com/example/autostream-encoder-recorder/internal/lifecycle"
	"github.com/example/autostream-encoder-recorder/internal/observability"
	"github.com/example/autostream-encoder-recorder/internal/outputrelay"
	"github.com/example/autostream-encoder-recorder/internal/streamproc"
	"github.com/example/autostream-encoder-recorder/internal/version"
	"github.com/example/autostream-encoder-recorder/internal/videoingest"
	"github.com/example/autostream-encoder-recorder/internal/workerevents"
)

type Status struct {
	ServiceType string    `json:"service_type"`
	ServiceID   string    `json:"service_id"`
	Status      string    `json:"status"`
	CheckedAt   time.Time `json:"checked_at"`
}

type updaterVersionResponse struct {
	Version        string `json:"version"`
	ServiceID      string `json:"service_id"`
	ServiceType    string `json:"service_type"`
	ConfigRevision int64  `json:"config_revision"`
}

type TokenVerifier struct {
	PlainToken             string
	SHA256Hex              string
	UseNodeRuntimeToken    bool
	WorkerEventsPlainToken string
	WorkerEventsSHA256Hex  string
	DiscordAudioPlainToken string
	DiscordAudioSHA256Hex  string
	IngestTokenSigningKey  string
	RequireSignedIngest    bool
}

const (
	maxControlBodyBytes             = 64 * 1024
	maxWorkerEventBodyBytes         = 256 * 1024
	defaultDiscordAudioBodyBytes    = 512 * 1024
	defaultDiscordAudioMaxPackets   = 100
	defaultDiscordAudioMaxOpusBytes = 4096
)

var (
	errRawArchiveSecretFieldsNotAllowed   = errors.New("raw_archive_secret_fields_not_allowed")
	errRawYouTubeSecretFieldNotAllowed    = errors.New("raw_youtube_secret_field_not_allowed")
	errRuntimeSecretResolverNotConfigured = errors.New("runtime_secret_resolver_not_configured")
	errUnsupportedArchiveAuthMode         = errors.New("unsupported_archive_auth_mode")
)

func TokenVerifierFromEnv() TokenVerifier {
	verifier := TokenVerifier{
		PlainToken:             os.Getenv("SERVICE_CONTROL_TOKEN"),
		SHA256Hex:              os.Getenv("SERVICE_CONTROL_TOKEN_SHA256"),
		WorkerEventsPlainToken: os.Getenv("ENCODER_WORKER_EVENTS_TOKEN"),
		WorkerEventsSHA256Hex:  os.Getenv("ENCODER_WORKER_EVENTS_TOKEN_SHA256"),
		DiscordAudioPlainToken: os.Getenv("ENCODER_DISCORD_AUDIO_TOKEN"),
		DiscordAudioSHA256Hex:  os.Getenv("ENCODER_DISCORD_AUDIO_TOKEN_SHA256"),
		IngestTokenSigningKey:  control.StreamIngestSigningKey(),
		RequireSignedIngest:    envBool("AUTOSTREAM_REQUIRE_SIGNED_INGEST_TOKENS", true),
	}
	verifier.UseNodeRuntimeToken = control.NodeConfigPathFromEnv() != ""
	if verifier.UseNodeRuntimeToken {
		// Read the signing key from config.yml for each verification so a
		// Panel-issued config rotation takes effect without a process restart.
		verifier.IngestTokenSigningKey = ""
	}
	return verifier
}

func (v TokenVerifier) Verify(header string) bool {
	if v.UseNodeRuntimeToken {
		return verifyBearerToken(header, control.NodeRuntimeTokenFromEnv(), "")
	}
	if verifyBearerToken(header, v.PlainToken, v.SHA256Hex) {
		return true
	}
	return false
}

func (v TokenVerifier) VerifyWorkerEvents(header, streamID string) bool {
	_, ok := v.WorkerEventsClaims(header, streamID)
	return ok
}

func (v TokenVerifier) VerifyDiscordAudio(header, streamID string) bool {
	_, ok := v.DiscordAudioClaims(header, streamID)
	return ok
}

func (v TokenVerifier) WorkerEventsClaims(header, streamID string) (ingesttoken.Claims, bool) {
	if claims, ok := v.verifySignedIngest(header, ingesttoken.Expected{StreamID: streamID, ServiceType: "worker", Purpose: "worker_events", Audience: "encoder_recorder"}); ok {
		return claims, true
	}
	if !v.RequireSignedIngest && tokenConfigured(v.WorkerEventsPlainToken, v.WorkerEventsSHA256Hex) && verifyBearerToken(header, v.WorkerEventsPlainToken, v.WorkerEventsSHA256Hex) {
		return ingesttoken.Claims{}, true
	}
	return ingesttoken.Claims{}, false
}

func (v TokenVerifier) DiscordAudioClaims(header, streamID string) (ingesttoken.Claims, bool) {
	if claims, ok := v.verifySignedIngest(header, ingesttoken.Expected{StreamID: streamID, ServiceType: "discord_bot", Purpose: "discord_audio", Audience: "encoder_recorder"}); ok {
		return claims, true
	}
	if !v.RequireSignedIngest && tokenConfigured(v.DiscordAudioPlainToken, v.DiscordAudioSHA256Hex) && verifyBearerToken(header, v.DiscordAudioPlainToken, v.DiscordAudioSHA256Hex) {
		return ingesttoken.Claims{}, true
	}
	return ingesttoken.Claims{}, false
}

func (v TokenVerifier) WorkerVideoClaims(token, streamID string) (ingesttoken.Claims, bool) {
	return v.verifySignedIngestToken(strings.TrimSpace(token), ingesttoken.Expected{StreamID: streamID, ServiceType: "worker", Purpose: "worker_video", Audience: "encoder_recorder"})
}

func (v TokenVerifier) verifySignedIngest(header string, expected ingesttoken.Expected) (ingesttoken.Claims, bool) {
	return v.verifySignedIngestToken(bearerToken(header), expected)
}

func (v TokenVerifier) verifySignedIngestToken(token string, expected ingesttoken.Expected) (ingesttoken.Claims, bool) {
	signingKey := strings.TrimSpace(v.IngestTokenSigningKey)
	if signingKey == "" {
		signingKey = control.StreamIngestSigningKey()
	}
	if token == "" || !ingesttoken.IsSigned(token) || signingKey == "" {
		return ingesttoken.Claims{}, false
	}
	claims, err := ingesttoken.Verify(signingKey, token, expected)
	return claims, err == nil
}

func tokenConfigured(plain, sha256Hex string) bool {
	return strings.TrimSpace(plain) != "" || strings.TrimSpace(sha256Hex) != ""
}

func envBool(key string, fallback bool) bool {
	raw := strings.ToLower(strings.TrimSpace(os.Getenv(key)))
	if raw == "" {
		return fallback
	}
	return raw == "1" || raw == "true" || raw == "yes" || raw == "on"
}

func verifyBearerToken(header, plainToken, sha256Hex string) bool {
	token := bearerToken(header)
	if token == "" {
		return false
	}
	if sha256Hex != "" {
		sum := sha256.Sum256([]byte(token))
		got := hex.EncodeToString(sum[:])
		return subtle.ConstantTimeCompare([]byte(got), []byte(strings.ToLower(sha256Hex))) == 1
	}
	if plainToken != "" {
		return subtle.ConstantTimeCompare([]byte(token), []byte(plainToken)) == 1
	}
	return false
}

func bearerToken(header string) string {
	if !strings.HasPrefix(header, "Bearer ") {
		return ""
	}
	return strings.TrimSpace(strings.TrimPrefix(header, "Bearer "))
}

func requireServiceToken(w http.ResponseWriter, r *http.Request, verifier TokenVerifier) bool {
	if verifier.Verify(r.Header.Get("Authorization")) {
		return true
	}
	writeJSON(w, http.StatusUnauthorized, map[string]string{"code": "missing_or_invalid_service_token"})
	return false
}

func NewServer(serviceType string) http.Handler {
	return NewServerWithProcessManager(serviceType, streamproc.NewManagerFromEnv())
}

func NewServerWithProcessManager(serviceType string, processManager *streamproc.Manager) http.Handler {
	archiveRoot := os.Getenv("AUTOSTREAM_ARCHIVE_DIR")
	if archiveRoot == "" && processManager != nil {
		archiveRoot = processManager.ArchiveRoot
	}
	if archiveRoot == "" {
		archiveRoot = "/var/lib/autostream/archives"
	}
	return NewServerWithManagers(serviceType, processManager, workerevents.NewManager(archiveRoot), TokenVerifierFromEnv())
}

func NewServerWithManagers(serviceType string, processManager *streamproc.Manager, eventManager *workerevents.Manager, verifier TokenVerifier) http.Handler {
	return NewServerWithManagersAndSecretResolver(serviceType, processManager, eventManager, verifier, nil)
}

type RuntimeSecretResolver func(ctx context.Context, streamID, archiveProfileID, secretName string) (string, error)
type RuntimeConfigProvider func(ctx context.Context) (control.RuntimeConfig, error)

func NewServerWithManagersAndSecretResolver(serviceType string, processManager *streamproc.Manager, eventManager *workerevents.Manager, verifier TokenVerifier, resolver RuntimeSecretResolver) http.Handler {
	return NewServerWithManagersAndRuntimeConfig(serviceType, processManager, eventManager, verifier, resolver, nil)
}

func NewServerWithManagersAndRuntimeConfig(serviceType string, processManager *streamproc.Manager, eventManager *workerevents.Manager, verifier TokenVerifier, resolver RuntimeSecretResolver, runtimeConfig RuntimeConfigProvider) http.Handler {
	return NewServerWithManagersAndRuntimeConfigAndUpdaterIdentity(serviceType, processManager, eventManager, verifier, resolver, runtimeConfig, NewUpdaterIdentityLatch(serviceType))
}

func NewServerWithManagersAndRuntimeConfigAndUpdaterIdentity(serviceType string, processManager *streamproc.Manager, eventManager *workerevents.Manager, verifier TokenVerifier, resolver RuntimeSecretResolver, runtimeConfig RuntimeConfigProvider, updaterIdentity *UpdaterIdentityLatch) http.Handler {
	return newServerWithManagersAndRuntimeConfigAndUpdaterIdentity(serviceType, processManager, eventManager, verifier, resolver, runtimeConfig, updaterIdentity, videoingest.NewManagerFromEnv())
}

func newServerWithManagersAndRuntimeConfigAndUpdaterIdentity(serviceType string, processManager *streamproc.Manager, eventManager *workerevents.Manager, verifier TokenVerifier, resolver RuntimeSecretResolver, runtimeConfig RuntimeConfigProvider, updaterIdentity *UpdaterIdentityLatch, videoManager *videoingest.Manager) http.Handler {
	if updaterIdentity == nil {
		panic("encoder recorder updater identity latch is required")
	}
	if _, err := updaterIdentity.ResolveFromEnv(); err != nil && !errors.Is(err, ErrUpdaterIdentityPending) {
		panic(err)
	}
	processArchiveRoot := "/var/lib/autostream/archives"
	if processManager != nil && strings.TrimSpace(processManager.ArchiveRoot) != "" {
		processArchiveRoot = processManager.ArchiveRoot
	}
	if eventManager == nil {
		eventManager = workerevents.NewManager(processArchiveRoot)
	}
	eventArchiveRoot := processArchiveRoot
	if strings.TrimSpace(eventManager.ArchiveRoot) != "" {
		eventArchiveRoot = eventManager.ArchiveRoot
	}
	audioManager := audioingest.NewManager(eventArchiveRoot)
	audioManager.MaxPackets = envInt("AUDIO_INGEST_MAX_PACKETS", defaultDiscordAudioMaxPackets)
	audioManager.MaxOpusSize = envInt("AUDIO_INGEST_MAX_OPUS_BYTES", defaultDiscordAudioMaxOpusBytes)
	if processManager != nil {
		previousProcessExitHook := processManager.ProcessExitHook
		processManager.ProcessExitHook = func(streamID string) {
			if previousProcessExitHook != nil {
				previousProcessExitHook(streamID)
			}
			audioManager.StopBridge(streamID)
			if videoManager != nil {
				videoManager.StopBridge(streamID)
			}
		}
	}
	if processManager != nil && videoManager != nil && videoManager.Reporter == nil {
		videoManager.Reporter = processManager.Reporter
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /updater/version", func(w http.ResponseWriter, r *http.Request) {
		identity, err := updaterIdentity.ResolveFromEnv()
		if err != nil {
			code := "updater_identity_invalid"
			if errors.Is(err, ErrUpdaterIdentityPending) {
				code = "updater_identity_pending"
			}
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"code": code})
			return
		}
		writeJSON(w, http.StatusOK, updaterVersionResponse{
			Version:        version.Current(),
			ServiceID:      identity.ServiceID,
			ServiceType:    identity.ServiceType,
			ConfigRevision: identity.ConfigRevision,
		})
	})
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, Status{ServiceType: serviceType, ServiceID: control.ConfigFromEnv().ServiceID, Status: "ready", CheckedAt: time.Now().UTC()})
	})
	mux.HandleFunc("GET /preflight", servicePreflight(verifier))
	mux.HandleFunc("POST /heartbeat", func(w http.ResponseWriter, r *http.Request) {
		if !requireServiceToken(w, r, verifier) {
			return
		}
		writeJSON(w, http.StatusAccepted, map[string]string{"status": "accepted"})
	})
	mux.HandleFunc("POST /streams/dry-run", dryRunStream(verifier, runtimeConfig))
	mux.HandleFunc("POST /streams/start", startStream(processManager, audioManager, videoManager, verifier, resolver, runtimeConfig))
	mux.HandleFunc("PUT /streams/{id}/runtime-settings", updateStreamRuntimeSettings(processManager, verifier, runtimeConfig))
	mux.HandleFunc("POST /streams/{id}/stop", stopStream(processManager, audioManager, videoManager, verifier))
	mux.HandleFunc("GET /streams/{id}/process-status", streamProcessStatus(processManager, verifier))
	mux.HandleFunc("GET /streams/{id}/preview/{name}", streamPreview(processArchiveRoot, verifier))
	mux.HandleFunc("GET /streams/{id}/audio-status", discordAudioStatus(audioManager, verifier))
	mux.HandleFunc("POST /streams/package", packageStream(verifier, resolver, runtimeConfig))
	mux.HandleFunc("GET /streams/{id}/artifacts/{name}", downloadArchiveArtifact(eventManager.ArchiveRoot, verifier))
	mux.HandleFunc("DELETE /streams/{id}/artifacts/{name}", deleteArchiveArtifact(eventManager.ArchiveRoot, verifier))
	mux.HandleFunc("PUT /streams/{id}/artifacts/{name}", renameArchiveArtifact(eventManager.ArchiveRoot, verifier))
	mux.HandleFunc("POST /worker-events", workerEvents(eventManager, processManager, verifier))
	mux.HandleFunc("GET /streams/{id}/worker-events", recentWorkerEvents(eventManager, verifier))
	mux.HandleFunc("POST /streams/{id}/audio/opus", discordOpusAudio(audioManager, processManager, verifier))
	return securityHeaders(mux)
}

type runtimeSettingsRequest struct {
	EncoderAudioGainDB float64 `json:"encoder_audio_gain_db"`
	OverlayProfileID   string  `json:"overlay_profile_id"`
}

func updateStreamRuntimeSettings(processManager *streamproc.Manager, verifier TokenVerifier, runtimeConfig RuntimeConfigProvider) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireServiceToken(w, r, verifier) {
			return
		}
		if processManager == nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"code": "process_manager_not_configured"})
			return
		}
		var body runtimeSettingsRequest
		if status, err := decodeLimitedJSON(w, r, maxControlBodyBytes, &body); err != nil {
			writeJSON(w, status, map[string]string{"code": limitedJSONErrorCode(status)})
			return
		}
		job := lifecycle.StreamJob{StreamID: r.PathValue("id"), OverlayProfileID: strings.TrimSpace(body.OverlayProfileID)}
		if err := applyOverlayRuntimeConfig(r.Context(), &job, runtimeConfig); err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"code": "runtime_config_fetch_failed"})
			return
		}
		snapshot, err := processManager.UpdateRuntimeSettings(job.StreamID, streamproc.RuntimeSettings{
			EncoderAudioGainDB: body.EncoderAudioGainDB,
			OverlayProfileID:   job.OverlayProfileID,
			OverlayConfig:      job.OverlayConfig,
		})
		switch {
		case errors.Is(err, streamproc.ErrInvalidRuntimeSettings):
			writeJSON(w, http.StatusBadRequest, map[string]string{"code": "invalid_encoder_runtime_settings"})
		case errors.Is(err, streamproc.ErrNotRunning):
			writeJSON(w, http.StatusConflict, map[string]string{"code": "stream_not_running"})
		case err != nil:
			writeJSON(w, http.StatusBadGateway, map[string]string{"code": "encoder_runtime_settings_apply_failed"})
		default:
			writeJSON(w, http.StatusOK, publicProcessSnapshot(snapshot))
		}
	}
}

type preflightResponse struct {
	CheckedAt time.Time        `json:"checked_at"`
	Ready     bool             `json:"ready"`
	Checks    []preflightCheck `json:"checks"`
	Summary   map[string]any   `json:"summary"`
}

type preflightCheck struct {
	ID       string `json:"id"`
	Status   string `json:"status"`
	Severity string `json:"severity"`
	Message  string `json:"message"`
}

func servicePreflight(verifier TokenVerifier) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireServiceToken(w, r, verifier) {
			return
		}
		response := buildPreflight(verifier)
		writeJSON(w, http.StatusOK, response)
	}
}

func buildPreflight(verifier TokenVerifier) preflightResponse {
	checks := []preflightCheck{
		serviceTokenPreflight(verifier),
		ffmpegPreflight(envDefault("FFMPEG_BIN", "ffmpeg")),
		archiveRootPreflight(envDefault("AUTOSTREAM_ARCHIVE_DIR", "/var/lib/autostream/archives")),
		outputRelayPreflight(),
		youtubeRuntimeConfigPreflight("youtube_rtmp_url", "YOUTUBE_RTMP_URL", "YouTube RTMPS URL"),
		youtubeRuntimeConfigPreflight("youtube_stream_key", "YOUTUBE_STREAM_KEY", "YouTube stream key"),
		googleDrivePreflight(),
		observabilityPreflight(),
	}
	ready := true
	for _, check := range checks {
		if check.Severity == "critical" && check.Status != "ok" {
			ready = false
			break
		}
	}
	return preflightResponse{
		CheckedAt: time.Now().UTC(),
		Ready:     ready,
		Checks:    checks,
		Summary: map[string]any{
			"ffmpeg_bin":                  envDefault("FFMPEG_BIN", "ffmpeg"),
			"archive_root_configured":     strings.TrimSpace(envDefault("AUTOSTREAM_ARCHIVE_DIR", "/var/lib/autostream/archives")) != "",
			"google_drive_runtime_config": "control_panel_required",
			"google_drive_env_fallback":   false,
			"observability_configured":    observabilityDirectConfigured() || observabilityControlPanelProxyConfigured(),
		},
	}
}

func serviceTokenPreflight(verifier TokenVerifier) preflightCheck {
	if verifier.UseNodeRuntimeToken {
		if strings.TrimSpace(control.NodeRuntimeTokenFromEnv()) != "" {
			return preflightCheck{ID: "service_control_token", Status: "ok", Severity: "critical", Message: "Node Runtime Token from AUTOSTREAM_NODE_CONFIG is configured."}
		}
		if control.NodeConfigPendingFromEnv() {
			return preflightCheck{ID: "service_control_token", Status: "missing", Severity: "critical", Message: "Node Runtime Token is not available yet; run the Panel-generated Auto Configure command."}
		}
		return preflightCheck{ID: "service_control_token", Status: "missing", Severity: "critical", Message: "AUTOSTREAM_NODE_CONFIG exists but does not contain a usable Node Runtime Token."}
	}
	if strings.TrimSpace(verifier.SHA256Hex) != "" {
		return preflightCheck{ID: "service_control_token", Status: "ok", Severity: "critical", Message: "SERVICE_CONTROL_TOKEN_SHA256 is configured."}
	}
	if strings.TrimSpace(verifier.PlainToken) != "" {
		return preflightCheck{ID: "service_control_token", Status: "ok", Severity: "warning", Message: "SERVICE_CONTROL_TOKEN is configured. Prefer SERVICE_CONTROL_TOKEN_SHA256 for production."}
	}
	return preflightCheck{ID: "service_control_token", Status: "missing", Severity: "critical", Message: "Inbound service control token is not configured."}
}

func ffmpegPreflight(bin string) preflightCheck {
	if strings.TrimSpace(bin) == "" {
		return preflightCheck{ID: "ffmpeg_binary", Status: "missing", Severity: "critical", Message: "FFMPEG_BIN is empty."}
	}
	if _, err := exec.LookPath(bin); err != nil {
		return preflightCheck{ID: "ffmpeg_binary", Status: "not_found", Severity: "critical", Message: "FFmpeg binary was not found on PATH or at the configured location."}
	}
	return preflightCheck{ID: "ffmpeg_binary", Status: "ok", Severity: "critical", Message: "FFmpeg binary is available."}
}

func archiveRootPreflight(root string) preflightCheck {
	if strings.TrimSpace(root) == "" {
		return preflightCheck{ID: "archive_root", Status: "missing", Severity: "critical", Message: "AUTOSTREAM_ARCHIVE_DIR is empty."}
	}
	if err := os.MkdirAll(root, 0o750); err != nil {
		return preflightCheck{ID: "archive_root", Status: "unavailable", Severity: "critical", Message: "Archive root directory cannot be created or opened."}
	}
	probe, err := os.CreateTemp(root, ".autostream-preflight-*")
	if err != nil {
		return preflightCheck{ID: "archive_root", Status: "not_writable", Severity: "critical", Message: "Archive root directory is not writable."}
	}
	name := probe.Name()
	_ = probe.Close()
	_ = os.Remove(name)
	if !pathInsideRoot(root, name) {
		return preflightCheck{ID: "archive_root", Status: "invalid", Severity: "critical", Message: "Archive root write probe escaped the configured root."}
	}
	return preflightCheck{ID: "archive_root", Status: "ok", Severity: "critical", Message: "Archive root directory is writable."}
}

func pathInsideRoot(root, path string) bool {
	rootAbs, err := filepath.Abs(filepath.Clean(root))
	if err != nil {
		return false
	}
	pathAbs, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(rootAbs, pathAbs)
	if err != nil {
		return false
	}
	return rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

func outputRelayPreflight() preflightCheck {
	policy := outputrelay.FromEnv()
	if err := policy.ValidateConfiguration(); err != nil {
		if errors.Is(err, outputrelay.ErrRelayRequired) {
			return preflightCheck{ID: "output_relay", Status: "missing", Severity: "critical", Message: "AUTOSTREAM_OUTPUT_RELAY_URL is required in production to keep YouTube stream keys out of FFmpeg process arguments."}
		}
		message := "AUTOSTREAM_OUTPUT_RELAY_MODE and AUTOSTREAM_OUTPUT_RELAY_URL form an invalid output Relay configuration."
		if errors.Is(err, outputrelay.ErrStaticBindingRequired) {
			message = "AUTOSTREAM_OUTPUT_RELAY_BINDING_ID is required when AUTOSTREAM_OUTPUT_RELAY_MODE=live_api_static."
		} else if errors.Is(err, outputrelay.ErrInvalidRelayBindingID) {
			message = "AUTOSTREAM_OUTPUT_RELAY_BINDING_ID must match relay-xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx using lowercase hexadecimal UUID characters."
		} else if errors.Is(err, outputrelay.ErrUnsafeRelayTarget) {
			message = "AUTOSTREAM_OUTPUT_RELAY_URL must resolve to a loopback RTMP/RTMPS relay target."
		}
		return preflightCheck{ID: "output_relay", Status: "invalid", Severity: "critical", Message: message}
	}
	if !policy.UsesLocalRelay() {
		return preflightCheck{ID: "output_relay", Status: "compatibility_mode", Severity: "warning", Message: "Output relay is not configured; FFmpeg will use the direct RTMPS target in compatibility mode."}
	}
	target := relayOutputTargetForPreflight(policy.URL, "preflight-stream")
	if err := ffmpeg.ValidateRelayOutputTarget(target); err != nil {
		return preflightCheck{ID: "output_relay", Status: "invalid", Severity: "critical", Message: "AUTOSTREAM_OUTPUT_RELAY_URL must resolve to a loopback RTMP/RTMPS relay target."}
	}
	if policy.Mode == outputrelay.ModeLiveAPIStatic {
		return preflightCheck{ID: "output_relay", Status: "ok", Severity: "critical", Message: "Static Live API output Relay is configured; FFmpeg receives only the local relay target."}
	}
	return preflightCheck{ID: "output_relay", Status: "ok", Severity: "critical", Message: "Legacy stream-key output Relay is configured; FFmpeg receives only the local relay target."}
}

func relayOutputTargetForPreflight(template, streamID string) string {
	escapedStreamID := url.PathEscape(strings.TrimSpace(streamID))
	if strings.Contains(template, "{stream_id}") {
		return strings.ReplaceAll(template, "{stream_id}", escapedStreamID)
	}
	return strings.TrimRight(template, "/") + "/" + escapedStreamID
}

func youtubeRuntimeConfigPreflight(id, key, label string) preflightCheck {
	if strings.TrimSpace(os.Getenv(key)) != "" {
		return preflightCheck{ID: id, Status: "ok", Severity: "warning", Message: label + " env fallback is configured. Prefer Control Panel YouTube output runtime config."}
	}
	return preflightCheck{ID: id, Status: "runtime_config_required", Severity: "warning", Message: label + " should be supplied by Control Panel YouTube output runtime config; env fallback is optional."}
}

func googleDrivePreflight() preflightCheck {
	if strings.TrimSpace(os.Getenv("GOOGLE_DRIVE_AUTH_MODE")) != "" || strings.TrimSpace(os.Getenv("GOOGLE_APPLICATION_CREDENTIALS")) != "" {
		return preflightCheck{ID: "google_drive", Status: "unsupported_env_fallback", Severity: "warning", Message: "Google Drive env fallback is no longer supported; configure archive OAuth per stream in Control Panel."}
	}
	return preflightCheck{ID: "google_drive", Status: "runtime_config_required", Severity: "warning", Message: "Google Drive upload is supplied by Control Panel archive runtime config."}
}

func observabilityPreflight() preflightCheck {
	if observabilityDirectConfigured() {
		return preflightCheck{ID: "observability", Status: "ok", Severity: "warning", Message: "Direct Observability reporting is configured."}
	}
	if observabilityControlPanelProxyConfigured() {
		return preflightCheck{ID: "observability", Status: "proxied", Severity: "warning", Message: "Observability reporting uses Control Panel and the Node Runtime Token."}
	}
	urlConfigured := strings.TrimSpace(os.Getenv("OBSERVABILITY_URL")) != ""
	tokenConfigured := strings.TrimSpace(os.Getenv("OBSERVABILITY_TOKEN")) != ""
	if !urlConfigured && !tokenConfigured {
		return preflightCheck{ID: "observability", Status: "disabled", Severity: "warning", Message: "Observability is not configured; local service can run but incidents and metrics will not be reported."}
	}
	if !urlConfigured || !tokenConfigured {
		return preflightCheck{ID: "observability", Status: "partial", Severity: "warning", Message: "OBSERVABILITY_URL and OBSERVABILITY_TOKEN must both be configured."}
	}
	return preflightCheck{ID: "observability", Status: "disabled", Severity: "warning", Message: "Observability is not configured; local service can run but incidents and metrics will not be reported."}
}

func observabilityDirectConfigured() bool {
	return strings.TrimSpace(os.Getenv("OBSERVABILITY_URL")) != "" && strings.TrimSpace(os.Getenv("OBSERVABILITY_TOKEN")) != ""
}

func observabilityControlPanelProxyConfigured() bool {
	cfg := control.ConfigFromEnv()
	return strings.TrimSpace(cfg.ControlPanelURL) != "" && strings.TrimSpace(cfg.Token) != "" && strings.TrimSpace(cfg.ConfigError) == ""
}

func dryRunStream(verifier TokenVerifier, runtimeConfig RuntimeConfigProvider) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireServiceToken(w, r, verifier) {
			return
		}
		var job lifecycle.StreamJob
		if status, err := decodeLimitedJSON(w, r, maxControlBodyBytes, &job); err != nil {
			writeJSON(w, status, map[string]string{"code": limitedJSONErrorCode(status)})
			return
		}
		if err := applyYouTubeRuntimeConfig(r.Context(), &job, runtimeConfig); err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"code": "runtime_config_fetch_failed"})
			return
		}
		if err := applyArchiveRuntimeConfig(r.Context(), &job, runtimeConfig); err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"code": "runtime_config_fetch_failed"})
			return
		}
		if err := applyOverlayRuntimeConfig(r.Context(), &job, runtimeConfig); err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"code": "runtime_config_fetch_failed"})
			return
		}
		if rawYouTubeStreamKeyInputDisallowed(job) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"code": "raw_youtube_stream_key_not_allowed"})
			return
		}
		relayPolicy := outputrelay.FromEnv()
		usesLocalRelay, err := relayPolicy.AuthorizeYouTubeOutput(job.YouTubeOutputMode, job.YouTubeOutputReady, job.OutputRelayBindingID)
		if err != nil {
			writeOutputRelayPolicyError(w, err, "dry_run_failed")
			return
		}
		outputTarget := ""
		if usesLocalRelay {
			outputTarget = relayOutputTargetForPreflight(relayPolicy.URL, job.StreamID)
			if err := ffmpeg.ValidateRelayOutputTarget(outputTarget); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"code": "dry_run_failed"})
				return
			}
			clearUnusedYouTubeOutputTarget(&job)
		} else {
			if strings.TrimSpace(job.StreamKey) == "" && strings.TrimSpace(job.StreamKeySecretName) != "" {
				job.StreamKey = "<RUNTIME_STREAM_KEY>"
			}
			if missing := applyYouTubeRuntimeConfigFallback(&job); len(missing) > 0 {
				writeJSON(w, http.StatusBadRequest, map[string]any{"code": "youtube_runtime_config_required", "missing": missing})
				return
			}
		}
		archiveRoot := os.Getenv("AUTOSTREAM_ARCHIVE_DIR")
		if archiveRoot == "" {
			archiveRoot = "/var/lib/autostream/archives"
		}
		ffmpegBin := os.Getenv("FFMPEG_BIN")
		if ffmpegBin == "" {
			ffmpegBin = "ffmpeg"
		}
		manager := lifecycle.Manager{
			ArchiveRoot: archiveRoot,
			FFmpegBin:   ffmpegBin,
			Runner:      &ffmpeg.DryRunRunner{},
			Uploader:    archive.DryRunUploader{},
		}
		result, err := manager.DryRunToOutputTarget(r.Context(), job, outputTarget)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"code": "dry_run_failed"})
			return
		}
		writeJSON(w, http.StatusAccepted, result.Metadata)
	}
}

type startStreamRequest struct {
	lifecycle.StreamJob
	WorkerVideoIngest      bool   `json:"worker_video_ingest,omitempty"`
	WorkerVideoIngestToken string `json:"worker_video_ingest_token,omitempty"`
}

type startStreamResponse struct {
	processSnapshotResponse
	VideoIngest *videoingest.Bridge `json:"video_ingest,omitempty"`
}

func startStream(processManager *streamproc.Manager, audioManager *audioingest.Manager, videoManager *videoingest.Manager, verifier TokenVerifier, resolver RuntimeSecretResolver, runtimeConfig RuntimeConfigProvider) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireServiceToken(w, r, verifier) {
			return
		}
		if processManager == nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"code": "process_manager_not_configured"})
			return
		}
		var startRequest startStreamRequest
		if status, err := decodeLimitedJSON(w, r, maxControlBodyBytes, &startRequest); err != nil {
			writeJSON(w, status, map[string]string{"code": limitedJSONErrorCode(status)})
			return
		}
		job := startRequest.StreamJob
		if !startRequest.WorkerVideoIngest && strings.TrimSpace(startRequest.WorkerVideoIngestToken) != "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"code": "worker_video_ingest_not_enabled"})
			return
		}
		if err := applyYouTubeRuntimeConfig(r.Context(), &job, runtimeConfig); err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"code": "runtime_config_fetch_failed"})
			return
		}
		if err := applyArchiveRuntimeConfig(r.Context(), &job, runtimeConfig); err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"code": "runtime_config_fetch_failed"})
			return
		}
		if err := applyOverlayRuntimeConfig(r.Context(), &job, runtimeConfig); err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"code": "runtime_config_fetch_failed"})
			return
		}
		if rawYouTubeStreamKeyInputDisallowed(job) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"code": "raw_youtube_stream_key_not_allowed"})
			return
		}
		usesLocalRelay, err := processManager.AuthorizeOutputRelay(job)
		if err != nil {
			writeOutputRelayPolicyError(w, err, "start_stream_failed")
			return
		}
		if usesLocalRelay {
			clearUnusedYouTubeOutputTarget(&job)
		} else {
			if err := resolveYouTubeRuntimeSecrets(r.Context(), &job, resolver); err != nil {
				writeRuntimeSecretResolveError(w, err)
				return
			}
			if missing := applyYouTubeRuntimeConfigFallback(&job); len(missing) > 0 {
				writeJSON(w, http.StatusBadRequest, map[string]any{"code": "youtube_runtime_config_required", "missing": missing})
				return
			}
		}
		if err := resolveArchiveRuntimeSecrets(r.Context(), &job, resolver); err != nil {
			writeRuntimeSecretResolveError(w, err)
			return
		}
		audioBridgeMode := false
		videoBridgeMode := false
		var videoBridge videoingest.Bridge
		if startRequest.WorkerVideoIngest {
			if strings.TrimSpace(job.InputURL) != "" || strings.TrimSpace(job.InputMode) != "" {
				writeJSON(w, http.StatusBadRequest, map[string]string{"code": "worker_video_input_must_be_service_managed"})
				return
			}
			if _, ok := verifier.WorkerVideoClaims(startRequest.WorkerVideoIngestToken, job.StreamID); !ok {
				writeJSON(w, http.StatusUnauthorized, map[string]string{"code": "missing_or_invalid_worker_video_ingest_token"})
				return
			}
			if audioManager == nil || videoManager == nil {
				writeJSON(w, http.StatusServiceUnavailable, map[string]string{"code": "worker_video_ingest_unavailable"})
				return
			}
			if existing, statusErr := processManager.Status(job.StreamID); statusErr == nil && (existing.Status == "starting" || existing.Status == "running" || existing.Status == "stopping" || existing.Status == "packaging") {
				writeJSON(w, http.StatusConflict, map[string]string{"code": "stream_already_running"})
				return
			}
			videoBridge, err = videoManager.StartBridge(job.StreamID, startRequest.WorkerVideoIngestToken)
			if err != nil {
				if errors.Is(err, videoingest.ErrAlreadyRunning) {
					writeJSON(w, http.StatusConflict, map[string]string{"code": "stream_already_running"})
					return
				}
				writeJSON(w, http.StatusServiceUnavailable, map[string]string{"code": "worker_video_ingest_unavailable"})
				return
			}
			videoBridgeMode = true
			audioBridge, err := audioManager.StartBridge(job.StreamID)
			if err != nil {
				videoManager.StopBridge(job.StreamID)
				writeJSON(w, http.StatusBadRequest, map[string]string{"code": "audio_bridge_failed"})
				return
			}
			audioBridgeMode = true
			job.InputURL = videoBridge.InputURL
			job.AudioInputURL = audioBridge.InputURL
			job.InputMode = "worker_scene_frames_srt"
		} else if strings.TrimSpace(job.InputURL) == "" {
			if audioManager == nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"code": "input_url_required"})
				return
			}
			bridge, err := audioManager.StartBridge(job.StreamID)
			if err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"code": "audio_bridge_failed"})
				return
			}
			job.InputURL = bridge.InputURL
			job.InputMode = "discord_opus_rtp"
			audioBridgeMode = true
		} else if strings.HasPrefix(strings.TrimSpace(job.InputURL), "internal_discord_audio:") || strings.HasPrefix(strings.TrimSpace(job.InputURL), "internal_worker_video:") {
			writeJSON(w, http.StatusBadRequest, map[string]string{"code": "internal_audio_input_not_allowed"})
			return
		}
		snapshot, err := processManager.Start(job)
		if errors.Is(err, streamproc.ErrAlreadyRunning) {
			if audioBridgeMode && audioManager != nil {
				audioManager.StopBridge(job.StreamID)
			}
			if videoBridgeMode && videoManager != nil {
				videoManager.StopBridge(job.StreamID)
			}
			writeJSON(w, http.StatusConflict, map[string]string{"code": "stream_already_running"})
			return
		}
		if err != nil {
			if audioBridgeMode && audioManager != nil {
				audioManager.StopBridge(job.StreamID)
			}
			if videoBridgeMode && videoManager != nil {
				videoManager.StopBridge(job.StreamID)
			}
			writeJSON(w, http.StatusBadRequest, map[string]string{"code": "start_stream_failed"})
			return
		}
		if audioBridgeMode {
			go reportDiscordAudioHealth(processManager, audioManager, job.StreamID)
		}
		response := startStreamResponse{processSnapshotResponse: publicProcessSnapshot(snapshot)}
		if videoBridgeMode {
			response.VideoIngest = &videoBridge
			w.Header().Set("Cache-Control", "no-store")
		}
		writeJSON(w, http.StatusAccepted, response)
	}
}

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
	setStringIfEmpty(&dst.BasePath, src.BasePath())
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

func applyYouTubeRuntimeConfigFallback(job *lifecycle.StreamJob) []string {
	if job == nil {
		return nil
	}
	missing := missingYouTubeRuntimeFields(*job)
	if len(missing) == 0 {
		return nil
	}
	if requireControlPanelRuntimeConfig() {
		return missing
	}
	if job.StreamKey == "" {
		job.StreamKey = os.Getenv("YOUTUBE_STREAM_KEY")
	}
	if job.RTMPURL == "" {
		job.RTMPURL = os.Getenv("YOUTUBE_RTMP_URL")
	}
	return missingYouTubeRuntimeFields(*job)
}

func missingYouTubeRuntimeFields(job lifecycle.StreamJob) []string {
	var missing []string
	if strings.TrimSpace(job.RTMPURL) == "" {
		missing = append(missing, "rtmp_url")
	}
	if strings.TrimSpace(job.StreamKey) == "" {
		missing = append(missing, "stream_key")
	}
	return missing
}

func requireControlPanelRuntimeConfig() bool {
	if envBool("AUTOSTREAM_REQUIRE_CONTROL_PANEL_RUNTIME_CONFIG", false) {
		return true
	}
	return strings.EqualFold(strings.TrimSpace(os.Getenv("AUTOSTREAM_ENV")), "production")
}

func rawYouTubeStreamKeyInputDisallowed(job lifecycle.StreamJob) bool {
	return requireControlPanelRuntimeConfig() &&
		strings.TrimSpace(job.StreamKey) != "" &&
		strings.TrimSpace(job.StreamKeySecretName) == ""
}

func resolveYouTubeRuntimeSecrets(ctx context.Context, job *lifecycle.StreamJob, resolver RuntimeSecretResolver) error {
	if job == nil || strings.TrimSpace(job.StreamKeySecretName) == "" {
		return nil
	}
	if resolver == nil {
		return errRuntimeSecretResolverNotConfigured
	}
	if strings.TrimSpace(job.StreamKey) != "" {
		return errRawYouTubeSecretFieldNotAllowed
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

func stopStream(processManager *streamproc.Manager, audioManager *audioingest.Manager, videoManager *videoingest.Manager, verifier TokenVerifier) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireServiceToken(w, r, verifier) {
			return
		}
		if processManager == nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"code": "process_manager_not_configured"})
			return
		}
		snapshot, err := processManager.Stop(r.PathValue("id"))
		if errors.Is(err, streamproc.ErrAlreadyStopped) {
			if audioManager != nil {
				audioManager.StopBridge(r.PathValue("id"))
			}
			if videoManager != nil {
				videoManager.StopBridge(r.PathValue("id"))
			}
			writeJSON(w, http.StatusAccepted, map[string]string{"status": "already_stopped"})
			return
		}
		if errors.Is(err, streamproc.ErrNotRunning) {
			if currentStreamID := processManager.CurrentStreamID(); currentStreamID != "" && currentStreamID != r.PathValue("id") {
				writeJSON(w, http.StatusConflict, map[string]string{"code": "stream_already_running"})
				return
			}
			writeJSON(w, http.StatusNotFound, map[string]string{"code": "stream_not_running"})
			return
		}
		if errors.Is(err, streamproc.ErrStarting) {
			writeJSON(w, http.StatusConflict, map[string]string{"code": "stream_starting"})
			return
		}
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"code": "stop_stream_failed"})
			return
		}
		if audioManager != nil {
			audioManager.StopBridge(r.PathValue("id"))
		}
		if videoManager != nil {
			videoManager.StopBridge(r.PathValue("id"))
		}
		writeJSON(w, http.StatusAccepted, publicProcessSnapshot(snapshot))
	}
}

func streamProcessStatus(processManager *streamproc.Manager, verifier TokenVerifier) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireServiceToken(w, r, verifier) {
			return
		}
		if processManager == nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"code": "process_manager_not_configured"})
			return
		}
		snapshot, err := processManager.Status(r.PathValue("id"))
		if errors.Is(err, streamproc.ErrNotRunning) {
			writeJSON(w, http.StatusNotFound, map[string]string{"code": "stream_not_running"})
			return
		}
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"code": "get_process_status_failed"})
			return
		}
		writeJSON(w, http.StatusOK, publicProcessSnapshot(snapshot))
	}
}

type processSnapshotResponse struct {
	StreamID           string            `json:"stream_id"`
	Name               string            `json:"name"`
	Status             string            `json:"status"`
	StartedAtJST       string            `json:"started_at_jst"`
	StoppedAtJST       string            `json:"stopped_at_jst,omitempty"`
	Archive            map[string]string `json:"archive"`
	Error              string            `json:"error,omitempty"`
	EncoderAudioGainDB float64           `json:"encoder_audio_gain_db"`
	OverlayProfileID   string            `json:"overlay_profile_id,omitempty"`
}

func publicProcessSnapshot(snapshot streamproc.Snapshot) processSnapshotResponse {
	return processSnapshotResponse{
		StreamID:           snapshot.StreamID,
		Name:               snapshot.Name,
		Status:             snapshot.Status,
		StartedAtJST:       snapshot.StartedAtJST,
		StoppedAtJST:       snapshot.StoppedAtJST,
		Archive:            snapshot.Archive,
		Error:              snapshot.Error,
		EncoderAudioGainDB: snapshot.EncoderAudioGainDB,
		OverlayProfileID:   snapshot.OverlayProfileID,
	}
}

func streamPreview(archiveRoot string, verifier TokenVerifier) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireServiceToken(w, r, verifier) {
			return
		}
		layout, err := archive.NewLayout(archiveRoot, r.PathValue("id"))
		if err != nil || !archive.IsPreviewFileName(r.PathValue("name")) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"code": "invalid_preview_path"})
			return
		}
		file, info, err := archive.OpenPreviewFile(layout, r.PathValue("name"))
		if errors.Is(err, os.ErrNotExist) {
			writeJSON(w, http.StatusNotFound, map[string]string{"code": "preview_not_found"})
			return
		}
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"code": "invalid_preview_file"})
			return
		}
		defer file.Close()

		w.Header().Set("Vary", "Authorization")
		if r.PathValue("name") == archive.PreviewPlaylistName {
			w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
			w.Header().Set("Cache-Control", "no-store")
		} else {
			w.Header().Set("Content-Type", "video/mp2t")
			w.Header().Set("Cache-Control", "private, max-age=30")
		}
		http.ServeContent(w, r, r.PathValue("name"), info.ModTime(), file)
	}
}

func discordAudioStatus(audioManager *audioingest.Manager, verifier TokenVerifier) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireServiceToken(w, r, verifier) {
			return
		}
		if audioManager == nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"code": "audio_ingest_not_configured"})
			return
		}
		writeJSON(w, http.StatusOK, audioManager.Status(r.PathValue("id"), time.Now().UTC()))
	}
}

func packageStream(verifier TokenVerifier, resolver RuntimeSecretResolver, runtimeConfig RuntimeConfigProvider) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireServiceToken(w, r, verifier) {
			return
		}
		var job lifecycle.PackageJob
		if status, err := decodeLimitedJSON(w, r, maxControlBodyBytes, &job); err != nil {
			writeJSON(w, status, map[string]string{"code": limitedJSONErrorCode(status)})
			return
		}
		if err := applyPackageArchiveRuntimeConfig(r.Context(), &job, runtimeConfig); err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"code": "runtime_config_fetch_failed"})
			return
		}
		if err := resolvePackageArchiveRuntimeSecrets(r.Context(), &job, resolver); err != nil {
			writeRuntimeSecretResolveError(w, err)
			return
		}
		archiveRoot := os.Getenv("AUTOSTREAM_ARCHIVE_DIR")
		if archiveRoot == "" {
			archiveRoot = "/var/lib/autostream/archives"
		}
		ffmpegBin := os.Getenv("FFMPEG_BIN")
		if ffmpegBin == "" {
			ffmpegBin = "ffmpeg"
		}
		var runner ffmpeg.Runner = ffmpeg.CommandRunner{}
		if job.DryRun {
			runner = &ffmpeg.DryRunRunner{}
		}
		uploader := archive.RetryUploader{
			Inner:  uploaderFromEnv(job.DryRun),
			Policy: archive.RetryPolicy{MaxAttempts: envInt("GOOGLE_DRIVE_UPLOAD_RETRY_MAX", 5), BaseDelay: time.Duration(envInt("GOOGLE_DRIVE_UPLOAD_RETRY_BASE_DELAY_SEC", 2)) * time.Second},
		}
		manager := lifecycle.Manager{ArchiveRoot: archiveRoot, FFmpegBin: ffmpegBin, Runner: runner, Uploader: uploader}
		started := time.Now().UTC()
		result, err := manager.Package(r.Context(), job)
		if err != nil {
			reportPackageFailed(r.Context(), job, time.Since(started), err)
			writeJSON(w, http.StatusBadRequest, packageFailureResponse(err, job.DryRun))
			return
		}
		reportPackageCompleted(r.Context(), job, result, time.Since(started))
		reportControlPanelArtifacts(r.Context(), job.StreamID, result)
		writeJSON(w, http.StatusAccepted, result.Metadata)
	}
}

func downloadArchiveArtifact(archiveRoot string, verifier TokenVerifier) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireServiceToken(w, r, verifier) {
			return
		}
		path, info, err := safeArchiveArtifactPath(archiveRoot, r.PathValue("id"), r.PathValue("name"))
		if errors.Is(err, os.ErrNotExist) {
			writeJSON(w, http.StatusNotFound, map[string]string{"code": "artifact_not_found"})
			return
		}
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"code": "invalid_archive_artifact"})
			return
		}
		file, err := os.Open(path)
		if errors.Is(err, os.ErrNotExist) {
			writeJSON(w, http.StatusNotFound, map[string]string{"code": "artifact_not_found"})
			return
		}
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"code": "open_archive_artifact_failed"})
			return
		}
		defer file.Close()
		if err := verifyOpenArchiveArtifact(path, file, info); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"code": "invalid_archive_artifact"})
			return
		}
		contentType := mime.TypeByExtension(filepath.Ext(r.PathValue("name")))
		if contentType == "" {
			contentType = "application/octet-stream"
		}
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("Content-Disposition", `attachment; filename="`+safeContentDispositionName(r.PathValue("name"))+`"`)
		http.ServeContent(w, r, r.PathValue("name"), info.ModTime(), file)
	}
}

func deleteArchiveArtifact(archiveRoot string, verifier TokenVerifier) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireServiceToken(w, r, verifier) {
			return
		}
		path, _, err := safeArchiveArtifactPath(archiveRoot, r.PathValue("id"), r.PathValue("name"))
		if errors.Is(err, os.ErrNotExist) {
			writeJSON(w, http.StatusNotFound, map[string]string{"code": "artifact_not_found"})
			return
		}
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"code": "invalid_archive_artifact"})
			return
		}
		if err := os.Remove(path); errors.Is(err, os.ErrNotExist) {
			writeJSON(w, http.StatusNotFound, map[string]string{"code": "artifact_not_found"})
			return
		} else if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"code": "delete_archive_artifact_failed"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
	}
}

func renameArchiveArtifact(archiveRoot string, verifier TokenVerifier) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireServiceToken(w, r, verifier) {
			return
		}
		var body struct {
			Name string `json:"name"`
		}
		if status, err := decodeLimitedJSON(w, r, maxControlBodyBytes, &body); err != nil {
			writeJSON(w, status, map[string]string{"code": limitedJSONErrorCode(status)})
			return
		}
		source, _, err := safeArchiveArtifactPath(archiveRoot, r.PathValue("id"), r.PathValue("name"))
		if errors.Is(err, os.ErrNotExist) {
			writeJSON(w, http.StatusNotFound, map[string]string{"code": "artifact_not_found"})
			return
		}
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"code": "invalid_archive_artifact"})
			return
		}
		target, _, err := safeArchiveArtifactPathForName(archiveRoot, r.PathValue("id"), body.Name, false)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"code": "invalid_archive_artifact"})
			return
		}
		if _, err := os.Lstat(target); err == nil {
			writeJSON(w, http.StatusConflict, map[string]string{"code": "artifact_exists"})
			return
		} else if !errors.Is(err, os.ErrNotExist) {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"code": "stat_archive_artifact_failed"})
			return
		}
		if err := os.Rename(source, target); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"code": "rename_archive_artifact_failed"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "renamed", "name": strings.TrimSpace(body.Name)})
	}
}

func safeArchiveArtifactPath(archiveRoot, streamID, name string) (string, os.FileInfo, error) {
	return safeArchiveArtifactPathForName(archiveRoot, streamID, name, true)
}

func safeArchiveArtifactPathForName(archiveRoot, streamID, name string, requireExisting bool) (string, os.FileInfo, error) {
	layout, err := archive.NewLayout(archiveRoot, streamID)
	if err != nil {
		return "", nil, err
	}
	name = strings.TrimSpace(name)
	if !safeArchiveArtifactName(name) {
		return "", nil, errors.New("unsafe archive artifact name")
	}
	if err := archive.EnsureDirNoSymlinks(layout.RootDir, layout.FinalDir()); err != nil {
		return "", nil, err
	}
	path := filepath.Join(layout.FinalDir(), name)
	rootAbs, err := filepath.Abs(layout.RootDir)
	if err != nil {
		return "", nil, err
	}
	pathAbs, err := filepath.Abs(path)
	if err != nil {
		return "", nil, err
	}
	rel, err := filepath.Rel(rootAbs, pathAbs)
	if err != nil {
		return "", nil, err
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return "", nil, errors.New("archive artifact path escaped root")
	}
	info, err := os.Lstat(pathAbs)
	if err != nil {
		if !requireExisting && errors.Is(err, os.ErrNotExist) {
			return pathAbs, nil, nil
		}
		return "", nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return "", nil, errors.New("archive artifact must be a regular file")
	}
	return pathAbs, info, nil
}

func verifyOpenArchiveArtifact(path string, file *os.File, before os.FileInfo) error {
	after, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if after.Mode()&os.ModeSymlink != 0 || !after.Mode().IsRegular() {
		return errors.New("archive artifact must be a regular file")
	}
	opened, err := file.Stat()
	if err != nil {
		return err
	}
	if !opened.Mode().IsRegular() || !os.SameFile(before, after) || !os.SameFile(opened, after) {
		return errors.New("archive artifact changed while opening")
	}
	return nil
}

func safeArchiveArtifactName(name string) bool {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 255 || strings.Contains(name, "..") || strings.ContainsAny(name, `/\`) {
		return false
	}
	ext := strings.ToLower(filepath.Ext(name))
	if ext != ".mp4" && ext != ".mkv" && ext != ".json" && ext != ".jsonl" && ext != ".vtt" {
		return false
	}
	for _, r := range name {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.' {
			continue
		}
		return false
	}
	return true
}

func safeContentDispositionName(name string) string {
	name = strings.TrimSpace(name)
	name = strings.ReplaceAll(name, `"`, "")
	name = strings.ReplaceAll(name, "\\", "_")
	name = strings.ReplaceAll(name, "/", "_")
	if name == "" {
		return "archive"
	}
	return name
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
	if errors.Is(err, errRawYouTubeSecretFieldNotAllowed) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"code": "raw_youtube_secret_field_not_allowed"})
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

func workerEvents(eventManager *workerevents.Manager, processManager *streamproc.Manager, verifier TokenVerifier) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var event workerevents.Event
		if status, err := decodeLimitedJSON(w, r, maxWorkerEventBodyBytes, &event); err != nil {
			writeJSON(w, status, map[string]string{"code": limitedJSONErrorCode(status)})
			return
		}
		claims, authorized := verifier.WorkerEventsClaims(r.Header.Get("Authorization"), event.StreamID)
		if !authorized {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"code": "missing_or_invalid_worker_events_token"})
			return
		}
		if claims.ServiceID != "" && claims.ServiceID != event.ServiceID {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"code": "worker_service_id_mismatch"})
			return
		}
		if !isRunningStream(processManager, event.StreamID) {
			writeJSON(w, http.StatusConflict, map[string]string{"code": "stream_not_running"})
			return
		}
		result, err := eventManager.Add(event)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"code": "worker_event_rejected"})
			return
		}
		reportWorkerEventReceived(r.Context(), event)
		writeJSON(w, http.StatusAccepted, result)
	}
}

func recentWorkerEvents(eventManager *workerevents.Manager, verifier TokenVerifier) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !verifier.Verify(r.Header.Get("Authorization")) {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"code": "missing_or_invalid_service_token"})
			return
		}
		events, err := eventManager.Recent(r.PathValue("id"))
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"code": "worker_events_unavailable"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"events": events})
	}
}

func discordOpusAudio(audioManager *audioingest.Manager, processManager *streamproc.Manager, verifier TokenVerifier) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if audioManager == nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"code": "audio_ingest_not_configured"})
			return
		}
		var req audioingest.IngestRequest
		if status, err := decodeLimitedJSON(w, r, int64(envInt("AUDIO_INGEST_MAX_BODY_BYTES", defaultDiscordAudioBodyBytes)), &req); err != nil {
			writeJSON(w, status, map[string]string{"code": limitedJSONErrorCode(status)})
			return
		}
		if req.StreamID == "" {
			req.StreamID = r.PathValue("id")
		}
		if req.StreamID != r.PathValue("id") {
			writeJSON(w, http.StatusBadRequest, map[string]string{"code": "stream_id_mismatch"})
			return
		}
		claims, authorized := verifier.DiscordAudioClaims(r.Header.Get("Authorization"), req.StreamID)
		if !authorized {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"code": "missing_or_invalid_discord_audio_token"})
			return
		}
		if claims.ServiceID != "" && claims.ServiceID != req.Source {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"code": "discord_service_id_mismatch"})
			return
		}
		if !isRunningStream(processManager, req.StreamID) {
			writeJSON(w, http.StatusConflict, map[string]string{"code": "stream_not_running"})
			return
		}
		if !audioManager.Status(req.StreamID, time.Now().UTC()).BridgeActive {
			writeJSON(w, http.StatusConflict, map[string]string{"code": "audio_bridge_not_active"})
			return
		}
		result, err := audioManager.Add(req)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"code": "audio_ingest_rejected"})
			return
		}
		reportDiscordAudioReceived(r.Context(), req.StreamID, result.AcceptedCount)
		writeJSON(w, http.StatusAccepted, result)
	}
}

func decodeLimitedJSON(w http.ResponseWriter, r *http.Request, limit int64, dst any) (int, error) {
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	decoder := json.NewDecoder(r.Body)
	if err := decoder.Decode(dst); err != nil {
		return limitedJSONStatus(err), err
	}
	if err := decoder.Decode(&struct{}{}); errors.Is(err, io.EOF) {
		return http.StatusOK, nil
	} else {
		return limitedJSONStatus(err), err
	}
}

func limitedJSONStatus(err error) int {
	var maxErr *http.MaxBytesError
	if errors.As(err, &maxErr) {
		return http.StatusRequestEntityTooLarge
	}
	return http.StatusBadRequest
}

func limitedJSONErrorCode(status int) string {
	if status == http.StatusRequestEntityTooLarge {
		return "request_body_too_large"
	}
	return "bad_request"
}

func isRunningStream(processManager *streamproc.Manager, streamID string) bool {
	if processManager == nil || strings.TrimSpace(streamID) == "" {
		return false
	}
	snapshot, err := processManager.Status(streamID)
	return err == nil && snapshot.Status == "running"
}

func reportWorkerEventReceived(ctx context.Context, event workerevents.Event) {
	reporter := observability.NewClientFromEnv()
	if !reporter.Enabled() {
		return
	}
	_ = reporter.Event(ctx, event.StreamID, "worker.event.received", "accepted", map[string]any{"event_type": event.Type, "attempt": event.Attempt})
}

func reportDiscordAudioReceived(ctx context.Context, streamID string, count int) {
	reporter := observability.NewClientFromEnv()
	if !reporter.Enabled() {
		return
	}
	reportMetric(ctx, reporter, streamID, "discord.audio_receiving", 1)
	_ = reporter.Event(ctx, streamID, "discord.audio_ingest.received", "accepted", map[string]any{"packet_count": count})
}

func reportDiscordAudioHealth(processManager *streamproc.Manager, audioManager *audioingest.Manager, streamID string) {
	if processManager == nil || audioManager == nil || streamID == "" {
		return
	}
	reporter := observability.NewClientFromEnv()
	if !reporter.Enabled() {
		return
	}
	interval := time.Duration(envInt("AUDIO_INGEST_METRICS_INTERVAL_SEC", 5)) * time.Second
	timeoutSec := float64(envInt("AUDIO_INGEST_TIMEOUT_SEC", 5))
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		snapshot, err := processManager.Status(streamID)
		if err != nil || (snapshot.Status != "running" && snapshot.Status != "stopping") {
			return
		}
		stats := audioManager.Status(streamID, time.Now().UTC())
		receiving := 0.0
		timeout := stats.LastPacketAgeSec
		if stats.PacketsTotal > 0 && stats.LastPacketAgeSec < timeoutSec {
			receiving = 1
			timeout = 0
		}
		reportMetric(context.Background(), reporter, streamID, "discord.audio_receiving", receiving)
		reportMetric(context.Background(), reporter, streamID, "discord.audio_packets_total", float64(stats.PacketsTotal))
		reportMetric(context.Background(), reporter, streamID, "media.input_timeout_sec", timeout)
		<-ticker.C
	}
}

func reportPackageFailed(ctx context.Context, job lifecycle.PackageJob, elapsed time.Duration, err error) {
	reporter := observability.NewClientFromEnv()
	if !reporter.Enabled() {
		return
	}
	phase := lifecycle.ErrorPhase(err)
	if phase == "upload" {
		reportMetric(ctx, reporter, job.StreamID, "archive.package_status", 1)
		reportMetric(ctx, reporter, job.StreamID, "gdrive.upload_status", 0)
	} else {
		reportMetric(ctx, reporter, job.StreamID, "archive.package_status", 0)
	}
	reportMetric(ctx, reporter, job.StreamID, "gdrive.upload_duration_sec", elapsed.Seconds())
	_ = reporter.Event(ctx, job.StreamID, "archive.package.failed", "failed", packageFailureAttributes(err, job.DryRun))
}

func reportPackageCompleted(ctx context.Context, job lifecycle.PackageJob, result lifecycle.Result, elapsed time.Duration) {
	reporter := observability.NewClientFromEnv()
	if !reporter.Enabled() {
		return
	}
	reportMetric(ctx, reporter, job.StreamID, "archive.package_status", 1)
	reportMetric(ctx, reporter, job.StreamID, "archive.final_mp4_exists", 1)
	reportMetric(ctx, reporter, job.StreamID, "recorder.remux_duration_ms", result.RemuxDurationMS)
	reportMetric(ctx, reporter, job.StreamID, "gdrive.upload_status", 1)
	reportMetric(ctx, reporter, job.StreamID, "gdrive.upload_retry_count", float64(maxInt(result.Metadata.Upload.Attempts-1, 0)))
	reportMetric(ctx, reporter, job.StreamID, "gdrive.upload_duration_sec", elapsed.Seconds())
	reportMetric(ctx, reporter, job.StreamID, "gdrive.upload_file_count", float64(result.Metadata.Upload.UploadedFileCount()))
	reportMetric(ctx, reporter, job.StreamID, "gdrive.upload_folder_fingerprint_present", boolMetric(result.Metadata.Upload.HasFolderFingerprint()))
	reportMetric(ctx, reporter, job.StreamID, "gdrive.upload_final_mp4_fingerprint_present", boolMetric(result.Metadata.Upload.HasFileFingerprint("final.mp4")))
	reportMetric(ctx, reporter, job.StreamID, "gdrive.upload_metadata_fingerprint_present", boolMetric(result.Metadata.Upload.HasFileFingerprint("metadata.json")))
	_ = reporter.Event(ctx, job.StreamID, "archive.package.completed", "completed", map[string]any{
		"dry_run":           job.DryRun,
		"upload_dry_run":    result.Metadata.Upload.DryRun,
		"upload_attempts":   result.Metadata.Upload.Attempts,
		"file_count":        len(result.Metadata.Upload.FileIDs),
		"remux_duration_ms": result.RemuxDurationMS,
	})
}

func reportControlPanelArtifacts(ctx context.Context, streamID string, result lifecycle.Result) {
	config := control.ConfigFromEnv()
	if config.ControlPanelURL == "" || config.Token == "" {
		return
	}
	artifacts := control.ArchiveArtifacts(result.Layout)
	if len(artifacts) == 0 {
		return
	}
	client := control.Client{Config: config}
	if err := client.ReportArtifacts(ctx, streamID, artifacts); err != nil {
		log.Printf("control panel artifact report failed: %v", err)
	}
}

func reportMetric(ctx context.Context, reporter observability.Client, streamID, name string, value float64) {
	_ = reporter.Report(ctx, observability.Signal{Type: "metric", Name: name, StreamID: streamID, Value: &value})
}

func packageFailureAttributes(err error, dryRun bool) map[string]any {
	phase := lifecycle.ErrorPhase(err)
	if phase == "" {
		phase = "unknown"
	}
	return map[string]any{
		"failure_phase": phase,
		"error_class":   lifecycle.ErrorClass(err),
		"dry_run":       dryRun,
	}
}

func packageFailureResponse(err error, dryRun bool) map[string]any {
	attrs := packageFailureAttributes(err, dryRun)
	return map[string]any{
		"code":          "package_failed",
		"failure_phase": attrs["failure_phase"],
		"error_class":   attrs["error_class"],
		"dry_run":       attrs["dry_run"],
	}
}

func uploaderFromEnv(dryRun bool) archive.ArchiveUploader {
	return archive.DryRunUploader{}
}

func envInt(key string, fallback int) int {
	value, err := strconv.Atoi(os.Getenv(key))
	if err != nil || value <= 0 {
		return fallback
	}
	return value
}

func envDefault(key, fallback string) string {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	return value
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func boolMetric(ok bool) float64 {
	if ok {
		return 1
	}
	return 0
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
