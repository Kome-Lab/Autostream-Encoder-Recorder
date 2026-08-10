package streamproc

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/example/autostream-encoder-recorder/internal/archive"
	"github.com/example/autostream-encoder-recorder/internal/control"
	"github.com/example/autostream-encoder-recorder/internal/ffmpeg"
	"github.com/example/autostream-encoder-recorder/internal/lifecycle"
	"github.com/example/autostream-encoder-recorder/internal/observability"
	"github.com/example/autostream-encoder-recorder/internal/outputrelay"
)

const (
	staticRelayBindingID      = "relay-11111111-1111-1111-1111-111111111111"
	otherStaticRelayBindingID = "relay-22222222-2222-2222-2222-222222222222"
)

type fakeStarter struct {
	mu        sync.Mutex
	bin       string
	args      []string
	process   *fakeProcess
	processes []*fakeProcess
}

func (s *fakeStarter) Start(ctx context.Context, bin string, args []string) (RunningProcess, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.bin = bin
	s.args = append([]string(nil), args...)
	s.process = &fakeProcess{done: make(chan error, 1)}
	s.processes = append(s.processes, s.process)
	return s.process, nil
}

type fakeProcess struct {
	done       chan error
	terminated bool
	killed     bool
}

func (p *fakeProcess) PID() int {
	return 1234
}

func (p *fakeProcess) Wait() error {
	return <-p.done
}

func (p *fakeProcess) Terminate() error {
	p.terminated = true
	p.done <- nil
	return nil
}

func (p *fakeProcess) Kill() error {
	p.killed = true
	p.done <- nil
	return nil
}

func testInputResolver(ctx context.Context, host string) ([]net.IP, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return []net.IP{net.ParseIP("93.184.216.34")}, nil
}

