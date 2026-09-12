package streamproc

import (
	"context"
	"errors"
	"net"
	"os"
	"strings"
	"testing"

	"github.com/example/autostream-encoder-recorder/internal/archive"
	"github.com/example/autostream-encoder-recorder/internal/lifecycle"
	"github.com/example/autostream-encoder-recorder/internal/outputrelay"
)

func TestManagerRejectsRemovedOutputRelayModeBeforeStartingFFmpeg(t *testing.T) {
	starter := &fakeStarter{}
	manager := &Manager{
		ArchiveRoot:          t.TempDir(),
		FFmpegBin:            "ffmpeg",
		Starter:              starter,
		InputResolver:        testInputResolver,
		AllowHostnameInputs:  true,
		OutputRelayURL:       "rtmp://127.0.0.1/autostream/{stream_id}",
		OutputRelayMode:      "legacy_ffmpeg_output",
		OutputRelayBindingID: "stale-binding-must-not-matter",
	}

	_, err := manager.Start(lifecycle.StreamJob{
		StreamID: "stream-01", Name: "Stream", InputURL: "srt://input.example.com:9000",
		RTMPURL: "rtmps://youtube.example.com/live2", StreamKey: "secret-stream-key", YouTubeOutputMode: "stream_key",
	})
	if !errors.Is(err, outputrelay.ErrInvalidConfiguration) {
		t.Fatalf("removed relay mode error = %v, want invalid configuration", err)
	}
	if starter.process != nil || len(starter.args) != 0 {
		t.Fatalf("ffmpeg must not start for a removed relay mode: process=%#v args=%#v", starter.process, starter.args)
	}
}

func TestManagerRejectsLiveAPIWithStaticOutputRelayBeforeStartingFFmpeg(t *testing.T) {
	root := t.TempDir()
	starter := &fakeStarter{}
	manager := &Manager{
		ArchiveRoot:          root,
		FFmpegBin:            "ffmpeg",
		Starter:              starter,
		InputResolver:        testInputResolver,
		AllowHostnameInputs:  true,
		OutputRelayURL:       "rtmp://127.0.0.1/autostream/{stream_id}",
		OutputRelayMode:      outputrelay.ModeManagedLiveAPI,
		OutputRelayBindingID: staticRelayBindingID,
	}
	_, err := manager.Start(lifecycle.StreamJob{
		StreamID: "stream-01", Name: "Morning Stream", InputURL: "srt://input.example.com:9000",
		RTMPURL: "rtmps://youtube.example.com/live2", StreamKey: "secret-stream-key", YouTubeOutputMode: "live_api",
	})
	if !errors.Is(err, ErrLiveAPIRequiresManagedOutputRelay) {
		t.Fatalf("expected static relay live api rejection, got %v", err)
	}
	if starter.process != nil || len(starter.args) != 0 {
		t.Fatalf("ffmpeg must not start for static relay live api: process=%#v args=%#v", starter.process, starter.args)
	}
}

func TestManagerRejectsNonStaticYouTubeOutputModesWithStaticOutputRelayBeforeInputResolutionOrFFmpeg(t *testing.T) {
	for _, outputMode := range []string{"stream_key", "live_api", "live_api_dry_run", ""} {
		t.Run(outputMode, func(t *testing.T) {
			root := t.TempDir()
			starter := &fakeStarter{}
			inputResolverCalls := 0
			manager := &Manager{
				ArchiveRoot:          root,
				FFmpegBin:            "ffmpeg",
				Starter:              starter,
				AllowHostnameInputs:  true,
				OutputRelayURL:       "rtmp://127.0.0.1/autostream/{stream_id}",
				OutputRelayMode:      outputrelay.ModeManagedLiveAPI,
				OutputRelayBindingID: staticRelayBindingID,
				InputResolver: func(context.Context, string) ([]net.IP, error) {
					inputResolverCalls++
					return nil, errors.New("input target must not be resolved")
				},
			}

			_, err := manager.Start(lifecycle.StreamJob{
				StreamID: "stream-01", Name: "Morning Stream", InputURL: "srt://input.example.com:9000",
				RTMPURL: "rtmps://youtube.example.com/live2", StreamKey: "caller-supplied-youtube-key",
				YouTubeOutputMode: outputMode, OutputRelayBindingID: staticRelayBindingID, YouTubeOutputReady: true,
			})
			if !errors.Is(err, ErrLiveAPIRequiresManagedOutputRelay) {
				t.Fatalf("output mode %q error = %v, want static relay rejection", outputMode, err)
			}
			if inputResolverCalls != 0 {
				t.Fatalf("output mode %q resolved the input before static relay rejection", outputMode)
			}
			if starter.process != nil || len(starter.args) != 0 {
				t.Fatalf("output mode %q started ffmpeg: process=%#v args=%#v", outputMode, starter.process, starter.args)
			}
		})
	}
}

