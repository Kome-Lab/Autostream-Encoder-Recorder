package streamproc

import (
	"errors"
	"strings"
	"testing"

	"github.com/example/autostream-encoder-recorder/internal/ffmpeg"
	"github.com/example/autostream-encoder-recorder/internal/lifecycle"
	"github.com/example/autostream-encoder-recorder/internal/outputrelay"
)

func TestNewManagerFromEnvRequiresInputAllowedHostsByDefault(t *testing.T) {
	t.Setenv("AUTOSTREAM_ARCHIVE_DIR", t.TempDir())
	t.Setenv("FFMPEG_BIN", "ffmpeg")
	t.Setenv("AUTOSTREAM_OUTPUT_RELAY_MODE", outputrelay.ModeDirect)
	manager := NewManagerFromEnv()
	manager.Starter = &fakeStarter{}
	manager.InputResolver = testInputResolver
	_, err := manager.Start(lifecycle.StreamJob{
		StreamID:  "stream-01",
		Name:      "Morning Stream",
		InputURL:  "srt://source.example.com:9000",
		RTMPURL:   "rtmps://youtube.example.com/live2",
		StreamKey: "key", YouTubeOutputMode: "stream_key",
	})
	if !errors.Is(err, ffmpeg.ErrUnsafeInputTarget) {
		t.Fatalf("expected external input to require AUTOSTREAM_INPUT_ALLOWED_HOSTS by default, got %v", err)
	}
}

func TestNewManagerFromEnvRequiresExplicitOutputModeInProduction(t *testing.T) {
	t.Setenv("AUTOSTREAM_ENV", "production")
	manager := NewManagerFromEnv()
	policy := outputrelay.NewWithRequireRelay(manager.OutputRelayURL, manager.OutputRelayMode, manager.OutputRelayBindingID, manager.RequireOutputRelay)
	if !errors.Is(policy.ValidateConfiguration(), outputrelay.ErrInvalidConfiguration) {
		t.Fatalf("missing output mode must fail closed, got mode=%q", manager.OutputRelayMode)
	}
}

func TestNewManagerFromEnvFailsClosedBeforeFFmpegStartWhenRelayIsExplicitlyRequired(t *testing.T) {
	t.Setenv("AUTOSTREAM_ENV", "production")
	t.Setenv("AUTOSTREAM_REQUIRE_OUTPUT_RELAY", "true")
	t.Setenv("AUTOSTREAM_ARCHIVE_DIR", t.TempDir())
	t.Setenv("FFMPEG_BIN", "ffmpeg")
	t.Setenv("AUTOSTREAM_INPUT_ALLOWED_HOSTS", "source.example.com")
	t.Setenv("AUTOSTREAM_ALLOW_HOSTNAME_INPUTS", "true")
	t.Setenv("AUTOSTREAM_OUTPUT_RELAY_MODE", outputrelay.ModeDirect)

	starter := &fakeStarter{}
	manager := NewManagerFromEnv()
	manager.Starter = starter
	manager.InputResolver = testInputResolver

	_, err := manager.Start(lifecycle.StreamJob{
		StreamID:  "stream-01",
		Name:      "Morning Stream",
		InputURL:  "srt://source.example.com:9000",
		RTMPURL:   "rtmps://youtube.example.com/live2",
		StreamKey: "secret-stream-key", YouTubeOutputMode: "stream_key",
	})
	if err == nil || !strings.Contains(err.Error(), "output relay URL is required") {
		t.Fatalf("expected production relay requirement to fail closed, got %v", err)
	}
	if starter.process != nil || len(starter.args) > 0 {
		t.Fatalf("ffmpeg must not start when production output relay is missing: process=%#v args=%#v", starter.process, starter.args)
	}
}

func TestNewManagerFromEnvReadsOutputRelayURL(t *testing.T) {
	t.Setenv("AUTOSTREAM_OUTPUT_RELAY_URL", "rtmp://127.0.0.1/autostream/{stream_id}")
	t.Setenv("AUTOSTREAM_OUTPUT_RELAY_MODE", outputrelay.ModeManagedLiveAPI)
	t.Setenv("AUTOSTREAM_OUTPUT_RELAY_BINDING_ID", staticRelayBindingID)
	manager := NewManagerFromEnv()
	if manager.OutputRelayURL != "rtmp://127.0.0.1/autostream/{stream_id}" {
		t.Fatalf("unexpected output relay URL: %q", manager.OutputRelayURL)
	}
	if manager.OutputRelayBindingID != staticRelayBindingID {
		t.Fatalf("unexpected output relay binding ID: %q", manager.OutputRelayBindingID)
	}
	if manager.OutputRelayMode != outputrelay.ModeManagedLiveAPI {
		t.Fatalf("unexpected output relay mode: %q", manager.OutputRelayMode)
	}
}

func TestNewManagerFromEnvAllowsConfiguredInputAllowedHosts(t *testing.T) {
	t.Setenv("AUTOSTREAM_ARCHIVE_DIR", t.TempDir())
	t.Setenv("FFMPEG_BIN", "ffmpeg")
	t.Setenv("AUTOSTREAM_INPUT_ALLOWED_HOSTS", "source.example.com")
	t.Setenv("AUTOSTREAM_ALLOW_HOSTNAME_INPUTS", "true")
	t.Setenv("AUTOSTREAM_OUTPUT_RELAY_MODE", outputrelay.ModeDirect)
	manager := NewManagerFromEnv()
	manager.Starter = &fakeStarter{}
	manager.InputResolver = testInputResolver
	_, err := manager.Start(lifecycle.StreamJob{
		StreamID:  "stream-01",
		Name:      "Morning Stream",
		InputURL:  "srt://source.example.com:9000",
		RTMPURL:   "rtmps://youtube.example.com/live2",
		StreamKey: "key", YouTubeOutputMode: "stream_key",
	})
	if err != nil {
		t.Fatalf("expected allowlisted external input to start: %v", err)
	}
}
