package streamproc

import (
	"context"
	"errors"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/example/autostream-encoder-recorder/internal/archive"
	"github.com/example/autostream-encoder-recorder/internal/ffmpeg"
	"github.com/example/autostream-encoder-recorder/internal/lifecycle"
	"github.com/example/autostream-encoder-recorder/internal/outputrelay"
)

func TestManagerRejectsArchiveRunWithoutStartedAtBeforeStartingFFmpeg(t *testing.T) {
	starter := &fakeStarter{}
	manager := &Manager{Starter: starter, OutputRelayMode: outputrelay.ModeDirect}

	_, err := manager.Start(lifecycle.StreamJob{
		StreamID:     "stream-01",
		Name:         "Morning Stream",
		ArchiveRunID: "run-01",
	})
	if err == nil || !strings.Contains(err.Error(), "archive run started_at is required") {
		t.Fatalf("expected archive run start time validation, got %v", err)
	}
	if starter.process != nil || len(starter.args) > 0 {
		t.Fatalf("ffmpeg must not start for an invalid archive run: process=%#v args=%#v", starter.process, starter.args)
	}
}

func TestManagerStartUsesLiveWatermarkFeedAndAddsOverlayFilter(t *testing.T) {
	root := t.TempDir()
	starter := &fakeStarter{}
	manager := &Manager{ArchiveRoot: root, FFmpegBin: "ffmpeg", Starter: starter, InputResolver: testInputResolver, AllowHostnameInputs: true, OutputRelayURL: "rtmp://127.0.0.1/autostream/{stream_id}", OutputRelayMode: outputrelay.ModeManagedLiveAPI, OutputRelayBindingID: staticRelayBindingID}
	_, err := manager.Start(lifecycle.StreamJob{
		StreamID: "stream-watermark", Name: "Watermarked Stream", InputURL: "rtsp://input.example.com/live", YouTubeOutputMode: "live_api_relay_static", OutputRelayBindingID: staticRelayBindingID, YouTubeOutputReady: true,
		OverlayProfileID: "overlay-01", OverlayConfig: map[string]any{"watermark_enabled": true, "watermark_image_data_url": "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII="},
	})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(starter.args, " ")
	if !strings.Contains(joined, "-filter_complex") || !strings.Contains(joined, "scale=1920:1080[wm]") || !strings.Contains(joined, "overlay=0:0") {
		t.Fatalf("watermark filter missing from FFmpeg args: %#v", starter.args)
	}
	if strings.Contains(joined, "data:image/") {
		t.Fatalf("raw watermark data URL leaked into FFmpeg args: %#v", starter.args)
	}
	if !strings.Contains(joined, "-f png_pipe -framerate 2 -i tcp://127.0.0.1:") {
		t.Fatalf("live watermark input missing from FFmpeg args: %#v", starter.args)
	}
}

