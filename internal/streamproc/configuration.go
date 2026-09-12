package streamproc

import (
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/example/autostream-encoder-recorder/internal/archive"
	"github.com/example/autostream-encoder-recorder/internal/control"
	"github.com/example/autostream-encoder-recorder/internal/ffmpeg"
	"github.com/example/autostream-encoder-recorder/internal/lifecycle"
	"github.com/example/autostream-encoder-recorder/internal/observability"
	"github.com/example/autostream-encoder-recorder/internal/outputrelay"
	"github.com/example/autostream-encoder-recorder/internal/videocover"
)

func NewManagerFromEnv() *Manager {
	obs := observability.NewClientFromEnv()
	var reporter Reporter
	if obs.Enabled() {
		reporter = obs
	}
	archiveRoot := envDefault("AUTOSTREAM_ARCHIVE_DIR", "/var/lib/autostream/archives")
	ffmpegBin := envDefault("FFMPEG_BIN", "ffmpeg")
	controlConfig := control.ConfigFromEnv()
	var artifactReporter ArtifactReporter
	var coverAssets *videocover.Loader
	if controlConfig.ControlPanelURL != "" && controlConfig.Token != "" {
		controlClient := control.Client{Config: controlConfig}
		artifactReporter = controlClient
		if controlConfig.Validate() == nil {
			coverAssets = videocover.NewLoader(controlClient, envInt("VIDEO_COVER_CACHE_ENTRIES", 16), int64(envInt("VIDEO_COVER_CACHE_MIB", 64))<<20)
		}
		if reporter == nil {
			reporter = controlClient
		}
	}
	return &Manager{
		ArchiveRoot:              archiveRoot,
		FFmpegBin:                ffmpegBin,
		Starter:                  ExecStarter{},
		Reporter:                 reporter,
		ArtifactReporter:         artifactReporter,
		MetricsInterval:          envDuration("ENCODER_METRICS_INTERVAL_SEC", 10*time.Second),
		PackageTimeout:           envDuration("ENCODER_PACKAGE_TIMEOUT_SEC", 2*time.Hour),
		ArtifactReportTimeout:    envDuration("ENCODER_ARTIFACT_REPORT_TIMEOUT_SEC", 10*time.Second),
		InputAllowedHosts:        splitCSV(envDefault("AUTOSTREAM_INPUT_ALLOWED_HOSTS", os.Getenv("ENCODER_INPUT_ALLOWED_HOSTS"))),
		AllowDirectHLS:           envBool("AUTOSTREAM_ALLOW_DIRECT_HLS_INPUT", false),
		AllowHostnameInputs:      envBool("AUTOSTREAM_ALLOW_HOSTNAME_INPUTS", false),
		RequireInputAllowedHosts: envBool("AUTOSTREAM_REQUIRE_INPUT_ALLOWED_HOSTS", true),
		OutputRelayURL:           strings.TrimSpace(os.Getenv("AUTOSTREAM_OUTPUT_RELAY_URL")),
		OutputRelayMode:          strings.TrimSpace(os.Getenv("AUTOSTREAM_OUTPUT_RELAY_MODE")),
		// Binding IDs are exact canonical identities; do not trim whitespace and
		// accidentally turn an invalid persisted value into a trusted binding.
		OutputRelayBindingID: os.Getenv("AUTOSTREAM_OUTPUT_RELAY_BINDING_ID"),
		RequireOutputRelay:   outputrelay.RequireRelayFromEnv(),
		CoverAssets:          coverAssets,
		CoverWitness:         progressCoverWitness{},
		WatermarkWitness:     progressWatermarkWitness{},
		CoverApplyTimeout:    envDuration("VIDEO_COVER_APPLY_TIMEOUT_SEC", 6*time.Second),
		CoverFetchTimeout:    envDuration("VIDEO_COVER_FETCH_TIMEOUT_SEC", 10*time.Second),
		Packager: lifecycle.Manager{
			ArchiveRoot: archiveRoot,
			FFmpegBin:   ffmpegBin,
			Runner:      ffmpeg.CommandRunner{},
			Uploader: archive.RetryUploader{
				Inner:  uploaderFromEnv(false),
				Policy: archive.RetryPolicy{MaxAttempts: envInt("GOOGLE_DRIVE_UPLOAD_RETRY_MAX", 5), BaseDelay: time.Duration(envInt("GOOGLE_DRIVE_UPLOAD_RETRY_BASE_DELAY_SEC", 2)) * time.Second},
			},
		},
	}
}

func (m *Manager) archiveRoot() string {
	return valueOrDefault(m.ArchiveRoot, "/var/lib/autostream/archives")
}

func (m *Manager) ffmpegBin() string {
	return valueOrDefault(m.FFmpegBin, "ffmpeg")
}

func valueOrDefault(value, fallback string) string {
	if strings.TrimSpace(value) != "" {
		return value
	}
	return fallback
}

func envDefault(key, fallback string) string {
	return valueOrDefault(os.Getenv(key), fallback)
}

func splitCSV(value string) []string {
	parts := strings.Split(value, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

func envInt(key string, fallback int) int {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	var out int
	for _, ch := range value {
		if ch < '0' || ch > '9' {
			return fallback
		}
		out = out*10 + int(ch-'0')
	}
	if out <= 0 {
		return fallback
	}
	return out
}

func envDuration(key string, fallback time.Duration) time.Duration {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	duration, err := time.ParseDuration(value + "s")
	if err != nil || duration <= 0 {
		return fallback
	}
	return duration
}

func envFloat(key string, fallback float64) float64 {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return fallback
	}
	return parsed
}

func envBool(key string, fallback bool) bool {
	value := strings.TrimSpace(strings.ToLower(os.Getenv(key)))
	if value == "" {
		return fallback
	}
	switch value {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return fallback
	}
}

func uploaderFromEnv(dryRun bool) archive.ArchiveUploader {
	return archive.DryRunUploader{}
}

func jst() *time.Location {
	return time.FixedZone("JST", 9*60*60)
}
