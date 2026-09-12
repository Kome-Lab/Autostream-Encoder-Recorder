package streamproc

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/example/autostream-encoder-recorder/internal/archive"
	"github.com/example/autostream-encoder-recorder/internal/lifecycle"
	"github.com/example/autostream-encoder-recorder/internal/outputrelay"
)

func TestManagerStartWritesMetadataAndMasksStreamKey(t *testing.T) {
	root := t.TempDir()
	starter := &fakeStarter{}
	manager := &Manager{ArchiveRoot: root, FFmpegBin: "ffmpeg", Starter: starter, InputResolver: testInputResolver, AllowHostnameInputs: true, OutputRelayURL: "rtmp://127.0.0.1/autostream/{stream_id}", OutputRelayMode: outputrelay.ModeManagedLiveAPI, OutputRelayBindingID: staticRelayBindingID}
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
	for _, want := range []string{"f=hls", "onfail=ignore", "hls_time=2", "hls_list_size=0", "independent_segments", "segment-%06d.ts"} {
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
	for _, want := range []string{`"youtube_output_mode": "live_api_relay_static"`, `"output_route": "local_relay"`} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("safe output route diagnostic %q missing from metadata: %s", want, string(body))
		}
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
		StreamKey: "direct-secret-stream-key", YouTubeOutputMode: "stream_key",
	}, Snapshot{StartedAtJST: "2026-06-08T12:00:00+09:00", Archive: map[string]string{"recording_mkv": "final.mkv"}}, args, "ffmpeg", "direct")
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
	}, Snapshot{StartedAtJST: "2026-06-08T12:00:00+09:00", Archive: map[string]string{"recording_mkv": "final.mkv"}}, []string{"-f", "lavfi"}, "ffmpeg", "direct")
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
	manager := &Manager{ArchiveRoot: root, FFmpegBin: "ffmpeg", Starter: starter, InputResolver: testInputResolver, AllowHostnameInputs: true, OutputRelayMode: outputrelay.ModeDirect}
	_, err = manager.Start(lifecycle.StreamJob{
		StreamID:  "stream-01",
		Name:      "Morning Stream",
		InputURL:  "srt://input.example.com:9000",
		RTMPURL:   "rtmps://youtube.example.com/live2",
		StreamKey: "key", YouTubeOutputMode: "stream_key",
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
	manager := &Manager{ArchiveRoot: root, FFmpegBin: "ffmpeg", Starter: starter, InputResolver: testInputResolver, AllowHostnameInputs: true, OutputRelayMode: outputrelay.ModeDirect}
	_, err = manager.Start(lifecycle.StreamJob{
		StreamID:  "stream-01",
		Name:      "Morning Stream",
		InputURL:  "srt://input.example.com:9000",
		RTMPURL:   "rtmps://youtube.example.com/live2",
		StreamKey: "key", YouTubeOutputMode: "stream_key",
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
	manager := &Manager{ArchiveRoot: root, FFmpegBin: "ffmpeg", Starter: starter, InputResolver: testInputResolver, AllowHostnameInputs: true, OutputRelayMode: outputrelay.ModeDirect}
	_, err = manager.Start(lifecycle.StreamJob{
		StreamID:  "stream-01",
		Name:      "Morning Stream",
		InputURL:  "srt://input.example.com:9000",
		RTMPURL:   "rtmps://youtube.example.com/live2",
		StreamKey: "key", YouTubeOutputMode: "stream_key",
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
	manager := &Manager{ArchiveRoot: root, FFmpegBin: "ffmpeg", Starter: starter, InputResolver: testInputResolver, AllowHostnameInputs: true, OutputRelayMode: outputrelay.ModeDirect}
	_, err := manager.Start(lifecycle.StreamJob{
		StreamID:  "stream-01",
		Name:      "Morning Stream",
		InputURL:  "srt://input.example.com:9000",
		RTMPURL:   "rtmps://youtube.example.com/live2",
		StreamKey: "key", YouTubeOutputMode: "stream_key",
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
