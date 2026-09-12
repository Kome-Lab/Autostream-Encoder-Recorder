package httpapi

import (
	"errors"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/example/autostream-encoder-recorder/internal/control"
	"github.com/example/autostream-encoder-recorder/internal/ffmpeg"
	"github.com/example/autostream-encoder-recorder/internal/outputrelay"
)

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
		youtubeRuntimeConfigPreflight("youtube_rtmp_url", "YouTube RTMPS URL"),
		youtubeRuntimeConfigPreflight("youtube_stream_key", "YouTube stream key"),
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
			message = "AUTOSTREAM_OUTPUT_RELAY_BINDING_ID is required when AUTOSTREAM_OUTPUT_RELAY_MODE=live_api_relay_static."
		} else if errors.Is(err, outputrelay.ErrInvalidRelayBindingID) {
			message = "AUTOSTREAM_OUTPUT_RELAY_BINDING_ID must match relay-xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx using lowercase hexadecimal UUID characters."
		} else if errors.Is(err, outputrelay.ErrUnsafeRelayTarget) {
			message = "AUTOSTREAM_OUTPUT_RELAY_URL must resolve to a loopback RTMP/RTMPS relay target."
		}
		return preflightCheck{ID: "output_relay", Status: "invalid", Severity: "critical", Message: message}
	}
	if !policy.UsesLocalRelay() {
		return preflightCheck{ID: "output_relay", Status: "ok", Severity: "critical", Message: "Canonical direct output mode is configured; the target is supplied by Control Panel runtime config."}
	}
	target := relayOutputTargetForPreflight(policy.URL, "preflight-stream")
	if err := ffmpeg.ValidateRelayOutputTarget(target); err != nil {
		return preflightCheck{ID: "output_relay", Status: "invalid", Severity: "critical", Message: "AUTOSTREAM_OUTPUT_RELAY_URL must resolve to a loopback RTMP/RTMPS relay target."}
	}
	if policy.Mode == outputrelay.ModeManagedLiveAPI {
		return preflightCheck{ID: "output_relay", Status: "ok", Severity: "critical", Message: "Static Live API output Relay is configured; FFmpeg receives only the local relay target."}
	}
	return preflightCheck{ID: "output_relay", Status: "ok", Severity: "critical", Message: "Canonical output routing is configured."}
}

func relayOutputTargetForPreflight(template, streamID string) string {
	escapedStreamID := url.PathEscape(strings.TrimSpace(streamID))
	if strings.Contains(template, "{stream_id}") {
		return strings.ReplaceAll(template, "{stream_id}", escapedStreamID)
	}
	return strings.TrimRight(template, "/") + "/" + escapedStreamID
}

func youtubeRuntimeConfigPreflight(id, label string) preflightCheck {
	return preflightCheck{ID: id, Status: "runtime_config_required", Severity: "warning", Message: label + " must be supplied by Control Panel YouTube output runtime config."}
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