func TestManagerStartsStaticLiveAPIOutputRelayWithMatchingBindingWithoutYouTubeTarget(t *testing.T) {
	root := t.TempDir()
	starter := &fakeStarter{}
	manager := &Manager{
		ArchiveRoot:          root,
		FFmpegBin:            "ffmpeg",
		Starter:              starter,
		InputResolver:        testInputResolver,
		AllowHostnameInputs:  true,
		OutputRelayURL:       "rtmp://127.0.0.1/autostream/{stream_id}",
		OutputRelayMode:      outputrelay.ModeManagedLiveAPI,
		OutputRelayBindingID: staticRelayBindingID,
	}
	snapshot, err := manager.Start(lifecycle.StreamJob{
		StreamID: "stream-01", Name: "Morning Stream", InputURL: "srt://input.example.com:9000",
		RTMPURL: "rtmps://youtube.example.com/live2", StreamKey: "caller-supplied-youtube-key",
		YouTubeOutputMode: "live_api_relay_static", OutputRelayBindingID: staticRelayBindingID, YouTubeOutputReady: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Status != "running" || starter.process == nil {
		t.Fatalf("matching static relay binding did not start: snapshot=%#v process=%#v", snapshot, starter.process)
	}
	args := strings.Join(starter.args, " ")
	if !strings.Contains(args, "rtmp://127.0.0.1/autostream/stream-01") || strings.Contains(args, "youtube.example.com") || strings.Contains(args, "caller-supplied-youtube-key") {
		t.Fatalf("static live api relay must use only the local output target: %#v", starter.args)
	}
	layout, err := archive.NewLayout(root, "stream-01")
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := os.ReadFile(layout.TmpMetadata())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(metadata), "youtube.example.com") || strings.Contains(string(metadata), "caller-supplied-youtube-key") {
		t.Fatalf("static live api relay must not retain caller-supplied YouTube material: %s", metadata)
	}
}

func TestManagerRejectsStaticLiveAPIOutputRelayBindingMismatchBeforeStartingFFmpeg(t *testing.T) {
	root := t.TempDir()
	starter := &fakeStarter{}
	manager := &Manager{
		ArchiveRoot:          root,
		FFmpegBin:            "ffmpeg",
		Starter:              starter,
		InputResolver:        testInputResolver,
		AllowHostnameInputs:  true,
		OutputRelayURL:       "rtmp://127.0.0.1/autostream/{stream_id}",
		OutputRelayMode:      outputrelay.ModeManagedLiveAPI,
		OutputRelayBindingID: staticRelayBindingID,
	}
	_, err := manager.Start(lifecycle.StreamJob{
		StreamID: "stream-01", Name: "Morning Stream", InputURL: "srt://input.example.com:9000",
		YouTubeOutputMode: "live_api_relay_static", OutputRelayBindingID: otherStaticRelayBindingID, YouTubeOutputReady: true,
	})
	if !errors.Is(err, ErrLiveAPIRelayBindingMismatch) {
		t.Fatalf("expected static relay binding mismatch, got %v", err)
	}
	if starter.process != nil || len(starter.args) != 0 {
		t.Fatalf("ffmpeg must not start for a static relay binding mismatch: process=%#v args=%#v", starter.process, starter.args)
	}
}

func TestManagerRejectsStaticLiveAPIOutputRelayWhenRuntimeIsNotReadyBeforeStartingFFmpeg(t *testing.T) {
	root := t.TempDir()
	starter := &fakeStarter{}
	manager := &Manager{
		ArchiveRoot:          root,
		FFmpegBin:            "ffmpeg",
		Starter:              starter,
		InputResolver:        testInputResolver,
		AllowHostnameInputs:  true,
		OutputRelayURL:       "rtmp://127.0.0.1/autostream/{stream_id}",
		OutputRelayMode:      outputrelay.ModeManagedLiveAPI,
		OutputRelayBindingID: staticRelayBindingID,
	}
	_, err := manager.Start(lifecycle.StreamJob{
		StreamID: "stream-01", Name: "Morning Stream", InputURL: "srt://input.example.com:9000",
		YouTubeOutputMode: "live_api_relay_static", OutputRelayBindingID: staticRelayBindingID,
	})
	if !errors.Is(err, ErrLiveAPIRelayStaticNotReady) {
		t.Fatalf("expected static live api unready rejection, got %v", err)
	}
	if starter.process != nil || len(starter.args) != 0 {
		t.Fatalf("ffmpeg must not start for an unready static relay runtime: process=%#v args=%#v", starter.process, starter.args)
	}
}

func TestManagerStartRequiresOutputRelayWhenConfigured(t *testing.T) {
	root := t.TempDir()
	inputResolverCalls := 0
	manager := &Manager{
		ArchiveRoot: root,
		FFmpegBin:   "ffmpeg",
		Starter:     &fakeStarter{},
		InputResolver: func(context.Context, string) ([]net.IP, error) {
			inputResolverCalls++
			return nil, errors.New("input must not be resolved when the required relay is missing")
		},
		AllowHostnameInputs: true,
		RequireOutputRelay:  true,
		OutputRelayMode:     outputrelay.ModeDirect,
	}
	_, err := manager.Start(lifecycle.StreamJob{
		StreamID: "stream-01", Name: "Morning Stream", InputURL: "srt://input.example.com:9000",
		RTMPURL: "rtmps://youtube.example.com/live2", StreamKey: "secret-stream-key", YouTubeOutputMode: "stream_key",
	})
	if !errors.Is(err, outputrelay.ErrRelayRequired) {
		t.Fatalf("expected output relay requirement to fail closed, got %v", err)
	}
	if inputResolverCalls != 0 {
		t.Fatalf("required relay rejection must happen before input resolution, calls=%d", inputResolverCalls)
	}
}

func TestManagerRejectsUnsafeOutputRelayBeforeStartingFFmpeg(t *testing.T) {
	root := t.TempDir()
	starter := &fakeStarter{}
	manager := &Manager{
		ArchiveRoot:          root,
		FFmpegBin:            "ffmpeg",
		Starter:              starter,
		InputResolver:        testInputResolver,
		AllowHostnameInputs:  true,
		OutputRelayURL:       "rtmps://youtube.example.com/live2/direct-secret-stream-key",
		OutputRelayMode:      outputrelay.ModeManagedLiveAPI,
		OutputRelayBindingID: staticRelayBindingID,
		RequireOutputRelay:   true,
	}
	_, err := manager.Start(lifecycle.StreamJob{
		StreamID:  "stream-01",
		Name:      "Morning Stream",
		InputURL:  "srt://input.example.com:9000",
		RTMPURL:   "rtmps://youtube.example.com/live2",
		StreamKey: "direct-secret-stream-key", YouTubeOutputMode: "stream_key",
	})
	if err == nil {
		t.Fatal("expected unsafe output relay target to fail closed")
	}
	if starter.process != nil || len(starter.args) > 0 {
		t.Fatalf("ffmpeg must not start with unsafe output relay target: process=%#v args=%#v", starter.process, starter.args)
	}
}

func TestRelayOutputTargetAppendsEscapedStreamID(t *testing.T) {
	got := relayOutputTarget("rtmp://127.0.0.1/autostream", "stream 01")
	if got != "rtmp://127.0.0.1/autostream/stream%2001" {
		t.Fatalf("unexpected relay target: %s", got)
	}
}
