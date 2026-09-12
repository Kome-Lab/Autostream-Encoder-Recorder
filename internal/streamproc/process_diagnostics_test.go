package streamproc

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/example/autostream-encoder-recorder/internal/archive"
	"github.com/example/autostream-encoder-recorder/internal/lifecycle"
	"github.com/example/autostream-encoder-recorder/internal/observability"
	"github.com/example/autostream-encoder-recorder/internal/outputrelay"
)

func TestManagerHeartbeatMetricsReflectRunningProcess(t *testing.T) {
	manager := &Manager{ArchiveRoot: t.TempDir(), FFmpegBin: "ffmpeg", Starter: &fakeStarter{}, InputResolver: testInputResolver, AllowHostnameInputs: true, OutputRelayMode: outputrelay.ModeDirect}
	if metrics := manager.HeartbeatMetrics(); metrics["encoder.process_alive"] != 0 || metrics["encoder.active_process_count"] != 0 {
		t.Fatalf("unexpected idle metrics: %#v", metrics)
	}
	job := lifecycle.StreamJob{StreamID: "stream-01", Name: "Morning Stream", InputURL: "srt://input.example.com:9000", RTMPURL: "rtmps://youtube.example.com/live2", StreamKey: "key", YouTubeOutputMode: "stream_key"}
	if _, err := manager.Start(job); err != nil {
		t.Fatal(err)
	}
	if got := manager.CurrentStreamID(); got != "stream-01" {
		t.Fatalf("unexpected current stream id: %q", got)
	}
	metrics := manager.HeartbeatMetrics()
	if metrics["encoder.process_alive"] != 1 || metrics["encoder.active_process_count"] != 1 {
		t.Fatalf("unexpected running metrics: %#v", metrics)
	}
}