func TestManagerStartUsesPerJobEncoderProfile(t *testing.T) {
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
		Profile:              ffmpeg.DefaultProfile(),
	}
	_, err := manager.Start(lifecycle.StreamJob{
		StreamID: "stream-profile-720p", Name: "720p Stream", InputURL: "rtsp://input.example.com/live",
		YouTubeOutputMode: "live_api_relay_static", OutputRelayBindingID: staticRelayBindingID, YouTubeOutputReady: true,
		EncoderProfileID: "encoder-720p",
		EncoderProfile: ffmpeg.EncoderProfile{
			Width: 1280, Height: 720, FPS: 30, VideoBitrate: "4500k", AudioBitrate: "128k", SampleRate: 48000, KeyframeSec: 2,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(starter.args, " ")
	for _, want := range []string{"scale=1280:720", "-b:v 4500k", "-r 30", "-g 60"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("per-job encoder profile option %q missing from FFmpeg args: %s", want, joined)
		}
	}
	layout, err := archive.NewLayout(root, "stream-profile-720p")
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := os.ReadFile(layout.TmpMetadata())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"encoder_profile_id": "encoder-720p"`, `"output_width": 1280`, `"output_height": 720`, `"output_fps": 30`} {
		if !strings.Contains(string(metadata), want) {
			t.Fatalf("selected encoder profile diagnostic %q missing from metadata: %s", want, metadata)
		}
	}
}

func TestManagerRejectsDuplicateRunningStream(t *testing.T) {
	root := t.TempDir()
	manager := &Manager{ArchiveRoot: root, FFmpegBin: "ffmpeg", Starter: &fakeStarter{}, InputResolver: testInputResolver, AllowHostnameInputs: true, OutputRelayMode: outputrelay.ModeDirect}
	job := lifecycle.StreamJob{StreamID: "stream-01", Name: "Morning Stream", InputURL: "srt://input.example.com:9000", RTMPURL: "rtmps://youtube.example.com/live2", StreamKey: "key", YouTubeOutputMode: "stream_key"}
	if _, err := manager.Start(job); err != nil {
		t.Fatal(err)
	}
	layout, err := archive.NewLayout(root, job.StreamID)
	if err != nil {
		t.Fatal(err)
	}
	activePlaylist := []byte("#EXTM3U\n#EXTINF:2,\nsegment-000001.ts\n")
	if err := os.WriteFile(layout.PreviewPlaylist(), activePlaylist, 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Start(job); !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("expected ErrAlreadyRunning, got %v", err)
	}
	if body, err := os.ReadFile(layout.PreviewPlaylist()); err != nil || string(body) != string(activePlaylist) {
		t.Fatalf("duplicate start modified active preview: body=%q err=%v", body, err)
	}
}

func TestManagerRejectsDuplicateStartingStream(t *testing.T) {
	starter := &blockingStarter{started: make(chan struct{}), release: make(chan struct{})}
	manager := &Manager{ArchiveRoot: t.TempDir(), FFmpegBin: "ffmpeg", Starter: starter, InputResolver: testInputResolver, AllowHostnameInputs: true, OutputRelayMode: outputrelay.ModeDirect}
	job := lifecycle.StreamJob{StreamID: "stream-01", Name: "Morning Stream", InputURL: "srt://input.example.com:9000", RTMPURL: "rtmps://youtube.example.com/live2", StreamKey: "key", YouTubeOutputMode: "stream_key"}

	errCh := make(chan error, 1)
	go func() {
		_, err := manager.Start(job)
		errCh <- err
	}()
	<-starter.started
	if _, err := manager.Start(job); !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("expected ErrAlreadyRunning during starting state, got %v", err)
	}
	close(starter.release)
	if err := <-errCh; err != nil {
		t.Fatalf("first start failed: %v", err)
	}
	starter.mu.Lock()
	count := starter.count
	starter.mu.Unlock()
	if count != 1 {
		t.Fatalf("expected one ffmpeg start, got %d", count)
	}
}

func TestManagerStopRejectsStartingStream(t *testing.T) {
	starter := &blockingStarter{started: make(chan struct{}), release: make(chan struct{})}
	manager := &Manager{ArchiveRoot: t.TempDir(), FFmpegBin: "ffmpeg", Starter: starter, InputResolver: testInputResolver, AllowHostnameInputs: true, OutputRelayMode: outputrelay.ModeDirect}
	job := lifecycle.StreamJob{StreamID: "stream-01", Name: "Morning Stream", InputURL: "srt://input.example.com:9000", RTMPURL: "rtmps://youtube.example.com/live2", StreamKey: "key", YouTubeOutputMode: "stream_key"}

	startErr := make(chan error, 1)
	go func() {
		_, err := manager.Start(job)
		startErr <- err
	}()
	<-starter.started
	if _, err := manager.Stop(job.StreamID); !errors.Is(err, ErrStarting) {
		t.Fatalf("stop starting stream error = %v, want ErrStarting", err)
	}
	close(starter.release)
	if err := <-startErr; err != nil {
		t.Fatalf("start stream: %v", err)
	}
	if _, err := manager.Stop(job.StreamID); err != nil {
		t.Fatalf("stop running stream: %v", err)
	}
}

func TestManagerRejectsClientSuppliedInternalDiscordAudioPath(t *testing.T) {
	starter := &fakeStarter{}
	manager := &Manager{ArchiveRoot: t.TempDir(), FFmpegBin: "ffmpeg", Starter: starter, InputResolver: testInputResolver, AllowHostnameInputs: true, OutputRelayMode: outputrelay.ModeDirect}
	job := lifecycle.StreamJob{
		StreamID:  "stream-01",
		Name:      "Discord Audio Stream",
		InputMode: "discord_opus_rtp",
		InputURL:  "internal_discord_audio:C:/tmp/attacker.sdp",
		RTMPURL:   "rtmps://youtube.example.com/live2",
		StreamKey: "key", YouTubeOutputMode: "stream_key",
	}
	if _, err := manager.Start(job); !errors.Is(err, ffmpeg.ErrUnsafeInputTarget) {
		t.Fatalf("expected unsafe input error, got %v", err)
	}
	if starter.process != nil {
		t.Fatalf("ffmpeg should not be started for unsafe internal input: %#v", starter.process)
	}
}

func TestManagerRejectsInternalDiscordAudioWithoutDiscordMode(t *testing.T) {
	starter := &fakeStarter{}
	manager := &Manager{ArchiveRoot: t.TempDir(), FFmpegBin: "ffmpeg", Starter: starter, InputResolver: testInputResolver, AllowHostnameInputs: true, OutputRelayMode: outputrelay.ModeDirect}
	job := lifecycle.StreamJob{
		StreamID:  "stream-01",
		Name:      "Discord Audio Stream",
		InputURL:  "internal_discord_audio:C:/tmp/discord-opus.sdp",
		RTMPURL:   "rtmps://youtube.example.com/live2",
		StreamKey: "key", YouTubeOutputMode: "stream_key",
	}
	if _, err := manager.Start(job); !errors.Is(err, ffmpeg.ErrUnsafeInputTarget) {
		t.Fatalf("expected unsafe input error, got %v", err)
	}
	if starter.process != nil {
		t.Fatalf("ffmpeg should not be started for unsafe internal input: %#v", starter.process)
	}
}

func TestManagerRejectsInputHostOutsideAllowlistBeforeStartingFFmpeg(t *testing.T) {
	starter := &fakeStarter{}
	manager := &Manager{
		ArchiveRoot:       t.TempDir(),
		FFmpegBin:         "ffmpeg",
		Starter:           starter,
		InputAllowedHosts: []string{"trusted.example.com"},
		InputResolver:     testInputResolver,
		OutputRelayMode:   outputrelay.ModeDirect,
	}
	job := lifecycle.StreamJob{
		StreamID:  "stream-01",
		Name:      "Morning Stream",
		InputURL:  "srt://untrusted.example.com:9000",
		RTMPURL:   "rtmps://youtube.example.com/live2",
		StreamKey: "key", YouTubeOutputMode: "stream_key",
	}
	if _, err := manager.Start(job); !errors.Is(err, ffmpeg.ErrUnsafeInputTarget) {
		t.Fatalf("expected unsafe input error, got %v", err)
	}
	if starter.process != nil {
		t.Fatalf("ffmpeg should not be started for disallowed input host: %#v", starter.process)
	}
}

func TestManagerRejectsResolvedUnsafeInputBeforeStartingFFmpeg(t *testing.T) {
	starter := &fakeStarter{}
	manager := &Manager{
		ArchiveRoot: t.TempDir(),
		FFmpegBin:   "ffmpeg",
		Starter:     starter,
		InputResolver: func(ctx context.Context, host string) ([]net.IP, error) {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			return []net.IP{net.ParseIP("169.254.169.254")}, nil
		},
		OutputRelayMode: outputrelay.ModeDirect,
	}
	job := lifecycle.StreamJob{
		StreamID:  "stream-01",
		Name:      "Morning Stream",
		InputURL:  "rtsp://camera.example.com/live",
		RTMPURL:   "rtmps://youtube.example.com/live2",
		StreamKey: "key", YouTubeOutputMode: "stream_key",
	}
	if _, err := manager.Start(job); !errors.Is(err, ffmpeg.ErrUnsafeInputTarget) {
		t.Fatalf("expected unsafe input error, got %v", err)
	}
	if starter.process != nil {
		t.Fatalf("ffmpeg should not be started for resolved unsafe host: %#v", starter.process)
	}
}

func TestManagerRejectsDirectHLSInputByDefault(t *testing.T) {
	starter := &fakeStarter{}
	manager := &Manager{
		ArchiveRoot:   t.TempDir(),
		FFmpegBin:     "ffmpeg",
		Starter:       starter,
		InputResolver: testInputResolver, AllowHostnameInputs: true,
		OutputRelayMode: outputrelay.ModeDirect,
	}
	job := lifecycle.StreamJob{
		StreamID:  "stream-01",
		Name:      "Morning Stream",
		InputURL:  "https://cdn.example.com/live/index.m3u8",
		RTMPURL:   "rtmps://youtube.example.com/live2",
		StreamKey: "key", YouTubeOutputMode: "stream_key",
	}
	if _, err := manager.Start(job); !errors.Is(err, ffmpeg.ErrUnsafeInputTarget) {
		t.Fatalf("expected unsafe input error, got %v", err)
	}
	if starter.process != nil {
		t.Fatalf("ffmpeg should not be started for direct HLS input by default: %#v", starter.process)
	}
}

func TestManagerAllowsDirectHLSInputOnlyWhenExplicitlyEnabled(t *testing.T) {
	starter := &fakeStarter{}
	manager := &Manager{
		ArchiveRoot:         t.TempDir(),
		FFmpegBin:           "ffmpeg",
		Starter:             starter,
		InputResolver:       testInputResolver,
		AllowDirectHLS:      true,
		AllowHostnameInputs: true,
		OutputRelayMode:     outputrelay.ModeDirect,
	}
	job := lifecycle.StreamJob{
		StreamID:  "stream-01",
		Name:      "Morning Stream",
		InputURL:  "https://cdn.example.com/live/index.m3u8",
		RTMPURL:   "rtmps://youtube.example.com/live2",
		StreamKey: "key", YouTubeOutputMode: "stream_key",
	}
	if _, err := manager.Start(job); err != nil {
		t.Fatalf("expected explicitly enabled direct HLS input to start: %v", err)
	}
	if starter.process == nil {
		t.Fatal("expected ffmpeg to be started for explicitly enabled direct HLS input")
	}
}

func TestManagerStopTransitionsToStopped(t *testing.T) {
	starter := &fakeStarter{}
	manager := &Manager{ArchiveRoot: t.TempDir(), FFmpegBin: "ffmpeg", Starter: starter, InputResolver: testInputResolver, AllowHostnameInputs: true, OutputRelayMode: outputrelay.ModeDirect}
	job := lifecycle.StreamJob{StreamID: "stream-01", Name: "Morning Stream", InputURL: "srt://input.example.com:9000", RTMPURL: "rtmps://youtube.example.com/live2", StreamKey: "key", YouTubeOutputMode: "stream_key"}
	if _, err := manager.Start(job); err != nil {
		t.Fatal(err)
	}
	snapshot, err := manager.Stop("stream-01")
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Status != "stopping" {
		t.Fatalf("unexpected stop snapshot: %#v", snapshot)
	}
	if starter.process == nil || !starter.process.terminated || starter.process.killed {
		t.Fatalf("expected graceful terminate without kill, got process=%#v", starter.process)
	}
	deadline := time.After(2 * time.Second)
	for {
		status, err := manager.Status("stream-01")
		if err != nil {
			t.Fatal(err)
		}
		if status.Status == "stopped" {
			retry, retryErr := manager.Stop("stream-01")
			if !errors.Is(retryErr, ErrAlreadyStopped) {
				t.Fatalf("retry stop error = %v, want ErrAlreadyStopped", retryErr)
			}
			if retry.StreamID != "stream-01" || retry.Status != "stopped" {
				t.Fatalf("retry stop snapshot = %#v", retry)
			}
			return
		}
		select {
		case <-deadline:
			t.Fatalf("process did not stop: %#v", status)
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}
}

func TestManagerStopReceiptSurvivesRestartForItsExactTarget(t *testing.T) {
	root := t.TempDir()
	job := lifecycle.StreamJob{StreamID: "stream-a", Name: "Stream A", InputURL: "srt://input.example.com:9000", RTMPURL: "rtmps://youtube.example.com/live2", StreamKey: "key", YouTubeOutputMode: "stream_key"}
	beforeRestart := &Manager{ArchiveRoot: root, FFmpegBin: "ffmpeg", Starter: &fakeStarter{}, InputResolver: testInputResolver, AllowHostnameInputs: true, OutputRelayMode: outputrelay.ModeDirect}
	if _, err := beforeRestart.Start(job); err != nil {
		t.Fatal(err)
	}
	if _, err := beforeRestart.Stop(job.StreamID); err != nil {
		t.Fatal(err)
	}

	afterRestart := &Manager{ArchiveRoot: root, OutputRelayMode: outputrelay.ModeDirect}
	snapshot, err := afterRestart.Stop(job.StreamID)
	if !errors.Is(err, ErrAlreadyStopped) {
		t.Fatalf("stop after restart error = %v, want ErrAlreadyStopped", err)
	}
	if snapshot.StreamID != job.StreamID || snapshot.Status != "stopped" {
		t.Fatalf("stop after restart snapshot = %#v", snapshot)
	}
}

func TestManagerStartClearsDurableStopReceipt(t *testing.T) {
	root := t.TempDir()
	manager := &Manager{ArchiveRoot: root, FFmpegBin: "ffmpeg", Starter: &fakeStarter{}, InputResolver: testInputResolver, AllowHostnameInputs: true, OutputRelayMode: outputrelay.ModeDirect}
	if err := manager.writeStopReceipt("stream-a", stopReceipt{StreamID: "stream-a", ExpiresAt: time.Now().UTC().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Start(lifecycle.StreamJob{StreamID: "stream-a", Name: "Stream A", InputURL: "srt://input.example.com:9000", RTMPURL: "rtmps://youtube.example.com/live2", StreamKey: "key", YouTubeOutputMode: "stream_key"}); err != nil {
		t.Fatal(err)
	}
	// A fresh manager models the process-local state loss caused by a service
	// restart. It must not rediscover the pre-start receipt from disk.
	afterRestart := &Manager{ArchiveRoot: root, OutputRelayMode: outputrelay.ModeDirect}
	if exists, err := afterRestart.hasStopReceipt("stream-a"); err != nil || exists {
		t.Fatalf("start did not durably clear stop receipt across restart: exists=%v err=%v", exists, err)
	}
	if _, err := manager.Stop("stream-a"); err != nil {
		t.Fatal(err)
	}
}

func TestManagerExpiredStopReceiptDoesNotAcknowledgeUnknownTarget(t *testing.T) {
	manager := &Manager{ArchiveRoot: t.TempDir(), OutputRelayMode: outputrelay.ModeDirect}
	if err := manager.writeStopReceipt("stream-a", stopReceipt{StreamID: "stream-a", ExpiresAt: time.Now().UTC().Add(-time.Second)}); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Stop("stream-a"); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("expired receipt stop error = %v, want ErrNotRunning", err)
	}
}

func TestManagerStopAllTerminatesRunningStreams(t *testing.T) {
	starter := &fakeStarter{}
	manager := &Manager{ArchiveRoot: t.TempDir(), FFmpegBin: "ffmpeg", Starter: starter, InputResolver: testInputResolver, AllowHostnameInputs: true, OutputRelayMode: outputrelay.ModeDirect}
	jobs := []lifecycle.StreamJob{
		{StreamID: "stream-01", Name: "Morning Stream", InputURL: "srt://input.example.com:9000", RTMPURL: "rtmps://youtube.example.com/live2", StreamKey: "key-1", YouTubeOutputMode: "stream_key"},
		{StreamID: "stream-02", Name: "Evening Stream", InputURL: "srt://input.example.com:9001", RTMPURL: "rtmps://youtube.example.com/live2", StreamKey: "key-2", YouTubeOutputMode: "stream_key"},
	}
	for _, job := range jobs {
		if _, err := manager.Start(job); err != nil {
			t.Fatal(err)
		}
	}
	errs := manager.StopAll()
	if len(errs) != 0 {
		t.Fatalf("unexpected stop errors: %#v", errs)
	}
	if len(starter.processes) != len(jobs) {
		t.Fatalf("unexpected process count: %d", len(starter.processes))
	}
	for i, process := range starter.processes {
		if !process.terminated || process.killed {
			t.Fatalf("process %d was not gracefully terminated: %#v", i, process)
		}
	}
	deadline := time.After(2 * time.Second)
	for {
		allStopped := true
		for _, job := range jobs {
			status, err := manager.Status(job.StreamID)
			if err != nil {
				t.Fatal(err)
			}
			if status.Status != "stopped" {
				allStopped = false
				break
			}
		}
		if allStopped {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("processes did not stop")
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}
}