func TestManagerStartWritesMetadataAndMasksStreamKey(t *testing.T) {
	root := t.TempDir()
	starter := &fakeStarter{}
	manager := &Manager{ArchiveRoot: root, FFmpegBin: "ffmpeg", Starter: starter, InputResolver: testInputResolver, AllowHostnameInputs: true, OutputRelayURL: "rtmp://127.0.0.1/autostream/{stream_id}", OutputRelayMode: outputrelay.ModeLiveAPIStatic, OutputRelayBindingID: staticRelayBindingID}
	snapshot, err := manager.Start(lifecycle.StreamJob{
		StreamID: "stream-01", Name: "Morning Stream", InputURL: "rtsp://camera:camera-password@input.example.com/live/%70%61%74%68-token",
		RTMPURL: "rtmps://youtube.example.com/live2", StreamKey: "secret-stream-key", YouTubeOutputMode: "live_api_relay_static", OutputRelayBindingID: staticRelayBindingID, YouTubeOutputReady: true,
		StartedAt: time.Date(2026, 5, 29, 1, 2, 3, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Status != "running" || snapshot.PID != 1234 {
		t.Fatalf("unexpected snapshot: %#v", snapshot)
	}
	if starter.bin != "ffmpeg" {
		t.Fatalf("unexpected bin: %s", starter.bin)
	}
	joinedArgs := strings.Join(starter.args, " ")
	if strings.Contains(joinedArgs, "secret-stream-key") || strings.Contains(joinedArgs, "rtmps://youtube.example.com/live2") {
		t.Fatalf("stream key or upstream RTMPS URL leaked in relay process args: %s", joinedArgs)
	}
	if !strings.Contains(joinedArgs, "rtmp://127.0.0.1/autostream/stream-01") {
		t.Fatalf("expected local relay target in process args: %s", joinedArgs)
	}
	if !strings.Contains(joinedArgs, "ffmpeg-progress.txt") {
		t.Fatalf("expected ffmpeg progress file in args: %s", joinedArgs)
	}
	for _, want := range []string{"f=hls", "onfail=ignore", "hls_time=2", "hls_list_size=6", "segment-%06d.ts"} {
		if !strings.Contains(joinedArgs, want) {
			t.Fatalf("expected HLS preview option %q in args: %s", want, joinedArgs)
		}
	}
	layout, err := archive.NewLayout(root, "stream-01")
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(layout.TmpMetadata())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "secret-stream-key") || strings.Contains(string(body), "camera-password") || strings.Contains(string(body), "%70%61%74%68-token") || strings.Contains(string(body), "path-token") {
		t.Fatalf("secret leaked in metadata: %s", string(body))
	}
	if !strings.Contains(string(body), `rtsp://input.example.com/\u003cREDACTED\u003e`) {
		t.Fatalf("expected masked input URL in metadata: %s", string(body))
	}
	if !strings.Contains(string(body), "preview/index.m3u8") || !strings.Contains(string(body), "preview/segment-%06d.ts") {
		t.Fatalf("expected logical preview paths in metadata: %s", string(body))
	}
	if strings.Contains(string(body), root) || strings.Contains(string(body), `\tmp\`) || strings.Contains(string(body), `/tmp/`) {
		t.Fatalf("local archive path leaked in start metadata: %s", string(body))
	}
	if snapshot.Archive["recording_mkv"] != "final.mkv" || snapshot.Archive["preview_playlist"] != "preview/index.m3u8" || strings.Contains(snapshot.Archive["recording_mkv"], root) {
		t.Fatalf("snapshot archive should expose logical artifact names only: %#v", snapshot.Archive)
	}
	if info, err := os.Lstat(layout.PreviewDir()); err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("expected non-symlink preview directory before FFmpeg start: info=%v err=%v", info, err)
	}
	if _, err := os.Stat(layout.TmpLogs()); err != nil {
		t.Fatalf("expected logs: %v", err)
	}
}

func TestManagerLegacyStreamKeyRelayStartsWithoutRetainingUpstreamTarget(t *testing.T) {
	root := t.TempDir()
	starter := &fakeStarter{}
	manager := &Manager{
		ArchiveRoot:          root,
		FFmpegBin:            "ffmpeg",
		Starter:              starter,
		InputResolver:        testInputResolver,
		AllowHostnameInputs:  true,
		OutputRelayURL:       "rtmp://127.0.0.1/autostream/{stream_id}",
		OutputRelayBindingID: "stale-binding-must-not-matter",
	}

	_, err := manager.Start(lifecycle.StreamJob{
		StreamID: "stream-legacy-01", Name: "Legacy Stream", InputURL: "srt://input.example.com:9000",
		RTMPURL: "rtmps://youtube.example.com/live2", StreamKey: "legacy-upstream-key", YouTubeOutputMode: "stream_key",
	})
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Join(starter.args, " ")
	for _, forbidden := range []string{"youtube.example.com", "legacy-upstream-key"} {
		if strings.Contains(args, forbidden) {
			t.Fatalf("legacy relay FFmpeg args leaked unused upstream material %q: %#v", forbidden, starter.args)
		}
	}
	if !strings.Contains(args, "rtmp://127.0.0.1/autostream/stream-legacy-01") {
		t.Fatalf("legacy relay must use its local output target: %#v", starter.args)
	}
	layout, err := archive.NewLayout(root, "stream-legacy-01")
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := os.ReadFile(layout.TmpMetadata())
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"youtube.example.com", "legacy-upstream-key"} {
		if strings.Contains(string(metadata), forbidden) {
			t.Fatalf("legacy relay metadata leaked unused upstream material %q: %s", forbidden, metadata)
		}
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
		OutputRelayMode:      outputrelay.ModeLiveAPIStatic,
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
				OutputRelayMode:      outputrelay.ModeLiveAPIStatic,
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
		OutputRelayMode:      outputrelay.ModeLiveAPIStatic,
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
		OutputRelayMode:      outputrelay.ModeLiveAPIStatic,
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
		OutputRelayMode:      outputrelay.ModeLiveAPIStatic,
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

func TestWriteStartMetadataRedactsDirectRTMPSOutputTarget(t *testing.T) {
	root := t.TempDir()
	layout, err := archive.NewLayout(root, "stream-01")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(layout.TmpDir(), 0o750); err != nil {
		t.Fatal(err)
	}
	args := []string{
		"-f", "tee",
		"[f=flv]rtmps://youtube.example.com/live2/direct-secret-stream-key|[f=matroska]" + layout.FinalMKV(),
	}
	err = writeStartMetadata(layout, lifecycle.StreamJob{
		StreamID:  "stream-01",
		Name:      "Morning Stream",
		InputURL:  "rtsp://camera:camera-password@input.example.com/live/path-token",
		RTMPURL:   "rtmps://youtube.example.com/live2",
		StreamKey: "direct-secret-stream-key",
	}, Snapshot{StartedAtJST: "2026-06-08T12:00:00+09:00", Archive: map[string]string{"recording_mkv": "final.mkv"}}, args, "ffmpeg")
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(layout.TmpMetadata())
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	for _, forbidden := range []string{"direct-secret-stream-key", "camera-password", "path-token", "live2/direct-secret-stream-key", root, layout.FinalMKV()} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("metadata leaked %q: %s", forbidden, text)
		}
	}
	if !strings.Contains(text, "rtmps://youtube.example.com/\\u003cREDACTED\\u003e") {
		t.Fatalf("expected direct RTMPS output target to be masked in metadata: %s", text)
	}
	if !strings.Contains(text, "final.mkv") {
		t.Fatalf("expected logical archive name to remain in metadata: %s", text)
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
	}
	_, err := manager.Start(lifecycle.StreamJob{
		StreamID: "stream-01", Name: "Morning Stream", InputURL: "srt://input.example.com:9000",
		RTMPURL: "rtmps://youtube.example.com/live2", StreamKey: "secret-stream-key",
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
		ArchiveRoot:         root,
		FFmpegBin:           "ffmpeg",
		Starter:             starter,
		InputResolver:       testInputResolver,
		AllowHostnameInputs: true,
		OutputRelayURL:      "rtmps://youtube.example.com/live2/direct-secret-stream-key",
		RequireOutputRelay:  true,
	}
	_, err := manager.Start(lifecycle.StreamJob{
		StreamID:  "stream-01",
		Name:      "Morning Stream",
		InputURL:  "srt://input.example.com:9000",
		RTMPURL:   "rtmps://youtube.example.com/live2",
		StreamKey: "direct-secret-stream-key",
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

func TestWriteStartMetadataRejectsSymlink(t *testing.T) {
	root := t.TempDir()
	layout, err := archive.NewLayout(root, "stream-01")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(layout.TmpDir(), 0o750); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(root, "outside.json")
	if err := os.WriteFile(outside, []byte("outside"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, layout.TmpMetadata()); err != nil {
		t.Skipf("symlink creation is not available in this environment: %v", err)
	}
	err = writeStartMetadata(layout, lifecycle.StreamJob{
		StreamID: "stream-01",
		Name:     "Morning Stream",
		RTMPURL:  "rtmps://youtube.example.com/live2",
	}, Snapshot{StartedAtJST: "2026-06-08T12:00:00+09:00", Archive: map[string]string{"recording_mkv": "final.mkv"}}, []string{"-f", "lavfi"}, "ffmpeg")
	if err == nil {
		t.Fatal("expected symlink metadata write to fail")
	}
	body, err := os.ReadFile(outside)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "outside" {
		t.Fatalf("symlink target was modified: %q", string(body))
	}
}

func TestManagerRejectsTmpDirectorySymlinkBeforeStartingFFmpeg(t *testing.T) {
	root := t.TempDir()
	layout, err := archive.NewLayout(root, "stream-01")
	if err != nil {
		t.Fatal(err)
	}
	tmpRoot := filepath.Join(root, "tmp")
	if err := os.MkdirAll(tmpRoot, 0o750); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(root, "outside")
	if err := os.MkdirAll(outside, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, layout.TmpDir()); err != nil {
		t.Skipf("symlink creation is not available in this environment: %v", err)
	}
	starter := &fakeStarter{}
	manager := &Manager{ArchiveRoot: root, FFmpegBin: "ffmpeg", Starter: starter, InputResolver: testInputResolver, AllowHostnameInputs: true}
	_, err = manager.Start(lifecycle.StreamJob{
		StreamID:  "stream-01",
		Name:      "Morning Stream",
		InputURL:  "srt://input.example.com:9000",
		RTMPURL:   "rtmps://youtube.example.com/live2",
		StreamKey: "key",
	})
	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("expected symlink directory rejection, got %v", err)
	}
	if starter.process != nil {
		t.Fatalf("ffmpeg should not be started for symlink archive directory: %#v", starter.process)
	}
}

func TestManagerRejectsFinalMKVSymlinkBeforeStartingFFmpeg(t *testing.T) {
	root := t.TempDir()
	layout, err := archive.NewLayout(root, "stream-01")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(layout.TmpDir(), 0o750); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(root, "outside.mkv")
	if err := os.WriteFile(outside, []byte("outside"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, layout.FinalMKV()); err != nil {
		t.Skipf("symlink creation is not available in this environment: %v", err)
	}
	starter := &fakeStarter{}
	manager := &Manager{ArchiveRoot: root, FFmpegBin: "ffmpeg", Starter: starter, InputResolver: testInputResolver, AllowHostnameInputs: true}
	_, err = manager.Start(lifecycle.StreamJob{
		StreamID:  "stream-01",
		Name:      "Morning Stream",
		InputURL:  "srt://input.example.com:9000",
		RTMPURL:   "rtmps://youtube.example.com/live2",
		StreamKey: "key",
	})
	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("expected final.mkv symlink rejection, got %v", err)
	}
	if starter.process != nil {
		t.Fatalf("ffmpeg should not be started for symlink final.mkv: %#v", starter.process)
	}
	body, err := os.ReadFile(outside)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "outside" {
		t.Fatalf("symlink target was modified: %q", string(body))
	}
}

func TestManagerRejectsPreviewDirectorySymlinkBeforeStartingFFmpeg(t *testing.T) {
	root := t.TempDir()
	layout, err := archive.NewLayout(root, "stream-01")
	if err != nil {
		t.Fatal(err)
	}
	if err := archive.EnsureDirNoSymlinks(root, layout.TmpDir()); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(root, "outside-preview")
	if err := os.MkdirAll(outside, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, layout.PreviewDir()); err != nil {
		t.Skipf("symlink creation is not available in this environment: %v", err)
	}
	starter := &fakeStarter{}
	manager := &Manager{ArchiveRoot: root, FFmpegBin: "ffmpeg", Starter: starter, InputResolver: testInputResolver, AllowHostnameInputs: true}
	_, err = manager.Start(lifecycle.StreamJob{
		StreamID:  "stream-01",
		Name:      "Morning Stream",
		InputURL:  "srt://input.example.com:9000",
		RTMPURL:   "rtmps://youtube.example.com/live2",
		StreamKey: "key",
	})
	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("expected preview directory symlink rejection, got %v", err)
	}
	if starter.process != nil {
		t.Fatalf("ffmpeg should not start with a symlinked preview directory: %#v", starter.process)
	}
}

func TestManagerReservesFinalMKVBeforeStartingFFmpeg(t *testing.T) {
	root := t.TempDir()
	starter := &fakeStarter{}
	manager := &Manager{ArchiveRoot: root, FFmpegBin: "ffmpeg", Starter: starter, InputResolver: testInputResolver, AllowHostnameInputs: true}
	_, err := manager.Start(lifecycle.StreamJob{
		StreamID:  "stream-01",
		Name:      "Morning Stream",
		InputURL:  "srt://input.example.com:9000",
		RTMPURL:   "rtmps://youtube.example.com/live2",
		StreamKey: "key",
	})
	if err != nil {
		t.Fatal(err)
	}
	layout, err := archive.NewLayout(root, "stream-01")
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(layout.FinalMKV())
	if err != nil {
		t.Fatal(err)
	}
	if !info.Mode().IsRegular() {
		t.Fatalf("expected final.mkv to be reserved as a regular file, got %s", info.Mode())
	}
	if starter.process == nil {
		t.Fatal("ffmpeg should start after reserving final.mkv")
	}
}

func TestNewManagerFromEnvRequiresInputAllowedHostsByDefault(t *testing.T) {
	t.Setenv("AUTOSTREAM_ARCHIVE_DIR", t.TempDir())
	t.Setenv("FFMPEG_BIN", "ffmpeg")
	manager := NewManagerFromEnv()
	manager.Starter = &fakeStarter{}
	manager.InputResolver = testInputResolver
	_, err := manager.Start(lifecycle.StreamJob{
		StreamID:  "stream-01",
		Name:      "Morning Stream",
		InputURL:  "srt://source.example.com:9000",
		RTMPURL:   "rtmps://youtube.example.com/live2",
		StreamKey: "key",
	})
	if !errors.Is(err, ffmpeg.ErrUnsafeInputTarget) {
		t.Fatalf("expected external input to require AUTOSTREAM_INPUT_ALLOWED_HOSTS by default, got %v", err)
	}
}

func TestNewManagerFromEnvDefaultsToDirectInProduction(t *testing.T) {
	t.Setenv("AUTOSTREAM_ENV", "production")
	manager := NewManagerFromEnv()
	if manager.RequireOutputRelay {
		t.Fatal("production should default to direct output when Relay is not explicitly required")
	}
}

func TestNewManagerFromEnvFailsClosedBeforeFFmpegStartWhenRelayIsExplicitlyRequired(t *testing.T) {
	t.Setenv("AUTOSTREAM_ENV", "production")
	t.Setenv("AUTOSTREAM_REQUIRE_OUTPUT_RELAY", "true")
	t.Setenv("AUTOSTREAM_ARCHIVE_DIR", t.TempDir())
	t.Setenv("FFMPEG_BIN", "ffmpeg")
	t.Setenv("AUTOSTREAM_INPUT_ALLOWED_HOSTS", "source.example.com")
	t.Setenv("AUTOSTREAM_ALLOW_HOSTNAME_INPUTS", "true")

	starter := &fakeStarter{}
	manager := NewManagerFromEnv()
	manager.Starter = starter
	manager.InputResolver = testInputResolver

	_, err := manager.Start(lifecycle.StreamJob{
		StreamID:  "stream-01",
		Name:      "Morning Stream",
		InputURL:  "srt://source.example.com:9000",
		RTMPURL:   "rtmps://youtube.example.com/live2",
		StreamKey: "secret-stream-key",
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
	t.Setenv("AUTOSTREAM_OUTPUT_RELAY_MODE", outputrelay.ModeLiveAPIStatic)
	t.Setenv("AUTOSTREAM_OUTPUT_RELAY_BINDING_ID", staticRelayBindingID)
	manager := NewManagerFromEnv()
	if manager.OutputRelayURL != "rtmp://127.0.0.1/autostream/{stream_id}" {
		t.Fatalf("unexpected output relay URL: %q", manager.OutputRelayURL)
	}
	if manager.OutputRelayBindingID != staticRelayBindingID {
		t.Fatalf("unexpected output relay binding ID: %q", manager.OutputRelayBindingID)
	}
	if manager.OutputRelayMode != outputrelay.ModeLiveAPIStatic {
		t.Fatalf("unexpected output relay mode: %q", manager.OutputRelayMode)
	}
}

func TestNewManagerFromEnvAllowsConfiguredInputAllowedHosts(t *testing.T) {
	t.Setenv("AUTOSTREAM_ARCHIVE_DIR", t.TempDir())
	t.Setenv("FFMPEG_BIN", "ffmpeg")
	t.Setenv("AUTOSTREAM_INPUT_ALLOWED_HOSTS", "source.example.com")
	t.Setenv("AUTOSTREAM_ALLOW_HOSTNAME_INPUTS", "true")
	manager := NewManagerFromEnv()
	manager.Starter = &fakeStarter{}
	manager.InputResolver = testInputResolver
	_, err := manager.Start(lifecycle.StreamJob{
		StreamID:  "stream-01",
		Name:      "Morning Stream",
		InputURL:  "srt://source.example.com:9000",
		RTMPURL:   "rtmps://youtube.example.com/live2",
		StreamKey: "key",
	})
	if err != nil {
		t.Fatalf("expected allowlisted external input to start: %v", err)
	}
}

func TestManagerRejectsDuplicateRunningStream(t *testing.T) {
	root := t.TempDir()
	manager := &Manager{ArchiveRoot: root, FFmpegBin: "ffmpeg", Starter: &fakeStarter{}, InputResolver: testInputResolver, AllowHostnameInputs: true}
	job := lifecycle.StreamJob{StreamID: "stream-01", Name: "Morning Stream", InputURL: "srt://input.example.com:9000", RTMPURL: "rtmps://youtube.example.com/live2", StreamKey: "key"}
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

type blockingStarter struct {
	started chan struct{}
	release chan struct{}
	mu      sync.Mutex
	count   int
}

func (s *blockingStarter) Start(ctx context.Context, bin string, args []string) (RunningProcess, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.count++
	if s.count == 1 {
		close(s.started)
	}
	s.mu.Unlock()
	<-s.release
	return &fakeProcess{done: make(chan error, 1)}, nil
}

func TestManagerRejectsDuplicateStartingStream(t *testing.T) {
	starter := &blockingStarter{started: make(chan struct{}), release: make(chan struct{})}
	manager := &Manager{ArchiveRoot: t.TempDir(), FFmpegBin: "ffmpeg", Starter: starter, InputResolver: testInputResolver, AllowHostnameInputs: true}
	job := lifecycle.StreamJob{StreamID: "stream-01", Name: "Morning Stream", InputURL: "srt://input.example.com:9000", RTMPURL: "rtmps://youtube.example.com/live2", StreamKey: "key"}

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
	manager := &Manager{ArchiveRoot: t.TempDir(), FFmpegBin: "ffmpeg", Starter: starter, InputResolver: testInputResolver, AllowHostnameInputs: true}
	job := lifecycle.StreamJob{StreamID: "stream-01", Name: "Morning Stream", InputURL: "srt://input.example.com:9000", RTMPURL: "rtmps://youtube.example.com/live2", StreamKey: "key"}

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
	manager := &Manager{ArchiveRoot: t.TempDir(), FFmpegBin: "ffmpeg", Starter: starter, InputResolver: testInputResolver, AllowHostnameInputs: true}
	job := lifecycle.StreamJob{
		StreamID:  "stream-01",
		Name:      "Discord Audio Stream",
		InputMode: "discord_opus_rtp",
		InputURL:  "internal_discord_audio:C:/tmp/attacker.sdp",
		RTMPURL:   "rtmps://youtube.example.com/live2",
		StreamKey: "key",
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
	manager := &Manager{ArchiveRoot: t.TempDir(), FFmpegBin: "ffmpeg", Starter: starter, InputResolver: testInputResolver, AllowHostnameInputs: true}
	job := lifecycle.StreamJob{
		StreamID:  "stream-01",
		Name:      "Discord Audio Stream",
		InputURL:  "internal_discord_audio:C:/tmp/discord-opus.sdp",
		RTMPURL:   "rtmps://youtube.example.com/live2",
		StreamKey: "key",
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
	}
	job := lifecycle.StreamJob{
		StreamID:  "stream-01",
		Name:      "Morning Stream",
		InputURL:  "srt://untrusted.example.com:9000",
		RTMPURL:   "rtmps://youtube.example.com/live2",
		StreamKey: "key",
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
	}
	job := lifecycle.StreamJob{
		StreamID:  "stream-01",
		Name:      "Morning Stream",
		InputURL:  "rtsp://camera.example.com/live",
		RTMPURL:   "rtmps://youtube.example.com/live2",
		StreamKey: "key",
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
	}
	job := lifecycle.StreamJob{
		StreamID:  "stream-01",
		Name:      "Morning Stream",
		InputURL:  "https://cdn.example.com/live/index.m3u8",
		RTMPURL:   "rtmps://youtube.example.com/live2",
		StreamKey: "key",
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
	}
	job := lifecycle.StreamJob{
		StreamID:  "stream-01",
		Name:      "Morning Stream",
		InputURL:  "https://cdn.example.com/live/index.m3u8",
		RTMPURL:   "rtmps://youtube.example.com/live2",
		StreamKey: "key",
	}
	if _, err := manager.Start(job); err != nil {
		t.Fatalf("expected explicitly enabled direct HLS input to start: %v", err)
	}
	if starter.process == nil {
		t.Fatal("expected ffmpeg to be started for explicitly enabled direct HLS input")
	}
}

func TestManagerHeartbeatMetricsReflectRunningProcess(t *testing.T) {
	manager := &Manager{ArchiveRoot: t.TempDir(), FFmpegBin: "ffmpeg", Starter: &fakeStarter{}, InputResolver: testInputResolver, AllowHostnameInputs: true}
	if metrics := manager.HeartbeatMetrics(); metrics["encoder.process_alive"] != 0 || metrics["encoder.active_process_count"] != 0 {
		t.Fatalf("unexpected idle metrics: %#v", metrics)
	}
	job := lifecycle.StreamJob{StreamID: "stream-01", Name: "Morning Stream", InputURL: "srt://input.example.com:9000", RTMPURL: "rtmps://youtube.example.com/live2", StreamKey: "key"}
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

func TestManagerStopTransitionsToStopped(t *testing.T) {
	starter := &fakeStarter{}
	manager := &Manager{ArchiveRoot: t.TempDir(), FFmpegBin: "ffmpeg", Starter: starter, InputResolver: testInputResolver, AllowHostnameInputs: true}
	job := lifecycle.StreamJob{StreamID: "stream-01", Name: "Morning Stream", InputURL: "srt://input.example.com:9000", RTMPURL: "rtmps://youtube.example.com/live2", StreamKey: "key"}
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
	job := lifecycle.StreamJob{StreamID: "stream-a", Name: "Stream A", InputURL: "srt://input.example.com:9000", RTMPURL: "rtmps://youtube.example.com/live2", StreamKey: "key"}
	beforeRestart := &Manager{ArchiveRoot: root, FFmpegBin: "ffmpeg", Starter: &fakeStarter{}, InputResolver: testInputResolver, AllowHostnameInputs: true}
	if _, err := beforeRestart.Start(job); err != nil {
		t.Fatal(err)
	}
	if _, err := beforeRestart.Stop(job.StreamID); err != nil {
		t.Fatal(err)
	}

	afterRestart := &Manager{ArchiveRoot: root}
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
	manager := &Manager{ArchiveRoot: root, FFmpegBin: "ffmpeg", Starter: &fakeStarter{}, InputResolver: testInputResolver, AllowHostnameInputs: true}
	if err := manager.writeStopReceipt("stream-a", stopReceipt{StreamID: "stream-a", ExpiresAt: time.Now().UTC().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Start(lifecycle.StreamJob{StreamID: "stream-a", Name: "Stream A", InputURL: "srt://input.example.com:9000", RTMPURL: "rtmps://youtube.example.com/live2", StreamKey: "key"}); err != nil {
		t.Fatal(err)
	}
	// A fresh manager models the process-local state loss caused by a service
	// restart. It must not rediscover the pre-start receipt from disk.
	afterRestart := &Manager{ArchiveRoot: root}
	if exists, err := afterRestart.hasStopReceipt("stream-a"); err != nil || exists {
		t.Fatalf("start did not durably clear stop receipt across restart: exists=%v err=%v", exists, err)
	}
	if _, err := manager.Stop("stream-a"); err != nil {
		t.Fatal(err)
	}
}

func TestManagerExpiredStopReceiptDoesNotAcknowledgeUnknownTarget(t *testing.T) {
	manager := &Manager{ArchiveRoot: t.TempDir()}
	if err := manager.writeStopReceipt("stream-a", stopReceipt{StreamID: "stream-a", ExpiresAt: time.Now().UTC().Add(-time.Second)}); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Stop("stream-a"); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("expired receipt stop error = %v, want ErrNotRunning", err)
	}
}

func TestManagerStopAllTerminatesRunningStreams(t *testing.T) {
	starter := &fakeStarter{}
	manager := &Manager{ArchiveRoot: t.TempDir(), FFmpegBin: "ffmpeg", Starter: starter, InputResolver: testInputResolver, AllowHostnameInputs: true}
	jobs := []lifecycle.StreamJob{
		{StreamID: "stream-01", Name: "Morning Stream", InputURL: "srt://input.example.com:9000", RTMPURL: "rtmps://youtube.example.com/live2", StreamKey: "key-1"},
		{StreamID: "stream-02", Name: "Evening Stream", InputURL: "srt://input.example.com:9001", RTMPURL: "rtmps://youtube.example.com/live2", StreamKey: "key-2"},
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

func TestManagerStopAllAndDrainWaitsForArchivePackaging(t *testing.T) {
	packager := &fakePackager{root: t.TempDir(), delay: 80 * time.Millisecond}
	manager := &Manager{
		ArchiveRoot:   t.TempDir(),
		FFmpegBin:     "ffmpeg",
		Starter:       &fakeStarter{},
		Packager:      packager,
		InputResolver: testInputResolver, AllowHostnameInputs: true,
	}
	job := lifecycle.StreamJob{StreamID: "stream-01", Name: "Morning Stream", InputURL: "srt://input.example.com:9000", RTMPURL: "rtmps://youtube.example.com/live2", StreamKey: "key"}
	if _, err := manager.Start(job); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if errs := manager.StopAllAndDrain(ctx); len(errs) != 0 {
		t.Fatalf("unexpected stop/drain errors: %#v", errs)
	}
	if !packager.calledWith(job.StreamID) {
		t.Fatalf("packager was not called before drain returned: %#v", packager.jobs)
	}
	status, err := manager.Status(job.StreamID)
	if err != nil {
		t.Fatal(err)
	}
	if status.Status != "completed" {
		t.Fatalf("expected completed status after drain, got %#v", status)
	}
}

func TestManagerScrubsResolvedSecretsFromTrackedProcessAfterPackaging(t *testing.T) {
	packager := &fakePackager{root: t.TempDir()}
	manager := &Manager{
		ArchiveRoot:   t.TempDir(),
		FFmpegBin:     "ffmpeg",
		Starter:       &fakeStarter{},
		Packager:      packager,
		InputResolver: testInputResolver, AllowHostnameInputs: true,
	}
	job := lifecycle.StreamJob{
		StreamID:  "stream-01",
		Name:      "Morning Stream",
		InputURL:  "rtsp://camera:camera-password@input.example.com/live",
		RTMPURL:   "rtmps://youtube.example.com/live2",
		StreamKey: "secret-stream-key",
		ArchiveConfig: lifecycle.ArchiveConfig{
			AuthMode:                            "oauth2",
			ArchiveProfileID:                    "archive-profile-01",
			FolderID:                            "drive-folder-id",
			FolderIDSecretName:                  "drive_destination:dest-01:folder_id",
			ServiceAccountJSON:                  `{"type":"service_account","private_key":"raw-private-key"}`,
			ServiceAccountCredentialsSecretName: "google_drive_credentials",
			ClientSecret:                        "google-client-secret",
			ClientSecretSecretName:              "oauth_provider:provider-01:client_secret",
			RefreshToken:                        "google-refresh-token",
			RefreshTokenSecretName:              "oauth_account:account-01:refresh_token",
			SharedDrive:                         true,
		},
	}
	if _, err := manager.Start(job); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if errs := manager.StopAllAndDrain(ctx); len(errs) != 0 {
		t.Fatalf("unexpected stop/drain errors: %#v", errs)
	}
	if got, ok := packager.jobWith(job.StreamID); !ok || got.ArchiveConfig.FolderID != "drive-folder-id" || got.ArchiveConfig.RefreshToken != "google-refresh-token" {
		t.Fatalf("packager did not receive resolved archive secrets before scrub: %#v", got.ArchiveConfig)
	}
	manager.mu.Lock()
	tracked := manager.processes[job.StreamID]
	manager.mu.Unlock()
	if tracked == nil {
		t.Fatal("tracked process missing")
	}
	if tracked.job.StreamKey != "" || tracked.job.InputURL != "" || tracked.job.RTMPURL != "" {
		t.Fatalf("tracked job retained media secrets after packaging: %#v", tracked.job)
	}
	cfg := tracked.job.ArchiveConfig
	if cfg.FolderID != "" || cfg.ServiceAccountJSON != "" || cfg.ClientSecret != "" || cfg.RefreshToken != "" {
		t.Fatalf("tracked job retained archive secrets after packaging: %#v", cfg)
	}
	if cfg.FolderIDSecretName == "" || cfg.ClientSecretSecretName == "" || cfg.RefreshTokenSecretName == "" || !cfg.SharedDrive {
		t.Fatalf("tracked job should retain non-secret archive references: %#v", cfg)
	}
}

func TestManagerReportsLifecycleSignals(t *testing.T) {
	reporter := &fakeReporter{}
	manager := &Manager{ArchiveRoot: t.TempDir(), FFmpegBin: "ffmpeg", Starter: &fakeStarter{}, Reporter: reporter, InputResolver: testInputResolver, AllowHostnameInputs: true}
	job := lifecycle.StreamJob{StreamID: "stream-01", Name: "Morning Stream", InputURL: "srt://input.example.com:9000", RTMPURL: "rtmps://youtube.example.com/live2", StreamKey: "key"}
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
	manager := &Manager{ArchiveRoot: t.TempDir(), FFmpegBin: "ffmpeg", Starter: starter, Reporter: reporter, InputResolver: testInputResolver, AllowHostnameInputs: true}
	job := lifecycle.StreamJob{
		StreamID:  "stream-01",
		Name:      "Morning Stream",
		InputURL:  "rtsp://camera:camera-password@input.example.com/live/%70%61%74%68-token",
		RTMPURL:   "rtmps://youtube.example.com/live2",
		StreamKey: "secret-stream-key",
	}
	if _, err := manager.Start(job); err != nil {
		t.Fatal(err)
	}
	starter.process.done <- errors.New("ffmpeg failed for rtsp://camera:camera-password@input.example.com/live/%70%61%74%68-token and rtmps://youtube.example.com/live2/secret-stream-key")
	deadline := time.After(2 * time.Second)
	for {
		status, err := manager.Status(job.StreamID)
		if err != nil {
			t.Fatal(err)
		}
		if status.Status == "failed" {
			if strings.Contains(status.Error, "camera-password") || strings.Contains(status.Error, "secret-stream-key") || strings.Contains(status.Error, "%70%61%74%68-token") || strings.Contains(status.Error, "path-token") {
				t.Fatalf("secret leaked in process status error: %s", status.Error)
			}
			if !strings.Contains(status.Error, "rtsp://input.example.com/<REDACTED>") {
				t.Fatalf("expected host-only input URL in process status error: %s", status.Error)
			}
			signal, ok := reporter.find("encoder.process.exited")
			if !ok {
				t.Fatalf("missing process exited signal: %#v", reporter.names())
			}
			attrError, _ := signal.Attributes["error"].(string)
			if strings.Contains(attrError, "camera-password") || strings.Contains(attrError, "secret-stream-key") || strings.Contains(attrError, "%70%61%74%68-token") || strings.Contains(attrError, "path-token") {
				t.Fatalf("secret leaked in observability error: %s", attrError)
			}
			if !strings.Contains(attrError, "rtsp://input.example.com/<REDACTED>") {
				t.Fatalf("expected host-only input URL in observability error: %s", attrError)
			}
			return
		}
		select {
		case <-deadline:
			t.Fatalf("process failure was not observed: %#v", status)
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}
}

func TestManagerReportsRecorderMetricsWhileRunning(t *testing.T) {
	reporter := &fakeReporter{}
	root := t.TempDir()
	manager := &Manager{ArchiveRoot: root, FFmpegBin: "ffmpeg", Starter: &fakeStarter{}, Reporter: reporter, MetricsInterval: 10 * time.Millisecond, InputResolver: testInputResolver, AllowHostnameInputs: true}
	job := lifecycle.StreamJob{StreamID: "stream-01", Name: "Morning Stream", InputURL: "srt://input.example.com:9000", RTMPURL: "rtmps://youtube.example.com/live2", StreamKey: "key"}
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
	if err := os.WriteFile(layout.TmpFFmpegProgress(), []byte("fps=58.5\nbitrate=7450.1kbits/s\ndrop_frames=3\nprogress=continue\n"), 0o640); err != nil {
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
	manager := &Manager{ArchiveRoot: t.TempDir(), FFmpegBin: "ffmpeg", Starter: &fakeStarter{}, Reporter: reporter, MetricsInterval: 10 * time.Millisecond, InputResolver: testInputResolver, AllowHostnameInputs: true}
	job := lifecycle.StreamJob{StreamID: "stream-01", Name: "Morning Stream", InputURL: "srt://input.example.com:9000", RTMPURL: "rtmps://youtube.example.com/live2", StreamKey: "key"}
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

func TestManagerPackagesArchiveAfterStoppedProcess(t *testing.T) {
	reporter := &fakeReporter{}
	packager := &fakePackager{root: t.TempDir()}
	artifactReporter := &fakeArtifactReporter{}
	manager := &Manager{
		ArchiveRoot:         t.TempDir(),
		FFmpegBin:           "ffmpeg",
		Starter:             &fakeStarter{},
		Reporter:            reporter,
		ArtifactReporter:    artifactReporter,
		Packager:            packager,
		InputResolver:       testInputResolver,
		AllowHostnameInputs: true,
	}
	job := lifecycle.StreamJob{
		StreamID: "stream-01", Name: "Morning Stream", InputURL: "srt://input.example.com:9000", RTMPURL: "rtmps://youtube.example.com/live2", StreamKey: "key",
		StartedAt: time.Date(2026, 5, 31, 1, 2, 3, 0, time.UTC),
		ArchiveConfig: lifecycle.ArchiveConfig{
			AuthMode:    "service_account",
			FolderID:    "drive-folder-id",
			BasePath:    "AutoStream",
			SharedDrive: true,
		},
	}
	if _, err := manager.Start(job); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Stop("stream-01"); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(2 * time.Second)
	for {
		status, err := manager.Status("stream-01")
		if err != nil {
			t.Fatal(err)
		}
		if status.Status == "completed" {
			if status.Archive["final_mp4"] != "final.mp4" {
				t.Fatalf("final_mp4 missing from completed snapshot: %#v", status)
			}
			if !packager.calledWith(job.StreamID) {
				t.Fatalf("packager was not called with stream id: %#v", packager.jobs)
			}
			if got, ok := packager.jobWith(job.StreamID); !ok || got.ArchiveConfig.FolderID != "drive-folder-id" || !got.ArchiveConfig.SharedDrive {
				t.Fatalf("archive config was not forwarded to packager: %#v", got.ArchiveConfig)
			}
			if !reporter.has("archive.package.started") || !reporter.has("archive.package.completed") {
				t.Fatalf("missing package signals: %#v", reporter.names())
			}
			for _, metric := range []string{"archive.package_status", "archive.final_mp4_exists", "recorder.remux_duration_ms", "gdrive.upload_status", "gdrive.upload_retry_count", "gdrive.upload_duration_sec", "gdrive.upload_file_count", "gdrive.upload_folder_fingerprint_present", "gdrive.upload_final_mp4_fingerprint_present", "gdrive.upload_metadata_fingerprint_present"} {
				if !reporter.has(metric) {
					t.Fatalf("missing package metric %s: %#v", metric, reporter.names())
				}
			}
			if !artifactReporter.calledWith(job.StreamID, "final.mp4") {
				t.Fatalf("archive artifacts were not reported: %#v", artifactReporter.calls)
			}
			if !reporter.has("archive.artifact_report.completed") {
				t.Fatalf("missing artifact report completion signal: %#v", reporter.names())
			}
			return
		}
		select {
		case <-deadline:
			t.Fatalf("archive was not packaged: %#v", status)
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}
}

func TestManagerArtifactReportFailureDoesNotFailCompletedArchive(t *testing.T) {
	reporter := &fakeReporter{}
	artifactReporter := &fakeArtifactReporter{err: errors.New("control panel unavailable")}
	manager := &Manager{
		ArchiveRoot:         t.TempDir(),
		FFmpegBin:           "ffmpeg",
		Starter:             &fakeStarter{},
		Reporter:            reporter,
		ArtifactReporter:    artifactReporter,
		Packager:            &fakePackager{root: t.TempDir()},
		InputResolver:       testInputResolver,
		AllowHostnameInputs: true,
	}
	job := lifecycle.StreamJob{
		StreamID: "stream-01", Name: "Morning Stream", InputURL: "srt://input.example.com:9000", RTMPURL: "rtmps://youtube.example.com/live2", StreamKey: "key",
		StartedAt: time.Date(2026, 5, 31, 1, 2, 3, 0, time.UTC),
	}
	if _, err := manager.Start(job); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Stop(job.StreamID); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(2 * time.Second)
	for {
		status, err := manager.Status(job.StreamID)
		if err != nil {
			t.Fatal(err)
		}
		if status.Status == "completed" {
			if !reporter.has("archive.artifact_report.failed") {
				t.Fatalf("missing artifact report failure signal: %#v", reporter.names())
			}
			event, ok := reporter.find("archive.artifact_report.failed")
			if !ok {
				t.Fatal("artifact report failure event was not recorded")
			}
			if _, leaked := event.Attributes["error"]; leaked {
				t.Fatalf("raw report error leaked: %#v", event.Attributes)
			}
			return
		}
		select {
		case <-deadline:
			t.Fatalf("artifact report failure changed archive completion: %#v", status)
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}
}

func TestManagerPackageFailureDoesNotReportGDriveUploadFailed(t *testing.T) {
	reporter := &fakeReporter{}
	packager := &fakePackager{root: t.TempDir(), err: errors.New("remux failed")}
	manager := &Manager{ArchiveRoot: t.TempDir(), FFmpegBin: "ffmpeg", Starter: &fakeStarter{}, Reporter: reporter, Packager: packager, InputResolver: testInputResolver, AllowHostnameInputs: true}
	job := lifecycle.StreamJob{
		StreamID: "stream-01", Name: "Morning Stream", InputURL: "srt://input.example.com:9000", RTMPURL: "rtmps://youtube.example.com/live2", StreamKey: "key",
		StartedAt: time.Date(2026, 5, 31, 1, 2, 3, 0, time.UTC),
	}
	if _, err := manager.Start(job); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Stop("stream-01"); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(2 * time.Second)
	for {
		status, err := manager.Status("stream-01")
		if err != nil {
			t.Fatal(err)
		}
		if status.Status == "package_failed" {
			if !reporter.has("archive.package_status") || !reporter.has("archive.package.failed") {
				t.Fatalf("missing package failure signals: %#v", reporter.names())
			}
			if reporter.has("gdrive.upload_status") {
				t.Fatalf("package failure must not be reported as gdrive upload failure: %#v", reporter.names())
			}
			return
		}
		select {
		case <-deadline:
			t.Fatalf("archive failure was not reported: %#v", status)
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}
}

func TestManagerPackageTimeoutStopsHungPackaging(t *testing.T) {
	reporter := &fakeReporter{}
	packager := &fakePackager{root: t.TempDir(), delay: time.Second}
	manager := &Manager{
		ArchiveRoot:         t.TempDir(),
		FFmpegBin:           "ffmpeg",
		Starter:             &fakeStarter{},
		Reporter:            reporter,
		Packager:            packager,
		PackageTimeout:      20 * time.Millisecond,
		InputResolver:       testInputResolver,
		AllowHostnameInputs: true,
	}
	job := lifecycle.StreamJob{
		StreamID: "stream-01", Name: "Morning Stream", InputURL: "srt://input.example.com:9000", RTMPURL: "rtmps://youtube.example.com/live2", StreamKey: "key",
		StartedAt: time.Date(2026, 5, 31, 1, 2, 3, 0, time.UTC),
	}
	if _, err := manager.Start(job); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Stop("stream-01"); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(2 * time.Second)
	for {
		status, err := manager.Status("stream-01")
		if err != nil {
			t.Fatal(err)
		}
		if status.Status == "package_failed" {
			if !reporter.has("archive.package.failed") {
				t.Fatalf("missing timeout failure signal: %#v", reporter.names())
			}
			return
		}
		select {
		case <-deadline:
			t.Fatalf("hung package operation was not timed out: %#v", status)
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}
}

func TestManagerUploadFailureReportsGDriveFailureWithoutRawError(t *testing.T) {
	reporter := &fakeReporter{}
	packager := &fakePackager{root: t.TempDir(), err: lifecycle.PackageError{Phase: "upload", Err: errors.New("https://example.com/upload?token=secret")}}
	manager := &Manager{ArchiveRoot: t.TempDir(), FFmpegBin: "ffmpeg", Starter: &fakeStarter{}, Reporter: reporter, Packager: packager, InputResolver: testInputResolver, AllowHostnameInputs: true}
	job := lifecycle.StreamJob{
		StreamID: "stream-01", Name: "Morning Stream", InputURL: "srt://input.example.com:9000", RTMPURL: "rtmps://youtube.example.com/live2", StreamKey: "key",
		StartedAt: time.Date(2026, 5, 31, 1, 2, 3, 0, time.UTC),
	}
	if _, err := manager.Start(job); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Stop("stream-01"); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(2 * time.Second)
	for {
		status, err := manager.Status("stream-01")
		if err != nil {
			t.Fatal(err)
		}
		if status.Status == "package_failed" {
			if !reporter.hasValue("archive.package_status", 1) || !reporter.hasValue("gdrive.upload_status", 0) {
				t.Fatalf("expected upload failure metrics, got %#v", reporter.signalsSnapshot())
			}
			event, ok := reporter.find("archive.package.failed")
			if !ok {
				t.Fatalf("missing package failure event: %#v", reporter.names())
			}
			if _, leaked := event.Attributes["error"]; leaked || strings.Contains(anyMapString(event.Attributes), "secret") {
				t.Fatalf("raw failure detail leaked in event attributes: %#v", event.Attributes)
			}
			if event.Attributes["failure_phase"] != "upload" || event.Attributes["error_class"] != "archive_upload_failed" {
				t.Fatalf("unexpected failure attributes: %#v", event.Attributes)
			}
			return
		}
		select {
		case <-deadline:
			t.Fatalf("archive failure was not reported: %#v", status)
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}
}

type fakeReporter struct {
	mu      sync.Mutex
	signals []observability.Signal
}

type fakePackager struct {
	mu    sync.Mutex
	root  string
	err   error
	delay time.Duration
	jobs  []lifecycle.PackageJob
}

type artifactReportCall struct {
	streamID  string
	artifacts []control.Artifact
}

type fakeArtifactReporter struct {
	mu    sync.Mutex
	err   error
	calls []artifactReportCall
}

func (r *fakeArtifactReporter) ReportArtifacts(ctx context.Context, streamID string, artifacts []control.Artifact) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, artifactReportCall{
		streamID:  streamID,
		artifacts: append([]control.Artifact(nil), artifacts...),
	})
	return r.err
}

func (r *fakeArtifactReporter) calledWith(streamID, name string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, call := range r.calls {
		if call.streamID != streamID {
			continue
		}
		for _, artifact := range call.artifacts {
			if artifact.Name == name && artifact.RelativePath == "final/"+streamID+"/"+name {
				return true
			}
		}
	}
	return false
}

func (p *fakePackager) Package(ctx context.Context, job lifecycle.PackageJob) (lifecycle.Result, error) {
	if err := ctx.Err(); err != nil {
		return lifecycle.Result{}, err
	}
	p.mu.Lock()
	p.jobs = append(p.jobs, job)
	p.mu.Unlock()
	if p.delay > 0 {
		select {
		case <-ctx.Done():
			return lifecycle.Result{}, ctx.Err()
		case <-time.After(p.delay):
		}
	}
	if p.err != nil {
		return lifecycle.Result{}, p.err
	}
	layout, err := archive.NewLayout(filepath.Join(p.root, "archives"), job.StreamID)
	if err != nil {
		return lifecycle.Result{}, err
	}
	if err := os.MkdirAll(layout.FinalDir(), 0o750); err != nil {
		return lifecycle.Result{}, err
	}
	if err := os.WriteFile(layout.FinalMP4(), []byte("packaged video"), 0o640); err != nil {
		return lifecycle.Result{}, err
	}
	if err := os.WriteFile(layout.FinalMetadata(), []byte(`{"stream_id":"`+job.StreamID+`"}`+"\n"), 0o640); err != nil {
		return lifecycle.Result{}, err
	}
	return lifecycle.Result{
		Layout: layout,
		Metadata: lifecycle.Metadata{
			StreamID: job.StreamID,
			Name:     job.Name,
			Upload: archive.UploadResult{
				DryRun:   true,
				FolderID: "dry-run-folder",
				Attempts: 1,
				FileIDs:  map[string]string{"final.mp4": "dry-run-file", "metadata.json": "dry-run-file"},
			},
		},
		RemuxDurationMS: 125,
	}, nil
}

func (p *fakePackager) calledWith(streamID string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, job := range p.jobs {
		if job.StreamID == streamID {
			return true
		}
	}
	return false
}

func (p *fakePackager) jobWith(streamID string) (lifecycle.PackageJob, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, job := range p.jobs {
		if job.StreamID == streamID {
			return job, true
		}
	}
	return lifecycle.PackageJob{}, false
}

func (r *fakeReporter) Report(ctx context.Context, signal observability.Signal) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.signals = append(r.signals, signal)
	return nil
}

func (r *fakeReporter) has(name string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, signal := range r.signals {
		if signal.Name == name {
			return true
		}
	}
	return false
}

func (r *fakeReporter) hasValueAtLeast(name string, min float64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, signal := range r.signals {
		if signal.Name == name && signal.Value != nil && *signal.Value >= min {
			return true
		}
	}
	return false
}

func (r *fakeReporter) hasValue(name string, want float64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, signal := range r.signals {
		if signal.Name == name && signal.Value != nil && *signal.Value == want {
			return true
		}
	}
	return false
}

func (r *fakeReporter) find(name string) (observability.Signal, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, signal := range r.signals {
		if signal.Name == name {
			return signal, true
		}
	}
	return observability.Signal{}, false
}

func (r *fakeReporter) signalsSnapshot() []observability.Signal {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]observability.Signal(nil), r.signals...)
}

func (r *fakeReporter) names() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.signals))
	for _, signal := range r.signals {
		out = append(out, signal.Name)
	}
	return out
}

func anyMapString(items map[string]any) string {
	out := ""
	for key, value := range items {
		out += key + "=" + valueString(value) + ";"
	}
	return out
}

func valueString(value any) string {
	if text, ok := value.(string); ok {
		return text
	}
	return ""
}