func TestManagerReportsLifecycleSignals(t *testing.T) {
	reporter := &fakeReporter{}
	manager := &Manager{ArchiveRoot: t.TempDir(), FFmpegBin: "ffmpeg", Starter: &fakeStarter{}, Reporter: reporter, InputResolver: testInputResolver, AllowHostnameInputs: true, OutputRelayMode: outputrelay.ModeDirect}
	job := lifecycle.StreamJob{StreamID: "stream-01", Name: "Morning Stream", InputURL: "srt://input.example.com:9000", RTMPURL: "rtmps://youtube.example.com/live2", StreamKey: "key", YouTubeOutputMode: "stream_key"}
	if _, err := manager.Start(job); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Stop("stream-01"); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(2 * time.Second)
	for {
		if reporter.has("encoder.process.started") && reporter.has("encoder.process.stopping") && reporter.has("encoder.process.stopped") {
			started, ok := reporter.find("encoder.process.started")
			if !ok {
				t.Fatal("missing started signal")
			}
			if _, ok := started.Attributes["pid"]; ok {
				t.Fatalf("process pid leaked in observability signal: %#v", started.Attributes)
			}
			if _, ok := started.Attributes["args"]; ok {
				t.Fatalf("process args leaked in observability signal: %#v", started.Attributes)
			}
			return
		}
		select {
		case <-deadline:
			t.Fatalf("missing lifecycle signals: %#v", reporter.names())
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}
}

func TestManagerRedactsProcessExitError(t *testing.T) {
	reporter := &fakeReporter{}
	starter := &fakeStarter{}
	manager := &Manager{ArchiveRoot: t.TempDir(), FFmpegBin: "ffmpeg", Starter: starter, Reporter: reporter, InputResolver: testInputResolver, AllowHostnameInputs: true, OutputRelayMode: outputrelay.ModeDirect}
	job := lifecycle.StreamJob{
		StreamID:  "stream-01",
		Name:      "Morning Stream",
		InputURL:  "rtsp://camera:camera-password@input.example.com/live/%70%61%74%68-token",
		RTMPURL:   "rtmps://youtube.example.com/live2",
		StreamKey: "secret-stream-key", YouTubeOutputMode: "stream_key",
	}
	if _, err := manager.Start(job); err != nil {
		t.Fatal(err)
	}
	starter.process.done <- errors.New("ffmpeg failed for rtsp://camera:camera-password@input.example.com/live/%70%61%74%68-token and rtmps://youtube.example.com/live2/secret-stream-key")
	status, signal, err := waitForProcessExitObservation(func() (Snapshot, error) { return manager.Status(job.StreamID) }, reporter, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(status.Error, "camera-password") || strings.Contains(status.Error, "secret-stream-key") || strings.Contains(status.Error, "%70%61%74%68-token") || strings.Contains(status.Error, "path-token") {
		t.Fatalf("secret leaked in process status error: %s", status.Error)
	}
	if !strings.Contains(status.Error, "rtsp://input.example.com/<REDACTED>") {
		t.Fatalf("expected host-only input URL in process status error: %s", status.Error)
	}
	attrError, _ := signal.Attributes["error"].(string)
	if strings.Contains(attrError, "camera-password") || strings.Contains(attrError, "secret-stream-key") || strings.Contains(attrError, "%70%61%74%68-token") || strings.Contains(attrError, "path-token") {
		t.Fatalf("secret leaked in observability error: %s", attrError)
	}
	if !strings.Contains(attrError, "rtsp://input.example.com/<REDACTED>") {
		t.Fatalf("expected host-only input URL in observability error: %s", attrError)
	}
}

func TestManagerReportsSafeProcessExitDiagnostics(t *testing.T) {
	reporter := &fakeReporter{}
	starter := &fakeStarter{}
	manager := &Manager{ArchiveRoot: t.TempDir(), FFmpegBin: "ffmpeg", Starter: starter, Reporter: reporter, InputResolver: testInputResolver, AllowHostnameInputs: true, OutputRelayMode: outputrelay.ModeDirect}
	job := lifecycle.StreamJob{
		StreamID:  "stream-01",
		Name:      "Morning Stream",
		InputURL:  "rtsp://camera:camera-password@input.example.com/live/path-token",
		RTMPURL:   "rtmps://youtube.example.com/live2",
		StreamKey: "secret-stream-key", YouTubeOutputMode: "stream_key",
	}
	if _, err := manager.Start(job); err != nil {
		t.Fatal(err)
	}
	starter.process.stderr = "[tcp] Connection refused while opening rtmps://youtube.example.com/live2/secret-stream-key"
	starter.process.done <- errors.New("exit status 234")
	status, signal, err := waitForProcessExitObservation(func() (Snapshot, error) { return manager.Status(job.StreamID) }, reporter, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(status.Error, "secret-stream-key") || strings.Contains(status.Error, "camera-password") {
		t.Fatalf("process diagnostic leaked a secret: %s", status.Error)
	}
	if got, want := signal.Attributes["error_class"], "transport"; got != want {
		t.Fatalf("error class = %#v, want %q", got, want)
	}
	stderr, _ := signal.Attributes["stderr_tail"].(string)
	if strings.Contains(stderr, "secret-stream-key") || strings.Contains(stderr, "camera-password") {
		t.Fatalf("stderr diagnostic leaked a secret: %s", stderr)
	}
	if !strings.Contains(stderr, "Connection refused") {
		t.Fatalf("stderr diagnostic missing safe failure text: %s", stderr)
	}
}

func waitForProcessExitObservation(readStatus func() (Snapshot, error), reporter *fakeReporter, timeout time.Duration) (Snapshot, observability.Signal, error) {
	deadline := time.After(timeout)
	for {
		status, err := readStatus()
		if err != nil {
			return status, observability.Signal{}, err
		}
		signal, reported := reporter.find("encoder.process.exited")
		if status.Status == "failed" && reported {
			return status, signal, nil
		}
		select {
		case <-deadline:
			return status, signal, fmt.Errorf("process failure and exit notification were not both observed: status=%#v signals=%#v", status, reporter.names())
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}
}

func TestProcessExitObservationWaitsForRealReport(t *testing.T) {
	manager, reporter, release := processExitObservationManager(t)
	readStatus := func() (Snapshot, error) { return manager.Status("observation") }
	status, _, err := waitForProcessExitObservation(readStatus, reporter, 20*time.Millisecond)
	if err == nil || status.Status != "failed" {
		t.Fatalf("failed state alone completed the observation: status=%#v err=%v", status, err)
	}
	if reporter.has("encoder.process.exited") {
		t.Fatal("exit notification arrived while the existing hook was held")
	}
	release()
	status, signal, err := waitForProcessExitObservation(readStatus, reporter, 2*time.Second)
	if err != nil || status.Status != "failed" || signal.Name != "encoder.process.exited" || signal.StreamID != "observation" {
		t.Fatalf("real process exit was not observed after releasing the hook: status=%#v signal=%#v err=%v", status, signal, err)
	}
}

func TestProcessExitObservationRejectsIncompleteOrErroredStatus(t *testing.T) {
	manager, reporter, release := processExitObservationManager(t)
	release()
	if _, _, err := waitForProcessExitObservation(func() (Snapshot, error) { return manager.Status("observation") }, reporter, 2*time.Second); err != nil {
		t.Fatal(err)
	}
	t.Run("status never failed despite real notification", func(t *testing.T) {
		if _, _, err := waitForProcessExitObservation(func() (Snapshot, error) { return Snapshot{Status: "running"}, nil }, reporter, 20*time.Millisecond); err == nil {
			t.Fatal("notification alone completed the observation")
		}
	})
	t.Run("status error", func(t *testing.T) {
		if _, _, err := waitForProcessExitObservation(func() (Snapshot, error) { return manager.Status("missing") }, reporter, 20*time.Millisecond); !errors.Is(err, ErrNotRunning) {
			t.Fatalf("status error was not preserved: %v", err)
		}
	})
}

func processExitObservationManager(t *testing.T) (*Manager, *fakeReporter, func()) {
	t.Helper()
	reporter := &fakeReporter{}
	process := &fakeProcess{done: make(chan error, 1)}
	process.done <- errors.New("exit status 234")
	held, released, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(released) }) }
	manager := &Manager{
		ArchiveRoot: t.TempDir(), Reporter: reporter,
		processes:       map[string]*trackedProcess{"observation": {snapshot: Snapshot{StreamID: "observation", Status: "running"}, job: lifecycle.StreamJob{StreamID: "observation"}, process: process}},
		ProcessExitHook: func(string) { close(held); <-released },
	}
	go func() { defer close(finished); manager.wait("observation", process, nil) }()
	t.Cleanup(func() { release(); <-finished })
	select {
	case <-held:
	case <-time.After(2 * time.Second):
		t.Fatal("real Manager did not reach its process exit hook")
	}
	return manager, reporter, release
}

func TestManagerReportsSafeStoppedProcessDiagnostics(t *testing.T) {
	reporter := &fakeReporter{}
	root := t.TempDir()
	starter := &fakeStarter{}
	manager := &Manager{ArchiveRoot: root, FFmpegBin: "ffmpeg", Starter: starter, Reporter: reporter, InputResolver: testInputResolver, AllowHostnameInputs: true, OutputRelayMode: outputrelay.ModeDirect}
	job := lifecycle.StreamJob{
		StreamID:  "stream-01",
		Name:      "Morning Stream",
		InputURL:  "srt://input.example.com:9000",
		RTMPURL:   "rtmps://youtube.example.com/live2",
		StreamKey: "secret-stream-key", YouTubeOutputMode: "stream_key",
	}
	if _, err := manager.Start(job); err != nil {
		t.Fatal(err)
	}
	layout, err := archive.NewLayout(root, job.StreamID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(layout.FinalMKV(), bytes.Repeat([]byte("x"), 293), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(layout.TmpFFmpegProgress(), []byte("frame=1\nfps=0.06\nout_time_us=7872000\nspeed=0.452x\nprogress=end\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	starter.process.stderr = "safe stop diagnostic for rtmps://youtube.example.com/live2/secret-stream-key"
	if _, err := manager.Stop(job.StreamID); err != nil {
		t.Fatal(err)
	}

	deadline := time.After(2 * time.Second)
	for {
		signal, ok := reporter.find("encoder.process.stopped")
		if ok {
			if got, want := signal.Attributes["stop_requested"], true; got != want {
				t.Fatalf("stop_requested = %#v, want %v", got, want)
			}
			if got, want := signal.Attributes["error_class"], "stop_requested"; got != want {
				t.Fatalf("error_class = %#v, want %q", got, want)
			}
			if got, want := signal.Attributes["stderr_tail_present"], true; got != want {
				t.Fatalf("stderr_tail_present = %#v, want %v", got, want)
			}
			stderr, _ := signal.Attributes["stderr_tail"].(string)
			if strings.Contains(stderr, "secret-stream-key") || strings.Contains(stderr, "rtmps://youtube.example.com/live2") {
				t.Fatalf("stopped stderr diagnostic leaked a secret: %s", stderr)
			}
			if !strings.Contains(stderr, "safe stop diagnostic") {
				t.Fatalf("stopped stderr diagnostic missing safe text: %s", stderr)
			}
			if got, want := signal.Attributes["ffmpeg_progress"], "end"; got != want {
				t.Fatalf("ffmpeg_progress = %#v, want %q", got, want)
			}
			if got, want := signal.Attributes["ffmpeg_frame"], int64(1); got != want {
				t.Fatalf("ffmpeg_frame = %#v, want %d", got, want)
			}
			if got, want := signal.Attributes["ffmpeg_out_time_us"], int64(7872000); got != want {
				t.Fatalf("ffmpeg_out_time_us = %#v, want %d", got, want)
			}
			if got, want := signal.Attributes["ffmpeg_speed_ratio"], 0.452; got != want {
				t.Fatalf("ffmpeg_speed_ratio = %#v, want %v", got, want)
			}
			if got, want := signal.Attributes["final_mkv_bytes"], int64(293); got != want {
				t.Fatalf("final_mkv_bytes = %#v, want %d", got, want)
			}
			return
		}
		select {
		case <-deadline:
			t.Fatalf("stopped process diagnostics were not observed: %#v", reporter.names())
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}
}

func TestManagerDoesNotMaskArchiveOutputFailureDuringRequestedStop(t *testing.T) {
	reporter := &fakeReporter{}
	root := t.TempDir()
	starter := &fakeStarter{}
	manager := &Manager{ArchiveRoot: root, FFmpegBin: "ffmpeg", Starter: starter, Reporter: reporter, InputResolver: testInputResolver, AllowHostnameInputs: true, OutputRelayMode: outputrelay.ModeDirect}
	job := lifecycle.StreamJob{
		StreamID:  "stream-archive-output-failure",
		Name:      "Archive Output Failure",
		InputURL:  "srt://input.example.com:9000",
		RTMPURL:   "rtmps://youtube.example.com/live2",
		StreamKey: "secret-stream-key", YouTubeOutputMode: "stream_key",
	}
	if _, err := manager.Start(job); err != nil {
		t.Fatal(err)
	}
	starter.process.stderr = "tee: Slave muxer #1 failed: Error writing trailer"
	if _, err := manager.Stop(job.StreamID); err != nil {
		t.Fatal(err)
	}

	deadline := time.After(2 * time.Second)
	for {
		signal, ok := reporter.find("encoder.process.stopped")
		if ok {
			if got, want := signal.Attributes["error_class"], "archive_output"; got != want {
				t.Fatalf("error_class = %#v, want %q", got, want)
			}
			if got, want := signal.Attributes["process_error"], true; got != want {
				t.Fatalf("process_error = %#v, want %v", got, want)
			}
			if got, want := signal.Attributes["archive_partial"], true; got != want {
				t.Fatalf("archive_partial = %#v, want %v", got, want)
			}
			return
		}
		select {
		case <-deadline:
			t.Fatalf("requested-stop output failure was not reported: %#v", reporter.names())
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}
}

func TestManagerReportsRecorderMetricsWhileRunning(t *testing.T) {
	reporter := &fakeReporter{}
	root := t.TempDir()
	manager := &Manager{ArchiveRoot: root, FFmpegBin: "ffmpeg", Starter: &fakeStarter{}, Reporter: reporter, MetricsInterval: 10 * time.Millisecond, InputResolver: testInputResolver, AllowHostnameInputs: true, OutputRelayMode: outputrelay.ModeDirect}
	job := lifecycle.StreamJob{StreamID: "stream-01", Name: "Morning Stream", InputURL: "srt://input.example.com:9000", RTMPURL: "rtmps://youtube.example.com/live2", StreamKey: "key", YouTubeOutputMode: "stream_key"}
	if _, err := manager.Start(job); err != nil {
		t.Fatal(err)
	}
	layout, err := archive.NewLayout(root, "stream-01")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(layout.FinalMKV(), []byte("recorded bytes"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(layout.TmpFFmpegProgress(), []byte("fps=58.5\nbitrate=7450.1kbits/s\ndrop_frames=3\nspeed=0.938x\nprogress=continue\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(layout.TmpFFmpegAudioStats(), []byte("lavfi.astats.Overall.RMS_level=-55.0\nlavfi.astats.Overall.Peak_level=-0.5\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(2 * time.Second)
	for {
		if reporter.has("encoder.process_alive") &&
			reporter.has("recorder.file_size_bytes") &&
			reporter.has("recorder.write_bitrate_kbps") &&
			reporter.has("encoder.output_fps") &&
			reporter.has("encoder.output_bitrate_kbps") &&
			reporter.has("encoder.output_speed_ratio") &&
			reporter.has("encoder.dropped_frames_total") &&
			reporter.has("encoder.audio_level_db") &&
			reporter.has("encoder.audio_silence_sec") &&
			reporter.has("encoder.audio_clipping_total") &&
			reporter.has("media.input_timeout_sec") {
			if _, err := manager.Stop("stream-01"); err != nil {
				t.Fatal(err)
			}
			return
		}
		select {
		case <-deadline:
			t.Fatalf("missing recorder metrics: %#v", reporter.names())
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}
}

func TestManagerReportsMediaInputTimeoutWhenProgressStalls(t *testing.T) {
	reporter := &fakeReporter{}
	manager := &Manager{ArchiveRoot: t.TempDir(), FFmpegBin: "ffmpeg", Starter: &fakeStarter{}, Reporter: reporter, MetricsInterval: 10 * time.Millisecond, InputResolver: testInputResolver, AllowHostnameInputs: true, OutputRelayMode: outputrelay.ModeDirect}
	job := lifecycle.StreamJob{StreamID: "stream-01", Name: "Morning Stream", InputURL: "srt://input.example.com:9000", RTMPURL: "rtmps://youtube.example.com/live2", StreamKey: "key", YouTubeOutputMode: "stream_key"}
	if _, err := manager.Start(job); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_, _ = manager.Stop("stream-01")
	}()
	deadline := time.After(2 * time.Second)
	for {
		if reporter.hasValueAtLeast("media.input_timeout_sec", 0.01) {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("missing input timeout metric: %#v", reporter.names())
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}
}
